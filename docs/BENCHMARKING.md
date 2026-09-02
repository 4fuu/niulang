# Measuring this transport

> [!NOTE]
> **Status:** Current benchmark methodology for Niulang protocol 3
> **Last reviewed:** 2026-09-02

This is the reproducibility guide for the measurement rig. It exists because the motivating
link moved between roughly 0% and 50% packet loss within minutes, so
running one transport's trials and then another's compares two path windows
rather than two transports. Every performance claim in this repository should
be reproducible with the commands here.

The harness can establish correctness, behavior under controlled impairment,
and a fair same-window comparison; it cannot turn one path into a universal
performance claim.

## One protocol, three workload views

Short-lived, interactive, and bulk are evaluation families for the same
Niulang architecture. The benchmark changes the offered workload; it does not
select three transports.

| Family | Primary outputs | Minimum comparison |
| --- | --- | --- |
| Short-lived | fresh/warm connect, first byte, completion, failure tail | matched small requests with `--latency` |
| Interactive | latency/jitter/delivery while bulk occupies the bottleneck | `--interactive` with an idle-path control |
| Bulk | useful goodput, completion count, physical/logical bytes, CPU and memory | matched downloads and uploads long enough to leave startup |

For a transport or controller change, report applicable results from every
family. A bulk improvement is incomplete evidence if it moves interactive tail
latency or small-flow setup in the wrong direction.

### Mixed storefront pages

`niulangbench --page` replaces the uniform bulk object with the fixed
`storefront-v1` profile: a document, styles, JavaScript, icons, thumbnails,
screenshot, hero image, and a 512 KiB first video segment begin together. An
8 MiB video tail begins only after the first segment completes. The visible
critical set is 4000 KiB and the complete workload is 12192 KiB.

The primary page metric is `critical_complete_ms`, the time until every visible
resource and the first video segment has arrived. `video_first_segment_ms`
checks media startup independently, `critical_spread_ms` exposes a straggling
visible resource, and `full_complete_ms` plus goodput prevents a scheduler from
making the page look fast by starving sustained media. JSON retains each
resource's start, connect, first-byte, completion, byte count, and status. The
regression gate checks page-critical, video-first, and full-completion p95 in
addition to completion and goodput.

Every resource is a separate SOCKS TCP flow sharing the same warm outer HTTP/3
connection, with one Extended CONNECT request stream per Niulang lane. This
models a browser using several origin/CDN connections, which is the layer
Niulang can schedule without decrypting traffic. It does not claim
to expose HTTP/2 or HTTP/3 stream priorities hidden inside one encrypted,
ordered inner connection: bytes later in that connection cannot legally pass
earlier bytes at the proxy. Use the profile to judge cross-flow isolation and
fairness, not application-layer resource discovery.

Niulang therefore schedules only what this layer can know honestly. DATA
carrier handoffs on QUIC connections with the same stable provider-path
identity share one default scheduling window; control frames, UDP packets, and
bypass it. Each flow receives at most 512 KiB of preferred startup service. The
path admits at most eight carrier handoffs at once and two per flow, serves
preferred startup or interactive work against sustained bulk at a 3:1 ratio,
and ages bulk into service after 250 ms. Coded DATA releases its grant only
after its source symbols reach the datagram carrier, not when they enter the
coded path's own queue. Flow close wakes pending grants and reclaims granted
ones.

This is cross-flow fairness, not TLS content inference. The proxy does not know
whether an encrypted flow is HTML, an image, or video; bounded startup service
lets all of those make initial progress, while the existing observed flow
classifier eventually moves sustained downloads and media tails to bulk.
`--flow-scheduling=false` is the matched control, and
`--flow-startup-bytes` overrides the startup bound for experiments.

```sh
go run ./cmd/niulangbench --page --stacks baseline,niulang \
    --congestion erasure --rtt 100 --rate 50 --loss 1 \
    --trials 5 --seed 4701 --json /tmp/page.json
```

The 2026-08-27 development campaign used five trials per cell, seed 7401,
the one-BDP queue, and the complete profile above. Times are milliseconds; a
failed trial remains in its cell's completion count and tail statistics.

| RTT / rate / loss | Reference complete | Reference critical / video / full p95 | Niulang complete | Niulang critical / video / full p95 |
| --- | ---: | ---: | ---: | ---: |
| 50 ms / 50 Mbit/s / 0% | 5/5 | 906 / 680 / 2213 | 5/5 | 1008 / 694 / 2220 |
| 226 ms / 50 Mbit/s / 0% | 5/5 | 1908 / 1694 / 3379 | 5/5 | 1729 / 1514 / 3333 |
| 50 ms / 50 Mbit/s / 1% | 5/5 | 14172 / 7075 / 33779 | 5/5 | 1039 / 721 / 2302 |
| 100 ms / 50 Mbit/s / 5% | 0/5 | incomplete | 5/5 | 1468 / 1316 / 3017 |
| 100 ms / 50 Mbit/s / 15%, burst 6 | 0/5 | incomplete | 5/5 | 1949 / 1670 / 3776 |
| 226 ms / 10 Mbit/s / 1% | 0/5 | 59534 / 31259 / 59538 | 5/5 | 5475 / 3930 / 12471 |

