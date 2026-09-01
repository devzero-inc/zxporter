//go:build linux

package socketprobe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	bpf "github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/go-logr/logr"
	"golang.org/x/sys/unix"

	"github.com/devzero-inc/zxporter/internal/networkmonitor/ebpf"
	"github.com/devzero-inc/zxporter/internal/networkmonitor/redmetrics"
)

// The kernel functions the probes attach to. All are TCP specific, so no
// filtering for protocol is needed in the programs.
const (
	fnTCPSendmsg = "tcp_sendmsg"
	fnTCPRecvmsg = "tcp_recvmsg"
	fnTCPClose   = "tcp_close"
)

// retprobeMaxActive is how many receives the kretprobe fallback can have in flight
// at once. It is set explicitly because the kernel's default is not a capacity so
// much as a placeholder: max(10, 2 * num_possible_cpus()), which is ten on a
// two-vCPU node. A kretprobe holds a return instance for as long as the probed
// function runs, and tcp_recvmsg runs for as long as a keep-alive connection is
// idle, so a server holding a few dozen idle connections exhausts the default and
// stops reporting receives for every container on the node -- silently, since the
// kernel counts the misses in nmissed and nothing reads it.
//
// The number is the in-flight map's capacity. Past that many concurrent receives
// the map starts evicting entries and a larger pool would buy nothing, and below it
// the pool would be the binding constraint. Each instance is a small fixed struct,
// so the reservation is on the order of half a megabyte.
//
// This only bounds the damage. The trampoline variant has no pool at all, which is
// why it is preferred and this is the fallback.
//
// Keep it equal to SR_INFLIGHT_ENTRIES in c/socketprobe.c.
const retprobeMaxActive = 8192

// recvObjects is a loaded object file, viewed through whichever of the two
// receive-side implementations the kernel accepted. Everything outside the receive
// path is identical between them.
type recvObjects interface {
	io.Closer

	// shared returns the programs and maps that do not vary by variant.
	shared() *commonObjects
	// attachRecv attaches the receive pair, or returns an error having attached
	// neither.
	attachRecv() ([]link.Link, error)
	// describe names the variant for logs.
	describe() string
}

// commonObjects are the parts of the object file both variants use.
type commonObjects struct {
	Events       *bpf.Map     `ebpf:"sr_events"`
	RecvInflight *bpf.Map     `ebpf:"sr_recv_inflight"`
	TCPSendmsg   *bpf.Program `ebpf:"sr_tcp_sendmsg"`
	TCPClose     *bpf.Program `ebpf:"sr_tcp_close"`
}

func (o *commonObjects) shared() *commonObjects { return o }

func (o *commonObjects) closeShared() {
	closeAll(o.Events, o.RecvInflight, o.TCPSendmsg, o.TCPClose)
}

// tracingObjects is the variant to want: fentry and fexit run on a trampoline the
// kernel patches into tcp_recvmsg, so there is no preallocated pool of return
// instances and nothing that a few hundred idle keep-alive connections can exhaust.
//
// It needs BTF trampolines (5.5) and bpf_get_func_ret (5.17). Kernels without them
// reject the programs at load time, which is what selects the fallback.
type tracingObjects struct {
	commonObjects

	RecvFentry *bpf.Program `ebpf:"sr_tcp_recvmsg_fentry"`
	RecvFexit  *bpf.Program `ebpf:"sr_tcp_recvmsg_fexit"`
}

func (o *tracingObjects) describe() string { return "fentry/fexit" }

func (o *tracingObjects) Close() error {
	closeAll(o.RecvFentry, o.RecvFexit)
	o.closeShared()
	return nil
}

func (o *tracingObjects) attachRecv() ([]link.Link, error) {
	entry, err := link.AttachTracing(link.TracingOptions{
		Program:    o.RecvFentry,
		AttachType: bpf.AttachTraceFEntry,
	})
	if err != nil {
		return nil, fmt.Errorf("attaching fentry on %s: %w", fnTCPRecvmsg, err)
	}

	exit, err := link.AttachTracing(link.TracingOptions{
		Program:    o.RecvFexit,
		AttachType: bpf.AttachTraceFExit,
	})
	if err != nil {
		_ = entry.Close()
		return nil, fmt.Errorf("attaching fexit on %s: %w", fnTCPRecvmsg, err)
	}

	return []link.Link{entry, exit}, nil
}

