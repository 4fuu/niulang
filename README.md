# queqiao

`queqiao` is an experimental, open-source performance-enhancing proxy for
high-latency or lossy long-haul links. It is designed for the specific case
where a client in China must always egress from one fixed US server.

**The path this targets is an erasure channel, not a congested one.** It drops
about 45% of packets independently of the rate anything sends at, and polices
above a knee at roughly 14.5 Mbit/s delivered. Everything in the transport
follows from that: a congestion controller that does not treat erasure as a
signal to back off, an erasure code sized from the measured channel for traffic
that wants latency, retransmission for traffic that wants bandwidth, and
sequencing by byte offset so an out-of-order arrival is placed rather than
waited for. [`docs/DESIGN.md`](docs/DESIGN.md) is the design of record.

One flow is carried by one connection. The project began as a multipath design
and no longer is: an open-loop probe of the live path delivers the same total
whether the offered rate is split over 1, 2, 4 or 8 connections, so connection
count cannot buy capacity here, and lanes past the knee turn memoryless loss
into the correlated kind that defeats the code. The original observation that
more TCP connections went faster was real and was misdiagnosed -- it is TCP's
Mathis limit at 42% loss, not an ISP policing per 4-tuple -- and fixing the
loss response captures the same gain on one connection. Separate connections
remain for two things that are not aggregation: isolating a bulk flow so it
does not delay interactive ones, and moving a session to a fresh connection
when its own dies.

The transport uses QUIC/UDP with an authenticated TLS/TCP fallback when UDP is
blocked or unstable.

Flow classification is inspired by PIAS: new flows receive a short
high-priority budget, sustained one-way flows are demoted to bulk, and
bidirectional bursty flows remain interactive. Classification uses byte counts
and timing, not HTTPS decryption or MITM.

## Using it

[`docs/DEPLOYING.md`](docs/DEPLOYING.md) is the practical guide: build, egress
host, local client, and how to point Clash Verge or any mihomo-based client at
it. The short version is that queqiao is not a protocol Clash speaks --
`queqiaod --mode local` runs beside Clash and exposes a plain SOCKS5 ingress,
and Clash forwards to it as a `socks5` node. Templates are in
[`deploy/`](deploy).

Two things in that document decide whether a deployment works at all. The
client must be started with `--local-address if:en0`, because Clash's TUN mode
would otherwise capture queqiao's own outer connection and carry it through the
tunnel queqiao is replacing. The cancelled-download flow leak described in the
same guide is fixed; its diagnosis and regression proof remain in
[`docs/STALL-20260817.md`](docs/STALL-20260817.md).

## Current status

The repository now contains an authenticated SOCKS-to-PEP prototype with
TLS/TCP, QUIC, lane joins, cross-lane reassembly, PIAS-inspired
classification, SOCKS5 TCP CONNECT and bounded UDP ASSOCIATE, new-flow
UDP/TCP racing, bounded lane recovery, completion tombstones, aggregate
pacing, and opt-in QUIC controllers. The stock apNet
QUIC controller is retained as the control; an independently implemented
BBRv1-shaped controller, an adaptive controller, and a Hysteria-style
fixed-rate (Brutal) controller can be selected for measurement.
An isolated development service is deployed without replacing the existing
tunnels. The latest five-block real-link evidence is recorded in
[`docs/MEASUREMENTS-20260810.md`](docs/MEASUREMENTS-20260810.md); the earlier
pilot remains in [`docs/MEASUREMENTS-20260809.md`](docs/MEASUREMENTS-20260809.md).

### Measuring against a reference

The live China-US link moves between roughly 0% and 50% packet loss within
minutes, so running one transport's trials and then another's compares two
different paths rather than two transports. Two controls exist for this:

- `internal/pathsim` is a deterministic UDP path emulator — fixed delay, seeded
  loss, a bottleneck with tail-drop queueing, and an optional per-source-address
  policer. One seed reproduces one loss pattern.
- `internal/baseline` (runnable as `cmd/queqiaoref`) is a TUIC-shaped reference
  proxy: one authenticated QUIC connection, one stream per relayed TCP
  connection, unframed copying, TUIC's published transport windows — built on
  the same QUIC stack and controllers queqiao uses, so a measured gap is
  attributable to the transport design rather than to the language or library.
- `internal/extproxy` drives real implementations over the same emulated path,
  because an in-tree control on its own is a weak claim: sing-box for native
  TUIC v5 and Hysteria2, and VLESS over TLS and over WebSocket on a stream
  relay. Five trials each at 200 ms with every stack completing every trial,
  queqiao measures 37.6 / 34.0 / 26.6 Mbit/s at 0 / 1 / 3% loss against native
  TUIC's 37.4 / 30.4 / 28.6 — broadly at parity, not uniformly ahead.

