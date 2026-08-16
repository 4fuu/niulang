# Design

This is what queqiao is. `DESIGN-MULTIPATH.md` and `DESIGN-ERASURE.md` are
dated records of how it got here and what each step measured; where they
disagree with this document, this one is current.

## What this is

**A single-connection transport for an erasure channel.**

One flow is carried by one connection. The channel between a client in China
and a fixed US egress erases roughly 42-45% of packets independently of the
rate anything sends at, and polices above a knee. Every mechanism here follows
from that one measured fact.

## The path, measured

From `PATH-CHARACTER-20260813.md`, by open-loop UDP probe rather than by
inference from the transport's own behaviour:

- **Downstream erases about 45% at any offered rate**, and the losses are
  independent: `P(loss | previous arrived)` equals the overall loss rate to
  within sampling noise. ICMP loses 37% at five packets a second, so it is not
  a queue. Nothing a sender does changes it.
- **Above a knee at about 14.5 Mbit/s delivered** there is a token-bucket
  policer at roughly 24 Mbit/s offered, refilled in quanta, whose losses
  cluster into runs. This is the only genuine congestion signal on the path.
- **Upstream is a different path**: it erases nothing measurable and polices at
  about 14.5 Mbit/s. A transport that assumed symmetry would be wrong in both
  directions at once.
- **Capacity is `(1 - p)` times the line rate.** No scheme beats 0.58 of what
  is offered, so about 14.5 Mbit/s delivered is the budget every design here
  works inside.

## Three axioms

**1. Loss is not congestion.** On a memoryless erasure channel there is nothing
to back off from, and backing off does not reduce the loss. Carrying QUIC
datagrams across the emulated path: quic-go's default (Reno/Cubic) delivers
0.09-0.13 Mbit/s, BBR 0.39-0.95, this repo's BBR-TUIC port 1.10-5.56, Brutal
told 25 Mbit/s reaches 14.03, and the erasure controller 10.31-11.50 against a
14.5 ceiling. Every loss-responsive controller gives the path away.

**2. There is no fairness obligation.** This transport is not required to be
TCP-friendly, multipath-friendly, or to take no more of a shared bottleneck
than one connection would. That is a deliberate discard, and it is what permits
a controller that does not treat erasure as a congestion signal. The one thing
it does not permit is starving the operator's own traffic, which is what the
aggregate token bucket and its interactive reserve are for.

**3. One flow, one connection.** More connections buy no capacity on this path,
and cost the mechanism that works. See below.

## Why not multipath

The project began as a multipath design, on a real observation: more TCP
connections delivered more throughput. One connection measured 0.03 Mbit/s, two
0.10, four 0.22, eight 0.52 -- linear in the connection count.

**The observation was real and the diagnosis was wrong.** That linearity is
TCP's Mathis limit, `MSS / (RTT * sqrt(p))`, which at `p = 0.42` and a 300 ms
round trip gives about 90 kbit/s per connection. The gain came from being
loss-limited, not from an ISP policing per 4-tuple. Multipath was a workaround
for a transport that mistook erasure for congestion, and it scaled because the
workaround was applied n times.

Fix the loss response and the workaround has nothing left to do. One connection
with the erasure controller delivers 10-11 Mbit/s live, where eight
loss-limited TCP connections together reached 0.52.

The open-loop probe settles it below the transport entirely: 1, 2, 4 and 8
connections at 30 Mbit/s each all deliver 14.3 to 14.7 Mbit/s in total, and the
same total offered rate split 1, 2 or 4 ways delivers the same total. **There is
no per-4-tuple policer to exploit.** Connection count provably cannot buy
capacity here.

Nor is there loss diversity to harvest, which is the other usual argument for
lanes. The erasure process is memoryless and shared: two connections see the
same process, and there is nothing to average over.