// kprobeObjects is the fallback for kernels that will not load the trampoline pair.
// It measures the same thing through the same two helpers; what differs is that a
// kretprobe has a return-instance pool, and so an upper bound on how many receives
// can be in flight before it starts dropping them.
type kprobeObjects struct {
	commonObjects

	RecvEntry *bpf.Program `ebpf:"sr_tcp_recvmsg_entry"`
	RecvExit  *bpf.Program `ebpf:"sr_tcp_recvmsg_exit"`
}

func (o *kprobeObjects) describe() string { return "kprobe/kretprobe" }

func (o *kprobeObjects) Close() error {
	closeAll(o.RecvEntry, o.RecvExit)
	o.closeShared()
	return nil
}

func (o *kprobeObjects) attachRecv() ([]link.Link, error) {
	entry, err := link.Kprobe(fnTCPRecvmsg, o.RecvEntry, nil)
	if err != nil {
		return nil, fmt.Errorf("attaching kprobe on %s: %w", fnTCPRecvmsg, err)
	}

	// RetprobeMaxActive is deprecated because it forces the older tracefs attach
	// path rather than the perf one. That is a fair trade here: the perf path
	// offers no way to size the pool at all, and the default size is the bug this
	// whole variant exists to bound.
	exit, err := link.Kretprobe(fnTCPRecvmsg, o.RecvExit, &link.KprobeOptions{
		RetprobeMaxActive: retprobeMaxActive, //nolint:staticcheck // the newer API cannot size the pool
	})
	if err != nil {
		_ = entry.Close()
		return nil, fmt.Errorf("attaching kretprobe on %s: %w", fnTCPRecvmsg, err)
	}

	return []link.Link{entry, exit}, nil
}

// loadObjects loads the object file, preferring the trampoline variant.
//
// Both variants are compiled into the one object file, but LoadAndAssign only loads
// and verifies the programs the given struct names. So asking for the trampoline
// pair on a kernel that cannot run it costs one rejected load, and the kretprobe
// programs are never verified on a kernel that does not need them.
func loadObjects(log logr.Logger) (recvObjects, error) {
	tracing := &tracingObjects{}
	tracingErr := loadSocketprobeObjects(tracing, nil)
	if tracingErr == nil {
		return tracing, nil
	}

	// Old kernels reject these programs, and that is the entire reason the fallback
	// exists -- so this is expected rather than a warning. Any other cause fails the
	// second load too, and is reported from there.
	log.V(1).Info("Kernel would not load the trampoline programs; falling back to kretprobe",
		"reason", tracingErr.Error())

	kprobe := &kprobeObjects{}
	if err := loadSocketprobeObjects(kprobe, nil); err != nil {
		return nil, fmt.Errorf("loading socket probe objects: %w", err)
	}
	return kprobe, nil
}

// closeAll releases loaded objects. A load that fails partway leaves the fields it
// never reached nil, and Close on a nil *ebpf.Map or *ebpf.Program is a no-op, so
// the caller does not have to know how far it got.
func closeAll(closers ...io.Closer) {
	for _, c := range closers {
		if c == nil {
			continue
		}
		_ = c.Close()
	}
}

// Probe owns the loaded programs, their attachments and the ring buffer reader.
// Nothing is loaded or attached until Attach is called, so a disabled feature
// costs exactly nothing.
type Probe struct {
	log logr.Logger

	mu       sync.Mutex
	objs     recvObjects
	links    []link.Link
	reader   *ringbuf.Reader
	offset   clockOffset
	attached bool
}

// New builds an unattached probe.
func New(log logr.Logger) *Probe {
	return &Probe{log: log.WithName("service_request_probe")}
}