This is not a universal latency win. On the clean 50 ms path, full completion
and goodput were effectively equal while Niulang's visible-set p95 was 102 ms
slower. It reduced median sender-induced queue drops from 748 to 282 in that
cell. At 226 ms and 50 Mbit/s it was faster on all three page tails and reduced
median queue drops from 3542 to 101. With path loss, completion and tails were
the larger distinction.

A clean-path BBR diagnostic made the visible set faster (761 ms p95) but raised
median sender-induced drops to 4465, versus 282 under the erasure policy. That
is an overshoot trade, not a low-loss improvement, so it is not evidence for a
more aggressive default. The useful general policy is instead to keep a mixed
short-flow fanout on one warm congestion context and bound proactive secondary
bulk connections. Niulang cannot identify a video segment hidden inside TLS;
specific media priority requires an application-visible signal and must not be
inferred from encrypted byte patterns.

Three same-binary, matched 10-trial controls validated the cross-flow scheduler
before it became the default. With scheduling disabled then enabled, a clean
50 ms, 50 Mbit/s path (seed 7401) stayed 10/10 complete and changed median
goodput from 45.031 to 45.141 Mbit/s. Visible-critical p50/p95 changed from
1007/1091 to 1000/1023 ms, video-first p50/p95 from 595/667 to 596/692 ms, and
full-page p95 from 2223 to 2226 ms. The page tail improved while media and total
completion remained healthy rather than receiving strict priority.

At 100 ms with 5% independent loss (seed 9401), both sides completed 10/10;
goodput changed from 33.070 to 33.483 Mbit/s, visible-critical p50/p95 from
1447/1554 to 1406/1509 ms, video-first p50/p95 from 1216/1282 to 1147/1268 ms,
and full-page p95 from 3056 to 3020 ms. At 100 ms with 15% loss in mean bursts
of six packets (seed 9601), completion changed from 9/10 to 10/10, median
goodput from 26.245 to 26.892 Mbit/s, visible-critical p95 from 2266 to 1908 ms,
and full-page p95 from 7889 to 4091 ms. Visible-critical p50 moved from 1746 to
1760 ms and video-first p95 from 1746 to 1759 ms, so the burst result supports
the tail and completion benefit without claiming every quantile improves.

Five-trial 4 MiB download controls found no completion or material bulk
regression. At 50 ms with no loss, one/eight-flow medians changed from
39.141/47.149 to 39.066/47.183 Mbit/s. At 100 ms with 5% independent loss they
changed from 25.946/41.330 to 26.013/42.106 Mbit/s. Every cell completed 5/5.
These results establish the narrow default; they do not imply that application
content can be recovered from TLS or that sustained media should be assigned a
special hidden type.

The campaign also found a correctness failure: a response producer could fill
its local TCP buffer, close, and have the proxy's five-second ambiguous-close
timer abort the still-progressing remote transfer. Strictly advancing
cumulative response ACKs now renew that inactivity grace; duplicate ACKs do
not. The previously failing 226 ms / 10 Mbit/s / 1% cell changed from 0/5 to
5/5 after that fix.

## The three pieces

**`internal/pathsim`** — a deterministic UDP path emulator. Clients send to the
relay in place of the server. It applies, per direction:

| Knob | Models |
| --- | --- |
| `OneWayDelay` | propagation delay; the emulated RTT is twice it |
| `DelayJitter` | per-packet delay variation, which also reorders |
| `LossRate` | overall drop probability |
| `UpstreamLossRate` | a different drop probability client-to-server |
| `LossBurstPackets` | correlated loss: a Gilbert chain with this mean burst |
| `RateBytesPerSec` | a bottleneck with tail-drop queueing |
| `PerFlowRateBytesPerSec` | a policer applied per source address, with a bucket scaled from its own rate |
| `PolicerRefillPeriod` | replace the queue with a token-bucket policer refilled in fixed quanta |
| `PolicerBurstBytes` | policer bucket depth; zero is one quantum plus one packet |
| `DelayWander` | amplitude of a correlated random walk on the one-way delay |
| `QueueBytes` | the bottleneck buffer; zero selects one BDP |
| `Seed` | makes the loss pattern reproducible |

### Jitter and wander are different impairments

`DelayJitter` draws per packet, so it reorders and leaves the smoothed round
trip near the minimum. `DelayWander` walks the one-way delay on a slower clock,
so a whole flight shifts together: the round trip varies while the minimum stays
put, and almost nothing reorders. A long-haul path does the second. The live
China-US link this project targets measures 226 to 440 ms with a 48 ms standard
deviation and a stable minimum, which `--delay-wander 107 --rtt 226`
approximates.

This mattered: a change to the congestion controller measured better on the
emulator without wander and cost more than half the throughput live. Adding
wander makes the emulator produce that regime -- it costs the transport 26% of
its goodput on an otherwise identical path -- but it does **not** by itself
reproduce that particular verdict. The emulator is a filter, not an oracle, and
this is the measured limit of the filter.

One seed reproduces one loss pattern exactly. That property has its own test,
because everything else depends on it.

**`internal/baseline`** (runnable as `cmd/niulangref`) — a TUIC-shaped reference
proxy: one authenticated QUIC connection, one bidirectional stream per relayed
TCP connection, a short destination header, then unframed copying. It runs on
the same quic-go fork and the same congestion controllers niulang uses, with
TUIC's published transport windows.

