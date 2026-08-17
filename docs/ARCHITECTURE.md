# queqiao architecture

## Problem statement

The measured China-to-US path has high RTT, rapidly changing erasure, and
severe tails for direct outer TCP transports. `queqiao` is a paired
performance-enhancing proxy (PEP): the local agent terminates the
application-side socket, and the fixed-egress agent creates the
destination-side socket. Bytes in between are numbered and framed so transport
recovery does not expose duplicate application bytes.

## Components

```text
application -> Clash/mihomo TUN or system proxy
                    |
                    v
              SOCKS5 TCP/UDP ingress
              classifier / scheduler
              pooled control connection ---- QUIC/UDP or TLS/TCP ----+-- egress agent
              isolated bulk connection  ---- QUIC/UDP ---------------+       |
                                                                              v
                                                                         destination
```

Clash/mihomo owns transparent TUN capture and routes selected traffic to the
local SOCKS5 endpoint. Queqiao installs no TUN device or host routes. Direct
in-process TUN or VLESS ingress is outside the two-process architecture.

## TCP data path

1. Ingress accepts a SOCKS5 TCP connection and assigns random session and flow
   identifiers.
2. The bounded destination address is authenticated and sent in `OPEN` or
   negotiated `OPEN_FAST` framing.
3. The egress agent applies destination policy, dials it, and reports success
   or a typed error. By default the client pipelines this open; operators can
   choose `--wait-for-open-ack` for precise early failure at the cost of a
   round trip.
4. Each direction is split into bounded DATA frames with monotonically
   increasing byte sequence numbers.
5. Bounded reassembly, cumulative ACKs, negotiated selective ACK ranges, and
   duplicate suppression preserve application byte order across replacement.
6. `CLOSE`, `RESET`, and completion tombstones provide bounded lifecycle and
   final-state replay. Protocol version 2 removed the unused WINDOW/PING/PONG
   frame types.

The application TLS session remains end-to-end between the application and its
destination. The PEP sees the SOCKS destination, byte counts, packet sizes, and
timing, but does not decrypt application traffic.

## Workload isolation

`NEW`, `INTERACTIVE`, and `BULK` classification is behavioral and uses byte
count, direction, recent payload distribution, idle gaps, age, and hysteresis.
It is a scheduling hint, not a security boundary.

The QUIC pool is enabled by default. Short and interactive flows share one
bounded control connection, amortizing authentication and keeping its
congestion window free from avoidable bulk queueing. If classified bulk work
and competing work would share that connection, the bulk flow is moved to one
lazily created secondary connection. One flow has exactly one data connection
at a time: secondary connections isolate workloads and replace failed paths;
they never aggregate one flow's capacity. The deleted multipath experiment and
its measurements are retained in
[`DESIGN-MULTIPATH.md`](DESIGN-MULTIPATH.md).

An optional aggregate token bucket and interactive reserve apply above all
connections. Each QUIC connection retains its own selected congestion
controller.

## UDP and transport fallback

SOCKS5 UDP ASSOCIATE preserves datagram boundaries. Where QUIC negotiates
DATAGRAM support, packets use it; TLS/TCP and capability-free peers use the
lane control stream. The receiver applies a bounded anti-replay window.

Each remote endpoint has `healthy`, `degraded`, and `blocked` UDP health. New
work in `auto` mode races QUIC against delayed TLS/TCP; repeated UDP failures
enter a cooldown, and later probes allow QUIC to become preferred again.

Failed TCP flows use bounded sequence-aware replay over an authenticated
replacement without duplicating bytes. Failed UDP associations retain the
local SOCKS endpoint and reclaim the same remote relay socket using a scoped,
single-use, expiring resume token. Datagrams in flight at failure can be lost;
UDP recovery is not presented as lossless.

## Trust and resource boundaries

TLS 1.3 protects both outer transports. Timestamped HMAC handshakes and nonce
tracking authenticate sessions and joins; connection-scoped capabilities gate
fast opens, selective ACKs, control reservation, and UDP resume. The egress
rejects private destinations unless explicitly configured otherwise.

Connections, sessions, flows, frame payloads, reassembly, replay, UDP relays,
anti-replay windows, handshakes, idle periods, and total lifetimes are bounded.
The loopback-only metrics endpoint reports flow, lane, controller, fallback,
replacement, timeout, and replay state. The detailed review and residual risks
are in [`SECURITY-REVIEW.md`](SECURITY-REVIEW.md).

No scheduler can exceed a hard aggregate bottleneck. The transport is designed
for one measured fixed-egress path and is not evidence of universal advantage;
see [`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md) for release evidence and
external qualifications.
