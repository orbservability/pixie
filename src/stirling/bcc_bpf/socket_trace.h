#pragma once

#ifdef __cplusplus
#include <cstdint>
#endif

#include <linux/in6.h>

// TODO(oazizi): Fix style consistency. Enums use our C++ style, while structs are old C style.

// TODO(yzhao): Investigate the performance cost of misaligned memory access (8 vs. 4 bytes).

// Indicates the syscall that recorded an event.
// TODO(oazizi/yzhao): Remove once no longer necessary.
enum EventType {
  kEventTypeUnknown,
  kEventTypeSyscallWriteEvent,
  kEventTypeSyscallSendEvent,
  kEventTypeSyscallReadEvent,
  kEventTypeSyscallRecvEvent,
};

// Protocol being used on a connection (HTTP, MySQL, etc.).
enum TrafficProtocol {
  kProtocolUnknown,
  kProtocolHTTP,
  kProtocolHTTP2,
  kProtocolMySQL,
  kNumProtocols
};

// The direction of traffic expected on a probe.
enum ReqRespRole { kRoleUnknown, kRoleRequestor, kRoleResponder };

// Which transactions to trace (direction and type).
const uint64_t kSocketTraceSendReq = 1 << 0;
const uint64_t kSocketTraceSendResp = 1 << 1;
const uint64_t kSocketTraceRecvReq = 1 << 2;
const uint64_t kSocketTraceRecvResp = 1 << 3;

struct traffic_class_t {
  // The protocol of traffic on the connection (HTTP, MySQL, etc.).
  enum TrafficProtocol protocol;
  // Classify traffic as requests, responses or mixed.
  enum ReqRespRole role;
};

struct conn_id_t {
  // Comes from the process from which this is captured.
  // See https://stackoverflow.com/a/9306150 for details.
  // Use union to give it two names. We use tgid in kernel-space, pid in user-space.
  union {
    uint32_t tgid;
    uint32_t pid;
  };
  // The start time of the PID, so we can disambiguate PIDs.
  union {
    uint64_t tgid_start_time_ns;
    uint64_t pid_start_time_ns;
  };
  // The file descriptor to the opened network connection.
  uint32_t fd;
  // Generation number of the FD (increments on each FD reuse in the TGID).
  uint32_t generation;
};

// This struct contains information collected when a connection is established,
// via an accept() syscall.
// This struct must be aligned, because BCC cannot currently handle unaligned structs.
struct conn_info_t {
  uint64_t timestamp_ns;
  // Connection identifier (PID, FD, etc.).
  struct conn_id_t conn_id;
  // IP address of the remote endpoint.
  struct sockaddr_in6 addr;
  // The protocol and message type of traffic on the connection (HTTP/Req, HTTP/Resp, MySQL/Req,
  // etc.).
  struct traffic_class_t traffic_class;
  // A 0-based number for the next write event on this connection.
  // This number is incremented each time a new write event is recorded.
  uint64_t wr_seq_num;
  // A 0-based number for the next read event on this connection.
  // This number is incremented each time a new read event is recorded.
  uint64_t rd_seq_num;
};

// This is the maximum value for the msg size.
// This is use for experiment dealing with large message
// tracing on a vfs_write.
#define MAX_MSG_SIZE 4096

struct socket_data_event_t {
  // We split attributes into a separate struct, because BPF gets upset if you do lots of
  // size arithmetic. This makes it so that it's attributes followed by message.
  struct attr_t {
    // The time stamp as this is captured by BPF program.
    uint64_t timestamp_ns;
    // Connection identifier (PID, FD, etc.).
    struct conn_id_t conn_id;
    // The protocol on the connection (HTTP, MySQL, etc.), and the server-client role.
    struct traffic_class_t traffic_class;
    // The type of the actual data that the msg field encodes, which is used by the caller
    // to determine how to interpret the data.
    uint32_t event_type;
    // A 0-based sequence number for this event on the connection.
    // Note that write/send have separate sequences than read/recv.
    uint64_t seq_num;
    // The size of the actual data, which can be < MAX_MSG_SIZE. We use this to truncated msg field
    // to minimize the amount of data being transferred.
    uint32_t msg_size;
  } attr;
  char msg[MAX_MSG_SIZE];
};
