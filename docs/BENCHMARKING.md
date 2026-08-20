# Measuring this transport

> [!NOTE]
> **Status:** Current benchmark methodology for public protocol 1
> **Last reviewed:** 2026-08-20

This is the reproducibility guide for the measurement rig. It exists because the motivating
link moved between roughly 0% and 50% packet loss within minutes, so
running one transport's trials and then another's compares two path windows
rather than two transports. Every performance claim in this repository should
be reproducible with the commands here.

Read the [project status](STATUS.md) before interpreting a result. The harness
can establish correctness, behavior under controlled impairment, and a fair
same-window comparison; it cannot turn one path into a universal performance
claim.

## One protocol, three workload views

Short-lived, interactive, and bulk are evaluation families for the same
Queqiao architecture. The benchmark changes the offered workload; it does not
select three transports.

| Family | Primary outputs | Minimum comparison |
| --- | --- | --- |
| Short-lived | fresh/warm connect, first byte, completion, failure tail | matched small requests with `--latency` |
| Interactive | latency/jitter/delivery while bulk occupies the bottleneck | `--interactive` with an idle-path control |
| Bulk | useful goodput, completion count, physical/logical bytes, CPU and memory | matched downloads and uploads long enough to leave startup |

For a transport or controller change, report applicable results from every
family. A bulk improvement is incomplete evidence if it moves interactive tail
latency or small-flow setup in the wrong direction.

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

**`internal/baseline`** (runnable as `cmd/queqiaoref`) — a TUIC-shaped reference
proxy: one authenticated QUIC connection, one bidirectional stream per relayed
TCP connection, a short destination header, then unframed copying. It runs on
the same quic-go fork and the same congestion controllers queqiao uses, with
TUIC's published transport windows.

It is a control, not a claim to be native TUIC. Comparing against a separately
built Rust implementation conflates the transport design with the language and
QUIC library; comparing against this isolates the design.

**`internal/extproxy`** — launches third-party implementations so the
comparison is not limited to an in-tree control. With `--sing-box PATH` the
benchmark gains four more stacks:

| Stack | Implementation | Carried by |
| --- | --- | --- |
| `tuic` | sing-box, native TUIC v5 | UDP relay |
| `hysteria2` | sing-box | UDP relay |
| `vless-tcp` | sing-box, VLESS over TLS | TCP relay |
| `vless-ws` | sing-box, VLESS over WebSocket | TCP relay |

Each runs as a server the emulator forwards to and a client exposing SOCKS5,
over exactly the same seeded path as queqiao. The client trusts exactly the
server's certificate, so nothing disables verification.

**`cmd/queqiaobench`** — runs the selected stacks over one emulated path in a
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
WebSocket's framing cost — and `tuic` against `queqiao` is a fair comparison.
`vless-tcp` against `tuic` is not. Emulating loss for a stream transport needs
an IP-layer facility such as dummynet, which needs privilege this harness does
not take.

## Reading the numbers

The summary reports **medians over all trials, counting failures at their
partial rate**, alongside the completion rate.

This matters. A median over completed trials alone rewards a transport for
giving up: in a 35%-burst-loss block the reference completed 7 of 12 trials and
queqiao 10 of 12, and scoring only the successes made the transport that
finished the hard trials look slower than the one that abandoned them.
`median_mbits_completed_only` is retained for continuity with older campaign
notes and must never be compared across stacks with different completion rates.

`--interactive` additionally issues small requests during the bulk transfer and
splits their latency into connect and first-byte. Those are different defects
with different fixes: a slow connect is flow setup, a slow first byte is
queueing behind the bulk transfer.

## Typical invocations

```sh
# Short-lived requests: cold/warm setup, first byte, and completion.
go run ./cmd/queqiaobench --rtt 200 --loss 1 --rate 100 \
    --bytes $((64*1024)) --latency --trials 8

# Interactive requests while the same protocol carries a bulk transfer.
go run ./cmd/queqiaobench --rtt 200 --loss 1 --rate 100 \
    --bytes $((50*1024*1024)) --interactive --trials 5

# Bulk completion and useful goodput.
go run ./cmd/queqiaobench --rtt 200 --loss 1 --rate 100 \
    --bytes $((100*1024*1024)) --trials 5

# Against real implementations rather than only the in-tree control.
go run ./cmd/queqiaobench --stacks baseline,queqiao,tuic,hysteria2 \
    --sing-box /path/to/sing-box --rtt 200 --loss 1 --rate 100 --trials 5

# The TCP family, which cannot be measured under loss.
go run ./cmd/queqiaobench --stacks vless-tcp,vless-ws \
    --sing-box /path/to/sing-box --rtt 200 --loss 0 --rate 100 --trials 4

# The standard matrix, five trials per cell.
./scripts/bench_matrix.sh --trials 5 --output /tmp/matrix.tsv

# An archival bundle: TSV, per-cell JSON, source/toolchain manifest, any dirty
# source patch, and checksums. The directory must not already exist.
./scripts/bench_matrix.sh --trials 5 --json-dir /tmp/queqiao-report

# One cell, both stacks, with a machine-readable record and a CI gate.
go run ./cmd/queqiaobench --rtt 200 --loss 3 --rate 100 --trials 5 \
    --json /tmp/result.json --gate --tolerance 0.10

# Correlated loss, controller held constant so the transports are compared
# rather than the controllers.
go run ./cmd/queqiaobench --rtt 178 --loss 35 --loss-burst 10 --rate 50 \
    --bytes $((4*1024*1024)) --congestion brutal --brutal-rate 12 --trials 12

# A reverse-path-heavy regime, which is where a transport that layers its own
# acknowledgements over QUIC gets into trouble.
go run ./cmd/queqiaobench --rtt 200 --loss 0.5 --loss-up 25 --rate 100 \
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
go run ./cmd/queqiaobench --contend queqiao,baseline --rtt 200 --rate 100 \
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
  queqiao measured 1.19 Mbit/s against the reference's 4.52, and with a threaded
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

It says nothing about correctness under lane failure, UDP blocking, or restart.
Those gates are in [`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md).
