#ifndef __TYPES_H__
#define __TYPES_H__

#include <vmlinux.h>
#include <vmlinux_missing.h>

#define TASK_COMM_LEN 16

typedef struct task_context {
  u64 start_time; // thread's start time
  u64 cgroup_id;
  u32 pid;           // PID as in the userspace term
  u32 tid;           // TID as in the userspace term
  u32 ppid;          // Parent PID as in the userspace term
  u32 host_pid;      // PID in host pid namespace
  u32 host_tid;      // TID in host pid namespace
  u32 host_ppid;     // Parent PID in host pid namespace
  u32 node_host_pid; // PID in same namespace as kubelet/container runtime is
                     // running
  u32 uid;
  u32 mnt_id;
  u32 pid_id;
  unsigned char comm[TASK_COMM_LEN];
  u64 leader_start_time; // task leader's monotonic start time
  u64 parent_start_time; // parent process task leader's monotonic start time
} task_context_t;

typedef struct network_connection_v4 {
  u32 local_address;
  u16 local_port;
  u32 remote_address;
  u16 remote_port;
} net_conn_v4_t;

typedef struct network_connection_v6 {
  struct in6_addr local_address;
  u16 local_port;
  struct in6_addr remote_address;
  u16 remote_port;
  u32 flowinfo;
  u32 scope_id;
} net_conn_v6_t;

typedef struct event_context {
  u32 pid;
  u32 eventid;
} event_context_t;

typedef struct net_event_context {
  // event_context_t eventctx; Context not used right now.
  struct { // event arguments (needs packing), use anonymous struct to ...
    u32 bytes;
    // ... (payload sent by bpf_perf_event_output)
  } __attribute__((__packed__)); // ... avoid address-of-packed-member warns
} __attribute__((__packed__)) net_event_context_t;

typedef union iphdrs_t {
  struct iphdr iphdr;
  struct ipv6hdr ipv6hdr;
} iphdrs;

// NOTE: proto header structs need full type in vmlinux.h (for correct skb copy)

typedef union protohdrs_t {
  struct tcphdr tcphdr;
  struct udphdr udphdr;
  struct icmphdr icmphdr;
  struct icmp6hdr icmp6hdr;
  union {
    u8 tcp_extra[40]; // data offset might set it up to 60 bytes
  };
} protohdrs;

typedef union {
  // Used for bpf2go to generate a proper golang struct.
  __u8 raw[16];
  __u32 v4addr;
  __be32 u6_addr32[4];
} __attribute__((packed)) addr_t;

typedef struct {
  addr_t saddr;
  addr_t daddr;
  __u16 sport;
  __u16 dport;
  __u16 family;
} __attribute__((packed)) tuple_t;

typedef struct nethdrs_t {
  iphdrs iphdrs;
  protohdrs protohdrs;
} nethdrs;
#endif
