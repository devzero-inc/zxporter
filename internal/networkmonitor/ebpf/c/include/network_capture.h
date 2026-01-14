#ifndef __NETWORK_CAPTURE_H__
#define __NETWORK_CAPTURE_H__

#include <bpf_helpers.h>
#include <common/network.h>
#include <maps.h>
#include <types.h>
#include <vmlinux.h>

typedef struct netflow_config {
  int map_index;
} netflow_config_t;

typedef struct process_identity {
  __u32 pid;
  __u64 pid_start_time;
  __u64 cgroup_id;
} __attribute__((__packed__)) process_identity_t;

struct traffic_summary {
  __u64 rx_packets;
  __u64 rx_bytes;

  __u64 tx_packets;
  __u64 tx_bytes;

  // We store process name on summary (ebpf map value) to make lookup key
  // smaller and save few extra instructions fetching comm from task context
  // each time. It can be stored once during initial flow creation.
  __u8 comm[TASK_COMM_LEN];

  // In order for BTF to be generated for this struct, a dummy variable needs to
  // be created.
} __attribute__((__packed__)) traffic_summary_dummy;

struct ip_key {
  struct process_identity process_identity;

  tuple_t tuple;
  __u8 proto;

  // In order for BTF to be generated for this struct, a dummy variable needs to
  // be created.
} __attribute__((__packed__)) ip_key_dummy;

enum flow_direction {
  INGRESS,
  EGRESS,
};

// NOTE: proto header structs need full type in vmlinux.h (for correct skb copy)

// cgroupctxmap

typedef enum net_packet {
  // Layer 3
  SUB_NET_PACKET_IP = 1 << 1,
  // Layer 4
  SUB_NET_PACKET_TCP = 1 << 2,
  SUB_NET_PACKET_UDP = 1 << 3,
  SUB_NET_PACKET_ICMP = 1 << 4,
  SUB_NET_PACKET_ICMPV6 = 1 << 5,
  // Layer 7
  SUB_NET_PACKET_DNS = 1 << 6,
  SUB_NET_PACKET_SOCKS5 = 1 << 8,
  SUB_NET_PACKET_SSH = 1 << 9,
} net_packet_t;

typedef struct net_event_contextmd {
  u32 header_size;
  u8 captured; // packet has already been captured
} __attribute__((__packed__)) net_event_contextmd_t;

// network related maps

typedef struct net_task_context {
  task_context_t taskctx;
} net_task_context_t;

// CONSTANTS
// Network return value (retval) codes

// Packet Direction (ingress/egress) Flag
#define packet_ingress (1 << 4)
#define packet_egress (1 << 5)
// Flows (begin/end) Flags per Protocol
#define flow_tcp_begin (1 << 6)  // syn+ack flag or first flow packet
#define flow_tcp_sample (1 << 7) // sample with statistics after first flow
#define flow_tcp_end (1 << 8)    // fin flag or last flow packet

// payload size: full packets, only headers
#define FULL 65536 // 1 << 16
#define HEADERS 0  // no payload

// when guessing by src/dst ports, declare at network.h
#define TCP_PORT_SSH 22
#define UDP_PORT_DNS 53
#define TCP_PORT_DNS 53
#define TCP_PORT_SOCKS5 1080

// layer 7 parsing related constants
#define socks5_min_len 4
#define ssh_min_len 4 // the initial SSH messages always send `SSH-`

#define MAX_NETFLOWS 65535

struct {
  __uint(type, BPF_MAP_TYPE_ARRAY_OF_MAPS);
  __uint(max_entries, 2);
  __type(key, int);
  __array(
      values, struct {
        __uint(type, BPF_MAP_TYPE_LRU_HASH);
        __uint(max_entries, MAX_NETFLOWS);
        __type(key, struct ip_key);
        __type(value, struct traffic_summary);
      });
} network_traffic_buffer_map SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_ARRAY);
  __uint(max_entries, 2);
  __type(key, u32);
  __type(value, netflow_config_t);
} netflow_config_map SEC(".maps");

/*
 * Fills the given ip_key with the provided data. One thing to watch out for is,
 * that the tuple will have the local addr and port filled into the saddr/sport
 * fields and remote will be in daddr/dport.
 * TODO: Right now we do not track source and destination ports due high
 * cardinality.
 */