It is a control, not a claim to be native TUIC. Comparing against a separately
built Rust implementation conflates the transport design with the language and
QUIC library; comparing against this isolates the design.

**`internal/extproxy`** — launches third-party implementations so the
comparison is not limited to an in-tree control:

| Stack | Implementation | Carried by | Needs |
| --- | --- | --- | --- |
| `tuic` | sing-box, native TUIC v5 | UDP relay | `--sing-box` |
| `hysteria2` | sing-box | UDP relay | `--sing-box` |
| `vless-tcp` | sing-box, VLESS over TLS | TCP relay | `--sing-box` |
| `vless-ws` | sing-box, VLESS over WebSocket | TCP relay | `--sing-box` |
| `kcptun` | kcptun, KCP with fixed-rate FEC | UDP relay | `--kcptun-client`, `--kcptun-server` |

Each runs as a server the emulator forwards to and a client exposing SOCKS5,
over exactly the same seeded path as niulang. Where the transport has TLS the
client trusts exactly the server's certificate, so nothing disables
verification. They are registry entries rather than special cases; see the
contract below for adding another.

`kcptun` is the one that is not a proxy. It forwards a local TCP port, so the
harness runs a SOCKS5 server of its own beyond the emulator for the tunnel's
server to forward to, and the tunnel's local port is what the benchmark speaks
to:

```
benchmark -> [kcptun client] -> emulator -> [kcptun server]
          -> [harness SOCKS5 target] -> destination
```

The measured path is crossed exactly once, as for every other stack; the two
extra hops are loopback. kcptun's upstream repository was withdrawn and
`github.com/xtaci/kcptun` no longer resolves, which is why the stack takes two
binary paths rather than naming a source: build from whatever copy you trust,
and record which one and at what commit alongside the run, as for any other
measured dependency. It is the comparison niulang's own coding most needs,
because both transports spend parity to avoid a round trip and choose how much
in opposite ways — see the fixed-parity section below before quoting a number.
kcptun ships one program per side, which is why it takes two paths; its
compression is disabled, because the benchmark's payload is a repeating byte
ramp that snappy would reduce to almost nothing, and the stack would then
report the compressor's rate rather than the path's.

**`cmd/niulangbench`** — runs the selected stacks over one emulated path in a
single process and reports per-trial rows, a summary, optional JSON, and an
optional regression gate.

## Do not compare a QUIC stack against a TCP stack

The two relays model different things, and their numbers are not
interchangeable.

The UDP relay models per-packet serialization, tail drop, and loss. The TCP
relay cannot: a userspace relay receives a byte stream, not segments, so
dropping bytes would deliver a hole rather than trigger a retransmission, and
the kernel TCP stack whose loss recovery is the interesting behaviour sits
below the relay. It therefore refuses a loss rate instead of silently
producing a lossless result, and it applies backpressure where the packet relay
would tail-drop.

So `vless-ws` against `vless-tcp` is a fair comparison — it isolates
WebSocket's framing cost — and `tuic` against `niulang` is a fair comparison.
`vless-tcp` against `tuic` is not. Emulating loss for a stream transport needs
an IP-layer facility such as dummynet, which needs privilege this harness does
not take.

## Adding an external transport

A stack is a pair of processes the harness starts, waits for, and stops. The
harness owns the emulated path, the addresses, the TLS material, the work
directory and the process lifetime. A stack answers two questions: what
configuration to write, and what to run. `internal/extproxy` keeps those apart
so a transport is added by registering it rather than by editing the launcher:

```go
// internal/extproxy/extproxy.go
var stacks = map[Kind]stack{
    TUIC: {transport: "udp", implementation: "sing-box", launch: singBoxLaunch},
    AnyTLS: {transport: "tcp", implementation: "sing-box", launch: singBoxLaunch},
    ...
}

// A launch builder returns what to write and what to run.
func singBoxLaunch(cfg Config) (Launch, error)
```

`Launch` carries `Files` (written before either process starts, keyed by a path
under `Config.WorkDir`), `ServerArgs` and `ClientArgs`, and optionally
`ServerBinary`/`ClientBinary` for an implementation that ships one program per
side. `Plan` resolves it without running anything, which is how a stack's
wiring is tested with nothing installed.

A release whose real identity setup needs a running gateway can own a custom
start lifecycle. The Queqiao adapter uses that path to initialize provider
state, issue a one-time invitation, enroll a device over a companion TCP relay,
and then force the measured client data plane to QUIC. Enrollment completes
before request timing begins and all state remains inside `WorkDir`.

What the running pair must satisfy:

1. **The server binds `ServerListen` exactly.** That is the address the
   emulator forwards to. A server that binds anything else produces a working
   measurement that never crosses the emulated path, which is worse than a
   failure because it looks like a result.
2. **The client dials `ClientRemote`**, which is the emulator, never the server
   directly.
3. **The client exposes SOCKS5 at `SOCKSListen`.** SOCKS5 is the whole contract
   with the benchmark, and the pair is considered ready when that address
   accepts a TCP connection.
4. **TLS material is the harness's.** The server uses `CertificatePath` and
   `KeyPath`; the client trusts exactly `CertificatePath`. No verification
   bypass, and the client's configuration must never reference the private key.
   Both are tested.
