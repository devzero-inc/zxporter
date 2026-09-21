package redmetrics

import (
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/timestamppb"
	"inet.af/netaddr"
	corev1 "k8s.io/api/core/v1"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
)

// DurationBucketCount is the number of cumulative latency buckets carried by one
// emitted item. The server REJECTS THE WHOLE REQUEST when an item carries any
// other number, on the grounds that a length mismatch means the two sides no
// longer agree about what the buckets mean.
const DurationBucketCount = 14

// PodLookup resolves a local pod by its IP.
//
// It is declared here rather than imported because the concrete cache lives in
// the parent package, which already depends on this one. The one method is
// satisfied structurally, so no wiring is needed at the call site.
type PodLookup interface {
	GetPodByIP(ip netaddr.IP) (*corev1.Pod, bool)
}

// Config tunes the aggregator's memory ceilings and how long it waits for an
// answer. None of them changes what is measured, only how much of it the agent is
// willing to hold and how long it holds it.
type Config struct {
	// ConnTTL is how long a connection's state survives without an observation.
	//
	// It is a leak backstop rather than the mechanism that reclaims connections. The
	// The probe attaches to both ways a connection ends, so this bounds only the
	// entries whose close was never seen -- an event lost to a full ring buffer, or a
	// socket already open when the probe attached.
	//
	// Which is why it can be generous, and why it should be. When idleness was the
	// only available death signal this had to be short enough to bound the table,
	// and a short one is actively destructive: a gRPC stream or an HTTP keep-alive
	// connection quiet for longer than the TTL was swept, then decoded again from
	// the middle when it resumed -- where nothing parses, so it landed opaque -- or,
	// if its next syscall was a send, classified outbound and ignored for the rest
	// of its life. Neither is a small loss on a node serving long-lived connections.
	//
	// Keep it at or above RequestTTL. Below it, a connection can be reclaimed while
	// it still holds requests the sweep has not yet had cause to file, and those
	// requests are lost rather than counted unanswered.
	ConnTTL time.Duration

	// MaxConnections caps concurrently tracked connections. Beyond it, new
	// connections are dropped rather than tracked -- the alternative is an
	// unbounded map on a DaemonSet that shares a node with the workloads it
	// measures.
	//
	// Outbound connections count against it too, even though nothing is measured
	// about them. The cap bounds memory, and an outbound entry occupies the table
	// exactly as an inbound one does; excluding them would make the number stop
	// describing the thing it exists to bound. On a node whose outbound churn is
	// large enough to fill the table this does cost inbound coverage, which the
	// one-shot capacity log is there to make visible.
	//
	// What the cap holds changed with the close probe. It used to hold every
	// connection seen within ConnTTL, which on a node churning short-lived
	// connections is overwhelmingly sockets the kernel had already freed -- so the
	// table filled with corpses and refused live connections at a fraction of the
	// offered rate. It now holds connections that are actually open, which is a
	// number set by concurrency rather than by rate.
	MaxConnections int

	// RequestTTL is how long a request may stay outstanding before it is filed as
	// unanswered: counted in request_count, in no latency bucket, and not in
	// error_count.
	//
	// It is a deadline on the agent's patience, not a statement about the service.
	// A request answered after it has been swept is not filed twice -- the response
	// finds nothing outstanding and is dropped -- so raising this trades memory for
	// the latency of genuinely slow requests being measured rather than merely
	// counted, and lowering it trades the other way.
	//
	// It applies only to a request left outstanding on a connection that stays open.
	// A connection that closes settles everything it was still holding at the close,
	// so this does not have to be long enough to cover a client that hangs up.
	RequestTTL time.Duration

	// MaxInFlightRequests caps outstanding requests across all connections. A
	// request arriving with no room takes its connection opaque; see
	// startRequestLocked for why the connection cannot simply skip it.
	MaxInFlightRequests int

	// MaxBufferedBytes caps the bytes held over across socket events, across all
	// connections, for units that arrived split. It exists because the per-buffer
	// cap alone does not bound the agent: MaxConnections connections each holding
	// two halves of one legitimately-split frame is a ceiling in the gigabytes, on
	// a DaemonSet that shares a node with the workloads it measures.
	//
	// A connection that cannot get room takes itself opaque rather than resuming
	// from the wrong offset, so exhausting this degrades coverage into conn_count
	// instead of degrading the node.
	MaxBufferedBytes int
}