**And lanes are worse than merely useless.** They multiply the offered rate into
one policer and push the aggregate past the knee, where loss stops being
memoryless and starts clustering. Burst factor -- mean loss-burst length over
what independence alone would give -- is exactly the factor by which
correlation shortens the effective length of a coded block. So lanes degrade
the erasure code, which is the mechanism that does work. Measured live: four
lanes deliver about 8.0 Mbit/s against a single lane's 10-11.

## What follows

**Code what wants latency, retransmit what wants bandwidth.** The two spend the
same thing in different currencies. On a memoryless erasure channel
retransmission resends only what was lost, `1/(1-p)`, where a code must
provision for the binomial. So a bulk download runs at 10.1 Mbit/s on a
retransmitting stream and 5.0 coded, and a small exchange goes the other way --
1.9 s uncoded against 618 ms coded -- because there the currency is a round
trip and not a byte. Nothing is configured: the code reports whether it is
coding from the measured floor.

**Control and data ride different substrates on one connection.** A lane is a
QUIC stream for control and that connection's datagrams for bulk, and the
framing routes by frame type. A stream delivers in order, so at 42% erasure
every gap stalls everything behind it. But acknowledgements must not be coded,
because they release the data whose blocks they would then queue behind: with
everything on one coded substrate the same channel carried 0.87 Mbit/s one way
and 0.008 with acknowledgements coming back.

**The code is a sliding window, not a block.** A block code must choose `(k,n)`
when it seals, which means knowing the path before it has finished sending into
it. A window sizes itself from what the channel is measured to be doing now.

**Sequence by byte offset, not by arrival.** Every chunk carries the offset it
occupies in the application stream, and the receiver reassembles by offset and
delivers contiguously. This is what stops one substrate's gap from gating the
other's progress, and it is a single-connection property: measured on one
connection with 20 ms of jitter, 8.4 Mbit/s against a relaying reference's 2.9,
because a chunk that arrives out of order is placed rather than waited for.

**The path is measured once.** `internal/pathmodel` holds what an endpoint pair
has been measured to do, and everything that adapts to the path reads it rather
than estimating separately. An estimate that starts at zero says the path is
clean, and everything sized by it is sized for a clean path.

## Several connections, for reasons that are not aggregation

Deleting aggregation does not mean one connection per client. Two other uses
survive because neither is about capacity:

- **Isolation.** A flow classified bulk moves to its own connection so it does
  not head-of-line block short flows sharing the pooled one. This is a latency
  argument and the capacity finding leaves it untouched: measured at 200 ms and
  1% loss under a 50 MiB transfer, interactive requests see a 208 ms median
  against 323 ms, where 208 ms is the idle round trip.
- **Failover.** A session moves to a fresh connection when its current one
  dies, retaining what is unacknowledged and re-issuing it. This is what
  survives a UDP blackhole falling back to TCP, and what let a 100 MB download
  complete intact across a server restart.

Short flows still share a pooled connection, which is what makes a flow after
the first cost no round trips: 1 ms against 306 ms.

## Non-goals

- Aggregating capacity across connections. The path has one bottleneck per
  endpoint pair and it has been probed.
- Fairness to other flows, TCP-friendliness, or coupled congestion control.
- Decrypting or classifying HTTPS. Classification is behavioural -- byte
  counts, direction, idle gaps -- and is a policy hint, not a security
  boundary.
- Circumventing the path's aggregate capacity limit.

## Where it stands

Measured live on 2026-08-16 against a UDP sweep of the same path that night:

| | the path gives | the transport gets | |
|---|---|---|---|
| download, one flow | 13.3 Mbit/s | 9.5-10 | 73% |
| download, eight flows | 13.3 | 12.1 aggregate | 91% |
| upload, one flow | 14.5 | 11.3-11.7 | 81% |
| short flow, round trip | 210 ms | 210-260 ms | one round trip |

Against a TUIC-shaped reference over fourteen alternating live rounds: 10.24
Mbit/s against 10.59, ahead in 6 of 14. **Parity, not an advantage** -- and that
is the number to believe over any emulated one. The project has not met the
release gates in `PRODUCTION-DESIGN.md`.
