package redmetrics

import (
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"inet.af/netaddr"
	corev1 "k8s.io/api/core/v1"
)

// fakeClock lets a test move wall clock deliberately. Minute buckets are stamped by
// the agent, so every interesting property of this package is a statement about
// where a wall clock stood.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// fakePods is a pod lookup keyed the same way the real cache is.
type fakePods map[netaddr.IP]*corev1.Pod

func (p fakePods) GetPodByIP(ip netaddr.IP) (*corev1.Pod, bool) {
	got, ok := p[ip]
	return got, ok
}

const (
	testPodIP    = "10.0.0.7"
	testPodPort  = 8080
	testPodNS    = "shop"
	testPodName  = "web-0"
	testPodUID   = "uid-web-0"
	testAppName  = "app"
	otherPodNS   = "billing"
	otherPodName = "api-0"
)

func testPods() fakePods {
	served := pod([]corev1.Container{container(testAppName, testPodPort), container("proxy", 15001)}, nil)
	served.Name = testPodName
	served.Namespace = testPodNS
	served.UID = testPodUID
	return fakePods{netaddr.MustParseIP(testPodIP): served}
}

// recvAt builds an inbound observation on a given socket at a given instant.
func recvAt(cookie uint64, at time.Time) SocketEvent {
	return SocketEvent{
		SocketCookie: cookie,
		Path:         SyscallRecv,
		LocalAddr:    netip.MustParseAddr(testPodIP),
		LocalPort:    testPodPort,
		RemoteAddr:   netip.MustParseAddr("192.168.1.4"),
		RemotePort:   54321,
		TrueLen:      128,
		Timestamp:    at,
	}
}

// closeAt is the socket going away. It carries no bytes, so it is the one event
// that says something about a connection without observing any traffic on it.
func closeAt(cookie uint64, at time.Time) SocketEvent {
	ev := recvAt(cookie, at)
	ev.Path = SyscallClose
	ev.TrueLen = 0
	return ev
}

func newTestAggregator(clock Clock, pods PodLookup) *Aggregator {
	return NewAggregator(Config{}, clock, pods, logr.Discard())
}

func minuteAt(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", s, err)
	}
	return parsed
}

// TestDefaultConfigKeepsConnectionsAtLeastAsLongAsRequests pins the one relationship
// between two knobs that are otherwise independent. A connection reclaimed while it
// still holds an unfiled request loses that request instead of counting it
// unanswered, so the connection has to outlive the deadline of anything on it.
//
// The two moved in opposite directions when the close probe arrived -- the connection
// TTL became a leak backstop and grew, the request deadline shrank to get data to the
// control plane sooner -- which is exactly the kind of change that would breach this
// silently.
func TestDefaultConfigKeepsConnectionsAtLeastAsLongAsRequests(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ConnTTL < cfg.RequestTTL {
		t.Fatalf("ConnTTL = %s is shorter than RequestTTL = %s: a connection would be "+
			"reclaimed while still holding a request that was never filed",
			cfg.ConnTTL, cfg.RequestTTL)
	}
}

// TestAggregatorCountsConnectionsNotObservations pins what conn_count means. A busy
// connection carrying a thousand syscalls is still one connection, and counting
// syscalls would make the number a proxy for traffic volume instead.
func TestAggregatorCountsConnectionsNotObservations(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())

	for i := 0; i < 50; i++ {
		agg.Observe(recvAt(1, clock.Now()))
	}
	agg.Observe(recvAt(2, clock.Now()))

	clock.set(minuteAt(t, "2026-08-25T10:01:05Z"))
	items := agg.Flush()

	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].ConnCount != 2 {
		t.Fatalf("conn_count = %d, want 2", items[0].ConnCount)
	}
	if items[0].ContainerName != testAppName || items[0].PodName != testPodName ||
		items[0].PodNamespace != testPodNS || items[0].PodUid != testPodUID {
		t.Fatalf("attribution = %+v", items[0])
	}
	if got := items[0].Timestamp.AsTime(); !got.Equal(minuteAt(t, "2026-08-25T10:00:00Z")) {
		t.Fatalf("bucket minute = %v, want 10:00:00", got)
	}
}

