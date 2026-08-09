# Production design for a fixed-egress PEP

This document records the design I would take to production after the
development measurements. It is deliberately conservative: the optimizer
must improve the China-to-`icourses-dev` leg without changing the egress
location, terminating application TLS, or silently taking over the existing
Clash profile.

## What the measurements imply

The prototype is functionally useful, but it is not yet a production
transport. On the direct physical path, a three-repeat 10 MiB HTTP matrix
completed over the prototype's TCP lane at a median of 4.7, 6.9, 17.6, and
16.5 Mbps for 1, 2, 4, and 8 *independent application flows*. A single
logical QUIC flow was much slower and often censored by the 90-second limit;
the server nevertheless observed payload striped across several lanes. This
is evidence that the framing and reassembly work, not evidence that the
current congestion controller is good.

The packet trace also found an effective path MTU problem. Starting QUIC at
1200-byte packets avoids the 1441-byte probe packets that stalled the first
implementation, but the stock quic-go controller still converges to very
low goodput on this loss/RTT path. A production build must therefore make
congestion control and MTU a first-class transport choice, not assume that
opening more CUBIC lanes is sufficient.

## Data-plane components

```text
Clash Verge
    |
local SOCKS5/TUN ingress -- classifier -- scheduler -- session controller
    |                         |                  |
    +-------------------------+------------------+-- encrypted lanes
                                                     |
                                         US session/reassembly agent
                                                     |
                                  destination dialer at icourses-dev
```

The application connection remains end-to-end encrypted. The PEP sees the
SOCKS destination, byte counts, packet sizes, and timing; it does not see
HTTPS URLs or plaintext and does not perform MITM.

### Session and lane model

One logical flow has a random `session_id` and `flow_id`. The first lane is a
control/interactive lane. Additional lanes are independent authenticated
QUIC connections, each carrying a reliable stream of bounded frames. Every
DATA frame carries a byte sequence number. The receiver uses a bounded
reassembly window and emits cumulative plus selective acknowledgements.

The scheduler must never wait for eight handshakes before acknowledging a
short request. It should start one lane, optionally pre-warm one spare lane,
and grow only after the classifier and a measured lane probe justify it.
Lane joins must be bounded-time and failure-tolerant; a failed join leaves
the original flow usable.

### Congestion control and pacing

Each QUIC lane needs a loss-aware controller with a conservative initial
packet size (1200 bytes) and path-specific MTU probing only after the base
path is proven. The current stock quic-go controller is a useful correctness
baseline, but not the production choice for this path. The next experiment
should compare:

1. a BBR-family controller with explicit loss and queue-delay limits;
2. a Hysteria 2/TUIC-style rate-based controller (including a Brutal-style
   mode where the operator supplies a tested target rate); and
3. stock CUBIC/New Reno as the TCP-fallback control.

The project should reuse a maintained QUIC implementation or congestion
module (for example the implementation used by Hysteria 2) rather than
forking cryptography or inventing a new ACK algorithm. A per-lane controller
alone is unsafe: eight controllers can overrun the path and starve SSH. Put
one aggregate token bucket above the lanes, reserve a small interactive
share, and increase the bulk budget only when queue delay and retransmission
signals stay within limits.

The controller should expose at least RTT, smoothed RTT, bytes in flight,
loss, pacing rate, congestion window, and delivery rate. These are needed to
make lane-growth decisions measurable and to diagnose carrier policers.

## PIAS-inspired workload policy

Classification is intentionally behavioral, not semantic:

- `NEW`: first few seconds or first tens of KiB; one lane and a short latency
  budget.
- `INTERACTIVE`: bidirectional small bursts, idle gaps, or low-volume
  long-lived traffic such as SSH and remote desktop; one lane plus reserved
  queueing capacity.
- `BULK`: sustained one-way transfer after a byte/age budget, with hysteresis;
  eligible for additional lanes and larger aggregate pacing budget.

The byte threshold must not be tied to an absolute Mbps floor. Otherwise a
large transfer on a throttled path never reaches `BULK`, exactly the failure
seen in the first prototype. Conversely, byte count alone can misclassify a
large one-way SSH output. Use recent payload-size distribution, direction,
idle gaps, and an operator-tunable hysteresis window. Classification is a
policy hint, not a security boundary, and it must be visible in metrics.

The scheduler should grow one lane at a time. A candidate lane is retained
only if its marginal goodput is positive and the interactive RTT budget is
not exceeded. It should retire the worst lane after a sustained negative
contribution, with a minimum dwell time to prevent oscillation.

## UDP preference and TCP fallback

Use a three-state health machine per endpoint: `HEALTHY`, `DEGRADED`, and
`BLOCKED`. A new flow in `AUTO` races an authenticated QUIC handshake against
TLS/TCP after a small delay; the first authenticated lane wins. Repeated UDP
failures enter cooldown, while a periodic low-cost probe allows recovery.

When UDP is blocked, new sessions use one TCP lane and keep the same frame
protocol and destination semantics. TCP striping is not the default: nested
reliable congestion controllers compound head-of-line blocking under loss.

Seamless fallback of an *existing* flow requires work that is not optional for
production:

1. receiver ACK/SACK ranges and a bounded sender replay buffer;
2. a resume token bound to session, flow, and lane generation;
3. replacement-lane authentication and sequence-range replay;
4. duplicate suppression before bytes reach the application; and
5. an upper bound on recovery time, after which the flow is reset explicitly.

The current prototype closes a flow when its lane fails. That is an honest
development behavior and must remain documented until resume is implemented.

## Clash Verge integration

The safe first integration is a local SOCKS5 node, for example
`127.0.0.1:12080`. An inactive Clash profile can send its final `MATCH` rule
to that node. The live profile remains untouched and can be restored by
removing that one node/rule.

The current client accepts only TCP CONNECT. A production Clash integration
needs a TUN or UDP-associate ingress for destination UDP and HTTP/3; without
it, QUIC from the application is either rejected or downgraded by the
application. No HTTPS MITM is needed for either mode.

On a host where Clash TUN installs a default route through `198.18.0.1`, the
outer dev endpoint must be explicitly excluded from that route or the client
must bind the physical source address. This is a measurement/deployment
detail, not a reason to use the fake DNS address as the PEP socket endpoint.

## Security and operations

- TLS 1.3 with certificate and ALPN verification.
- HMAC-authenticated, timestamped handshakes with nonce replay protection.
- Short-lived, scoped resume tokens; no reusable lane IDs without
  authentication.
- Destination allow/deny policy, including private-address rejection by
  default.
- Per-user/session/flow limits for handshakes, lanes, frames, reassembly
  bytes, and reconnect rate.
- Metrics and structured logs that never contain the shared secret or
  application plaintext.
- A systemd unit isolated from Xray, sing-box, Cloudflare, Nginx, and the
  existing Clash route. Deployment must be atomic and rollback must be a
  single service stop plus removal of the inactive Clash node.

## Release gates

Do not call the project production-ready until all of the following are
measured on the real China-US path:

1. complete single-flow downloads and uploads at 1/2/4/8 lanes, with at least
   five randomized repetitions and confidence intervals;
2. API/web fresh and reused latency, including p95/p99 and timeout rates;
3. interactive RTT under a simultaneous bulk transfer;
4. controlled loss, delay, reordering, and MTU tests;
5. UDP blocked, intermittently blocked, and recovered cases;
6. mid-session lane replacement without duplicate or missing bytes;
7. resource-limit, fuzz, race, and interoperability tests; and
8. a documented rollback that leaves the existing tunnel unchanged.

