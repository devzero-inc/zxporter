// Socket probes for service-level RED metrics.
//
// Six programs across three attach points, of which four attach on any one node:
//
//	kprobe/tcp_sendmsg      bytes leaving this node
//	kprobe/tcp_close        the socket is going away
//
//	fentry/tcp_recvmsg      remembers where the bytes will land
//	fexit/tcp_recvmsg       bytes that arrived at this node
//
//	kprobe/tcp_recvmsg      the same two, for kernels that cannot load the
//	kretprobe/tcp_recvmsg   trampoline pair
//
// The receive path is written twice on purpose and the loader picks one pair; see
// sr_recv_begin for why the trampoline is the one to want. Both pairs share the two
// helpers that do the actual work, so the fallback cannot drift into measuring
// something different from the primary.
//
// Addresses and ports are read off the socket structure, so nothing here parses
// an IP or TCP header. There is no connect probe and no accept probe: this file
// reports the local endpoint of every event and takes no view on direction. The
// aggregator decides that, from whether the local endpoint is one this node has
// been seen receiving on -- which is what lets a connection already open when the
// agent attached be recognised as inbound rather than written off.
//
// There IS a close probe, and earlier revisions of this file argued there should
// not be. Without one, death can only be inferred from idleness, and idleness
// does not distinguish a socket the kernel has already freed from a live gRPC
// stream between messages or an HTTP keep-alive connection between requests. The
// TTL that reclaims the first quickly enough to bound the connection table
// destroys the decode state of the second, which is unrecoverable: the HPACK
// dynamic table is gone, so later header blocks cannot be read at all. tcp_close
// runs exactly once per socket and its first argument is the same struct sock,
// so it yields the identical connection key through sr_fill_tuple with no new
// helper, no new map and no change to this record's layout.
//
// Two maps exist and neither is per-connection state. sr_events is the ring
// buffer the events travel on. sr_recv_inflight carries the destination buffer
// address from tcp_recvmsg entry to tcp_recvmsg return, because the address is
// only known at entry and the bytes only exist at return; its entries live for
// the duration of one syscall and are deleted on the way out.

#include <vmlinux.h>
#include <vmlinux_missing.h>

#include <bpf_core_read.h>
#include <bpf_endian.h>
#include <bpf_helpers.h>
#include <bpf_tracing.h>

char __license[] SEC("license") = "Dual MIT/GPL";

// bpf_get_func_ret is helper 184. The vendored bpf_helper_defs.h stops at 180, so it
// is declared here rather than by regenerating a header shared with every other
// program in this tree -- the declaration is the ABI, and helper numbers never move.
//
// It exists because tcp_recvmsg's argument list has changed across kernel versions,
// and an fexit program reads its arguments and its return value out of one array by
// index: the return value sits at index nr_args, so naming it as a trailing BPF_PROG
// parameter would hardcode an index that is right on one kernel and reads an
// argument on another. This helper asks the kernel where it actually is.
//
// Kernels older than 5.17 do not have it, so the fexit program fails to load there
// and the loader falls back to the kretprobe pair. That is the intended behaviour and
// the only reason the fallback still exists.
static long (*bpf_get_func_ret)(void *ctx, __u64 *value) = (void *) 184;

#ifndef statfunc
#define statfunc static __always_inline
#endif

// SR_MAX_PAYLOAD is the copy cap. At most this many bytes of any one call are
// carried into userspace; the call's true length is reported separately and is
// always accurate.
#define SR_MAX_PAYLOAD 2048

#define SR_DIR_SEND 0
#define SR_DIR_RECV 1
// SR_DIR_CLOSE carries no bytes: true_len and copy_len are zero and the payload is
// untouched. It says only that this socket is being torn down, so userspace can
// stop holding state for it.
#define SR_DIR_CLOSE 2