// TestAggregatorAlwaysEmitsFullBucketLadder pins a contract with the server, which
// rejects the WHOLE request when an item carries any other number of buckets. This
// connection never decodes, so every entry is zero -- and they still have to be
// there, because the length is what says the two sides agree about what the buckets
// mean.
func TestAggregatorAlwaysEmitsFullBucketLadder(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())
	agg.Observe(recvAt(1, clock.Now()))

	clock.set(minuteAt(t, "2026-08-25T10:01:00Z"))
	items := agg.Flush()

	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if len(items[0].DurationBuckets) != DurationBucketCount {
		t.Fatalf("duration_buckets length = %d, want %d",
			len(items[0].DurationBuckets), DurationBucketCount)
	}
	for i, v := range items[0].DurationBuckets {
		if v != 0 {
			t.Fatalf("bucket %d = %d, want 0 (an opaque connection has no latency)", i, v)
		}
	}
}

// TestAggregatorHoldsBackTheMinuteInProgress pins the boundary rule. Emitting the
// current minute would ship a partial delta and then, on the next flush, ship the
// rest of the same minute under the same key -- which the SummingMergeTree adds,
// producing a number that is right only by luck.
func TestAggregatorHoldsBackTheMinuteInProgress(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:10Z")}
	agg := newTestAggregator(clock, testPods())
	agg.Observe(recvAt(1, clock.Now()))

	clock.set(minuteAt(t, "2026-08-25T10:00:50Z"))
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %d items, want the in-progress minute held back", len(items))
	}

	clock.set(minuteAt(t, "2026-08-25T10:01:01Z"))
	items := agg.Flush()
	if len(items) != 1 || items[0].ConnCount != 1 {
		t.Fatalf("got %+v, want the whole minute once it closed", items)
	}
}

// TestAggregatorEmitsEachBucketExactlyOnce pins the property the whole no-retry rule
// rests on. A bucket that were emitted twice would be added to itself downstream.
func TestAggregatorEmitsEachBucketExactlyOnce(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())
	agg.Observe(recvAt(1, clock.Now()))

	clock.set(minuteAt(t, "2026-08-25T10:01:30Z"))
	if items := agg.Flush(); len(items) != 1 {
		t.Fatalf("first flush got %d items, want 1", len(items))
	}
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("second flush got %d items, want nothing left", len(items))
	}
}

// TestAggregatorNeverReopensAClosedMinute pins what happens to a straggler. Clock
// skew between the kernel timestamp and the flush can put an observation in a minute
// already declared final; filing it late would report traffic against a bucket the
// server has already merged.
func TestAggregatorNeverReopensAClosedMinute(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:01:30Z")}
	agg := newTestAggregator(clock, testPods())

	// Close 10:00 and 10:01 with nothing in them.
	agg.Flush()

	agg.Observe(recvAt(1, minuteAt(t, "2026-08-25T10:00:30Z")))

	clock.set(minuteAt(t, "2026-08-25T10:02:30Z"))
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %+v, want a straggler for a closed minute dropped", items)
	}
}

// TestAggregatorIgnoresOutboundConnections pins direction. An outbound connection is
// somebody else's inbound, counted on the node that serves it; counting it here would
// double every request in the cluster.
func TestAggregatorIgnoresOutboundConnections(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())

	outbound := recvAt(1, clock.Now())
	outbound.Path = SyscallSend
	agg.Observe(outbound)

	// Later receives on the same socket are the response coming back, and must not
	// turn the connection inbound after the fact.
	agg.Observe(recvAt(1, clock.Now()))

	clock.set(minuteAt(t, "2026-08-25T10:01:30Z"))
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %+v, want outbound connections ignored", items)
	}
}

// TestAggregatorDropsUnattributableTraffic pins the silent refusal. Traffic to an IP
// no local pod owns, or to a port two containers claim, is lost rather than filed
// under a guess.
func TestAggregatorDropsUnattributableTraffic(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}

	ambiguous := pod([]corev1.Container{container(testAppName), container("proxy")}, nil)
	ambiguous.Name = "amb-0"
	ambiguous.Namespace = testPodNS
	pods := fakePods{netaddr.MustParseIP("10.0.0.9"): ambiguous}

	agg := newTestAggregator(clock, pods)

	unknownHost := recvAt(1, clock.Now())
	agg.Observe(unknownHost)

	unknownPort := recvAt(2, clock.Now())
	unknownPort.LocalAddr = netip.MustParseAddr("10.0.0.9")
	agg.Observe(unknownPort)

	clock.set(minuteAt(t, "2026-08-25T10:01:30Z"))
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %+v, want unattributable traffic dropped", items)
	}
}

