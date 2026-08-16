# queqiao architecture

## Problem statement

The measured China-to-US path has a large difference between one and several
independent flows. Direct outer TCP transports show severe retransmission and
tail behavior. QUIC-based TUIC and Hysteria 2 are substantially more robust,
but ordinary Clash proxying still maps one application connection to one
logical proxy flow.

`queqiao` is a paired performance-enhancing proxy (PEP): the local agent
terminates the application-side socket, and the US agent creates the
destination-side socket. Bytes in between are carried as numbered frames over
one or more authenticated lanes.

## Components

```text
         local machine                         icourses-dev
  +--------------------------+          +--------------------------+
  | SOCKS5/TUN ingress       |          | authenticated listener   |
  | flow classifier           |          | session manager          |
  | PIAS scheduler            |  lanes   | reassembly / ordering    |
  | lane manager              | <------> | destination dialer       |
  | UDP/TCP transport probes |          | metrics / limits         |
  +--------------------------+          +--------------------------+
```

The first usable integration surface is a local SOCKS5 listener. TUN support
comes after the flow/session semantics are stable. Clash Verge can route its
final `MATCH` rule to the local SOCKS5 endpoint without knowing the custom
wire protocol.

## Data path

1. Ingress accepts a TCP connection and assigns a random `flow_id` within an
   authenticated `session_id`.
2. The destination address is sent in a bounded `OPEN` frame.
3. The US agent dials the destination and returns `OPEN_OK` or a typed error.
4. Each direction is split into bounded frames with monotonically increasing
   byte sequence numbers.
5. The scheduler selects lanes according to class, lane health, and global
   pacing. The receiver reorders frames before writing to the socket.
6. `CLOSE`, `RESET`, and `WINDOW` frames provide lifecycle and backpressure.

The application TLS session remains end-to-end between the application and
its destination. The optimizer sees byte counts, destination metadata, and
timing, but does not need HTTPS MITM.

## Workload classes

`NEW` receives a small byte/time budget and one lane. `INTERACTIVE` is selected
by bidirectionality, packet-size distribution, idle gaps, and a bounded
sustained rate. `BULK` requires a larger byte count plus sustained one-way
delivery. Transitions use hysteresis and a minimum dwell time.

Byte count alone is deliberately insufficient: SSH and remote desktop flows
can be long-lived, while Git can transition from interactive negotiation to a
large packfile transfer.

## Lane policy

New and interactive flows normally use one QUIC lane. Bulk flows start with
two lanes and may grow to a configured maximum if marginal goodput improves
without exceeding an interactive RTT budget. The scheduler uses a global
aggregate token bucket in addition to per-lane QUIC congestion control; this
prevents N independent congestion controllers from blindly taking the whole
path.

## Fallback

Each endpoint has an authenticated UDP probe. A new session may race a UDP
handshake and a TCP/TLS handshake. The first authenticated path wins. Health
states are `healthy`, `degraded`, and `blocked`; lane creation and replacement
respect the state. Seamless replacement of a failed lane requires session
resume tokens and sequence-aware replay, which is a later milestone.

TCP fallback normally uses one lane. Multiple reliable outer TCP lanes are
not the default because nested TCP head-of-line blocking can amplify loss.

## Threat model and limits

The server is trusted with destination metadata and with forwarding encrypted
application bytes. It must not be trusted with application plaintext. The
protocol must authenticate every session and reject unauthenticated lane
joins, oversized frames, flow-id reuse, replayed opens, and unbounded buffer
growth.

No scheduler can exceed a hard aggregate bottleneck. Multiple lanes are useful
only when the path's policy or congestion behavior is materially per-flow, or
when independent retransmission windows reduce loss interaction.