// SR_EVENTS_BYTES sizes the ring buffer. It is the only sizeable allocation this
// program makes and is charged to the DaemonSet's memory limit.
#define SR_EVENTS_BYTES (1 << 23) // 8 MiB

// SR_INFLIGHT_ENTRIES bounds the in-flight recvmsg scratch. It is an LRU map, so
// a leaked entry (a task killed between entry and return) is reclaimed by
// pressure rather than growing without bound.
#define SR_INFLIGHT_ENTRIES 8192

// sr_event is the wire format between kernel and userspace. Every field is
// naturally aligned, and the decoder in Go reads it by offset, so both sides
// pin the same literals: the offsets below are asserted here at compile time and
// again from the Go side's test. Reordering a field breaks this build rather
// than quietly shifting what userspace reads.
// The header is a struct of its own so that an event carrying no bytes can reserve
// only the 64 bytes it fills. A close event otherwise costs the same 2112 bytes as a
// full data event, and the ring buffer is the resource that decides whether a
// measurement is taken at all: when it fills, events are dropped silently, so bytes
// spent on a payload nobody wrote are bytes taken from requests nobody measures.
struct sr_event_hdr {
	__u64 socket_cookie;
	__u64 timestamp_ns;
	__u8 saddr[16];
	__u8 daddr[16];
	__u16 family;
	__u16 lport;
	__u16 dport;
	__u8 direction;
	__u8 _pad;
	__u32 true_len;
	__u32 copy_len;
};

struct sr_event {
	struct sr_event_hdr hdr;
	__u8 payload[SR_MAX_PAYLOAD];
};

// The record layout, pinned. These literals are the same ones the Go decoder
// reads by, so drift on either side is a build failure rather than a field read
// from the wrong place.
#define SR_ASSERT_OFFSET(field, want) \
	_Static_assert(__builtin_offsetof(struct sr_event, field) == (want), "sr_event." #field " moved")

SR_ASSERT_OFFSET(hdr.socket_cookie, 0);
SR_ASSERT_OFFSET(hdr.timestamp_ns, 8);
SR_ASSERT_OFFSET(hdr.saddr, 16);
SR_ASSERT_OFFSET(hdr.daddr, 32);
SR_ASSERT_OFFSET(hdr.family, 48);
SR_ASSERT_OFFSET(hdr.lport, 50);
SR_ASSERT_OFFSET(hdr.dport, 52);
SR_ASSERT_OFFSET(hdr.direction, 54);
SR_ASSERT_OFFSET(hdr.true_len, 56);
SR_ASSERT_OFFSET(hdr.copy_len, 60);
SR_ASSERT_OFFSET(payload, 64);
_Static_assert(sizeof(struct sr_event) == 64 + SR_MAX_PAYLOAD, "sr_event grew or gained padding");

// Nesting must not add padding: the header-only reservation is exactly the prefix a
// full event starts with, so userspace decodes both with one set of offsets.
_Static_assert(sizeof(struct sr_event_hdr) == 64, "sr_event_hdr is no longer the 64-byte record header");

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, SR_EVENTS_BYTES);
} sr_events SEC(".maps");

// sr_inflight is one in-progress tcp_recvmsg, keyed by the calling task.
struct sr_inflight {
	__u64 sk;
	__u64 buf;
	__u64 buf_len;
};

struct {
	__uint(type, BPF_MAP_TYPE_LRU_HASH);
	__uint(max_entries, SR_INFLIGHT_ENTRIES);
	__type(key, __u64);
	__type(value, struct sr_inflight);
} sr_recv_inflight SEC(".maps");

// CO-RE flavors for the iterator that carries a socket call's buffer. The member
// holding the buffer has moved three times: iov through 6.3, __iov from 6.4, and
// a separate single-buffer form from 6.0 (ubuf, becoming __ubuf_iovec in 6.10).
// Only the names matter here; libbpf resolves the offsets against the running
// kernel.
struct iov_iter___red {
	__u8 iter_type;
	void *ubuf;
	struct iovec __ubuf_iovec;
	const struct iovec *iov;
	const struct iovec *__iov;
};