// TestAggregatorReclaimsIdleConnections pins the TTL sweep as the backstop it is.
// Connections are normally released by their close event, so what reaches the TTL is
// an entry whose close never arrived -- lost to a full ring buffer, or a socket
// already open when the probe attached. Without the sweep those would accumulate for
// the life of the process.
func TestAggregatorReclaimsIdleConnections(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := NewAggregator(Config{ConnTTL: time.Minute}, clock, testPods(), logr.Discard())
	agg.Observe(recvAt(1, clock.Now()))

	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want 1", got)
	}

	clock.set(minuteAt(t, "2026-08-25T10:05:00Z"))
	agg.Flush()

	if got := agg.connCount(); got != 0 {
		t.Fatalf("tracked connections = %d, want the idle one reclaimed", got)
	}
}

// TestAggregatorCapsTrackedConnections pins the memory ceiling. This runs as a
// DaemonSet beside the workloads it measures, so an unbounded map is a way to take
// down the node it is reporting on.
func TestAggregatorCapsTrackedConnections(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := NewAggregator(Config{MaxConnections: 2}, clock, testPods(), logr.Discard())

	for cookie := uint64(1); cookie <= 10; cookie++ {
		agg.Observe(recvAt(cookie, clock.Now()))
	}

	if got := agg.connCount(); got != 2 {
		t.Fatalf("tracked connections = %d, want the cap of 2", got)
	}

	clock.set(minuteAt(t, "2026-08-25T10:01:30Z"))
	items := agg.Flush()
	if len(items) != 1 || items[0].ConnCount != 2 {
		t.Fatalf("got %+v, want only the tracked connections counted", items)
	}
}

// TestAggregatorReleasesAConnectionOnClose pins the close event as the thing that
// reclaims a connection. Everything else about the table -- what the cap holds, how
// long the TTL can be -- follows from the kernel saying when a socket is gone
// instead of the agent guessing from silence.
func TestAggregatorReleasesAConnectionOnClose(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(recvAt(1, clock.Now()))
	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want 1", got)
	}

	agg.Observe(closeAt(1, minuteAt(t, "2026-08-25T10:00:31Z")))
	if got := agg.connCount(); got != 0 {
		t.Fatalf("tracked connections = %d, want the closed one released", got)
	}
}

// TestAggregatorCloseFreesAnAdmissionSlot is the reason the close probe exists. The
// cap used to hold every connection seen within the TTL, which on a node churning
// short-lived connections is overwhelmingly sockets the kernel had already freed --
// so the table filled with corpses and refused live connections at a fraction of the
// offered rate. A slot has to come back when its socket does.
func TestAggregatorCloseFreesAnAdmissionSlot(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := NewAggregator(Config{MaxConnections: 1}, clock, testPods(), logr.Discard())

	agg.Observe(recvAt(1, clock.Now()))
	agg.Observe(recvAt(2, clock.Now()))
	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want the second refused at the cap", got)
	}

	agg.Observe(closeAt(1, clock.Now()))
	agg.Observe(recvAt(2, clock.Now()))

	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want the freed slot reused", got)
	}
	clock.set(minuteAt(t, "2026-08-25T10:01:30Z"))
	if items := agg.Flush(); len(items) != 1 {
		t.Fatalf("got %d items, want the readmitted connection measured", len(items))
	}
}

// TestAggregatorIgnoresACloseForAnUntrackedConnection pins that a close is not an
// observation. Most sockets on a node are nothing to do with a pod this agent
// measures, and their closes arrive all the same; if one created an entry, or
// consumed an admission slot on the way to being dropped, the cap would be spent on
// traffic that was never counted.
func TestAggregatorIgnoresACloseForAnUntrackedConnection(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := NewAggregator(Config{MaxConnections: 1}, clock, testPods(), logr.Discard())

	agg.Observe(closeAt(99, clock.Now()))
	if got := agg.connCount(); got != 0 {
		t.Fatalf("tracked connections = %d, want a close for an unknown socket ignored", got)
	}

	// The slot is still there to be used.
	agg.Observe(recvAt(1, clock.Now()))
	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want the slot still free", got)
	}
}

// TestAggregatorReleasesAnIgnoredConnectionOnClose pins the close path for a
// connection the aggregator decided not to decode. An outbound connection is still an
// entry in the table holding a slot, and it is the state the node has most of, so a
// close that only released decoded connections would leave the cap exactly as stuck
// as it was before there was a close probe.
func TestAggregatorReleasesAnIgnoredConnectionOnClose(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())

	outbound := recvAt(1, clock.Now())
	outbound.Path = SyscallSend
	agg.Observe(outbound)
	if got := agg.connCount(); got != 1 {
		t.Fatalf("tracked connections = %d, want the outbound connection tracked", got)
	}

	agg.Observe(closeAt(1, clock.Now()))
	if got := agg.connCount(); got != 0 {
		t.Fatalf("tracked connections = %d, want it released", got)
	}
}