5. **Configuration goes in `WorkDir` and nowhere else.** The harness creates it
   and removes it.
6. **The processes must die with their process group.** The harness starts each
   side in its own group and escalates if it does not exit; an implementation
   that daemonizes or leaves a helper behind holds its ports and silently
   contaminates every later trial.
7. **Declare the relay honestly.** `transport: "udp"` puts the stack behind the
   packet emulator, where loss, delay and rate apply. `"tcp"` means it can only
   be measured with `--loss 0`, for the reason in the section above.

The harness has to be told where the binary comes from, which is the one step
that is not in `internal/extproxy`. Add a flag for it in `cmd/niulangbench` and
a case to `externalBinaries` keyed on the implementation the registry entry
names; a stack that skips this registers fine and then refuses every run with
`stack "x" needs a y binary, and this benchmark has no flag for one`. An
implementation shipping one program per side takes two flags, as kcptun does.

### What the harness records

A stack is measured from the outside. Per trial it records elapsed seconds,
goodput, and whether the transfer completed -- and completion means the exact
expected byte count arrived, not merely that the request returned. `--latency`
and `--interactive` add cold/warm setup and first-byte splits, and `--contend`
records each stack's share of one shared bottleneck. `--json` writes all of it
with the commit, dirty bit, Go toolchain, target, module graph, exact
arguments, and seeded path parameters, which is what makes a cell reproducible
by somebody who was not there.

The harness records Niulang's bounded wire-cap telemetry because it runs that
stack in-process. It does not read arbitrary controller internals, and an
external stack is a pair of processes whose logs are captured only to explain
a failure. So a claim that depends on what the transport did internally --
parity share of bytes sent, retransmissions, window behaviour -- has to be
recorded alongside the run by
hand, from that implementation's own output. The fixed-parity section below is
one instance of that requirement rather than an exception to it.

A tunnel rather than a proxy has no SOCKS5 of its own: it forwards a local TCP
port to a target. Declare `socksTarget: true` in the registry entry and the
harness runs `extproxy.StartSOCKSTarget` beyond the emulator and passes its
address as `Config.SOCKSTarget`; forward to that, and expose the tunnel's own
local port as `SOCKSListen`. The target is in-process on purpose: an external
SOCKS5 server would be a third implementation in the measurement whose version
and buffering nobody recorded. `Plan` refuses such a stack when the target is
missing, because a tunnel forwarding to nothing fails every trial at SOCKS with
an error that says nothing about why.

`kcptun` is the worked example of all of this.

### Comparing a fixed-parity transport

A transport with FEC in the matrix is worth having, and it needs one thing said
about the comparison before the numbers are taken. Niulang chooses its parity
from the erasure it measures, and it revises that choice while a flow runs; a
kcptun-style code rate is a constant chosen in advance. A single configuration
is therefore a comparison against one guess, and whichever way it lands the
result is mostly about the guess.

So a comparison should carry:

- **Size the window before anything else.** This is not a refinement, it is
  the difference between a measurement and a rigged one. kcptun's default send
  window of 128 packets caps a 210 ms path at about 6.6 Mbit/s no matter what
  the code does. Measured live on that path, the same build went from 5.09 to
  25.36 Mbit/s -- five times faster -- when `--kcptun-sndwnd` and
  `--kcptun-rcvwnd` were raised to a delay-bandwidth product's worth of
  packets, and the gap to niulang fell from 9.6x to 2.1x. A comparison run at
  the default windows is measuring the window.
- **A parity sweep, not a point.** `--kcptun-parityshard` against a fixed
  `--kcptun-datashard`, spanning ratios above and below the erasure rate, with
  the best one shown against niulang rather than an arbitrary one. The
  defaults are kcptun's own (10 data, 3 parity, `fast`, 128/512 windows), which
  is a starting point rather than an answer.
- **The cost alongside the rate.** Parity is bandwidth spent on the same
  bottleneck, so report the parity share of bytes sent on both sides. Niulang's
  is `fec_repairs_total` over `fec_sent_total` in its runtime log, and kcptun's
  is the ratio the sweep fixed; a transport buying more of the wire can finish
  sooner while delivering fewer useful bytes per byte sent, and a goodput
  column alone hides that.
- **Every parameter that moves the answer.** The implementation version, and
  the settings the harness passes: `--kcptun-mode`, `--kcptun-datashard`,
  `--kcptun-parityshard`, `--kcptun-sndwnd`, `--kcptun-rcvwnd`. The windows are
  the congestion control in practice, so a run that does not state them is not
  reproducible. They appear in the archival JSON with the rest of the command.
- **The same rules as every other cell**: one seeded path, alternating trial
  order, medians over all trials counting failures at their partial rate, and a
  check against the calibration bound below.

```sh
# One cell of a parity sweep, against niulang on the same seeded path.
for parity in 1 3 6 10; do
  go run ./cmd/niulangbench --stacks niulang,kcptun \
      --kcptun-client ./kcptun/client --kcptun-server ./kcptun/server \
      --kcptun-mode fast3 --kcptun-datashard 10 --kcptun-parityshard "$parity" \
      --rtt 200 --loss 20 --rate 50 --bytes $((16*1024*1024)) --trials 5 \
      --json "/tmp/kcptun-parity-$parity.json"
done
```