struct msghdr___red {
	void *msg_name;
	struct iov_iter___red msg_iter;
};

// iter_type___red exists so the iterator forms can be recognised by name rather
// than by a numeric value, which was renumbered in 6.4. Only these two describe
// a userspace buffer: ITER_BVEC, ITER_KVEC, ITER_XARRAY and ITER_DISCARD point at
// kernel memory or at nothing, and their first member is not an iovec at all.
enum iter_type___red {
	ITER_IOVEC = 1000,
	ITER_UBUF = 1001,
};

// sr_iter_buf finds the first buffer segment of a socket call, returning its
// address and length. A call whose payload spans several segments is copied only
// as far as the first: the true length still reports the whole call.
//
// It reads a buffer only from the two iterator forms that describe one. Anything
// else -- a bvec, a kvec, an xarray -- is left alone: its first member is a page
// or a kernel address, not an iov_base, so reading it as one would be reading a
// different field entirely.
statfunc int sr_iter_buf(struct msghdr *msg, __u64 *buf, __u64 *buf_len)
{
	struct msghdr___red *m = (struct msghdr___red *)msg;

	*buf = 0;
	*buf_len = 0;

	if (bpf_core_field_exists(m->msg_iter.iter_type)) {
		__u8 iter_type = 0xff;
		bpf_core_read(&iter_type, sizeof(iter_type), &m->msg_iter.iter_type);

		if (bpf_core_enum_value_exists(enum iter_type___red, ITER_UBUF) &&
		    iter_type == (__u8)bpf_core_enum_value(enum iter_type___red, ITER_UBUF)) {
			if (bpf_core_field_exists(m->msg_iter.ubuf)) {
				bpf_core_read(buf, sizeof(*buf), &m->msg_iter.ubuf);
			} else if (bpf_core_field_exists(m->msg_iter.__ubuf_iovec)) {
				bpf_core_read(buf, sizeof(*buf), &m->msg_iter.__ubuf_iovec.iov_base);
				bpf_core_read(buf_len, sizeof(*buf_len), &m->msg_iter.__ubuf_iovec.iov_len);
			}
			return *buf != 0 ? 0 : -1;
		}

		// Every kernel that has iter_type also has ITER_IOVEC, so a type that is
		// neither it nor ITER_UBUF is an iterator this program does not read.
		if (iter_type != (__u8)bpf_core_enum_value(enum iter_type___red, ITER_IOVEC))
			return -1;
	}

	const struct iovec *iov = NULL;
	if (bpf_core_field_exists(m->msg_iter.__iov))
		bpf_core_read(&iov, sizeof(iov), &m->msg_iter.__iov);
	else if (bpf_core_field_exists(m->msg_iter.iov))
		bpf_core_read(&iov, sizeof(iov), &m->msg_iter.iov);

	if (!iov)
		return -1;

	bpf_core_read(buf, sizeof(*buf), &iov->iov_base);
	bpf_core_read(buf_len, sizeof(*buf_len), &iov->iov_len);
	return *buf != 0 ? 0 : -1;
}

// sr_fill_tuple reads both ends of the connection off the socket structure.
statfunc int sr_fill_tuple(struct sock *sk, struct sr_event_hdr *hdr)
{
	__u16 family = BPF_CORE_READ(sk, __sk_common.skc_family);
	hdr->family = family;

	if (family == AF_INET) {
		__be32 saddr = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 daddr = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(hdr->saddr, &saddr, sizeof(saddr));
		__builtin_memcpy(hdr->daddr, &daddr, sizeof(daddr));
	} else if (family == AF_INET6) {
		BPF_CORE_READ_INTO(&hdr->saddr, sk, __sk_common.skc_v6_rcv_saddr);
		BPF_CORE_READ_INTO(&hdr->daddr, sk, __sk_common.skc_v6_daddr);
	} else {
		return -1;
	}

	// skc_num is already host order; skc_dport is not.
	hdr->lport = BPF_CORE_READ(sk, __sk_common.skc_num);
	hdr->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	return 0;
}