// TestAggregatorFlushOrderIsDeterministic pins the emitted order. Go randomises map
// iteration, so without the sort the same observations would produce a different
// batch on every run -- untestable, and unreadable in a log of what was sent.
func TestAggregatorFlushOrderIsDeterministic(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}

	pods := testPods()
	other := pod([]corev1.Container{container("api", testPodPort)}, nil)
	other.Name = otherPodName
	other.Namespace = otherPodNS
	other.UID = "uid-api-0"
	pods[netaddr.MustParseIP("10.0.0.8")] = other

	agg := newTestAggregator(clock, pods)

	second := recvAt(1, clock.Now())
	second.LocalAddr = netip.MustParseAddr("10.0.0.8")
	agg.Observe(second)
	agg.Observe(recvAt(2, clock.Now()))

	clock.set(minuteAt(t, "2026-08-25T10:01:30Z"))
	items := agg.Flush()

	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].PodNamespace != otherPodNS || items[1].PodNamespace != testPodNS {
		t.Fatalf("order = %q, %q; want namespaces sorted",
			items[0].PodNamespace, items[1].PodNamespace)
	}
}

// TestAggregatorNeverMovesTheFrontierBackwards pins the frontier as monotonic.
//
// A minute is emitted exactly once and "everything before this is final" is the
// only thing stopping it being emitted twice. If a flush reading an earlier clock
// could lower that mark -- two flushes racing, or a clock that steps back -- a
// minute already on the wire would accept new observations and be sent again. The
// second copy is added by the SummingMergeTree, not deduplicated, so it is
// indistinguishable from real traffic that never happened.
func TestAggregatorNeverMovesTheFrontierBackwards(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:01:10Z")}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(recvAt(1, minuteAt(t, "2026-08-25T10:00:30Z")))
	if items := agg.Flush(); len(items) != 1 {
		t.Fatalf("got %d items, want the 10:00 minute emitted", len(items))
	}

	// The clock steps back inside the minute that was just declared final.
	clock.set(minuteAt(t, "2026-08-25T10:00:50Z"))
	agg.Flush()

	// A fresh connection in the already-emitted minute must still be refused.
	agg.Observe(recvAt(2, minuteAt(t, "2026-08-25T10:00:40Z")))

	clock.set(minuteAt(t, "2026-08-25T10:02:00Z"))
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %+v, want nothing -- 10:00 was already final", items)
	}
}

// TestAggregatorCountsARecycledSocketAsANewConnection pins the connection identity
// against kernel socket reuse.
//
// The probe reports a kernel socket address, and the kernel recycles those. A node
// churning short-lived connections will hand the same address to a fresh
// connection well inside the TTL, and if that is taken for the connection that
// held it before, it is never counted -- conn_count would read low precisely where
// traffic is heaviest, with nothing to show it had happened.
func TestAggregatorCountsARecycledSocketAsANewConnection(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:20Z")}
	agg := newTestAggregator(clock, testPods())

	first := recvAt(1, clock.Now())
	agg.Observe(first)

	// Same kernel socket address, a different client: a recycled struct sock.
	recycled := first
	recycled.RemotePort = 54322
	agg.Observe(recycled)

	clock.set(minuteAt(t, "2026-08-25T10:01:10Z"))
	items := agg.Flush()
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].ConnCount != 2 {
		t.Fatalf("conn_count = %d, want 2 -- the recycled socket is a new connection",
			items[0].ConnCount)
	}
}

// TestAggregatorIsConcurrencySafe drives Observe from many goroutines while flushing,
// which is how it actually runs: the probe reads the ring buffer on its own goroutine
// and the monitor flushes on a ticker.
func TestAggregatorIsConcurrencySafe(t *testing.T) {
	clock := &fakeClock{now: minuteAt(t, "2026-08-25T10:00:30Z")}
	agg := newTestAggregator(clock, testPods())

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				agg.Observe(recvAt(uint64(g*1000+i), clock.Now()))
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			agg.Flush()
		}
	}()
	wg.Wait()
}

// connCount reports how many connections are currently tracked. It exists for the
// tests: the ceilings this package enforces are claims about the size of a map
// nothing outside the package can see.
func (a *Aggregator) connCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.conns)
}

// onPort moves an observation to a different local port, which is the only way a
// test can say "this is an outbound connection" in terms the aggregator can see.
func onPort(ev SocketEvent, port uint16) SocketEvent {
	ev.LocalPort = port
	return ev
}

