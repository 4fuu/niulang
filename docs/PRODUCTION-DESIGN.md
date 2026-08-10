# Production design for a fixed-egress PEP

This document records the design I would take to production after the
development measurements. It is deliberately conservative: the optimizer
must improve the China-to-`icourses-dev` leg without changing the egress
location, terminating application TLS, or silently taking over the existing
Clash profile.

## What the measurements imply

The prototype is functionally useful, but it is not yet a production
transport. The latest five-block campaign is summarized in
[`MEASUREMENTS-20260810.md`](MEASUREMENTS-20260810.md): adaptive reached a
101.8 Mbps median for eight concurrent 10 MiB HTTP flows with 40/40 complete,
while the stock control had 33/40 complete and Brutal had 39/40. The same
campaign still found OpenAI timeouts and long tails, so goodput alone is not a
release criterion.

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
reassembly window and emits cumulative acknowledgements. Selective ACK ranges
and explicit resume tokens remain future protocol work; the current replay
mechanism relies on duplicate-safe sequence reassembly.

The implementation also has an explicit, opt-in QUIC stream pool. When
`--quic-pool` is enabled, initial/control streams for concurrent flows share
one bounded QUIC connection and one congestion controller, while bulk lane
joins remain independent connections. This improves the short-flow handshake
amortization characteristic of TUIC, but it is not the default: the measured
path still needs a controller and queue policy that preserve bulk goodput.

The scheduler must never wait for eight handshakes before acknowledging a
short request. The unattended-safe default starts one lane and grows only
after the classifier and a measured lane probe justify it; operators can
explicitly pre-warm spare lanes after a path-specific campaign.
Lane joins must be bounded-time and failure-tolerant; a failed join leaves
the original flow usable. The adaptive manager opens at most one speculative
join per scheduler tick and stops joining as soon as both FIN directions are
observable. This is an important resource invariant: a session that is
already completing must not turn a rejected join into an unbounded stream of
zero-byte lanes.

The server’s stream-pool accept loop has a separate lifecycle invariant: the
per-stream handshake deadline must never be applied while waiting for another
stream on an established QUIC connection. A pooled or single-flow connection
may legitimately carry a transfer longer than the authentication timeout.
Each accepted stream gets its own authentication deadline; the outer
connection is bounded by QUIC idle timeout, keepalives, admission limits, and
server shutdown. Violating this invariant caused the development server to
close active long transfers after ten seconds and masqueraded as congestion
controller failure.

### Congestion control and pacing

Each QUIC lane needs a loss-aware controller with a conservative initial
packet size (1200 bytes) and path-specific MTU probing only after the base
path is proven. The stock apNet QUIC controller is a useful correctness
baseline, but it is not the best measured choice for this path. A matched
development experiment with the apNet fork found that a wanopt adaptive
controller improved median 256-KiB goodput from 0.31 to 0.50 Mbps for one lane
and from 1.56 to 3.00 Mbps for eight lanes. A Hysteria-style fixed-rate
controller at 1 MiB/s per lane reached 8.44 Mbps for a one-lane 10-MiB
download and 64.47 Mbps aggregate for eight lanes; an 8-MiB SSH upload reached
6.95 and 49.18 Mbps respectively. Ten fresh Google requests during eight bulk
downloads remained 10/10 successful (median 1.18 s, p95 2.19 s). These are
path-specific development measurements, not a universal claim about Brutal or
QUIC.

Before release, compare:

1. the opt-in independently implemented BBRv1-shaped controller with
   explicit loss and queue-delay limits;
2. the separate `bbr-tuic` controller, which ports TUIC's
   `quinn-congestions` estimator/state machine into Go, including its
   ACK-aggregation filter and recovery phases;
3. a Hysteria 2/TUIC-style rate-based controller (including a Brutal-style
   mode where the operator supplies a tested target rate); and
4. stock CUBIC/New Reno as the TCP-fallback control.

The BBR implementation must treat the QUIC fork's signed `ByteCount` as a
signed type when saturating counters, and its delivery sampler must use
cumulative ACK and send slopes rather than dividing an individual delayed ACK
by the full path RTT. The development tests now cover both invariants. Even
with those corrections, lane recovery has a finite attempt budget and
exponential backoff: a peer that accepts and immediately closes replacement
streams must not create an unbounded stream/dial storm while a final FIN is
pending.

The project uses the maintained apNet QUIC fork and keeps the stock controller
available. It does not import Hysteria's `internal/` packages or fork
cryptography. Both BBR modes are opt-in: the original wanopt mode is an
independent implementation of the public BBR model, while `bbr-tuic` is a
separate Go implementation aligned with TUIC's public behavior. Neither is a
claim that apNet quic-go itself provides BBR. A per-lane controller alone is unsafe: eight controllers can
overrun the path and starve SSH. The aggregate token bucket is now implemented
above all lanes, with a reserved interactive share; an 8 MiB/s budget plus a
512 KiB/s reserve preserved 10/10 Google requests during eight bulk downloads.
It remains opt-in until queue-delay and retransmission guardrails are exposed
as telemetry.

