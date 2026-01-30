#include <vmlinux.h>
#include <vmlinux_flavors.h>
#include <vmlinux_missing.h>

#include <bpf_core_read.h>
#include <bpf_endian.h>
#include <bpf_helpers.h>

#include <common/common.h>
#include <maps.h>
#include <network_capture.h>
#include <types.h>

char __license[] SEC("license") = "Dual MIT/GPL";

#define UDP_PORT_DNS 53

// Enable debug prints
// #define DEBUG 1

// NOTE: We do not use this map in the new tracer but keeping it for reference
// or if we want to enable event streaming later.
BPF_PERF_OUTPUT(events, 1024); // Events output map.

// Scratch buffer to avoid stack limit and support cgroup_skb packet capture
struct {
  __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
  __uint(max_entries, 1);
  __type(key, u32);
  __type(
      value, struct { u8 data[1024]; });
} dns_scratch_map SEC(".maps");

// Helper to extract headers and call record_netflow
statfunc int handle_skb(struct __sk_buff *ctx, enum flow_direction direction) {
  struct bpf_sock *sk = ctx->sk;
  if (!sk)
    return 1;

  sk = bpf_sk_fullsock(sk);
  if (!sk)
    return 1;

  // We only care about AF_INET and AF_INET6
  if (sk->family != AF_INET && sk->family != AF_INET6)
    return 1;

  nethdrs hdrs = {0}, *nethdrs = &hdrs;
  void *dest;
  u32 size = 0;

  // L3 Header
  if (sk->family == AF_INET) {
    dest = &nethdrs->iphdrs.iphdr;
    size = sizeof(struct iphdr);
    if (bpf_skb_load_bytes_relative(ctx, 0, dest, size, BPF_HDR_START_NET))
      return 1;

    if (nethdrs->iphdrs.iphdr.version != 4)
      return 1;

    // Check IHL
    u32 ihl = nethdrs->iphdrs.iphdr.ihl;
    if (ihl > 5) {
      size = ihl * 4;
      if (bpf_skb_load_bytes_relative(ctx, 0, dest, size, BPF_HDR_START_NET))
        return 1;
    }
  } else if (sk->family == AF_INET6) {
    dest = &nethdrs->iphdrs.ipv6hdr;
    size = sizeof(struct ipv6hdr);
    if (bpf_skb_load_bytes_relative(ctx, 0, dest, size, BPF_HDR_START_NET))
      return 1;

    if (nethdrs->iphdrs.ipv6hdr.version != 6)
      return 1;
  }

  // Determine L4 protocol and load headers
  u8 proto = 0;
  if (sk->family == AF_INET)
    proto = nethdrs->iphdrs.iphdr.protocol;
  else
    proto = nethdrs->iphdrs.ipv6hdr.nexthdr;

  u32 l3_size = size;

  if (proto == IPPROTO_TCP) {
    dest = &nethdrs->protohdrs.tcphdr;
    size = sizeof(struct tcphdr);
    if (bpf_skb_load_bytes_relative(ctx, l3_size, dest, size,
                                    BPF_HDR_START_NET))
      return 1;
  } else if (proto == IPPROTO_UDP) {
    dest = &nethdrs->protohdrs.udphdr;
    size = sizeof(struct udphdr);
    if (bpf_skb_load_bytes_relative(ctx, l3_size, dest, size,
                                    BPF_HDR_START_NET))
      return 1;
  } else {
    // We only track TCP and UDP for now (and maybe ICMP later)
    return 1;
  }

  // We use dummy values for process context since we aggregate by Pod (IP) in
  // userspace. PID=0, Comm="", CGroupID=0. If we wanted PID resolution, we'd
  // need socket lookups. Note: bpf_get_current_pid_tgid() in cgroup_skb context
  // often returns the interrupted context (e.g. softirq) or 0.

  unsigned char comm[TASK_COMM_LEN] = {0};
  u32 pid = 0;
  u64 start_time = 0;
  u64 cgroup_id = 0;

  // Check for DNS Traffic (UDP port 53) and emit event
  // We capture the full packet payload for userspace parsing
  bool is_dns = false;
  if (proto == IPPROTO_UDP) {
    // Check ports. We need to parse UDP header to get ports.
    // We already loaded udphdr into dest (nethdrs->protohdrs.udphdr).
    // Ports are in network byte order in the header structure.
    u16 sport = bpf_ntohs(nethdrs->protohdrs.udphdr.source);
    u16 dport = bpf_ntohs(nethdrs->protohdrs.udphdr.dest);

    if (sport == UDP_PORT_DNS || dport == UDP_PORT_DNS) {
      is_dns = true;
      bpf_printk("DNS UDP detected: %d -> %d\n", sport, dport);
    }
  }

  if (is_dns) {
    u32 zero = 0;
    struct {
      u8 data[1024];
    } *scratch = bpf_map_lookup_elem(&dns_scratch_map, &zero);

    if (scratch) {
      // Try to capture as much as possible using stepped buckets.
      // This is necessary because bpf_skb_load_bytes requires constant length
      // on older kernels (5.15), and fails if the packet is smaller than the
      // requested length. We try from largest to smallest.
      if (bpf_skb_load_bytes(ctx, 0, scratch->data, 512) == 0) {
        bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, scratch, 512);
      } else if (bpf_skb_load_bytes(ctx, 0, scratch->data, 448) == 0) {
        bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, scratch, 448);
      } else if (bpf_skb_load_bytes(ctx, 0, scratch->data, 384) == 0) {
        bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, scratch, 384);
      } else if (bpf_skb_load_bytes(ctx, 0, scratch->data, 320) == 0) {
        bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, scratch, 320);
      } else {
// Hyper-fine-grained capture extended for PrivateLink (30-280 bytes)
// Covers long domains + IPv6 which often fall in the 160-256 byte gap.
#pragma clang loop unroll(full)
        for (int i = 280; i >= 30; i--) {
          if (bpf_skb_load_bytes(ctx, 0, scratch->data, i) == 0) {
            bpf_perf_event_output(ctx, &events, BPF_F_CURRENT_CPU, scratch, i);
            goto done;
          }
        }
      }
    done:;
    }
  }

  record_netflow(ctx, comm, pid, start_time, cgroup_id, nethdrs, direction);

  return 1;
}

SEC("cgroup_skb/ingress")
int cgroup_ingress(struct __sk_buff *skb) { return handle_skb(skb, INGRESS); }

SEC("cgroup_skb/egress")
int cgroup_egress(struct __sk_buff *skb) { return handle_skb(skb, EGRESS); }