// DefaultConfig returns the shipped ceilings.
func DefaultConfig() Config {
	return Config{
		ConnTTL:             10 * time.Minute,
		MaxConnections:      16384,
		RequestTTL:          15 * time.Second,
		MaxInFlightRequests: 16384,
		MaxBufferedBytes:    8 << 20,
	}
}

// connKey identifies one connection for as long as the aggregator tracks it.
//
// The socket cookie alone will not do. It is a kernel socket address, unique only
// among sockets alive at the same instant, and the kernel recycles those
// structures aggressively -- a node churning short-lived connections hands the
// same address to a new connection well inside the TTL. Keyed on the cookie
// alone, that new connection would be mistaken for the old one, inheriting its
// decode position and its outstanding requests: not merely undercounted, but
// paired wrongly.
//
// Pairing the cookie with the tuple it served closes that: a recycled socket
// serving a different peer is a different key and starts clean. What remains is a
// socket recycled onto the identical four-tuple within the TTL, which requires
// the peer to reuse its ephemeral port too, and which is the same logical client
// reconnecting in any case.
type connKey struct {
	cookie     uint64
	localAddr  netip.Addr
	localPort  uint16
	remoteAddr netip.Addr
	remotePort uint16
}

func connKeyOf(ev SocketEvent) connKey {
	return connKey{
		cookie:     ev.SocketCookie,
		localAddr:  ev.LocalAddr,
		localPort:  ev.LocalPort,
		remoteAddr: ev.RemoteAddr,
		remotePort: ev.RemotePort,
	}
}

// maxListeningPorts bounds the set of local endpoints remembered as inbound.
//
// It is a constant rather than a Config knob because there is no deployment whose
// answer differs: the set holds one entry per local endpoint this node serves on,
// which is a number set by how many listening containers the kubelet is running,
// not by traffic. Four thousand of them is already far past any node's reality.
//
// Overflowing it costs nothing that was being measured. A connection whose port is
// not in the set is treated exactly as it was before the set existed -- outbound,
// ignored -- so the only loss is the chance to re-acquire a connection that was
// already open when the agent started.
const maxListeningPorts = 4096

// listenKey is a local endpoint this node has been observed receiving on.
//
// The address is part of it because a port on its own says nothing: 8080 on one
// pod is a server and 8080 on another is whatever the kernel handed out. Pod IPs
// are recycled, so an entry outliving its pod could match a new pod's ephemeral
// port -- which is why entries expire on the same TTL as connections rather than
// living for the process's lifetime.
type listenKey struct {
	addr netip.Addr
	port uint16
}

// bucketKey is the aggregation key: a minute and a container, deliberately not a
// peer. Keying on the caller is exactly what would make this map grow with the
// number of distinct clients rather than with the number of containers the
// kubelet is running.
type bucketKey struct {
	minute    time.Time
	container ContainerKey
}

// counters accumulates one bucket's deltas.
//
// Every field is additive across agents and across minutes, which is the whole
// reason the table can be a SummingMergeTree -- except durationMaxUs, which merges
// as a maximum, and the histogram, whose buckets are additive one by one.
type counters struct {
	// requestCount counts every request observed, answered or not.
	requestCount uint64

	// errorCount counts answered requests whose status was a 5XX.
	errorCount uint64

	// durationSumUs and hist cover ANSWERED requests only. An unanswered request
	// has no duration to add, which is why the mean is this sum over the last
	// histogram bucket rather than over requestCount.
	durationSumUs uint64
	hist          histogram

	// durationMaxUs is the slowest answered request in the bucket.
	durationMaxUs uint64

	// connCount counts connections that could not be decoded, at the moment they
	// were given up on. A connection decoded cleanly for its whole life contributes
	// nothing here; one that decoded for a while and then desynchronised contributes
	// to this AND to requestCount, for the requests it did read.
	connCount uint64
}