statfunc bool load_ip_key(struct ip_key *key, struct bpf_sock *sk,
                          nethdrs *nethdrs,
                          struct process_identity process_identity,
                          enum flow_direction flow_direction) {
  if (unlikely(!key || !sk || !nethdrs)) {
    return false;
  }

  key->tuple.family = sk->family;

  __u8 proto = 0;

  switch (sk->family) {
  case AF_INET:
    proto = nethdrs->iphdrs.iphdr.protocol;

    // NOTE(patrick.pichler): The mismatch between saddr and daddr for
    // ingress/egress is on purpose, as we want to store the local addr/port in
    // the saddr/sport and the remote addr/port in daddr/dport.
    switch (flow_direction) {
    case INGRESS:
      key->tuple.saddr.v4addr = nethdrs->iphdrs.iphdr.daddr;
      key->tuple.daddr.v4addr = nethdrs->iphdrs.iphdr.saddr;
      break;
    case EGRESS:
      key->tuple.saddr.v4addr = nethdrs->iphdrs.iphdr.saddr;
      key->tuple.daddr.v4addr = nethdrs->iphdrs.iphdr.daddr;
      break;
    }
    break;
  case AF_INET6:
    proto = nethdrs->iphdrs.ipv6hdr.nexthdr;

    // NOTE(patrick.pichler): The mismatch between saddr and daddr for
    // ingress/egress is on purpose, as we want to store the local addr/port in
    // the saddr/sport and the remote addr/port in daddr/dport.
    switch (flow_direction) {
    case INGRESS:
      __builtin_memcpy(key->tuple.saddr.u6_addr32,
                       nethdrs->iphdrs.ipv6hdr.daddr.in6_u.u6_addr32,
                       sizeof(key->tuple.saddr.u6_addr32));
      __builtin_memcpy(key->tuple.daddr.u6_addr32,
                       nethdrs->iphdrs.ipv6hdr.saddr.in6_u.u6_addr32,
                       sizeof(key->tuple.daddr.u6_addr32));
      break;
    case EGRESS:
      __builtin_memcpy(key->tuple.saddr.u6_addr32,
                       nethdrs->iphdrs.ipv6hdr.saddr.in6_u.u6_addr32,
                       sizeof(key->tuple.saddr.u6_addr32));
      __builtin_memcpy(key->tuple.daddr.u6_addr32,
                       nethdrs->iphdrs.ipv6hdr.daddr.in6_u.u6_addr32,
                       sizeof(key->tuple.daddr.u6_addr32));
      break;
    }
    break;
  default:
    return false;
  }

  key->proto = proto;
  key->process_identity = process_identity;

  return true;
}

statfunc void update_traffic_summary(struct traffic_summary *val, u64 bytes,
                                     enum flow_direction direction) {
  if (unlikely(!val)) {
    return;
  }

  switch (direction) {
  case INGRESS:
    __sync_fetch_and_add(&val->rx_bytes, bytes);
    __sync_fetch_and_add(&val->rx_packets, 1);
    break;
  case EGRESS:
    __sync_fetch_and_add(&val->tx_bytes, bytes);
    __sync_fetch_and_add(&val->tx_packets, 1);
    break;
  }
}

statfunc void record_netflow(struct __sk_buff *ctx, unsigned char *comm,
                             u32 pid, u64 leader_start_time, u64 cgroup_id,
                             nethdrs *nethdrs, enum flow_direction direction) {
  process_identity_t identity = {
      .pid = pid,
      .pid_start_time = leader_start_time,
      .cgroup_id = cgroup_id,
  };

  int zero = 0;
  netflow_config_t *config = bpf_map_lookup_elem(&netflow_config_map, &zero);
  if (!config)
    return;

  struct ip_key key = {0};

  if (!load_ip_key(&key, ctx->sk, nethdrs, identity, direction)) {
    return;
  }

  void *sum_map =
      bpf_map_lookup_elem(&network_traffic_buffer_map, &config->map_index);
  if (!sum_map)
    return;

  struct traffic_summary *summary = bpf_map_lookup_elem(sum_map, &key);
  if (summary == NULL) {
    struct traffic_summary new_summary = {0};

    __builtin_memcpy(new_summary.comm, comm, sizeof(new_summary.comm));

    // We do not really care if the update fails, as it would mean, another
    // thread added the entry.
    bpf_map_update_elem(sum_map, &key, &new_summary, BPF_NOEXIST);

    summary = bpf_map_lookup_elem(sum_map, &key);
    if (summary == NULL) // Something went terribly wrong...
      return;
  }

  update_traffic_summary(summary, ctx->len, direction);
}

#endif