## Reading the numbers

The summary reports **medians over all trials, counting failures at their
partial rate**, alongside the completion rate.

This matters. A median over completed trials alone rewards a transport for
giving up: in a 35%-burst-loss block the reference completed 7 of 12 trials and
niulang 10 of 12, and scoring only the successes made the transport that
finished the hard trials look slower than the one that abandoned them.
`median_mbits_completed_only` is retained for continuity with older campaign
notes and must never be compared across stacks with different completion rates.

`--interactive` additionally issues small requests during the bulk transfer and
splits their latency into connect and first-byte. Those are different defects
with different fixes: a slow connect is flow setup, a slow first byte is
queueing behind the bulk transfer.

The JSON record also keeps per-trial upstream and downstream path counters.
`packets_erased` is seeded ambient loss; `bottleneck_dropped` is queue or
policer overshoot. A controller has not reduced loss merely because useful
goodput rose -- it must be checked against both counters and interactive tail.

## Low-latency and policer experiments

Four focused scripts cover the cases a throughput-only matrix misses:

```sh
# Current erasure control, aggregate application budget, compensating Brutal,
# and a fixed wire-rate control on an 8 ms token-bucket policer.
./scripts/bench_policer_controls.sh /tmp/niulang-policer

# Fresh versus reused QUIC connections across clean and lossy RTTs.
./scripts/bench_connection_reuse.sh /tmp/niulang-reuse

# Independent loss, burst loss, and delay wander. Optionally add the real
# Hysteria2 implementation from sing-box.
SING_BOX=/path/to/sing-box \
    ./scripts/bench_loss_resilience.sh /tmp/niulang-resilience

# Application UDP delivery and latency over datagrams versus ordered streams;
# optionally include Hysteria2 in the same seeded path matrix.
SING_BOX=/path/to/sing-box \
    ./scripts/bench_udp_delivery.sh /tmp/niulang-udp

# A serial low-loss campaign for latency and relatively high bandwidth. Key
# cells use ten trials, other cells use five, and CPU/peak RSS are retained.
SING_BOX=/path/to/sing-box \
    ./scripts/bench_low_latency_bandwidth.sh /tmp/niulang-low-loss
```

The combined campaign makes 0%, 1%, and 5% independent loss its primary
regime. It covers 50--400 ms RTT, 50--100 Mbit/s bottlenecks, 256- and
1200-byte UDP payloads, three policer refill quanta, connection reuse, and a
small burst-loss boundary. One 15% UDP cell is retained only to quantify
ordered-stream head-of-line delay; it is not part of the low-loss verdict.
Each cell runs serially so another benchmark process cannot distort its tail or
resource data. `resources.tsv` reports wall, user and system CPU time plus peak
RSS for the whole cell, including external transports reaped by the harness.

Each in-process trial resets the shared path model before it starts. The
Niulang client binds `127.0.0.2` while the server remains on `127.0.0.1`, so
the client and server directions cannot collapse into the same production
`local->peer` model key merely because both endpoints are loopback. Omitting
either isolation contaminates later erasure/FEC trials with observations from
earlier seeds or from the reverse direction. Controllers that do not consume
the path model can hide this error, so a clean BBR or Brutal control is not
evidence that erasure-controller isolation is correct.

The fixed-rate control is `--congestion brutal-no-comp --brutal-rate N`.
It borrows one narrow idea from Hysteria2's Brutal controller: capacity is an
operator-supplied budget, not a number inferred from a short burst. Unlike the
normal `brutal` mode, it does not divide that budget by the ACK rate. Normal
Brutal's compensation is bounded to 1.25x, but even that is the wrong direction
at a policer: its additional packets become additional policer drops.

This mode is intentionally a **per-lane wire rate**, not an automatic path
controller and not an aggregate guarantee. Several active lanes can offer the
rate several times. `--aggregate-rate` and `--interactive-reserve` exercise the
existing shared application-frame budget, but that budget does not count QUIC
headers or FEC repairs and therefore is not a strict wire cap. The policer
script measures both controls rather than presenting either approximation as a
finished automatic brake.

Both Brutal modes also replace the erasure controller that feeds Niulang's
shared path model, so adaptive FEC does not receive an erasure estimate in
these controls. That is acceptable for isolating fixed pacing, but it is why
`brutal-no-comp` is not the finished low-loss design. A production version
would cap wire pacing at the shared path boundary while retaining the erasure
model and its adaptive coding.

The opt-in prototype of that design is `--wire-cap-rate N
--wire-interactive-reserve R` in `niulangbench`, or
`--wire-cap-bytes-per-sec N --wire-interactive-reserve-bytes-per-sec R` in
`niulangd`. It wraps the selected explicit QUIC controller instead of replacing
it. Connections to the same provider path share one total scheduler; validated
non-control data connections additionally share a bulk scheduler at `N-R`.
Pooled/control connections use the total scheduler, so the reserve remains
available for interactive work. The wrapper forwards the erasure controller's
extended ACK/loss events unchanged, preserving its path model and adaptive FEC.
`reno` is rejected because the QUIC fork does not expose its internal controller
for wrapping. Zero remains the default and preserves existing behavior.