// Aggregator turns socket observations into per-container, per-minute items.
//
// Its output is deltas. A bucket closes when the wall clock passes the minute it
// covers, is emitted exactly once, and is never reopened -- an observation
// arriving for a closed minute is dropped rather than filed late, because a late
// row is indistinguishable from real extra traffic once the SummingMergeTree has
// merged it. For the same reason nothing that consumes Flush may retry.
type Aggregator struct {
	cfg   Config
	clock Clock
	pods  PodLookup
	log   logr.Logger

	mu      sync.Mutex
	conns   map[connKey]*connection
	buckets map[bucketKey]*counters

	// listening is the local endpoints seen receiving before they sent, with the
	// last time each was seen. It is how a connection that was already open when
	// the agent started is recognised as inbound; see trackLocked.
	listening map[listenKey]time.Time

	// inFlight is the total outstanding requests across all connections, kept as a
	// running count because MaxInFlightRequests is a bound on the agent's memory
	// rather than on any one connection.
	inFlight int

	// buffered is the total bytes held over across socket events, for the same
	// reason: the bound that matters is the agent's, not one connection's.
	buffered int

	// closedBefore is the first minute that has NOT been emitted. Everything
	// earlier is final and gone.
	closedBefore time.Time

	// capacityLogged keeps the connection-cap warning to one line per process
	// rather than one per dropped connection.
	capacityLogged bool
}

// NewAggregator builds an aggregator. A zero-valued Config field takes the
// default, so callers can override one knob without restating the others.
func NewAggregator(cfg Config, clock Clock, pods PodLookup, log logr.Logger) *Aggregator {
	defaults := DefaultConfig()
	if cfg.ConnTTL <= 0 {
		cfg.ConnTTL = defaults.ConnTTL
	}
	if cfg.MaxConnections <= 0 {
		cfg.MaxConnections = defaults.MaxConnections
	}
	if cfg.RequestTTL <= 0 {
		cfg.RequestTTL = defaults.RequestTTL
	}
	if cfg.MaxInFlightRequests <= 0 {
		cfg.MaxInFlightRequests = defaults.MaxInFlightRequests
	}
	if cfg.MaxBufferedBytes <= 0 {
		cfg.MaxBufferedBytes = defaults.MaxBufferedBytes
	}
	if clock == nil {
		clock = SystemClock{}
	}

	return &Aggregator{
		cfg:       cfg,
		clock:     clock,
		pods:      pods,
		log:       log,
		conns:     make(map[connKey]*connection),
		buckets:   make(map[bucketKey]*counters),
		listening: make(map[listenKey]time.Time),
	}
}

// Observe files one socket observation. It is safe for concurrent use and is on
// the probe's hot path, so the expensive work -- pod lookup and port
// attribution -- happens once per connection and never again.
func (a *Aggregator) Observe(ev SocketEvent) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := connKeyOf(ev)
	conn, ok := a.conns[key]

	// Handled before the capacity gate below, and before the ignored/opaque return
	// further down. A close is not an observation of traffic: it must not consume an
	// admission slot, and it has to be acted on for a connection whose decoding has
	// already been given up, because releasing that state is the whole point.
	if ev.Path == SyscallClose {
		if ok {
			a.closeLocked(key, conn, ev.Timestamp)
		}
		return
	}

	if !ok {
		if len(a.conns) >= a.cfg.MaxConnections {
			if !a.capacityLogged {
				a.capacityLogged = true
				a.log.Info("container RED metrics connection table full, dropping new connections",
					"max_connections", a.cfg.MaxConnections)
			}
			return
		}
		conn = a.trackLocked(ev)
		a.conns[key] = conn
	}
	conn.lastSeen = ev.Timestamp

	// An ignored connection has nothing to measure and an opaque one has nothing
	// left to read; both are kept in the table so that neither conclusion is reached
	// twice, and so a resumed connection is not counted again.
	if conn.state == stateIgnored || conn.state == stateOpaque {
		return
	}

	a.decodeLocked(conn, ev)
}