// sr_copy_payload copies at most SR_MAX_PAYLOAD bytes and records how many landed.
// A failed read leaves copy_len at zero: the event is still emitted, because the
// true length is worth reporting on its own.
statfunc void sr_copy_payload(struct sr_event *ev, __u64 buf, __u64 avail)
{
	ev->hdr.copy_len = 0;
	if (!buf || avail == 0)
		return;

	__u64 want = avail;
	if (want > SR_MAX_PAYLOAD)
		want = SR_MAX_PAYLOAD;

	long err;
	if (want == SR_MAX_PAYLOAD)
		err = bpf_probe_read_user(ev->payload, SR_MAX_PAYLOAD, (const void *)buf);
	else
		err = bpf_probe_read_user(ev->payload, (__u32)want & (SR_MAX_PAYLOAD - 1), (const void *)buf);

	if (err == 0)
		ev->hdr.copy_len = (__u32)want;
}

// sr_fill_header writes every header field, and is the only thing that does. Both
// emit paths below go through it, so the header a close event carries cannot drift
// from the one a data event carries.
statfunc int sr_fill_header(struct sr_event_hdr *hdr, struct sock *sk, __u8 direction, __u64 true_len)
{
	if (sr_fill_tuple(sk, hdr) != 0)
		return -1;

	// The kernel socket address identifies the socket, and is unique only among
	// sockets alive at the same instant -- the kernel recycles the structure. It
	// is not a bpf_get_socket_cookie() value: that helper is not available to
	// kprobes, whose sock pointer the verifier will not accept for it. Userspace
	// therefore pairs this with the four-tuple above before treating two
	// observations as the same connection; the TTL sweep bounds memory and does
	// nothing about reuse.
	hdr->socket_cookie = (__u64)sk;
	hdr->timestamp_ns = bpf_ktime_get_ns();
	hdr->direction = direction;
	hdr->_pad = 0;
	hdr->true_len = true_len > 0xffffffffULL ? 0xffffffffU : (__u32)true_len;
	hdr->copy_len = 0;
	return 0;
}

// sr_emit builds and submits one event carrying a payload.
statfunc void sr_emit(struct sock *sk, __u8 direction, __u64 true_len, __u64 buf, __u64 buf_len)
{
	struct sr_event *ev = bpf_ringbuf_reserve(&sr_events, sizeof(*ev), 0);
	if (!ev)
		return;

	if (sr_fill_header(&ev->hdr, sk, direction, true_len) != 0) {
		bpf_ringbuf_discard(ev, 0);
		return;
	}

	// Never read past the segment the buffer address belongs to.
	__u64 avail = true_len;
	if (buf_len > 0 && buf_len < avail)
		avail = buf_len;
	sr_copy_payload(ev, buf, avail);

	bpf_ringbuf_submit(ev, 0);
}

// sr_emit_close submits a header with no payload behind it.
//
// It reserves sizeof(struct sr_event_hdr) rather than sizeof(struct sr_event) because
// a close carries no bytes: reserving the full record would spend 2112 bytes of ring
// buffer to deliver 64 bytes of meaning. The saving is not cosmetic. A single-request
// connection emits send, recv and close, so the full-size close is a third of the
// bytes that connection costs, and the ring buffer dropping events is invisible --
// dropped requests look like requests that never happened.
//
// This is safe for userspace because the Go decoder is length-driven: it requires
// only the 64-byte header and clamps its payload copy to what the record actually
// contains, so a header-only record decodes with an empty payload rather than
// reading past its end. TestDecodeEvent_CloseRecordIsHeaderOnly pins that.
statfunc void sr_emit_close(struct sock *sk)
{
	struct sr_event_hdr *hdr = bpf_ringbuf_reserve(&sr_events, sizeof(*hdr), 0);
	if (!hdr)
		return;

	if (sr_fill_header(hdr, sk, SR_DIR_CLOSE, 0) != 0) {
		bpf_ringbuf_discard(hdr, 0);
		return;
	}

	bpf_ringbuf_submit(hdr, 0);
}

SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(sr_tcp_sendmsg, struct sock *sk, struct msghdr *msg, size_t size)
{
	if (!sk || !msg || size == 0)
		return 0;

	__u64 buf = 0, buf_len = 0;
	sr_iter_buf(msg, &buf, &buf_len);
	sr_emit(sk, SR_DIR_SEND, size, buf, buf_len);
	return 0;
}

// sr_recv_begin remembers where a receive is going to land, because that is only
// knowable on the way in: by the time tcp_recvmsg returns, copy_to_iter has advanced
// the iterator past the bytes it just wrote, and for a buffer filled exactly to its
// end the iterator has moved on to the next segment entirely. The address has to be
// taken at entry or it is not the address the bytes went to.
//
// The entry observation is worth nothing on its own -- it says a read was attempted,
// not that anything arrived -- so it is paired with sr_recv_end below, and how that
// pairing is done is the whole reason this file carries two copies of the receive
// path.
//
// A kretprobe pairs them by taking a return instance from a pool at entry and holding
// it until the function returns. tcp_recvmsg blocks for as long as a keep-alive
// connection is idle, so a server holding N idle connections holds N instances, and
// the pool defaults to max(10, 2 * num_possible_cpus()) -- ten on a two-vCPU node.
// Past that rethook_try_get() returns NULL, the return handler never runs, the miss
// is counted in nmissed where nothing reads it, and every receive on the node is lost
// while the entry probe keeps firing. One moderately busy keep-alive server is enough
// to do that to every container on the node.
//
// A BPF trampoline has no pool. fentry and fexit run on a trampoline the kernel
// patches into the function, so calls in flight are bounded by the number of threads
// rather than by a preallocated array, and there is nothing to exhaust.
statfunc void sr_recv_begin(struct sock *sk, struct msghdr *msg)
{
	if (!sk || !msg)
		return;

	struct sr_inflight inflight = {};
	inflight.sk = (__u64)sk;
	if (sr_iter_buf(msg, &inflight.buf, &inflight.buf_len) != 0)
		inflight.buf = 0;

	__u64 id = bpf_get_current_pid_tgid();
	bpf_map_update_elem(&sr_recv_inflight, &id, &inflight, BPF_ANY);
}

// sr_recv_end emits what the paired sr_recv_begin was holding, and clears it either
// way: a receive that returned nothing still ends the syscall, and leaving the entry
// behind would let the next read on the same task emit against a stale buffer.
statfunc void sr_recv_end(long ret)
{
	__u64 id = bpf_get_current_pid_tgid();
	struct sr_inflight *inflight = bpf_map_lookup_elem(&sr_recv_inflight, &id);
	if (!inflight)
		return;

	struct sr_inflight saved = *inflight;
	bpf_map_delete_elem(&sr_recv_inflight, &id);

	if (ret <= 0 || saved.sk == 0)
		return;

	sr_emit((struct sock *)saved.sk, SR_DIR_RECV, (__u64)ret, saved.buf, saved.buf_len);
}

// Only the first two arguments are named. tcp_recvmsg has gained and lost trailing
// parameters across kernel versions and BPF_PROG reads them positionally, so naming
// more would be naming a different argument depending on the node; sk and msg are at
// indices 0 and 1 under every variant.
SEC("fentry/tcp_recvmsg")
int BPF_PROG(sr_tcp_recvmsg_fentry, struct sock *sk, struct msghdr *msg)
{
	sr_recv_begin(sk, msg);
	return 0;
}

