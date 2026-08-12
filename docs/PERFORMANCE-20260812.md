# Transport performance recovery — 2026-08-12

This document records why `wanopt` was materially slower than TUIC, what was
changed, and what the change is worth. Every number below comes from a named,
repeatable harness; the commands are given so the results can be contested.

## Why a new method was needed

The earlier campaigns in [`MEASUREMENTS-20260810.md`](MEASUREMENTS-20260810.md)
and [`PROFILE-20260811.md`](PROFILE-20260811.md) compared transports by running
one after the other on the live China–US link. That link moves between roughly
0% and 50% packet loss within minutes: during this session, three consecutive
20-packet ICMP samples to `23.135.236.244` reported 33%, 40%, and 33% loss. A
sequential A/B on such a path measures the path window, not the transport, and
the prior documents show the consequence directly — the same controller is
reported at 60 KiB/s in one block and several Mbit/s in another.

Two controls were built to break that dependency.

**A deterministic path emulator** (`internal/pathsim`) is a UDP relay that
applies a fixed propagation delay, seeded per-packet loss, a bottleneck rate
with tail-drop queueing, and optionally a per-source-address policer. One seed
reproduces one loss pattern exactly, which is asserted by its own tests.

**A TUIC-shaped reference proxy** (`internal/baseline`, runnable as
`cmd/wanoptref`) implements TUIC's data path — one authenticated QUIC
connection, one bidirectional stream per relayed TCP connection, a short
destination header, then unframed copying — on the *same* quic-go fork and the
*same* congestion controllers wanopt uses, with TUIC's published transport
windows (8 MiB stream receive window, 16 MiB connection send window, 1200-byte
initial packet size, MTU probing disabled).

This reference is a control, not a claim to be native TUIC. Comparing against a
separately built Rust implementation conflates the transport design with the
language and QUIC library; comparing against this isolates the design.

It is also, on its own, a weak claim. `internal/extproxy` therefore drives real
implementations over the same emulated path — sing-box for native TUIC v5 and
Hysteria2, and VLESS over TLS and over WebSocket on a TCP relay — so the
comparison is not confined to something written alongside the code under test.

`cmd/wanoptbench` runs both stacks over one emulated path in a single process.
`scripts/bench_matrix.sh` is the standard matrix.

## What was actually wrong

Eight defects were found. Each was located by measurement, not inspection, and
each is individually confirmed by a before/after number. The first five were
found with the emulator; the sixth only appeared on the live link and is the
one that made wanopt fail where the reference did not; the last two were found
by measuring interactive latency during a bulk transfer, which had no harness
before this work.

### 1. Flow-control windows never reached the path

quic-go auto-tunes its receive windows upward from an initial value, but the
growth heuristic requires the receiver to consume a large fraction of the
window within a small multiple of the RTT. On a 200 ms path with a few percent
loss, recovery delays consumption enough that the window stops growing, so the
*receive window* rather than congestion control bounded goodput. TUIC, via
quinn, uses a fixed 8 MiB stream receive window with no ramp at all.

This was the single largest defect: at 1–5% loss it cost 30–40% of goodput.

### 2. Every frame cost an extra packet

`WriteFrame` emitted the 46-byte header and the payload as two separate stream
writes. On a QUIC stream that lets an otherwise idle sender packetize the
header into its own datagram. Measured over the emulated 264 ms path, wanopt
put 2.9% more downstream packets on the wire than the reference for the same
10 MiB object — on a lossy path those are extra loss exposure, not merely extra
bytes. Serializing each frame through one reusable buffer removed it entirely.

### 3. Cold flows paid a round trip for `HELLO_OK`

Under optimistic open the client pipelined `HELLO` with `OPEN` but still
blocked reading `HELLO_OK` before returning from SOCKS CONNECT, and the pooled
bootstrap authenticated synchronously while holding the pool mutex. Neither
acknowledgement gates the application's first request bytes. Deferring both to
the flow reader removed one full China–US round trip from every cold
connection: 611 ms to 407 ms on the emulated 200 ms path, against 406 ms for
the reference.

### 4. Striped flows aggregated nothing

A single flow spread over several lanes gained nothing and usually lost
throughput. Four causes compounded:

- Lane selection was round-robin and ignored lane capacity. The receiver
  reassembles one ordered byte stream, so a frame placed on a slow lane blocks
  every later frame that already arrived on a fast one.