// trackLocked decides, once, what a newly seen connection is worth measuring.
//
// Bytes arriving before any were sent mean this node is answering a request; the
// other way round usually means it is making one, and outbound connections are
// somebody else's inbound, measured on the node that serves them.
//
// Usually, but not always -- and the exception is the reason this is not simply a
// test of the first syscall. The first syscall is the first one THIS AGENT SAW,
// which for a connection already open when the agent started has nothing to do with
// who opened it. A server holding long-lived keep-alive connections restarts its
// agent and every one of those connections is written off as outbound at whatever
// syscall happened to arrive first, permanently: the state is kept precisely so the
// decision is never revisited, and the connections outlive the process. What that
// looks like from the outside is a service that stops reporting traffic after an
// agent upgrade and does not start again until its clients reconnect.
//
// The discriminator that survives a restart is the local port. An inbound
// connection is served on the port the container listens on, which every other
// inbound connection to that container shares; an outbound one goes out on an
// ephemeral port belonging to that connection alone. So a send-first connection on
// a port this node has been seen RECEIVING on is a connection the agent joined
// late, not one this node opened, and it is picked up rather than written off.
//
// The set of such ports is learned from the recv-first connections themselves
// rather than read out of the pod spec. containerPort is advisory in Kubernetes --
// a container that declares nothing still listens perfectly well -- and consulting
// the spec would mean a pod lookup for every outbound connection on the node, which
// is a real cost on the agent's hot path to answer a question the traffic answers
// by itself within one request.
func (a *Aggregator) trackLocked(ev SocketEvent) *connection {
	// stateNew is the zero value, and is named here rather than left implicit
	// because it is the only place the decision is made: everything below is a reason
	// NOT to decode, so a connection that reaches the end of this function is one the
	// decoder will read -- as soon as its first bytes say which protocol to read it
	// as.
	conn := &connection{lastSeen: ev.Timestamp, state: stateNew}

	if ev.Path == SyscallRecv {
		container, ok := a.attribute(ev)
		if !ok {
			conn.state = stateIgnored
			return conn
		}
		conn.container = container

		// Recorded only once attribution has succeeded. An endpoint nothing can be
		// filed against is no use to a later connection either, and admitting it
		// would spend the set's capacity on ports that cannot produce a measurement.
		a.learnListeningLocked(ev)
		return conn
	}

	// A send on a port nothing has been received on. That is what an outbound
	// connection looks like, and it is by far the common case -- so this returns
	// before the pod lookup rather than after it.
	if !a.isListeningLocked(ev) {
		conn.state = stateIgnored
		return conn
	}

	container, ok := a.attribute(ev)
	if !ok {
		conn.state = stateIgnored
		return conn
	}

	conn.container = container
	conn.state = stateJoined
	return conn
}

// learnListeningLocked remembers that this node received on a local endpoint. The
// caller holds the lock.
//
// The timestamp is refreshed on every inbound connection, so an endpoint stays
// known for as long as it is being served and expires only once it stops.
func (a *Aggregator) learnListeningLocked(ev SocketEvent) {
	key := listenKey{addr: ev.LocalAddr, port: ev.LocalPort}
	if _, ok := a.listening[key]; !ok && len(a.listening) >= maxListeningPorts {
		return
	}
	a.listening[key] = ev.Timestamp
}