// TestAggregatorReacquiresAConnectionOpenBeforeItStarted pins the restart case.
//
// Nothing on a connection says who opened it, and for one that was already
// established when the agent attached, the first syscall the agent sees is simply
// whichever happened next. Reading that as the direction meant a server holding
// long-lived keep-alive connections was written off as outbound the moment its agent
// restarted, and stayed that way for as long as those connections lived -- which
// against a keep-alive client is indefinitely.
//
// Here the first thing seen on connection 2 is a send, on the port connection 1 has
// already been seen receiving on. That makes it an inbound connection joined late
// rather than one this node opened, and its next request has to be measured.
func TestAggregatorReacquiresAConnectionOpenBeforeItStarted(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	// An ordinary connection, which is what teaches the aggregator that this node
	// serves on this endpoint.
	agg.Observe(reqAt(1, at(t, 0), request("GET", "/a", "")))
	agg.Observe(respAt(1, at(t, 10*time.Millisecond), response(200, "ok")))

	// Connection 2 was already open. The first thing seen on it is the tail of a
	// response to a request nobody watched arrive, so nothing is measured from it.
	agg.Observe(respAt(2, at(t, 20*time.Millisecond), response(200, "mid")))

	// The next request is a message boundary, and from there the connection decodes
	// like any other.
	agg.Observe(reqAt(2, at(t, 100*time.Millisecond), request("GET", "/b", "")))
	agg.Observe(respAt(2, at(t, 150*time.Millisecond), response(500, "no")))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 2 {
		t.Fatalf("request_count = %d, want 2: the re-acquired connection was never measured", item.RequestCount)
	}
	if item.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1", item.ErrorCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: resynchronising at a request boundary is not giving up", item.ConnCount)
	}
	if want := uint64((10 + 50) * time.Millisecond.Microseconds()); item.DurationSumUs != want {
		t.Fatalf("duration_sum_us = %d, want %d", item.DurationSumUs, want)
	}
}

// TestAggregatorStillIgnoresOutboundConnectionsOnAServingNode pins the other half of
// the same rule. Learning that the node serves on one endpoint must not make every
// send-first connection inbound: a connection this node dials leaves from an
// ephemeral port of its own, and picking those up would count the cluster's traffic a
// second time on the wrong side.
func TestAggregatorStillIgnoresOutboundConnectionsOnAServingNode(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/a", "")))
	agg.Observe(respAt(1, at(t, 10*time.Millisecond), response(200, "ok")))

	// This pod calling somebody else: a request going out first, from a port nothing
	// has ever been received on.
	agg.Observe(onPort(respAt(2, at(t, 20*time.Millisecond), request("GET", "/x", "")), 43210))
	agg.Observe(onPort(reqAt(2, at(t, 30*time.Millisecond), response(200, "theirs")), 43210))

	item := flushOne(t, agg, clock, at(t, time.Minute+5*time.Second))

	if item.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1: an outbound connection was measured as inbound", item.RequestCount)
	}
	if item.ConnCount != 0 {
		t.Fatalf("conn_count = %d, want 0: an outbound connection is ignored, not given up on", item.ConnCount)
	}
}

// TestAggregatorForgetsAServingEndpointOnceItGoesQuiet pins the expiry. The endpoint
// set is evidence gathered from connections, and pod IPs are recycled -- so an entry
// that outlived every connection justifying it could go on to match the ephemeral
// port of whichever pod inherits the address next.
func TestAggregatorForgetsAServingEndpointOnceItGoesQuiet(t *testing.T) {
	clock := &fakeClock{now: at(t, 0)}
	agg := newTestAggregator(clock, testPods())

	agg.Observe(reqAt(1, at(t, 0), request("GET", "/a", "")))
	agg.Observe(respAt(1, at(t, 10*time.Millisecond), response(200, "ok")))
	agg.Observe(closeAt(1, at(t, 20*time.Millisecond)))

	// Well past ConnTTL, so this flush's sweep drops the endpoint along with
	// everything else that old.
	quiet := at(t, 11*time.Minute)
	clock.set(quiet)
	agg.Flush()

	// The same endpoint, no longer recognised: a send-first connection on it reads as
	// outbound again.
	agg.Observe(respAt(2, quiet, response(200, "mid")))
	agg.Observe(reqAt(2, quiet.Add(time.Second), request("GET", "/b", "")))
	agg.Observe(respAt(2, quiet.Add(2*time.Second), response(200, "ok")))

	clock.set(at(t, 13*time.Minute))
	if items := agg.Flush(); len(items) != 0 {
		t.Fatalf("got %d items, want 0: a forgotten endpoint is not a serving one", len(items))
	}
}