`cmd/queqiaobench` runs both over one emulated path, emits JSON, and can fail a
build with `--gate` when queqiao falls behind the reference;
`scripts/bench_matrix.sh` is the standard matrix and
`scripts/bench_live_matched.sh` alternates trials between two running proxies
on a real link. [`docs/BENCHMARKING.md`](docs/BENCHMARKING.md) explains what
each knob models and how to read the numbers. On the emulated matrix, queqiao is
at or above the reference for single-flow goodput at 0-20% loss, for 4 and 8
concurrent flows, for cold and warm request latency, and for CPU-bound datapath
cost. See
[`docs/DESIGN-MULTIPATH.md`](docs/DESIGN-MULTIPATH.md) for the striping
campaign and the five transport defects that measuring it exposed -- that
document is superseded, and the striping it measures is deleted -- and
[`docs/PERFORMANCE-20260812.md`](docs/PERFORMANCE-20260812.md) for the earlier
campaign.

### Measured against deployed proxies on a real path

[`docs/MEASUREMENTS-20260816.md`](docs/MEASUREMENTS-20260816.md) is a live
China-US campaign against the implementations people actually run -- sing-box
1.13.18 serving TUIC v5, Hysteria2, VLESS over TLS and VLESS over WebSocket --
with no emulator anywhere. On that path queqiao is the fastest transport
measured, 143.1 Mbit/s median against Hysteria2's 90.2 and TUIC's 76.8, ahead
in all six rounds, and the two TCP-based VLESS configurations are two orders of
magnitude behind at 0.63 and 0.39. Interactive latency is at parity with TUIC
and Hysteria2 idle, and VLESS cannot carry a real-time video stream at all,
losing a third to a half of it.

**One result goes the other way and is still blocking.** Under its own bulk load
queqiao's interactive tail degrades more than either competitor's -- SSH p99
302 to 940 ms, voice loss 1.9% to 9.0% -- which is what flow classification and
bulk isolation exist to prevent, and the emulated 208 ms result below does not
reproduce there.

That stall has since been diagnosed as two unrelated defects, neither of them
the path's doing -- it reproduces on the emulator with no network involved.
[`docs/STALL-20260817.md`](docs/STALL-20260817.md) has both. A seeded round
trip becoming a permanent `min_rtt` is **fixed**, and it was depressing every
throughput number above by about 30%: the same live windows now measure around
200 Mbit/s. The other defect was triggered by *aborted* transfers, which that
campaign's fixed-duration windows performed on every trial. It is also fixed:
local close is now a bounded lifecycle event, and the peer cancels response
work immediately rather than waiting for data the application discarded.

That path is also not the 45%-erasure channel this project targets, so the
campaign tests the transport rather than the design's central claim.

### The live link is the number to believe

Measured against the real China-US path -- 263 ms average round trip, 226 to
440 ms range, 5% loss, 48 ms of jitter -- fourteen alternating 8 MiB rounds:
queqiao 10.24 Mbit/s mean against the reference's 10.59, ahead in 6 of 14 rounds.
**Parity, not an advantage.**

That campaign also found the emulator wrong in a way no emulated cell showed. A
first live run lost 17 of 18 rounds, 5.42 against 8.85, and the cause was a
change to the congestion controller's application-limited test that had measured
*better* on the emulator. On a path whose round trip varies by a factor of two,
that marking is what keeps spuriously low delivery-rate samples out of BBR's
bandwidth filter; removing it halved the throughput. Reverting cost nothing
emulated. See [`docs/DESIGN-MULTIPATH.md`](docs/DESIGN-MULTIPATH.md) §7.6.

An earlier campaign at 33% measured loss, 20 alternating 4 MiB trials, completed
10/10 for both stacks with queqiao 23% ahead on the median; that run predates the
current transport and has not been repeated.

### Protecting interactive latency under bulk load

A bulk transfer sharing one congestion-controlled connection with interactive
traffic queues that traffic behind it. That is unavoidable for a single-connection
design, and it is what classifying flows is for: once a flow is classified bulk,
queqiao moves it onto its own QUIC connection and keeps the pooled control
connection for short and interactive flows.

Measured at 200 ms and 1% loss with a 50 MiB transfer running and small requests
alongside, five trials (medians, reference / queqiao):

| | Reference | queqiao |
| --- | ---: | ---: |
| bulk goodput | 57.3 Mbit/s | 52.0 Mbit/s |
| interactive median | 323 ms | 208 ms |
| interactive 95th percentile | 506 ms | 386 ms |

208 ms is the idle round trip, so interactive requests do not queue behind bulk
at all.