// isListeningLocked reports whether an event's local endpoint is one this node has
// been seen receiving on. The caller holds the lock.
func (a *Aggregator) isListeningLocked(ev SocketEvent) bool {
	_, ok := a.listening[listenKey{addr: ev.LocalAddr, port: ev.LocalPort}]
	return ok
}

// attribute resolves the container serving an inbound connection, reporting
// false when it cannot be established. Failure is silent by design: there is no
// loss counter in this scope, and a guess would file one workload's traffic
// against another workload's resource recommendations.
//
// It touches no mutable aggregator state -- only the pod cache, which has its own
// synchronisation -- so it carries no Locked suffix and may be called with or
// without the aggregator's lock held.
func (a *Aggregator) attribute(ev SocketEvent) (ContainerKey, bool) {
	if a.pods == nil {
		return ContainerKey{}, false
	}

	ip, ok := toNetaddr(ev.LocalAddr)
	if !ok {
		return ContainerKey{}, false
	}

	pod, ok := a.pods.GetPodByIP(ip)
	if !ok || pod == nil {
		return ContainerKey{}, false
	}

	name, ok := containerForPort(pod, ev.LocalPort)
	if !ok {
		return ContainerKey{}, false
	}

	return ContainerKey{
		PodName:       pod.Name,
		PodNamespace:  pod.Namespace,
		PodUID:        string(pod.UID),
		ContainerName: name,
	}, true
}

// countersLocked is the bucket a measurement taken at this instant belongs in, or
// nil when that minute has already been emitted.
//
// EVERY path that files anything goes through here. The frontier check cannot live
// at the call sites: a measurement's minute is the minute it BECAME FINAL, and the
// sweep files requests under a deadline that has already passed, so "now" is not a
// safe proxy for "not yet closed" anywhere in this file.
func (a *Aggregator) countersLocked(at time.Time, container ContainerKey) *counters {
	minute := at.Truncate(time.Minute)
	if !a.closedBefore.IsZero() && minute.Before(a.closedBefore) {
		return nil
	}

	key := bucketKey{minute: minute, container: container}
	bucket, ok := a.buckets[key]
	if !ok {
		bucket = &counters{}
		a.buckets[key] = bucket
	}
	return bucket
}

// fileAnsweredLocked records one request that was observed completing, against the
// minute it completed in.
//
// It is the counterpart of fileUnansweredLocked, and the pair is two functions
// rather than one with an answered flag because the flag made the caller pass a
// duration and a status that would be ignored: an unanswered request was filed as
// (0, 0, false), a triple that reads as a request answered instantly with status
// zero. Splitting them makes that state unrepresentable.
//
// It takes an error BIT rather than a status because the two protocols disagree
// about what an error is. On HTTP/1.1 it is a 5XX. On HTTP/2 it is a 5XX or a
// non-zero grpc-status on a response that said 200, and there is no status value
// that expresses the second -- a caller reducing it to a fake 500 would be filing a
// number the server never sent.
func (a *Aggregator) fileAnsweredLocked(c *connection, at time.Time, durationUs int64, isError bool) {
	bucket := a.countersLocked(at, c.container)
	if bucket == nil {
		return
	}

	bucket.requestCount++

	if isError {
		bucket.errorCount++
	}

	us := uint64(durationUs)
	bucket.durationSumUs += us
	if us > bucket.durationMaxUs {
		bucket.durationMaxUs = us
	}
	bucket.hist.observe(durationUs)
}

// fileUnansweredLocked records one request the agent gave up waiting for, against
// the minute it was given up in.
//
// An unanswered request is a request that happened, so it counts -- but it has no
// duration and no status, so it enters no latency bucket and cannot be an error.
// That asymmetry is deliberate and is why request_count and the last latency bucket
// are allowed to disagree.
func (a *Aggregator) fileUnansweredLocked(c *connection, at time.Time) {
	bucket := a.countersLocked(at, c.container)
	if bucket == nil {
		return
	}
	bucket.requestCount++
}