// Attach loads the programs and attaches all of them. It is the only step that can
// fail for reasons specific to the running kernel, so callers report health from
// its error rather than from the read loop.
func (p *Probe) Attach() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.attached {
		return nil
	}
	// The programs relocate against the running kernel, so they need the kernel's
	// own BTF. A node without it is unsupported rather than broken.
	if !ebpf.IsKernelBTFAvailable() {
		return fmt.Errorf("%w: the kernel does not expose its own BTF", ErrUnsupported)
	}

	// The DaemonSet already runs with SYS_RESOURCE, so this raises no new
	// privilege; on kernels with memcg accounting for BPF memory it is a no-op.
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("removing memlock limit: %w", err)
	}

	objs, err := loadObjects(p.log)
	if err != nil {
		return err
	}

	links, err := attachAll(objs)
	if err != nil {
		_ = objs.Close()
		return err
	}

	reader, err := ringbuf.NewReader(objs.shared().Events)
	if err != nil {
		closeLinks(links)
		_ = objs.Close()
		return fmt.Errorf("opening event ring buffer: %w", err)
	}

	offset, err := captureClockOffset()
	if err != nil {
		_ = reader.Close()
		closeLinks(links)
		_ = objs.Close()
		return fmt.Errorf("reading the monotonic clock: %w", err)
	}

	p.objs = objs
	p.links = links
	p.reader = reader
	p.offset = offset
	p.attached = true

	p.log.V(1).Info("socket probes attached", "programs", len(links), "receivePath", objs.describe())
	return nil
}

// attachAll attaches the send probe, the receive pair, and the close probe.
//
// All of them are required. Without the close probe the connection table can only
// be reclaimed on idleness, and the sweep that does so cannot tell a dead socket
// from a live idle one -- so a partial attach would be a quietly different
// measurement rather than a smaller one.
func attachAll(objs recvObjects) ([]link.Link, error) {
	shared := objs.shared()

	var links []link.Link

	send, err := link.Kprobe(fnTCPSendmsg, shared.TCPSendmsg, nil)
	if err != nil {
		return nil, fmt.Errorf("attaching kprobe on %s: %w", fnTCPSendmsg, err)
	}
	links = append(links, send)

	recv, err := objs.attachRecv()
	if err != nil {
		closeLinks(links)
		return nil, err
	}
	links = append(links, recv...)

	closed, err := link.Kprobe(fnTCPClose, shared.TCPClose, nil)
	if err != nil {
		closeLinks(links)
		return nil, fmt.Errorf("attaching kprobe on %s: %w", fnTCPClose, err)
	}
	links = append(links, closed)

	return links, nil
}

func closeLinks(links []link.Link) {
	for _, l := range links {
		_ = l.Close()
	}
}

// captureClockOffset pins the kernel monotonic clock to wall clock once, so event
// timestamps can be turned into minute buckets without re-reading the clock per
// event and without losing sub-millisecond resolution.
func captureClockOffset() (clockOffset, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return clockOffset{}, err
	}
	return clockOffset{
		wall:   time.Now(),
		baseNs: uint64(ts.Sec)*uint64(time.Second) + uint64(ts.Nsec), //nolint:gosec // clock_gettime never returns negatives
	}, nil
}

// Run reads events until the context is cancelled or the probe is closed,
// handing each decoded event to sink. Attach must have succeeded first.
func (p *Probe) Run(ctx context.Context, sink func(redmetrics.SocketEvent)) error {
	p.mu.Lock()
	reader, offset, attached := p.reader, p.offset, p.attached
	p.mu.Unlock()

	if !attached || reader == nil {
		return errors.New("socketprobe: Run called before a successful Attach")
	}

	// ringbuf.Reader.Read blocks; closing the reader is what unblocks it.
	stop := context.AfterFunc(ctx, func() { _ = reader.Close() })
	defer stop()

	var (
		record      ringbuf.Record
		undecodable uint64
	)
	for {
		if err := reader.ReadInto(&record); err != nil {
			if errors.Is(err, ringbuf.ErrClosed) || errors.Is(err, os.ErrClosed) {
				return nil
			}
			return fmt.Errorf("reading socket events: %w", err)
		}

		event, ok := decodeEvent(record.RawSample, offset)
		if !ok {
			// A record userspace cannot read means this build's view of the event
			// layout has drifted from the compiled program's, so every later record is
			// undecodable too and the first one is the whole story. Log once and keep
			// reading rather than counting it where nothing looks.
			undecodable++
			if undecodable == 1 {
				p.log.Info("Discarding undecodable socket event records",
					"recordBytes", len(record.RawSample), "headerBytes", recordHeaderSize)
			}
			continue
		}
		sink(event)
	}
}

// Close detaches the programs and releases the ring buffer. It is safe to call
// on a probe that never attached.
func (p *Probe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.attached {
		return nil
	}
	p.attached = false

	if p.reader != nil {
		_ = p.reader.Close()
		p.reader = nil
	}
	closeLinks(p.links)
	p.links = nil
	if p.objs != nil {
		_ = p.objs.Close()
		p.objs = nil
	}
	return nil
}