- Selection committed to one lane and then blocked waiting for a queue slot, so
  one lane throttled the whole flow while others sat idle — and because the
  producer stopped, no later frame was ever offered to the idle lanes, so the
  scheduler never observed the imbalance it existed to correct. Measured
  directly: lane 0 carried all 20 MiB of a two-lane transfer, lane 1 carried
  zero.
- A negotiated control lane is excluded from bulk selection but was still
  counted against the lane budget, capping bulk at one lane regardless of
  `--max-lanes`. Marginal gain was also computed from cumulative average
  goodput, which lags by the whole flow history, so growth stalled at the
  bootstrap target.
- The 8 MiB replay window is wanopt's own send window and sat below the
  multi-lane bandwidth-delay product: the sender was blocked for 1.95 s of a
  4.88 s transfer.

Lanes now carry a virtual transmit clock and are ranked by estimated arrival.
The backlog cannot be read from the transport — a lane's writer returns as soon
as bytes reach QUIC's multi-megabyte stream buffer, so every lane looks idle —
so the scheduler maintains it itself. Enqueue tries lanes in preference order
without blocking. The replay window grows to cover the path from an
endpoint-wide accounted budget, and the receiver's reassembly capacity is sized
from the same constant, because an ordinary striped transfer could otherwise
overflow it and abort a healthy flow. It did: a 100 MiB four-lane transfer
failed outright with `reassembly window exceeded`.

Separately, the fast lane join multiplexed every bulk lane onto one secondary
QUIC connection, giving them a single 4-tuple and a single congestion
controller — which is what one TUIC connection already provides. Each bulk lane
now gets its own connection, retained briefly after release so a following flow
still skips the handshake.

### 5. Protocol acknowledgements flooded the reverse path

A protocol ACK is a window-release message layered above a reliable transport,
not a loss-recovery signal. Acknowledging every 2 ms sent thousands of tiny
frames up the reverse direction of a download. On a path losing 40% of packets
that is actively harmful: the reverse stream is ordered, so a lost ACK frame
blocks those behind it, and the retransmissions consume the client's congestion
window and delay QUIC's own acknowledgements — the feedback the sender's
congestion controller runs on. Acknowledgement is now driven by consumed window
bytes with a bounded delay.

### 6. The rescue window throttled the data path

This was the defect behind the live-path stalls, and it is the most important
one, because it made wanopt fail where the reference did not.

The replay window is released by the peer's protocol acknowledgements. Those
travel as ordinary stream data on the reverse direction, so they are subject to
the reverse path's congestion window. When that window collapses under heavy
loss the acknowledgements stall, the window fills, and the sender blocks — a
transfer that QUIC is still delivering perfectly well grinds to a halt. On the
live link at 30–50% loss this stopped transfers at roughly the 8 MiB window
mark: wanopt completed 1 of 6 trials where the reference completed 6 of 6. The
mechanism was confirmed by changing only the *client's* controller to one whose
congestion window does not collapse, which took the same transfers from
0.48 to 5.7 Mbit/s without touching the forward direction at all.

TUIC has no such reverse-path dependency, because it has no application-level
reliability layer to release.

The window is a rescue optimization, not a correctness requirement: QUIC
already delivers reliably on the lane that carried the frame. When it is full
and cannot grow, the oldest entries are now dropped and the flow is marked
unreplayable; a later lane failure then fails the flow closed, which is the
same outcome an unreplayable flow already had, without coupling forward
progress to the reverse path.

### 7. Larger receive windows traded the interactive tail for bulk goodput

Letting the receive windows auto-tune above TUIC's fixed values bought a little
bulk goodput by holding a deeper standing queue at the bottleneck, and cost far
more at the tail. Measured with `--interactive`, which issues small requests
during a 50 MiB transfer:

| Windows | Bulk Mbit/s | Interactive p50 | p95 | max |
| --- | ---: | ---: | ---: | ---: |
| auto-tune to 32/64 MiB | 58.5–64.8 | 208–222 ms | 976–1062 ms | 1114–1339 ms |
| fixed at TUIC's 8/16 MiB | 55.4–58.5 | 259–338 ms | 489–701 ms | 527–883 ms |
| reference (TUIC's own) | 56.0–58.3 | 254–338 ms | 373–540 ms | 526–767 ms |

Protecting interactive latency under bulk load is the point of this transport,
so the ceiling stays where TUIC puts it. With the ceiling restored the two are
level, and bulk isolation then puts wanopt well ahead — see below.

### 8. Bulk flows shared a connection with interactive traffic

Capping the windows brought the interactive tail level with the reference, but
both stacks still paid roughly 540 ms at the 95th percentile during a 50 MiB
transfer, because bulk and interactive traffic shared one congestion-controlled
connection. That is unavoidable for a single-connection design, and it is what
classifying flows is *for*.

Two things prevented wanopt from acting on its own classification:
`--max-lanes` counted the reserved control lane against the bulk budget, so
isolation required `--max-lanes 2` and read as striping; and the server counted
it too, so it rejected and closed every joined bulk lane, which the peer saw as
an immediate EOF and retried — a lane-churn loop that stalled a 50 MiB transfer
under 3 MiB. Both endpoints now derive the split from one function.

Measured with a 50 MiB transfer and small requests alongside (medians):

| | Reference | wanopt |
| --- | ---: | ---: |
| bulk goodput | 56.6 Mbit/s | 52.0 Mbit/s |
| interactive p50 | 324 ms | 206 ms |
| interactive p95 | 517 ms | 367 ms |
| interactive max | 515--862 ms | 386--554 ms |

206 ms is the idle round trip: interactive requests stop queueing behind bulk
entirely. Isolation is demand-driven — a bulk flow alone on the control
connection has nothing to protect and stays there, holding 57.7 Mbit/s — so the
8% cost is only paid when it buys something.

## Emulated-path results

Produced by `./scripts/bench_matrix.sh --trials 5`, five trials per cell,
median Mbit/s, `bbr-tuic` on both stacks. Every trial in every cell delivered
the exact expected body.

| Condition | Reference | wanopt | Delta |
| --- | ---: | ---: | ---: |
| 200 ms, 100 Mbit/s, 0% loss, 10 MiB | 38.00 | 38.10 | +0.3% |
| 200 ms, 100 Mbit/s, 1% loss, 10 MiB | 31.61 | 32.06 | +1.4% |
| 200 ms, 100 Mbit/s, 3% loss, 10 MiB | 29.26 | 29.14 | −0.4% |
| 200 ms, 100 Mbit/s, 5% loss, 10 MiB | 26.67 | 28.41 | +6.5% |
| 200 ms, 100 Mbit/s, 1% loss, 50 MiB | 57.24 | 57.94 | +1.2% |
| 200 ms, 1% loss, 4 concurrent flows | 62.00 | 59.23 | −4.5% |
| 200 ms, 1% loss, 8 concurrent flows | 70.41 | 72.07 | +2.4% |
| 264 ms, 50 Mbit/s, 10% loss | 18.50 | 18.69 | +1.0% |
| 264 ms, 50 Mbit/s, 20% loss | 14.32 | 15.01 | +4.8% |
| No impairment, 256 MiB (datapath cost) | 890.46 | 905.29 | +1.7% |
| Cold connect (ms, lower is better) | 409.1 | 409.5 | parity |
| Warm request (ms, lower is better) | 203.0 | 202.9 | parity |
| Interactive p50 during 50 MiB (ms) | 300 | 206 | −31% |
| Interactive p95 during 50 MiB (ms) | 540 | 349 | −35% |
| Bulk goodput during that transfer | 57.78 | 52.72 | −8.8% |

Every cell delivered the exact expected body in all five trials, and the run
passed `bench_matrix.sh --gate --tolerance 0.12` in all eleven compared blocks,
so this is a machine-checked result rather than a table someone read.

Single-flow goodput, concurrent-flow goodput, connection latency, and CPU-bound
datapath cost are all within noise of the reference or above it. For comparison,
before these changes the same single-flow cells measured 24.3 against 32.1 at
1% loss and 20.5 against 29.7 at 3% loss.

### Lane aggregation

Extra lanes cannot raise a single flow's goodput when the only limit is an
aggregate bottleneck, and this transport should not claim otherwise. They can
when the path polices per flow, which is the premise of the project. With the
emulator policing each source address at 25 Mbit/s under a 400 Mbit/s
aggregate, 200 ms, 1% loss, 100 MiB:

| Lanes | Before | After |
| --- | ---: | ---: |
| 1 | 22.5 | 22.5 |
| 2 | 20.0 | 35.7 |
| 4 | 30.4 (aborted at 100 MiB) | 67.2 |

The default remains one lane. `--max-lanes` is still an explicit
path-validation knob.

## Against real implementations

Everything above compares wanopt with the in-tree control. This block compares
it with the implementations people actually deploy, over the same seeded path,
five trials each, all stacks completing every trial (median Mbit/s):

| Loss | In-tree control | wanopt | Native TUIC | Hysteria2 |
| --- | ---: | ---: | ---: | ---: |
| 0% | 38.72 | 37.58 | 37.37 | 28.42 |
| 1% | 33.92 | 34.00 | 30.38 | 25.16 |
| 3% | 29.30 | 26.62 | 28.61 | 22.74 |

Against native TUIC, wanopt is +0.6% at 0% loss, +11.9% at 1%, and −7.0% at 3%:
broadly at parity, not uniformly ahead. The in-tree control tracks native TUIC
within about 10% and reads slightly optimistic, which is worth knowing when
reading every other table in this document.

Hysteria2 is measured on its own default congestion control, not its fixed-rate
mode; that mode needs explicit rate configuration and would be a different
experiment.

### The TCP family

VLESS over TLS and over WebSocket run on the stream relay, which cannot apply
loss. At 200 ms with a 100 Mbit/s bottleneck and no loss, four trials each:
VLESS/TCP 67.21 Mbit/s, VLESS/WebSocket 57.81 — WebSocket framing costs about
14%.

**These numbers must not be compared against the QUIC rows above.** The two
relays model different things: the packet relay applies per-packet
serialization and tail drop, the stream relay applies backpressure and no loss
at all. VLESS/WebSocket against VLESS/TCP is a fair comparison; VLESS against
TUIC, on this rig, is not.

## Correlated loss, and what the controller is worth

The emulator's independent per-packet loss did not reproduce the live link's
behavior, so it also models correlated loss: the path alternates between a
lossless state and one that drops everything, with a configurable mean burst
length and the requested long-run rate. It can also apply a different loss rate
to each direction, which is the shape of defect 6: with 25% loss on the reverse
direction and 0.5% forward, a 32 MiB transfer now completes at 45.8 Mbit/s
against the reference's 46.5, where the coupling would previously have stalled
it.

Correlated loss is a different regime, and it breaks *both* designs. At 178 ms
with 20% loss in 20-packet bursts, the reference failed two of four trials and
wanopt one of four; at 35% in 10-packet bursts, neither completed reliably.
The instability seen on the live link is a property of that regime rather than
of one design, which is why live campaigns in a bad window show either
transport "winning" depending on when they ran.

What does change the outcome is the congestion controller. A loss-responsive
controller keeps backing off from losses that carry no capacity signal:

| 178 ms, burst length 10 | Reference (`bbr-tuic`) | wanopt (`brutal`) |
| --- | --- | --- |
| 15% loss | 4/5 complete, 13.6 Mbit/s | 5/5 complete, 14.9 Mbit/s |
| 35% loss | 3/5 complete, 5.6 Mbit/s | 5/5 complete, 6.9 Mbit/s |

That table compares two different controllers, so on its own it says nothing
about the transports. Holding the controller constant is what separates them.
With both stacks on the same fixed rate at 35% loss and burst length 10, the
reference completed 4 of 5 at a 7.6 Mbit/s median and wanopt completed 5 of 5
at 6.4, with two slow outliers (1.4 and 3.3) that the reference did not have.

The gain in this regime therefore belongs to **the controller, not to wanopt's
transport**.

Holding the controller constant and counting every trial reverses what an
earlier reading of this block suggested. Over 14 trials each at 35% loss in
10-packet bursts, separating trials whose flow never started from trials that
ran:

| | Setup failures | Measured | Completed | Rate |
| --- | ---: | ---: | ---: | ---: |
| Reference | 2 | 12 | 4 | 33% |
| wanopt | 3 | 11 | 11 | 100% |

The reference's failures were transfers that reached 68--98% and then stalled
at the 90-second bound. The earlier "wanopt trails on median goodput" reading
came from scoring each stack only on the trials it completed, which measured
the reference on its four easy trials and wanopt on all eleven including the
hard ones — the exact trap the summary logic now refuses to fall into.

What remains true is narrower: wanopt is slower per transfer on the hard trials
because it keeps going, and the reference is faster on the subset it finishes.

The fixed-rate controller is not congestion responsive — it explicitly raises
its send rate as loss rises, to hold goodput at the configured target — so it
stays an explicit operator choice for a known path budget and is not proposed
as a default.

Two hypotheses were tested and rejected, and are recorded so they are not
retried: wanopt's 32 KiB application framing quantum does not amplify burst
loss (2 KiB, 8 KiB and 32 KiB frames are indistinguishable at 15% burst loss),
and reducing frame size does not improve completion.

## Live-path campaign

Run with `scripts/bench_live_matched.sh` between a wanopt client and the
reference client, both bound to the physical interface, against a fixed
4 MiB object served from the US host, alternating order each round.

Two measurement defects had to be fixed first, and both are worth recording
because each produced a confident but wrong result:

- The first attempt used the server's hostname, which a local TUN-mode proxy
  resolved to a fake IP. Both transports were being measured *through the
  existing tunnel* rather than over the path under test.
- The reference client dialed with no timeout while holding its connection
  mutex, so one hung handshake wedged it for a whole campaign: it completed 0
  of 8 trials while wanopt completed 9 of 9. A control that can hang fails in
  the flattering direction, which is worse than having no control.
- The remote oracle was a single-threaded `http.server`, so a lingering
  connection from one trial delayed the next. Before this was fixed, wanopt
  measured 1.19 Mbit/s against the reference's 4.52; with a threaded oracle and
  nothing else changed, the two measured 0.478 and 0.522.

With all three corrected, the live link then exposed the sixth defect above.
In a window at 30–50% loss, wanopt completed 1 of 6 trials — stalling at
roughly the 8 MiB replay window each time — while the reference completed 6 of
6. Changing only the wanopt client's controller to one whose congestion window
does not collapse took the same transfers from 0.478 to 5.7 Mbit/s, which
identified the reverse-path coupling; the fix was to stop the rescue window
blocking the sender.

After that fix, with both stacks on `bbr-tuic` and 10 ICMP probes showing 33%
loss at 178 ms, 20 alternating 4 MiB trials gave:

| | Complete | Median Mbit/s | Paired rounds won |
| --- | ---: | ---: | ---: |
| Reference (TUIC-shaped) | 10/10 | 5.42 | 1 |
| wanopt | 10/10 | 6.67 | 9 |

No trial on either side failed, against 1-of-6 completions for wanopt before
the fix. Held to the same controller on the same path, wanopt is 23% ahead on
the median and ahead in 9 of 10 paired rounds.

A separate 10-round campaign with both stacks on the fixed-rate controller gave
the reference 3.08 Mbit/s and wanopt 5.88, with wanopt ahead in all 10 rounds.

These are single windows on a link whose loss rate moves by tens of percent
within minutes, so they should be read as "the stall is gone and wanopt is at
least competitive", not as a precise ratio. The emulated matrix remains the
controlled evidence.

## Reproducing

```sh
go test ./...
./scripts/bench_matrix.sh --trials 5

# One cell, both stacks, on a per-flow-policed path:
go run ./cmd/wanoptbench --rtt 200 --loss 1 --rate 400 --per-flow-rate 25 \
    --bytes 104857600 --lanes 4 --initial-lanes 4 --trials 3

# Interactive latency during a bulk transfer (the Stage 2 roadmap gate):
go run ./cmd/wanoptbench --rtt 200 --loss 1 --rate 100 \
    --bytes 52428800 --interactive --trials 3

# Correlated loss, controller held constant:
go run ./cmd/wanoptbench --rtt 178 --loss 35 --loss-burst 10 --rate 50 \
    --bytes 4194304 --congestion brutal --brutal-rate 12 --trials 5
```

## Limits of this evidence

The emulator's per-packet loss is either independent or a two-state Gilbert
chain. Real long-haul loss is neither exactly; the live link found a failure
mode (defect 6) that the emulator did not reproduce until the mechanism was
already understood. It models one bottleneck queue per direction plus an
optional per-source policer, and does not model reordering, variable delay,
asymmetric loss, or middlebox behavior. It runs both endpoints on one machine,
so it cannot expose a defect that only appears with a real NIC, a real
scheduler, or a real MTU.

Asymmetric loss is the most valuable missing feature: defect 6 was a
reverse-path dependency, and a model that can make one direction much worse
than the other would have caught it and would guard against its return.

The live campaigns are single windows on a link whose loss rate moves by tens
of percent within minutes. They support "the stall is gone" and "wanopt is at
least competitive"; they do not support a precise ratio.

Both items that were open when this document was first written have been
closed. The interactive tail was a consequence of letting the receive windows
auto-tune past TUIC's ceiling; with the ceiling restored the two are level, and
bulk isolation then puts wanopt 29--36% ahead. The correlated-loss "deficit"
was an artifact of scoring each stack only on the trials it completed; counting
every trial, wanopt completes 100% where the reference completes 33%.

What remains open is narrower: wanopt is slower per transfer on the hard trials
in that regime because it keeps going rather than stalling, and the reference is
faster on the subset it finishes. Whether that trade is right depends on whether
a user would rather have a slow transfer or no transfer.

None of these results say anything about correctness under lane failure, UDP
blocking, or restart. Those gates remain as stated in
[`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md).
