# Production design for a fixed-egress PEP

This document records the design I would take to production after the
development measurements. It is deliberately conservative: the optimizer
must improve the China-to-`<EGRESS-HOST>` leg without changing the egress
location, terminating application TLS, or silently taking over the existing
Clash profile.

## What the measurements imply

The fixed-egress transport is deployed experimentally, but it is not presented
as a general-purpose VPN. An early five-block campaign is summarized in
[`MEASUREMENTS-20260810.md`](MEASUREMENTS-20260810.md): adaptive reached a
101.8 Mbps median for eight concurrent 10 MiB HTTP flows with 40/40 complete,
while the stock control had 33/40 complete and Brutal had 39/40. The same
campaign found OpenAI timeouts and long tails that later regression work had
to resolve, demonstrating why goodput alone is not a release criterion.

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
local SOCKS5 ingress ------- classifier -- scheduler -- session controller
    |                         |                  |
    +-------------------------+------------------+-- encrypted lanes
                                                     |
                                         US session/reassembly agent
                                                     |
                                  destination dialer at <EGRESS-HOST>
```

The application connection remains end-to-end encrypted. The PEP sees the
SOCKS destination, byte counts, packet sizes, and timing; it does not see
HTTPS URLs or plaintext and does not perform MITM.

### Session and lane model

One logical flow has a random `session_id` and `flow_id`. Its data plane uses
exactly one authenticated connection at a time; connections are replacement
and workload-isolation boundaries, not stripes for aggregating one flow's
capacity. Every DATA frame carries a byte sequence number. The receiver uses a
bounded reassembly window and emits cumulative acknowledgements plus negotiated,
bounded selective ACK ranges. Duplicate-safe sequence reassembly and replay
make TCP-flow replacement safe. UDP associations use a separate relay-resume
token, described under UDP preference and TCP fallback below.

The QUIC stream pool is enabled by default. Initial/control streams for
concurrent flows share one bounded QUIC connection and one congestion
controller. When a classified bulk flow would otherwise share that connection
with interactive work, it is moved to one lazily created, bounded secondary
connection; the flow is never striped across both. This improves short-flow
handshake amortization while isolating bulk queueing.
The first pooled stream performs the PSK `HELLO`; later streams on a capable
peer use one `OPEN_FAST` frame, saving one China-US request/response exchange.
Authentication is scoped to the TLS/QUIC connection, and each fast stream
retains fresh session/flow identities plus normal destination-policy and
admission checks. Capability negotiation keeps the original 24-byte
`HELLO_OK` wire shape for two-way rolling upgrades. A pool reconnect clears
all learned authentication/capability state, and capability-free peers retain
the per-stream legacy handshake.

Lane joins are bounded-time and failure-tolerant; a failed isolation or
replacement join leaves the original flow usable. An already completing
session must not turn a rejected join into an unbounded stream of zero-byte
lanes.

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
development experiment with the apNet fork found that a queqiao adaptive
controller improved median 256-KiB goodput from 0.31 to 0.50 Mbps for one lane
and from 1.56 to 3.00 Mbps for eight lanes. A Hysteria-style fixed-rate
controller at 1 MiB/s per lane reached 8.44 Mbps for a one-lane 10-MiB
download and 64.47 Mbps aggregate for eight lanes; an 8-MiB SSH upload reached
6.95 and 49.18 Mbps respectively. Ten fresh Google requests during eight bulk
downloads remained 10/10 successful (median 1.18 s, p95 2.19 s). These are
path-specific development measurements, not a universal claim about Brutal or
QUIC.

The retained controller comparison set is:

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
cryptography. Both BBR modes are opt-in: the original queqiao mode is an
independent implementation of the public BBR model, while `bbr-tuic` is a
separate Go implementation aligned with TUIC's public behavior. Neither is a
claim that apNet quic-go itself provides BBR. A per-connection controller alone
is unsafe: concurrent controllers can overrun the path and starve SSH. The
aggregate token bucket is implemented
above all lanes, with a reserved interactive share; an 8 MiB/s budget plus a
512 KiB/s reserve preserved 10/10 Google requests during eight bulk downloads.
It remains operator-configured because a safe rate budget is path-specific;
queue-delay and retransmission state are exposed as telemetry.

The endpoint exposes aggregate active-QUIC connection count, latest/worst smoothed
RTT, bytes sent/received, QUIC loss counters, and (for the optional controllers)
bytes in flight, pacing rate, congestion window, minimum RTT, recovery state,
and delivery-rate estimates through the loopback-only metrics listener. These
signals are available for validation and are retained across replacement.

## PIAS-inspired workload policy

Classification is intentionally behavioral, not semantic:

- `NEW`: first few seconds or first tens of KiB; one lane and a short latency
  budget.
- `INTERACTIVE`: bidirectional small bursts, idle gaps, or low-volume
  long-lived traffic such as SSH and remote desktop; one lane plus reserved
  queueing capacity.
- `BULK`: sustained one-way transfer after a byte/age budget, with hysteresis;
  eligible for isolation on its own connection and a larger aggregate pacing
  budget.

The byte threshold must not be tied to an absolute Mbps floor. Otherwise a
large transfer on a throttled path never reaches `BULK`, exactly the failure
seen in the first prototype. Conversely, byte count alone can misclassify a
large one-way SSH output. Use recent payload-size distribution, direction,
idle gaps, and an operator-tunable hysteresis window. Classification is a
policy hint, not a security boundary, and it must be visible in metrics.

The scheduler keeps one data connection per flow. Demand-driven isolation is
attempted only when classified bulk work and competing work share the pooled
control connection. The secondary pool is bounded to one connection and
expires when idle; it is an isolation mechanism, not adaptive multipath.

## UDP preference and TCP fallback

Use a three-state health machine per endpoint: `HEALTHY`, `DEGRADED`, and
`BLOCKED`. A new flow in `AUTO` starts QUIC first and prepares an authenticated
TLS/TCP connection after a small delay, but TCP is a warm standby rather than
an equal race winner. A ready TCP connection waits through a separate QUIC
preference window. If that window expires, TCP may serve the current request
without producing negative UDP evidence; with pooling enabled, the coalesced
QUIC attempt continues in the background and restores the shared pool if it
succeeds. This matters on a lossy WAN: TCP can authenticate first and still
carry data orders of magnitude more slowly. Only an explicit QUIC reachability
failure paired with a successful TCP control is negative UDP evidence.
Repeated differential failures enter cooldown; expiry admits a fresh QUIC
attempt, and any QUIC success resets the health state.

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
bounded exponential backoff, and opens a replacement authenticated
association. The shared health machine suppresses QUIC after repeated
failures, so `auto` selects TLS/TCP for the rescue.

The remote relay now survives it too. The relay is a socket on the server, and
its source address is what the destination has been answering; opening a new
one moved the association to a different client mid-conversation, which the
local socket surviving does nothing about. A resumable open is answered with a
random 16-byte token naming that relay, the relay is retained when its lane
fails, and the replacement association presents the token to reclaim it. The
token is reissued on every open including a successful resume, so one that
outlived its use cannot be replayed against the association's later relays;
it is good once, expires in 30 seconds, and the number of relays a peer can
make the server hold is bounded. A client only offers one where the server
advertised `CapabilityUDPResume`, which both QUIC and TLS/TCP lanes do --
without it on TCP the rescue arrives on the one transport that cannot
reclaim anything.

This still does not pretend to be a lossless session resume, and no longer
tries to be: datagrams in flight when the lane dies are lost, which is what a
UDP datagram is allowed to be and is now also true of one lost in transit,
since packets ride QUIC datagrams. The duplicate policy the earlier note asked
for is the receiver's anti-replay window.

The controlled UDP-blackhole test transferred a complete 100 MiB response over
a TCP rescue lane. Selective ACK ranges, scoped resume tokens, and deterministic
intermittent-block coverage are implemented. This is still not a guarantee
under every loss pattern or middlebox.

A second real-path blackhole test covered SOCKS5 UDP ASSOCIATE itself. After a
valid DNS reply on QUIC, the server dropped only inbound UDP/12443. The same
local SOCKS UDP endpoint obtained another valid reply over a freshly
authenticated TCP association after 9.51 s, with one reconnect, one lane
replacement, one fallback, and zero rescue-attempt failures. The exact
firewall rule was removed and verified absent afterward. This establishes the
bounded fallback behavior but not lossless UDP resume; in-flight datagrams may
still be lost during the transition.

A later intermittent real-path run kept the same UDP association open while
UDP was blocked. Queries 27--37 timed out and queries 38--50 received valid DNS
replies through TCP while the rule was still present, for a measured
27.7-second first-loss-to-recovered-reply bound. After rule removal, a fresh
association returned to QUIC. The exact procedure and metrics are recorded in
[`RELEASE-HARDENING-20260817.md`](RELEASE-HARDENING-20260817.md).

## Clash Verge integration

The safe first integration is a local SOCKS5 node, for example
`127.0.0.1:12080`, added to the existing live profile's manual selector while
the previously selected node remains selected. This preserves the profile's
DNS, TUN, providers, and routing rules and avoids maintaining a duplicate
profile. Activation is one explicit selection under **Proxies**; rollback is
selecting the previous node and removing the Queqiao node and group entry. The
operator procedure is in [`DEPLOYING.md`](DEPLOYING.md#enabling-it-in-clash-verge).

The current client accepts TCP CONNECT and bounded UDP ASSOCIATE. Clash/mihomo
owns transparent TUN capture and hands selected TCP and UDP traffic to this
SOCKS endpoint. Direct in-process TUN or VLESS ingress is deliberately outside
the two-process architecture. Packets ride the connection's QUIC datagrams
where QUIC negotiated them, with the control stream as the fallback for a
TLS/TCP lane or a peer without datagram support, so the release gate for
loss-sensitive UDP is met in shape: a lost packet is no longer retransmitted
and no longer holds up the one behind it. Emulated at 15% loss and a 200 ms
round trip the worst delivered packet takes 202 ms against the stream's 448 to
658. Real blocked/restored-path behavior is recorded in
[`RELEASE-HARDENING-20260817.md`](RELEASE-HARDENING-20260817.md). No HTTPS MITM
is needed.

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
- Connection/session/flow limits for handshakes, lanes, frames, reassembly
  bytes, reconnect rate, and application-idle/maximum-lifetime duration. The
  shared credential does not provide per-user identities or quotas.
- Metrics and structured logs that never contain the shared secret or
  application plaintext.
- A systemd unit isolated from Xray, sing-box, Cloudflare, Nginx, and the
  existing Clash route. Deployment must be atomic and rollback must be a
  switch to the previous selected node, followed by a service stop and removal
  of the Queqiao node and group entry.

The limits above have to cover the work a peer can ask for and not only the
bytes it can send, which is the distinction fuzzing the coded path's decoders
found twice. A sliding-window symbol and a loss-estimator sample are each
identified by a number taken off the wire, and both layers advanced their
window towards that number one step at a time: a single datagram naming an
identifier 2^30 ahead spun the receive loop for as many steps as it named,
under the lock, allocating as it went. Neither is a frame-size limit or a
buffered-byte limit, and neither would have been found by one. Both are
bounded now, and `deep.yml` smokes every fuzz target weekly so the next one of
this shape is found by the repository rather than by the path.

The Stage 5 review also found that the stream semaphore did not bound a QUIC
connection that authenticated at TLS and then opened no stream. Accepted QUIC
connections now have a separate `MaxSessions`-sized admission bound. The full
trust-boundary review and residual risks are in `SECURITY-REVIEW.md`.

## Release gates

The repository-actionable gates and their evidence are:

1. real-path downloads, uploads, and alternating deployed-proxy comparisons in
   [`MEASUREMENTS-20260816.md`](MEASUREMENTS-20260816.md);
2. fresh/reused API latency and interactive behavior under bulk load in
   [`STALL-20260817.md`](STALL-20260817.md);
3. seeded loss, delay, reordering, queueing, and MTU campaigns described in
   [`BENCHMARKING.md`](BENCHMARKING.md), with versioned report provenance;
4. blocked, intermittently blocked, TCP-rescued, and restored-QUIC cases in
   [`RELEASE-HARDENING-20260817.md`](RELEASE-HARDENING-20260817.md);
5. replacement without duplicate TCP bytes or a changed UDP relay source,
   covered by deterministic/race tests and the same live fault campaign;
6. connection/session/frame/buffer limits, fuzz/race coverage, and a clean
   pinned vulnerability scan in [`SECURITY-REVIEW.md`](SECURITY-REVIEW.md); and
7. deterministic multi-platform archives, checksums, atomic installation, and
   rollback in [`RELEASING.md`](RELEASING.md).

Independent third-party review and operation across a wider variety of NATs,
paths, and middleboxes remain external deployment qualifications. They are not
claims that repository code or one China-US route can close by itself.