The endpoint exposes aggregate active-QUIC lane count, latest/worst smoothed
RTT, bytes sent/received, QUIC loss counters, and (for the optional controllers)
bytes in flight, pacing rate, congestion window, minimum RTT, recovery state,
and delivery-rate estimates through the loopback-only metrics listener. These
signals are now available for validation, but they remain a release gate for
automatic lane-growth decisions: telemetry must be retained across lane
replacement and tied to statistically valid marginal-gain samples before a
non-default policy is enabled.

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
contribution, with a minimum dwell time to prevent oscillation. The current
implementation now feeds the measured worst smoothed RTT into this guard and
retires the least productive non-control lane when the budget is exceeded or a
negative marginal-gain probe is observed;
controller-specific bytes-in-flight and delivery-rate signals are still needed
to make that decision fully loss-aware.

## UDP preference and TCP fallback

Use a three-state health machine per endpoint: `HEALTHY`, `DEGRADED`, and
`BLOCKED`. A new flow in `AUTO` races an authenticated QUIC handshake against
TLS/TCP after a small delay; the first authenticated lane wins. Repeated UDP
failures enter cooldown, while a periodic low-cost probe allows recovery.

When UDP is blocked, new sessions use one TCP lane and keep the same frame
protocol and destination semantics. TCP striping is not the default: nested
reliable congestion controllers compound head-of-line blocking under loss.

The implementation now covers the basic bounded recovery path:

1. a bounded sender replay buffer and cumulative ACKs;
2. authenticated replacement-lane joins bound to the session and flow;
3. duplicate suppression before bytes reach the application;
4. stale-lane retirement and a 90-second completion tombstone that replays
   both final directions; and
5. an upper bound on recovery time, after which the flow is reset explicitly.

For SOCKS5 UDP ASSOCIATE, the local UDP socket and its pinned peer survive a
transport failure. The client cancels both workers for the dead lane, applies
bounded exponential backoff, and opens a fresh authenticated association. The
shared health machine suppresses QUIC after repeated failures, so `auto`
selects TLS/TCP for the rescue. This deliberately does not pretend to be a
lossless session resume: datagrams already in flight when the lane dies may be
lost, and the old remote relay is released by its bounded idle timeout. A
future protocol version should add an authenticated association-resume token
and explicit packet replay/duplicate policy before loss-sensitive UDP is
called production-grade.

The controlled UDP-blackhole test transferred a complete 100 MiB response over
a TCP rescue lane. This is development evidence, not a guarantee under all
loss patterns: selective ACK ranges, path-independent resume tokens,
intermittent blocking, and a broader fault matrix remain release gates.

A second real-path blackhole test covered SOCKS5 UDP ASSOCIATE itself. After a
valid DNS reply on QUIC, the server dropped only inbound UDP/12443. The same
local SOCKS UDP endpoint obtained another valid reply over a freshly
authenticated TCP association after 9.51 s, with one reconnect, one lane
replacement, one fallback, and zero rescue-attempt failures. The exact
firewall rule was removed and verified absent afterward. This establishes the
bounded fallback behavior but not lossless UDP resume; in-flight datagrams may
still be lost during the transition.

## Clash Verge integration

The safe first integration is a local SOCKS5 node, for example
`127.0.0.1:12080`. An inactive Clash profile can send its final `MATCH` rule
to that node. The live profile remains untouched and can be restored by
removing that one node/rule.

The current client accepts TCP CONNECT and bounded UDP ASSOCIATE. A production
Clash integration still needs a TUN/VLESS ingress for transparent capture and
full HTTP/3 behavior; a SOCKS profile can use UDP ASSOCIATE for applications
that support SOCKS5 UDP. The current packet mode uses reliable stream frames,
so native QUIC DATAGRAM remains a performance release gate for loss-sensitive
UDP. No HTTPS MITM is needed for either mode.

On a host where Clash TUN installs a default route through `198.18.0.1`, the
outer dev endpoint must be explicitly excluded from that route or the client
must bind a physical source address. The client accepts a literal IP,
`if:NAME`, or `auto`; `auto` resolves an active non-loopback,
non-point-to-point IPv4 address before each dial and therefore handles normal
DHCP changes. If more than one physical address is active, configure `if:NAME`
or a literal address rather than relying on an ambiguous automatic choice.
This is a measurement/deployment detail, not a reason to use the fake DNS
address as the PEP socket endpoint.

## Security and operations

- TLS 1.3 with certificate and ALPN verification.
- HMAC-authenticated, timestamped handshakes with nonce replay protection.
- Short-lived, scoped resume tokens; no reusable lane IDs without
  authentication.
- Destination allow/deny policy, including private-address rejection by
  default.
- Per-user/session/flow limits for handshakes, lanes, frames, reassembly
  bytes, reconnect rate, and application-idle/maximum-lifetime duration.
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