// Flush closes every minute the wall clock has passed and returns them as items,
// holding back the minute in progress. Buckets are removed as they are emitted,
// so a second call returns nothing new -- and a caller that drops the result
// loses those measurements, which is the correct trade against re-sending them.
func (a *Aggregator) Flush() []*gen.ContainerRedMetricsItem {
	a.mu.Lock()
	defer a.mu.Unlock()

	// The clock is read under the lock so that two concurrent flushes cannot
	// interleave a stale reading with a fresh one. Reading it first let the
	// goroutine holding the earlier timestamp win the race to assign
	// closedBefore, moving the frontier backwards and reopening a minute that
	// had already been emitted -- a double count no test upstream could see.
	now := a.clock.Now()
	current := now.Truncate(time.Minute)

	// Swept BEFORE the buckets are collected, not after. The sweep files timed-out
	// requests under a deadline already in the past, so running it afterwards would
	// leave those requests sitting in a bucket this flush had just declared closed,
	// where the next flush would refuse to emit them.
	a.sweepLocked(now)

	items := make([]*gen.ContainerRedMetricsItem, 0, len(a.buckets))
	for key, bucket := range a.buckets {
		if !key.minute.Before(current) {
			continue
		}
		items = append(items, &gen.ContainerRedMetricsItem{
			PodName:         key.container.PodName,
			PodNamespace:    key.container.PodNamespace,
			PodUid:          key.container.PodUID,
			ContainerName:   key.container.ContainerName,
			Timestamp:       timestamppb.New(key.minute),
			RequestCount:    bucket.requestCount,
			ErrorCount:      bucket.errorCount,
			DurationSumUs:   bucket.durationSumUs,
			DurationMaxUs:   bucket.durationMaxUs,
			ConnCount:       bucket.connCount,
			DurationBuckets: bucket.hist.cumulative(),
		})
		delete(a.buckets, key)
	}

	// Monotonic by construction, for the same reason: the frontier is a claim that
	// everything earlier is final, and a claim like that can only ever move forward.
	if current.After(a.closedBefore) {
		a.closedBefore = current
	}

	// Map iteration order is random, so sort to make the emitted batch a function
	// of what was observed rather than of Go's hash seed. Tests depend on it and
	// so does anybody reading a log of what was sent.
	sort.Slice(items, func(i, j int) bool {
		l, r := items[i], items[j]
		if !l.Timestamp.AsTime().Equal(r.Timestamp.AsTime()) {
			return l.Timestamp.AsTime().Before(r.Timestamp.AsTime())
		}
		if l.PodNamespace != r.PodNamespace {
			return l.PodNamespace < r.PodNamespace
		}
		if l.PodName != r.PodName {
			return l.PodName < r.PodName
		}
		return l.ContainerName < r.ContainerName
	})

	return items
}

// sweepLocked files requests that have waited too long for an answer and drops
// connection state that has gone quiet for longer than the TTL. The caller holds
// the lock.
//
// The second half is a backstop. Connections are normally reclaimed by their close
// event, which arrives promptly and knows the connection is dead; what reaches the
// TTL is an entry whose close was missed, so ConnTTL can be long enough not to
// disturb connections that are merely idle.
func (a *Aggregator) sweepLocked(now time.Time) {
	cutoff := now.Add(-a.cfg.ConnTTL)

	// Expired on the same clock as connections, because the two answer the same
	// question. An endpoint is known to be listening on the evidence of connections
	// that arrived on it, and once those are old enough to be swept the evidence has
	// gone with them -- at which point the pod may be gone and its IP reissued.
	for key, at := range a.listening {
		if at.Before(cutoff) {
			delete(a.listening, key)
		}
	}

	for key, conn := range a.conns {
		a.sweepRequestsLocked(conn, now)
		if conn.lastSeen.Before(cutoff) {
			// Requests here are already impossible when ConnTTL >= RequestTTL, which
			// the config comment asks for: a connection idle that long has had every
			// one of its requests swept above. Under a shorter ConnTTL they are lost,
			// silently, along with the rest of the connection's state.
			a.inFlight -= len(conn.inflight)
			a.releaseBuffersLocked(conn)
			delete(a.conns, key)
		}
	}
}

