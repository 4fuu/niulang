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
language and QUIC library; comparing against this isolates the design. Live
comparisons against native TUIC remain worthwhile and are not replaced by it.

`cmd/wanoptbench` runs both stacks over one emulated path in a single process.
`scripts/bench_matrix.sh` is the standard matrix.

## What was actually wrong

Five defects were found. Each was located by measurement, not inspection, and
each is individually confirmed by a before/after number.

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

## Emulated-path results

Produced by `./scripts/bench_matrix.sh --trials 5`, five trials per cell,
median Mbit/s, `bbr-tuic` on both stacks. Every trial in every cell delivered
the exact expected body.

| Condition | Reference | wanopt | Delta |
| --- | ---: | ---: | ---: |
| 200 ms, 100 Mbit/s, 0% loss, 10 MiB | 37.79 | 37.46 | −0.9% |
| 200 ms, 100 Mbit/s, 1% loss, 10 MiB | 32.05 | 34.19 | +6.7% |
| 200 ms, 100 Mbit/s, 3% loss, 10 MiB | 29.10 | 29.56 | +1.6% |
| 200 ms, 100 Mbit/s, 5% loss, 10 MiB | 27.78 | 28.53 | +2.7% |
| 200 ms, 100 Mbit/s, 1% loss, 50 MiB | 57.38 | 61.55 | +7.3% |
| 200 ms, 1% loss, 4 concurrent flows | 61.42 | 61.73 | +0.5% |
| 200 ms, 1% loss, 8 concurrent flows | 70.68 | 70.23 | −0.6% |
| 264 ms, 50 Mbit/s, 10% loss | 18.03 | 17.91 | −0.7% |
| 264 ms, 50 Mbit/s, 20% loss | 14.86 | 14.96 | +0.7% |
| No impairment, 256 MiB (datapath cost) | 879.71 | 897.50 | +2.0% |
| Cold connect (ms, lower is better) | 409.9 | 409.6 | parity |
| Warm request (ms, lower is better) | 203.4 | 202.7 | parity |

Single-flow goodput, concurrent-flow goodput, connection latency, and CPU-bound
datapath cost are all at or above the reference. For comparison, before these
changes the same single-flow cells measured 24.3 against 32.1 at 1% loss and
20.5 against 29.7 at 3% loss.

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

## Reproducing

```sh
go test ./...
./scripts/bench_matrix.sh --trials 5

# One cell, both stacks, on a per-flow-policed path:
go run ./cmd/wanoptbench --rtt 200 --loss 1 --rate 400 --per-flow-rate 25 \
    --bytes 104857600 --lanes 4 --initial-lanes 4 --trials 3
```

## Limits of this evidence

The emulator models independent per-packet loss. Real long-haul loss is bursty
and correlated, and the live campaign below found a failure mode the emulator
did not reproduce. It also models one bottleneck queue per direction plus an
optional per-source policer; it does not model reordering, variable delay, or
middlebox behavior. It runs both endpoints on one machine, so it cannot expose
a defect that only appears with a real NIC, a real scheduler, or a real MTU.

None of these results say anything about correctness under lane failure, UDP
blocking, or restart. Those gates remain as stated in
[`PRODUCTION-DESIGN.md`](PRODUCTION-DESIGN.md).