This is a burst-bounded aggregate **QUIC packet-byte pacing cap**, not a strict
NIC wire cap. It charges the packet size reported synchronously to congestion
control, including QUIC overhead and repairs, but not UDP/IP headers. The
handshake occurs before the wrapper is installed; ACK-only and PTO packets can
bypass pacing eligibility and are charged afterward; and concurrent connection
send loops can each pass one eligibility check before either charge is visible.
The bounded debt is repaid by later packets and exported as telemetry. A strict
all-packet cap needs a pre-registration pacing hook in the QUIC implementation
rather than blocking `PacketConn.Write`: blocking there would register send/PTO
state before the packet actually left and corrupt RTT, PTO, and erasure
sampling.

The JSON `wire_cap` object records each endpoint's configured total and bulk
rates, charged QUIC bytes, overshoot packets, and sampled scheduler debt. The
runtime exports the corresponding `niulang_quic_wire_cap_*` metrics. Continue
to use pathsim's `packets_erased` and `bottleneck_dropped` as the authoritative
ambient-loss and sender-overshoot split.

On the 100 Mbit/s development policer, a 95 Mbit/s cap removed all measured
bottleneck drops at 8 and 16 ms refill intervals, but still dropped 6.996% at
1 ms. That boundary is consistent with the ten-packet burst allowance and
host timer granularity. It is evidence to keep the prototype opt-in, not a
reason to relabel it as a hard cap.

The default erasure controller now infers a queue-less policer's sustained rate,
but that estimate is not a wire cap. On the seeded 300 ms, 250000 B/s, 8
ms-refill characterization path, three full-suite runs and six isolated
Protocol 3 repetitions put the peak bandwidth estimate at 1.00--1.04x the path
while peak pacing still reached 4.1--4.2x and sender-induced loss remained
17.4--23.7%. The queue stayed at 1--2 ms, so the delay brake had no signal. The
accurate estimate is a resolved component of the old falsification; residual
overdrive remains unresolved and is gated by both a 3.5x pacing floor and a
15% loss floor in `internal/pep/case4_test.go`.

The connection-reuse script borrows the useful latency property of AnyTLS's
idle-session pool -- keep a path warm so a request does not pay another outer
handshake. Niulang already does this with `--quic-pool`; it should not import
AnyTLS's one-TCP-session-per-flow shape or padding as latency mechanisms. TCP
multiplexing adds head-of-line coupling, and padding adds bytes and writes.
The script tests only the reusable mechanism: cold and warm request latency
with the pool enabled and disabled.

A same-machine, same-seed 10-trial check compared the inherited raw-QUIC
carrier with protocol 2's real HTTP/3 carrier at 100 ms RTT, 5% independent
loss, 50 Mbit/s, and a 1 MiB flow (seed 12703). Both completed 10/10. Raw versus
HTTP/3 median/mean goodput was 10.986/11.282 versus 11.139/11.243 Mbit/s. Warm
request median/maximum was 102.383/102.810 versus 105.533/107.014 ms, and both
delivered 200/200 UDP packets; median per-trial UDP p95 was 104.314 versus
104.502 ms and the largest delivered latency was 106.066 versus 106.593 ms.
Cold request median/maximum did move from 756.590/1042.485 to
839.334/1533.205 ms. This narrow local-emulator result supports real HTTP/3
with the existing warm pool without claiming that the cold path is free or
that WAN performance is proven.

The 2026-08-27 HTTP/3 startup campaign then isolated a half-round-trip which
was not inherent to the carrier. The gateway used a regular QUIC listener, so
it could not start HTTP/3 until mutual TLS had completed and its SETTINGS
arrived another half round trip later. Using an early listener lets only the
static SETTINGS overlap the handshake; each request handler still waits for
the completed mutually authenticated handshake before deriving or authorizing
the device identity. The path congestion controller is also installed only
after authentication, preserving the previous handshake and data-controller
boundary.

Final tests alternated the `a7f4ce138d54df5d9a67b3558b62b0d4aa4b438e`
baseline and candidate for every seeded trial on one otherwise idle Linux
x86_64 machine. At 200 ms RTT, 50 Mbit/s, and 5% independent loss, the pooled
cold p50/p95 fell from 1625.585/2866.361 ms to 1465.493/2103.855 ms
(-9.85%/-26.60%). Pooled warm p50 remained 205.433 versus 205.471 ms, while
warm p95 improved from 414.597 to 407.356 ms. Both completed 30/30. The
candidate cold p50 was 103.54% of the previously recorded direct pre-HTTP/3
result, inside the 105% acceptance bound. Unpooled cold p50/p95 fell from
2166.831/3279.580 ms to 1880.729/3114.432 ms; warm p50/p95 fell from
623.300/1325.423 ms to 415.112/1018.351 ms.

At 100 ms RTT, 50 Mbit/s, and 15% loss in mean bursts of six, both versions
opened all 30 UDP associations and both completed 29/30 bulk objects. Baseline
and candidate bulk medians were 8.707 and 8.749 Mbit/s. UDP delivery was
2673/3000 versus 2665/3000, delivered p95 median was 105.490 versus 105.393 ms,
and the largest delivered latency was 780.592 versus 677.097 ms. Because the
matched baseline had no setup failure, this result does not claim that the
previously observed isolated EOF was fixed.