// No arguments are named here: the return value is the only thing this needs, and
// bpf_get_func_ret is what fetches it.
SEC("fexit/tcp_recvmsg")
int BPF_PROG(sr_tcp_recvmsg_fexit)
{
	__u64 ret = 0;

	if (bpf_get_func_ret(ctx, &ret) != 0)
		return 0;

	// tcp_recvmsg returns int. Narrowing before widening keeps a negative errno
	// negative instead of turning it into a very large positive length.
	sr_recv_end((long)(int)ret);
	return 0;
}

// The fallback pair, for kernels without BTF trampolines or without helper 184. It
// is attached with an explicit maxactive rather than the default ten -- see
// retprobeMaxActive in probe_linux.go -- which does not make the pool unexhaustible,
// only large enough to cover the same number of concurrent receives the in-flight
// map itself can hold.
SEC("kprobe/tcp_recvmsg")
int BPF_KPROBE(sr_tcp_recvmsg_entry, struct sock *sk, struct msghdr *msg)
{
	sr_recv_begin(sk, msg);
	return 0;
}

SEC("kretprobe/tcp_recvmsg")
int BPF_KRETPROBE(sr_tcp_recvmsg_exit, int ret)
{
	sr_recv_end(ret);
	return 0;
}

// tcp_close is the local file descriptor being closed, which has to happen for the
// socket structure to be freed, and it runs once. For a connection the application
// closes, it is where userspace learns a connection key will never be seen again, so
// a half-decoded stream can be dropped and its requests settled there rather than at
// the end of an idle timer that also expires connections which are merely quiet.
//
// It is not sufficient on its own, which is what sr_tcp_done below is for: a
// connection the kernel ends by itself arrives here with its local port already
// released, so the key read here is not the key the connection was tracked under.
//
// The socket may still linger in TIME_WAIT afterwards, and a retransmit or a late
// ACK may still travel on it. None of that reaches tcp_sendmsg or tcp_recvmsg,
// which are the syscall paths, so no event can arrive for a key after its close.
SEC("kprobe/tcp_close")
int BPF_KPROBE(sr_tcp_close, struct sock *sk)
{
	if (!sk)
		return 0;

	sr_emit_close(sk);
	return 0;
}

// tcp_done is the kernel ending a connection on its own -- a reset arriving, or a
// timeout firing -- rather than the application closing it. It has to be observed as
// well as tcp_close, because it destroys the part of the socket the connection key
// is built from while the file descriptor is still open:
//
//	tcp_done -> tcp_set_state(sk, TCP_CLOSE) -> inet_put_port(sk)
//
// inet_put_port unhashes the socket and zeroes sk->__sk_common.skc_num, so the local
// port is gone. The descriptor stays open until the application gets round to
// closing it, and only then does tcp_close run -- by which point sr_fill_tuple reads
// local port 0 and builds a key that matches nothing. Userspace drops that close as
// one for a connection it never saw, and the entry it should have released survives
// to the idle sweep: ConnTTL of a bounded table held by a connection that ended
// seconds ago.
//
// Measured on a saturated single-threaded server on a 6.12 kernel, 77 of every 416
// connections per second ended this way, filling a 16384-entry table in three
// minutes. The table is what admits new connections, so past that point the agent
// reported a few percent of the actual request rate until the sweep drained it.
//
// At kprobe entry the body of tcp_done has not run yet, so the tuple is still whole.
// Both probes stay attached: a socket closed without ever having connected never
// reaches tcp_done, and on the ordinary path both fire, the first releasing the entry
// and the second finding nothing to release. An unmatched close is already a no-op.
SEC("kprobe/tcp_done")
int BPF_KPROBE(sr_tcp_done, struct sock *sk)
{
	if (!sk)
		return 0;

	sr_emit_close(sk);
	return 0;
}