// closeLocked releases a connection the kernel has said is going away, settling
// whatever it was still holding.
//
// The connection is deleted rather than kept as a tombstone. Nothing can arrive on
// the key afterwards. From tcp_close that is because the local file descriptor is
// closing, and the two syscall paths that produce events cannot be entered on a
// closed descriptor. From tcp_done the descriptor is still open and a read can
// still drain what arrived before the reset -- but the same teardown that prompted
// the close already released the local port, so those events carry a different key
// and land as a connection of their own, which its own tcp_close then reclaims.
// A later socket recycled onto the same kernel address and the same four-tuple is
// the same logical client reconnecting, and starting it clean is correct.
func (a *Aggregator) closeLocked(key connKey, c *connection, at time.Time) {
	// Anything already past its deadline is filed at that deadline, exactly as the
	// sweep would have filed it: the moment the agent gave up is when the
	// measurement became final, and that is not the moment the socket happened to
	// close.
	a.sweepRequestsLocked(c, at)

	// What is left was still legitimately outstanding, and the close is what ended
	// it -- a client that hung up mid-response, or a server that died with the
	// request on the floor. That is a request that happened and was not answered, so
	// it counts, with no duration and no status, on the same terms as one that timed
	// out. Waiting for RequestTTL to notice would only file it later and in the
	// wrong minute.
	for range c.inflight {
		a.fileUnansweredLocked(c, at)
	}
	a.inFlight -= len(c.inflight)
	c.inflight = nil

	a.releaseBuffersLocked(c)
	delete(a.conns, key)
}

// sweepRequestsLocked files one connection's timed-out requests as unanswered.
//
// Each is filed at ITS OWN deadline rather than at now: the deadline is the moment
// the agent gave up, which is the moment the measurement became final, and filing
// them all at the sweep's timestamp would pile a minute's worth of abandoned
// requests into whichever minute the flush happened to land in.
func (a *Aggregator) sweepRequestsLocked(c *connection, now time.Time) {
	// The list is in observation order, which is deadline order on both protocols:
	// every request gets the same TTL, so a request observed earlier expires earlier.
	// HTTP/2 completes streams out of order and so removes entries from the middle,
	// but removal preserves the order of what stays, so the expired ones are still a
	// prefix and the scan still stops at the first survivor.
	expired := 0
	for expired < len(c.inflight) {
		deadline := c.inflight[expired].start.Add(a.cfg.RequestTTL)
		if deadline.After(now) {
			break
		}
		a.fileUnansweredLocked(c, deadline)
		expired++
	}
	if expired == 0 {
		return
	}

	copy(c.inflight, c.inflight[expired:])
	c.inflight = c.inflight[:len(c.inflight)-expired]
	a.inFlight -= expired
}

// toNetaddr converts a netip.Addr to the netaddr.IP the pod cache is keyed by.
//
// The two libraries have no interop, so the conversion goes through the raw
// bytes. Unmapping first matters: a v4-mapped v6 address and the plain v4
// address are the same host but different map keys, and the cache stores what
// netaddr.ParseIP produced from the pod's status IP.
func toNetaddr(a netip.Addr) (netaddr.IP, bool) {
	a = a.Unmap()
	switch {
	case a.Is4():
		return netaddr.IPFrom4(a.As4()), true
	case a.Is6():
		return netaddr.IPFrom16(a.As16()), true
	default:
		return netaddr.IP{}, false
	}
}