Isolation is demand-driven, and the test is made per lane selection rather than
once per flow: a bulk flow alone on the control connection has nothing to
protect, so it keeps using it and keeps full goodput; the moment another flow
appears, the next chunk goes elsewhere. Paying it unconditionally was measurably
wrong -- a bulk flow that gained a lane mid-transfer used to abandon a
fully-warmed path for one with a fresh congestion window, which on the policed
path cost 17% of the transfer. It requires `--quic-pool`, which is where the
shared control connection exists at all.

### Recommended configuration

The measured configuration is `--quic-pool --optimistic-open`. There is nothing
to size: a flow's data goes over one connection, and what it may commit ahead
of its transport is a fraction of that transport's own congestion window, so it
follows the path rather than a constant. Both flags remain opt-in because this
project's release gates are not met, not because they measured badly.

Outside the standard matrix, queqiao is ahead at 20 ms / 1 Gbit/s (708 against
685 Mbit/s), at parity at 30 ms / 200 Mbit/s, ahead under 25% upstream loss
(51.6 against 50.5), and far ahead where the path reorders: with 20 ms of
jitter it measures 8.4 Mbit/s against 2.9, because reassembling by byte offset
does not stall a stream on an out-of-order packet the way relaying one does.

The prototype is still not safe to use as a general-purpose production tunnel.
Broader loss/soak campaigns remain outstanding, and under extreme correlated
loss (35% in 10-packet bursts) queqiao's behavior still differs from the
reference's: it completes more transfers but is slower on the ones both
finish. That case is now diagnosed -- lanes die of QUIC's idle timeout mid-burst
and the rejoin is refused because the server's session went with its own
connection -- and a flake in the TCP rescue path (about one run in eight, and
every run under `-race`) is recorded in
[`docs/DESIGN-MULTIPATH.md`](docs/DESIGN-MULTIPATH.md). TUN/VLESS ingress is
not yet implemented. A mid-session UDP rescue now reclaims the same remote
relay socket by token, so the destination keeps seeing one source address, but
datagrams in flight when the lane died are still lost rather than replayed. The project has not passed all controlled-loss/resource release
gates in [`docs/PRODUCTION-DESIGN.md`](docs/PRODUCTION-DESIGN.md).

## Design goals

- One local SOCKS5/TUN-facing agent and one fixed-egress US agent.
- One application TCP flow can be framed, reordered, and striped over
  one QUIC connection, framed so that arrival order does not gate delivery.
  Striping a flow across several connections was tried, measured, and deleted:
  the path has one bottleneck per endpoint pair, and an open-loop probe
  delivers the same total however many connections carry it.
- A PIAS-inspired policy that protects one-shot and interactive flows and moves
  a classified bulk flow's *data* onto a connection of its own, keeping its
  *control* on the pooled one. That split does two things at once: short flows
  keep a connection with no bulk congestion window on it, and the bulk flow
  keeps an acknowledgement path with no bulk in front of it. A flow's data
  plane is one connection, by construction rather than by policy.
- No HTTPS MITM: the optimizer forwards encrypted application bytes.
- UDP health probing, UDP/TCP racing, fallback, and bounded mid-session lane
  replacement.
- SOCKS5 UDP rides the connection's QUIC datagrams where QUIC negotiated them,
  and the lane's control stream otherwise. An application that chose UDP has
  already decided a late packet is worse than a lost one, and a stream gives it
  the opposite. Measured across an emulated 15% loss path at a 200 ms round
  trip, the worst packet takes 202 ms on datagrams against 448 to 658 ms on the
  stream, where every loss holds up what is behind it. Nothing is configured:
  QUIC's own capability exchange decides it, so a TLS/TCP lane or a peer without
  datagram support keeps the previous framing. `--udp-on-stream` forces the old
  substrate at both endpoints, as the control for measuring the new one.
- Dedicated cold flows pipeline the authenticated `HELLO` and destination
  `OPEN` frames, preserving the original wire order of acknowledgements while
  removing one sequential China-US control exchange. Pooled streams retain
  capability-negotiated `OPEN_FAST`.
- One aggregate token bucket with an interactive reserve above all lanes.
- Optional localhost `/metrics` counters for flow completion, bytes, fallback,
  lane failure/replacement, PIAS class transitions, active QUIC RTT/loss,
  controller mode/max bandwidth/pacing/cwnd/bytes-in-flight/min-RTT/recovery
  telemetry, rescue-window evictions and unreplayable flows, bulk isolations,
  plus explicit flow-idle/lifetime timeouts. The endpoint is
  loopback-only in the development service.
- Reproducible measurements for latency, throughput, loss, queueing, and
  application-visible failures.