The 100 ms RTT, 50 Mbit/s, 5% independent-loss guard completed 10/10 on both
versions. Candidate bulk median was 1.86% lower, interactive p95 median was
4.332 ms higher, UDP delivery moved from 99.8% to 99.7%, and UDP p95 moved by
+0.295 ms. Ambient erasures were 1516 versus 1513 and neither version caused a
bottleneck drop. These stay within the predetermined bulk and UDP guards. The
full manifest, per-trial JSON and logs, resource records, checksums, machine
details, and source patch are retained outside the repository at
`.amp/campaigns/http3-startup-20260827-final/`.

`--udp-packets N` adds a bounded SOCKS UDP echo workload to each stack and
trial. Its JSON records application datagrams sent, received, and lost plus
delivered-packet p50/p95/max latency. `--udp-on-stream` is the ordered-stream
control: it can recover every packet, but a lost packet holds up the ones
behind it. The UDP suite measures that delivery-versus-tail tradeoff directly
under independent loss, burst loss, and a quantized policer. It does not call
outer QUIC erasures "application loss".

These are mechanism-inspired experiments, not protocol parity claims. A result
is a candidate for a deployment default only if completion, useful goodput,
bottleneck drops, cold/warm latency, and interactive p95 all remain acceptable,
then survives a matched live campaign.

## Typical invocations

```sh
# Short-lived requests: cold/warm setup, first byte, and completion.
go run ./cmd/niulangbench --rtt 200 --loss 1 --rate 100 \
    --bytes $((64*1024)) --latency --trials 8

# Interactive requests while the same protocol carries a bulk transfer.
go run ./cmd/niulangbench --rtt 200 --loss 1 --rate 100 \
    --bytes $((50*1024*1024)) --interactive --trials 5

# Bulk completion and useful goodput.
go run ./cmd/niulangbench --rtt 200 --loss 1 --rate 100 \
    --bytes $((100*1024*1024)) --trials 5

# Against real implementations rather than only the in-tree control.
go run ./cmd/niulangbench --stacks baseline,niulang,tuic,hysteria2 \
    --sing-box /path/to/sing-box --rtt 200 --loss 1 --rate 100 --trials 5

# Current product against the latest released upstream binary. Niulang's
# credentials are prepared before timing; Queqiao enrolls over its companion
# TCP listener, then its measured data plane is forced to QUIC.
go run ./cmd/niulangbench --stacks niulang,queqiao \
    --queqiao /path/to/queqiaod --rtt 200 --loss 1 --rate 100 --trials 5

# Against a fixed-rate erasure code, which is the comparison niulang's own
# coding most needs. Sweep the parity; see the fixed-parity section.
go run ./cmd/niulangbench --stacks niulang,kcptun \
    --kcptun-client /path/to/kcptun/client --kcptun-server /path/to/kcptun/server \
    --kcptun-mode fast3 --kcptun-parityshard 3 \
    --rtt 200 --loss 20 --rate 50 --bytes $((16*1024*1024)) --trials 5

# The TCP family, including AnyTLS, which cannot be measured under loss.
go run ./cmd/niulangbench --stacks anytls,vless-tcp,vless-ws \
    --sing-box /path/to/sing-box --rtt 200 --loss 0 --rate 100 --trials 4

# The standard matrix, five trials per cell.
./scripts/bench_matrix.sh --trials 5 --output /tmp/matrix.tsv

# An archival bundle: TSV, per-cell JSON, source/toolchain manifest, any dirty
# source patch, and checksums. The directory must not already exist.
./scripts/bench_matrix.sh --trials 5 --json-dir /tmp/niulang-report

# One cell, both stacks, with a machine-readable record and a CI gate.
go run ./cmd/niulangbench --rtt 200 --loss 3 --rate 100 --trials 5 \
    --json /tmp/result.json --gate --tolerance 0.10

# Correlated loss, controller held constant so the transports are compared
# rather than the controllers.
go run ./cmd/niulangbench --rtt 178 --loss 35 --loss-burst 10 --rate 50 \
    --bytes $((4*1024*1024)) --congestion brutal --brutal-rate 12 --trials 12

# A queue-less 25 Mbit/s policer with an 8 ms refill quantum. The JSON report
# separates ambient erasures from packets rejected by the policer.
go run ./cmd/niulangbench --stacks niulang --rtt 226 --rate 25 --loss 20 \
    --policer-refill 8ms --congestion brutal-no-comp --brutal-rate 23.75 \
    --interactive --json /tmp/policer.json

# Preserve erasure/FEC while sharing one 95 Mbit/s QUIC packet-byte cap across
# every connection to the provider. Bulk connections share 85 Mbit/s and the
# remaining 10 Mbit/s stays available to interactive/control traffic.
go run ./cmd/niulangbench --stacks niulang --rtt 226 --rate 100 --loss 1 \
    --policer-refill 8ms --congestion erasure --wire-cap-rate 95 \
    --wire-interactive-reserve 10 --interactive --json /tmp/wire-cap.json

# Residual application UDP loss and delivered-packet latency. The stream
# control makes head-of-line delay visible instead of hiding it as 100%
# delivery.
go run ./cmd/niulangbench --stacks niulang --rtt 226 --rate 50 --loss 15 \
    --udp-packets 80 --udp-interval 20ms --udp-settle 3s \
    --json /tmp/udp.json

# A reverse-path-heavy regime, which is where a transport that layers its own
# acknowledgements over QUIC gets into trouble.
go run ./cmd/niulangbench --rtt 200 --loss 0.5 --loss-up 25 --rate 100 \
    --bytes $((32*1024*1024)) --trials 4
```

## Measuring contention

Running one stack and then the other answers which is faster alone. It cannot
answer which takes more of a link when both want it, and for a transport whose
purpose is to win a contended bottleneck that is the question that matters.

`--contend` attaches two stacks to one shared `pathsim.Bottleneck` -- one
serialization clock, one queue, one loss process -- starts both transfers
together, and reports each one's share:

```sh
go run ./cmd/niulangbench --contend niulang,baseline --rtt 200 --rate 100 \
    --bytes $((20*1024*1024)) --trials 6
```

A share of 0.5 is an even split. This is what found the last argument against
striping before it was deleted: four lanes measured 60 Mbit/s against one
lane's 58 on a shared path run sequentially, which reads as harmless, and took
0.40 of the link against one lane's 0.51 when actually contended -- the same
sequential number, and the opposite conclusion.

## Calibrating the instrument

`TestForwardingCapacity` measures what the emulator itself can forward, which
bounds every transport number taken through it: 1722 Mbit/s at 200 ms, 1784 at
20 ms. Quote no result that is within a small factor of these without checking
it first.

## Archival reports and provenance

`bench_matrix.sh --json-dir DIR` creates a self-checking report bundle instead
of loose JSON files. `manifest.txt` records the commit, Go toolchain, target,
matrix settings, and shell-escaped invocation. Every per-cell JSON report has
schema version 1 and repeats the exact command arguments, seeded path settings,
VCS revision/dirty bit, Go target, and complete module dependency graph. Cold
and warm latency records and contention records are machine-readable too; they
are no longer terminal-only output. `SHA256SUMS` covers the bundle.

For a report intended to support a published claim, start from a clean tree so
the recorded commit is sufficient to reproduce its source. A development run
is still honest: `source-status.txt` records the dirty paths and `source.patch`
captures tracked changes. Untracked files cannot be reconstructed from that
patch, which is why an archival run must be clean.

The emulator is deterministic for a given seed, but elapsed time is not:
scheduler load, CPU frequency, kernel, and Go version still affect performance.
Reproduce comparisons on a comparable host and judge paired stacks from the
same cell, rather than expecting benchmark JSON to be byte-identical.

## Live campaigns

The emulator is the inner loop, not a replacement for the real link.
`scripts/bench_live_matched.sh` alternates trials between two already-running
SOCKS5 endpoints and swaps which goes first each round, keeping a comparison
inside one path window. Expect to need well over ten rounds, and report
completion counts rather than only medians.

Five mistakes produced confident, wrong results during this project's
campaigns. All five are cheap to avoid and expensive to miss:

- **Use the literal server IP.** A hostname that a local TUN-mode proxy
  resolves to a fake IP means both transports are measured *through the
  existing tunnel*, not over the path under test.
- **Bind the outer socket to the physical interface** (`--local-address`) on
  both clients, for the same reason, and give both the same timeout.
- **Prove the binding worked, don't assume it.** Have the server report the
  source address it sees. On 2026-08-16 an unbound datagram arrived from
  `<TUNNEL-EGRESS-IP>`, the existing tunnel's exit, and a bound one from
  `<CLIENT-PUBLIC-IP>`, the real uplink — same host, same destination, one flag
  apart. Third-party clients need their own equivalent (`inet4_bind_address`
  for sing-box outbounds), and they need checking too.
- **Clear `NO_PROXY` before using curl.** `curl` honours `NO_PROXY` even when a
  proxy is named explicitly with `--socks5-hostname`, so a shell exporting
  `NO_PROXY=*` sends every "proxied" transfer straight to the origin and
  reports it as a success. Use `env -u NO_PROXY -u no_proxy curl --noproxy ''`.
- **Make the remote oracle concurrent.** A single-threaded `http.server` lets a
  lingering connection from one trial delay the next; before this was fixed,
  niulang measured 1.19 Mbit/s against the reference's 4.52, and with a threaded
  oracle and nothing else changed the two measured 0.478 and 0.522.

**Measure a fixed duration, not a fixed object, when the stacks are far apart.**
A 16 MiB object is four round trips for a QUIC stack on a fast path and three
minutes for VLESS over TCP on the same path; a 20-second window measures the
rate each one actually sustains and keeps completion honest.

Stop every temporary listener when the campaign ends. An earlier session left
an authenticated listener bound to all interfaces for thirteen hours.

## What the rig cannot tell you

It models one bottleneck queue per direction plus an optional per-source
policer. It does not model middlebox behavior, path MTU changes, or NAT
rebinding. Both endpoints run on one machine, so it cannot expose a defect that
only appears with a real NIC or a real scheduler.

The throughput matrix says nothing about correctness under lane failure, UDP
blocking, or restart; those require separate correctness and failure-mode
tests. Protocol 3 has no alternate carrier: UDP blocking is an endpoint
failure for Niulang, and any path or protocol switch belongs outside it.