There is no lane count to configure, because there is no lane count. A flow's
data is carried by one connection. What used to sit here -- a controlled
marginal-gain search that added a connection, measured whether goodput rose,
and kept or retired it -- is deleted along with the striping it searched for.

What that search concluded, on the emulated path that rewards striping, is
recorded in [`docs/DESIGN-MULTIPATH.md`](docs/DESIGN-MULTIPATH.md); it does not
describe the path this targets. See [`docs/DESIGN.md`](docs/DESIGN.md) for why
that regime is not this one.

`--quic-pool` is an explicit opt-in that keeps one bounded QUIC connection for
initial/control streams and lets multiple short flows share its congestion
controller. On a capable peer, bulk promotion also lazily creates one
separately authenticated secondary QUIC pool and attaches the lane with a
capability-gated `OPEN_JOIN_FAST`; peers without that capability use the
legacy independent join. These modes must
be validated with the supplied single-flow and concurrent-flow harnesses
before being enabled in a live Clash profile. The first pooled stream performs
the authenticated `HELLO`; subsequent streams on a capable peer use a
connection-scoped fast open while retaining independent flow identities and
US-side destination-policy checks. Capability-free peers automatically keep
the legacy per-stream handshake. The secondary pool is bounded to one
connection, expires after 30 seconds of idle time, and is never required for
correctness or TCP fallback.

## Non-goals

- Circumventing a hard aggregate capacity limit on the China-US path.
- Automatically decrypting or classifying HTTPS URLs or payloads.
- Claiming that multiple lanes are always fair or faster.
- Replacing the existing tunnel before a measured rollback path exists.

## Development

```sh
go test ./...
go vet ./...
go run ./cmd/queqiaod --help
```

The real-link benchmark harness will live under `scripts/` and will use an
isolated listener on `icourses-dev`; the existing Xray, sing-box, and Clash
Verge services remain out of scope for automatic modification.

On a macOS host where Clash TUN captures the numeric server address, the local
agent can bind the outer socket to the physical source IP:

```sh
queqiaod --mode local --listen 127.0.0.1:12080 \
  --remote 23.135.236.244:12443 --server-name icourses-dev.01.me \
  --local-address auto --transport auto \
  --secret-file .dev/session.secret
```

`--local-address auto` discovers the active non-loopback, non-point-to-point
IPv4 address before each outer dial, so ordinary DHCP changes do not strand
the client. A literal address or `if:NAME` may be used when the host has more
than one physical IPv4 address. Binding it is preferable to using
the Clash fake-DNS address, which would route the PEP through the tunnel being
measured.

For a controlled bulk experiment, both endpoints can select the fixed-rate
controller. The rate is bytes per second per QUIC lane; it must be measured for
the path and reduced if loss or interactive tail latency rises:

```sh
queqiaod --mode local --listen 127.0.0.1:12080 \
  --remote 23.135.236.244:12443 --server-name icourses-dev.01.me \
  --local-address auto --transport quic \
  --congestion brutal --brutal-bytes-per-sec 1048576 \
  --secret-file .dev/session.secret
```

The matching server must use the same `--congestion` and rate. To constrain
all lanes and flows together while reserving service for interactive traffic,
add the same aggregate budget at both endpoints, for example:

```text
--aggregate-bytes-per-sec 8388608 \
--interactive-reserve-bytes-per-sec 524288
```

`adaptive` is the safer experimental choice when no target is known; `reno`
is the correctness baseline. `bbr` is the original path-specific experimental
controller. `bbr-tuic` is a separate, opt-in Go port of TUIC's
`quinn-congestions` BBR model; it adds ACK aggregation, a round-based
bandwidth filter, TUIC-style recovery, and ProbeRTT behavior, but it is not a
claim that the controller is faster on every path. Both BBR variants must pass
the same loss, queue-delay, and application-tail campaign before selection.
Brutal remains an operator-supplied measurement mode, not a safe unattended
default. These modes are not a recommendation to change a live Clash profile
without a rollback plan.

For an operator-only health endpoint, bind metrics to loopback and keep it off
the public listener:

```text
--metrics-listen 127.0.0.1:19090
```

Flows are bounded by default at 30 minutes without application payload and
24 hours total lifetime. These limits protect session and destination-socket
resources while still allowing quiet interactive sessions; tune them only
with an explicit operational policy using `--flow-idle-timeout` and
`--flow-max-lifetime`.

## Security model

The wire protocol must use an audited TLS/QUIC implementation and explicit
session authentication. It must impose limits on frame sizes, concurrent
flows, buffered bytes, handshake work, and reconnect attempts. Rolling a new
cryptographic primitive or accepting unauthenticated lane joins is out of
scope.
