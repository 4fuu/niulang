# Transport for an erasure channel (2026-08-15)

**Status: a dated findings log, not the design of record.** See
[`DESIGN.md`](DESIGN.md) for the current thesis, which this campaign is what
produced. Sections here are in the order they were found rather than the order
they should be read.

The path this project targets is not congested. It erases. About 42% of packets
are dropped independently of the sending rate — at 1 Mbit/s as readily as at 12,
and ICMP loses 37% at five packets a second. The measurement and its method are
in `docs/PATH-CHARACTER-20260813.md`; this document is what follows from it.

Three layers changed, and only one of them is the erasure code. The largest
single gain is the congestion controller, and the code's contribution is
latency rather than throughput. Both of those were surprises, and both are
measured below.

## What the path does

Two regimes, separated by a knee at about 14.5 Mbit/s delivered:

- **Below it**, a memoryless erasure channel of about 42%. `P(loss | previous
  arrived)` equals the overall loss rate to within sampling noise, so the
  losses are independent. Nothing a sender does changes it.
- **Above it**, a token-bucket policer at about 25 Mbit/s refilled in quanta,
  whose losses cluster into runs. This is the only genuine congestion signal on
  the path.

`internal/pathsim` reproduces both, checked against the live figures at 1, 4,
12 and 50 Mbit/s offered, on delivered rate and on loss structure together.

## Telling the regimes apart

`internal/lossmodel` reads two statistics from the sequence numbers that
arrive.

`P(loss | previous arrived)` against the overall loss rate says whether the
loss is independent: a memoryless channel makes them equal, a queue does not.
The **burst factor** — mean loss-burst length divided by what independence
alone would produce — is 1 for an independent channel and larger when losses
cluster, and it is exactly the factor by which correlation shortens the
effective length of a coded block.

Which part of the loss is congestion is a min-filter, in the same spirit as
BBR's windowed minimum round trip: the channel's erasure rate is the lowest
loss seen recently, because the channel does not stop being lossy when the
queue drains, and everything above that floor is what the sender caused. This
is what makes the split self-correcting rather than a policy — congestion
lasting longer than the filter's window becomes the floor, and is then treated
as the channel it has effectively become.

## The congestion controller: the largest gain

Measured across the emulated path, carrying QUIC datagrams:

| controller | delivered |
|---|---|
| quic-go default (Reno/Cubic) | 0.09–0.13 Mbit/s |
| BBR | 0.39–0.95 |
| BBR-TUIC (this repo's port) | 1.10–5.56 |
| Brutal, told 25 Mbit/s | 14.03 |
| **erasure** | **10.31–11.50** |

Capacity is 25 × 0.58 = 14.5 Mbit/s. Every loss-responsive controller gives the
path away; Brutal reaches it only by ignoring loss and pacing at a rate a human
typed in, which is the one thing this transport may not require.

`internal/congestion.ErasureSender` makes two corrections.

**Channel loss is not forwarded as congestion.** Only the share of losses above
the measured floor reaches BBR. The fractional part is carried rather than
rounded away, so a congestive share below a half still registers — that is
exactly the mild persistent congestion a controller most needs to see.

**The pacing rate is divided by the arrival rate.** This is the subtler of the
two and it is why plain BBR *collapses* rather than merely under-shooting.
BBR's bandwidth estimate is the rate that is **delivered**, while its pacer
governs the rate that is **sent**. On a clean path those are the same number.
On an erasure channel they differ by the arrival rate, and pacing a
delivered-rate estimate makes the sending rate its own input: sending S
delivers S(1−p), which becomes the next estimate, which is paced as S(1−p),
which delivers S(1−p)². It walks down to nothing. Dividing by the arrival rate
restores the property BBR assumes, and the loop then converges on the
bottleneck instead — in startup S grows by the gain each round until delivery
stops growing, which happens exactly when S reaches the bottleneck.

On a lossless path the floor is zero, the correction is one, and the
suppression never triggers; measured there it delivers 16.23 Mbit/s against the
stock controller's 10.68.

Available as `--congestion erasure`.

## The erasure code: latency, not bandwidth

`internal/fec` is a systematic Cauchy-matrix Reed-Solomon code over GF(256):
any k of n shards reconstruct, and a receiver that loses nothing does no work.
`internal/fec.Choose` sizes it from the measured floor, the burst factor and a
target residual — at the live channel, (124,256) at 2.06× overhead with a
residual near a thousandth. `internal/coded` carries it over an unreliable
datagram carrier with retransmission for the residual.

The honest accounting is that **on a memoryless erasure channel, retransmission
uses less bandwidth than coding.** ARQ resends only what was actually lost,
1/(1−p) = 1.72× at 42%. A block code must provision for the binomial and pays
about 2.06× even when sized ideally. Measured end to end over QUIC on the
emulated path, a 1 MiB transfer went at 1.47 Mbit/s coded against 1.77 Mbit/s
on a reliable stream — the stream is slightly *ahead* on throughput, and that
is not a defect to be tuned away but arithmetic.

What retransmission cannot do is deliver on time. A lost packet costs a round
trip, a packet lost twice costs two, and at 42% loss 18% of packets need three
transmissions or more. Small messages at a steady rate, timed write to read
over the same path and the same controller:

| | median | p90 | p99 |
|---|---|---|---|
| **coded datagrams** | **153 ms** | 628 ms | 1.21 s |
| reliable stream | 1.372 s | 1.691 s | 1.96 s |

Nine times better at the median. The stream's figure is head-of-line blocking:
at 42% loss something is always missing, and everything behind it waits.

So the code earns its place on interactive and loss-sensitive traffic, and
costs about 15% of throughput on bulk. That is a per-class decision, not a
global one, which is why `fec.Class` exists.

## Live validation (2026-08-15)

The emulator is fitted to the path, not the path. Three controllers were run
against the live link, downloading the same 50 MiB file through the proxy from
the measurement server, bound to the LAN address so the host TUN route is
bypassed, interleaved and repeated because the path moves between trials. The
channel measured at the same time: ~35% memoryless loss below the knee (burst
factor 1.10), rising to 52% and clustered (1.42) at 30 Mbit/s offered, with
delivered capacity about 14.5 Mbit/s.

One lane, 20 s per run, Mbit/s:

| round | reno | bbr-tuic | erasure |
|---|---|---|---|
| 1 | 0.11 | 1.87 | 10.31 |
| 2 | 0.10 | 10.27 | 11.63 |
| 3 | 0.08 | 10.31 | 10.90 |

The erasure controller is the fastest in every round and, more usefully, the
only consistent one: BBR-TUIC collapsed to 1.87 in the first round and reached
10 in the others, which is the loss-responsive failure appearing and clearing
with the path. At about 11 Mbit/s against a 14.5 Mbit/s capacity it takes 75%
of what the channel can deliver, where Reno takes under 1%.

The emulator therefore had the ordering right and the margin wrong: it
predicted the erasure controller would beat BBR-TUIC nine-fold, and live the
gap is nearer 1.5x against BBR-TUIC's good rounds. What the emulator does not
reproduce is BBR-TUIC's variance.

### Re-measured on the current build (2026-08-15)

The channel that day: 38% loss, burst factor 1.28, about 12.4 Mbit/s of
delivered capacity. A 50 MiB download, one lane, interleaved, three rounds:

| round | reno | erasure |
|---|---|---|
| 1 | 0.04 | 10.07 |
| 2 | 0.05 | 10.22 |
| 3 | 0.07 | 10.71 |

About 85% of what the path can deliver, against 0.4% for what used to be the
default. The erasure controller is now the default, so this is what a fresh
install does.

Small transfers are a different story and are not improved. Time to first byte
for a 2.7 KB response over sequential flows was 1.3 to 1.9 seconds with either
controller -- congestion control barely matters for a few packets, and what
dominates is loss recovery on a reliable stream. That is exactly the
head-of-line blocking the coded path removes, but a flow that short never
gathers enough samples for the shared model to conclude the path erases, so it
is carried uncoded. Seeding a new connection's model from the endpoint pair's
history, which is what the model is for, is the way in.

### Small transfers, live (2026-08-15)

The 305 ms the emulator gives a small exchange does not reproduce on the live
link, and that gap is the open question rather than a detail.

Twelve 2.7 KB requests over one established flow, time to first byte, at 37%
loss and a 300 ms round trip:

| | median | p90 | max |
|---|---|---|---|
| erasure | 1.099 s | 1.483 s | 1.740 s |
| reno | 1.580 s | 2.638 s | 5.660 s |

The coding does what it was built to do at the tail -- p90 1.48 s against 2.64,
and a worst case of 1.74 s against 5.66 -- which is the long recoveries being
repaired without a round trip. But the median is about three and a half round
trips where the emulator gives one, so something costs two round trips live
that the emulator does not model. That is worth finding before the 305 ms is
quoted as a property of the system.

Each request over a *new* flow costs 1.341 s against reno's 1.577, so flow
initiation is not where it goes.

### Striping costs more than it gains here

The same comparison at four lanes:

| round | bbr-tuic | erasure |
|---|---|---|
| 1 | 8.70 | 8.63 |
| 2 | 7.73 | 7.64 |

Four lanes are worse than one — about 8 Mbit/s against the single lane's 11 —
and the controller no longer matters, because both land in the same place.
This is consistent with what the open-loop probe found in
`docs/PATH-CHARACTER-20260813.md`: the bottleneck is per endpoint pair, not per
4-tuple, so lanes do not multiply the share. They do multiply the offered rate
into one policer, which pushes the aggregate past the knee where loss turns
correlated and wasteful, and each lane's controller applies the erasure
compensation independently so the overshoot compounds.

Two things follow. On this path the lane count should be one unless a measured
probe shows a per-4-tuple limiter, which is what the open-loop split test is
for. And the erasure compensation, if lanes are ever used together, has to be
shared across them rather than applied per lane.

## The coded path

`internal/coded` carries frames over the connection's datagrams, coded against
erasure. It does **not** make delivery reliable, and that is the point: the
session above already sequences by byte offset, acknowledges with ranges,
retains what is unacknowledged and re-issues it. A block either repairs or it
does not, and what it does not the session re-issues.

The first version was a reliable transport of its own -- block
acknowledgements, a retransmission timer, flow control, in-order delivery --
and it carried 1.2 Mbit/s where the path carried 14.5, because its feedback
was a timer where QUIC's is an arrival and its delivery was in-order where the
session above already tolerates gaps. Removing all of it took the package from
1028 lines to 593 and made it faster.

Blocks are sealed when the send queue drains. That is not a policy: under load
there is always another frame, so blocks fill and the code is efficient, and
when the producer stops the block goes at once. Neither a size nor a delay is
chosen, so neither has to be re-chosen when the path or the traffic changes --
and a fixed delay is worse than either, because on a request-response protocol
every delay lands on the critical path of the next request and they compound.

Measured: 400 of 400 frames across a 43% erasure channel, at a (20,54) code.

## The control and data split

A lane is a QUIC **stream for control** and that connection's **datagrams for
bulk**, and the framing routes by frame type. Both, on one connection, always.

The split is what makes coding usable. A stream delivers in order, so at 42%
erasure every gap stalls everything behind it -- 1.372 s median against a
coded path's 153 ms on 256-byte messages. But the session's acknowledgements
must not be coded, because they are what releases the data whose blocks they
would then be queued behind: with everything on one coded substrate the same
channel carried 0.87 Mbit/s one way and 0.008 with acknowledgements coming
back the other.

Nothing is configured. The coded path reports whether it is *coding*, from the
measured floor, and bulk stays on the stream when it is not -- which on a clean
path is exactly the old behaviour. That is what lets one build serve a clean
path and an erasure channel without being told which it is on.

The bulk path belongs to the connection, not the stream: per-stream paths put
several receive loops on one connection competing for its datagrams, and left
some pooled streams coded and some not, which loses every frame a coding sender
sends to a receiver with no bulk reader.

## Flow initiation costs no round trips

An application opens a flow far more often than it opens a connection to the
server, so what it feels is the per-flow cost, not the per-connection one. The
first connection may cost what it must; every flow after it should cost
nothing.

Measured across an emulated 300 ms path, from dialing the local SOCKS port to
having the reply in hand:

| | before | after |
|---|---|---|
| first flow | 922 ms (3.1 round trips) | 618 ms (2.1) |
| proving flow | — | 303 ms (1) |
| every flow after | 306 ms (1) | **1 ms (0)** |

Three things are paid once and reused. The QUIC connection is pooled. The
session's authentication is one exchange, and it is not pipelined with the
first open, because pipelining would send that open before the server's
capabilities were known and the first flow would forfeit its control lane --
the one round trip a connection is allowed. And a fast open is proved once:
the capability can be advertised by a peer that cannot yet honour it, and the
refusal is the only signal to stop offering it, so one flow waits for its
acknowledgement and no later flow does.

After that a flow is answered as soon as its open is queued. The
acknowledgement still arrives and is still validated by the flow reader, which
propagates a typed reset; what is given up is the ability to answer SOCKS with
a precise failure, so an unreachable destination becomes a connection that
opens and then closes. `--wait-for-open-ack` buys the distinction back for one
round trip per flow.

## Coding is per flow, and reliability is per attempt

Coding and retransmission cost the same thing in different currencies. On a
memoryless erasure channel retransmission resends only what was lost, 1/(1-p),
where a block code provisions for the binomial. Measured live at 37% loss, a
50 MiB download runs at about 9.1 Mbit/s with its data on the stream and 5.0
with all of it coded — the code spending exactly the difference the arithmetic
predicts. A 2.7 KB exchange goes the other way, 1.9 s uncoded against 305 ms
coded, because there the currency is a round trip and not a byte.

So bulk data stays on the stream and everything else is coded, from the class
the flow is already measured to have. A download pays for the second before it
is recognised as bulk, which costs about 20% of its throughput — 6.3 to 7.6
Mbit/s against the 9.1 a stream-only build gets — and buys every short flow the
difference above.

Reliability then has to be a property of the **attempt**, not of the lane. A
lane's answer changes: one that carried a chunk over the coded path may carry
the next over its stream. Asking the lane as it is now strands exactly the
chunks that were sent unreliably — nothing re-issues them, because the lane no
longer thinks it needs to — and measured live that took a download from 6.2
Mbit/s to 0.25. The scheduler records how each chunk was taken and decides from
that.

## Timeouts belong to the exchange they bound

Three bugs, one shape: a constant sized for whichever path it was chosen on.

`handleLocal` set one deadline on the local SOCKS connection covering both the
application's request -- a loopback read that owes nothing to the network --
and the remote flow open, which takes as long as the path does. Across the
measured channel the open took eleven seconds, the deadline expired, and the
client closed the application's connection *after both ends had opened the flow
successfully*. The application saw EOF from a flow that was working. The local
exchange is now bounded locally and the flow open by its own machinery.

Every handshake deadline was a wall-clock constant. At 42% erasure one exchange
in a hundred needs seven transmissions, so five seconds expires mid-handshake
and the peer's stream is closed under a working flow. A handshake now gets a
number of round trips, scaled from the measured round trip and never shorter
than what was configured. The server also logs when it gives up: its silence is
why this took so long to find.

## The coded lane, and why it was slow

`--coded-lanes` carries a flow's frames over QUIC datagrams instead of a QUIC
stream. A connection carries one coded lane rather than a pool, because the
lane owns the datagrams. What a connection carries is decided by racing its
first stream against its first datagram, so one server serves both kinds
without being told and a mismatch fails instead of hanging.

It was correct from the start and slow for three reasons, none of them a
tuning error. 48 KiB echoed across the emulated erasure channel took 84
seconds; it now takes 35, and the remaining gap has a known cause rather than
a suspicion.

**Sealing on the wrong signal.** The layer above writes a frame and waits for
the answer, so waiting for a block to fill deadlocks the handshake, and sealing
per write puts one frame in each block. Sealing after a fixed delay is worse
still: on a request-response protocol every delay lands on the critical path of
the next request and they compound, which is why two milliseconds measured
worse than none. The signal that fits is neither size nor time but whether
anything else is waiting. A lane queues its frames and seals when the queue
drains: under load there is always another frame, so blocks fill and the code
is efficient; when the producer stops, the block seals at once. Nothing is
chosen, so nothing has to be re-chosen when the path or the traffic changes.
84 s to 51 s.

**Deciding the code rate before knowing the path.** A block's (k,n) was fixed
when its bytes were buffered, and a sender that runs ahead of its own feedback
buffers its whole window before the first report arrives — so every block was
sealed believing the path was clean, and carried no parity at all. The transfer
then ran entirely on retransmission. Measured directly: starting from a known
floor rather than from zero is 1.03 against 1.74 Mbit/s. 51 s to 35 s.

**Acknowledgements queued behind the data they gate.** This was the largest,
and it is what the control and data split above was built for. The session's
own ACK frames travelled over the same coded lane whose progress they release,
so each one waited behind a block pipeline that was waiting for it. Measured:
the same channel carried 0.87 Mbit/s one way and 0.008 Mbit/s when the reverse
direction had to carry acknowledgements — a factor of a hundred. A lane is now
a stream for control and that connection's datagrams for bulk, and the framing
routes by frame type: only DATA is eligible for the coded path, and everything
that releases it stays on the stream.

Two defects it exposed are worth recording because neither was visible from
either side alone. The datagram limit is not a constant to guess: a connection
configured with a 1200-byte packet accepts a payload near 1150, so shards sized
at 1200 were refused while short handshake frames went through — a session that
completed its handshake and then lost its first real frame. The carrier now
derives the limit from the connection's packet size and corrects it from any
refusal. And a connection that owns a lane cannot be closed on the way out of
the accept path the way a stream pool can: that ordering is deliberate for a
pool, whose loop only ends once the connection is already gone, but it tore
down a coded session as it was authenticating.

## One path, measured once

`internal/pathmodel` holds what an endpoint pair has been measured to do:
the erasure floor, the bottleneck, and each contributor's share of it.

Everything that adapts to this path needs the same numbers, and each component
used to estimate them alone. The congestion controller measured the floor from
the packets it sent; the erasure code measured it again from the shards it
received; a second lane measured it a third time from scratch. Three estimates
of one quantity, each wrong until it converged, and each converging only on its
own traffic.

The cost is not duplication but the initial value. An estimate that starts at
zero says the path is clean, and everything sized by it is sized for a clean
path — which is exactly the fault above, and the reason four lanes used to
overshoot a bottleneck none of them had finished measuring.

So the path is measured once and read by everyone. The congestion controller
contributes what its own acknowledgements reveal, which is the erasure rate of
the direction it sends into; that is precisely the number the erasure code
needs and would otherwise wait a round trip to be told.

## The code is a sliding window, not a block

A block code has to choose (k, n) when it seals the block, which means it has
to know the path before it has finished sending into it. Everything after that
is fixed: if the erasure rate rises, the parity already on the wire is the
parity the block gets, and a block that turns out to be under-protected is lost
whole. The shared path model makes that first guess a good one, but it is still
a guess made at the wrong moment.

`internal/fec/window.go` is the answer. Source symbols go as they are produced,
unaltered; repair symbols follow at whatever rate the path is currently
measured to need, each a random linear combination over GF(256) of the last
window of source symbols. The receiver holds one linear system in reduced row
echelon form, and a row that comes down to a single unknown is a recovered
symbol.

Three things follow that a block code cannot have.

**Redundancy reflects what is known now.** The decision of how much parity to
send is taken after the data, not before it, so a rising erasure rate is
answered by the next repair rather than by the next block.

**The window is a continuous interleaver.** A repair reaches back over
everything in the window, so a burst that would exceed one block's parity is
spread across every repair covering it. Measured: an eight-symbol burst inside
a thirty-two-symbol window, with one repair per four symbols, is recovered
whole — no block of four could have held it.

**The same residual costs less parity.** A window's repairs chain: a repair
that resolves a neighbouring symbol frees an equation covering this one, and
that equation may come from a window this symbol was never in. So the code
behaves like a block several times the window's length. Measured at 42%
erasure, for a residual of a thousandth:

| window | repairs per symbol, window | repairs per symbol, block |
|---|---|---|
| 16 | 1.35 | 1.81 |
| 32 | 1.15 | 1.44 |
| 64 | 1.02 | 1.20 |
| 128 | 0.93 | no rate reaches it |

The last row is not a rounding difference. A block of 256 shards is all
GF(256) has distinct generator rows for, so a block code simply gives up above
it — while a window's coefficients are drawn per repair over at most a window's
symbols, so a wide window is exactly where the code is cheapest. `WindowRate`
therefore has its own sizing rather than borrowing `ShardsFor`'s, and
`TestTheWindowRateIsWhatTheWindowNeeds` runs the real code at the rate it asks
for and checks the residual it actually gets, in both directions: three
quarters of that rate must miss the target, or the rate is buying a residual
nobody asked for.

Whole frames are packed into one symbol and delivered the moment it arrives; a
frame too large for a symbol takes symbols of its own and waits only for them.
Nothing waits for anything else, which is the property that made datagrams
worth using instead of a stream. Measured on the emulated channel: 400 of 400
frames arrived through 43% erasure, and the same delivery cost 1860 datagrams
against the block code's 2196.

## What the path actually is, measured end to end

Everything above was built from the transport's own view of the path, which is
made of the transport's own traffic. On 2026-08-15 the path was measured
directly instead, with a UDP probe that sends at a chosen rate and counts what
arrives at the far end (`--mode blast`/`sink` for up, `receive`/`reflector`
for down, since this end is behind a NAT). The result refines the premise this
project was built on in two ways that matter.

**The erasure is directional.** Sending China → US, loss is negligible until
the rate limit:

| offered | loss | delivered |
|---|---|---|
| 5 Mbit/s | 1.3% | 4.93 |
| 10 | 0.0% | 10.00 |
| 15 | 3.8% | 14.33 |
| 20 | 27.5% | 14.41 |
| 30 | 51.7% | 14.38 |

Sending US → China, loss is a flat fraction of whatever is offered, exactly as
an erasure channel should be, until the same ceiling:

| offered | loss | delivered |
|---|---|---|
| 4 Mbit/s | 39.1% | 2.44 |
| 8 | 38.7% | 4.91 |
| 12 | 38.8% | 7.35 |
| 16 | 37.1% | 10.06 |
| 20 | 38.1% | 12.36 |
| 30 | 51.5% | 14.43 |

So the download direction is the erasure channel and the upload direction is
not. Nothing in the design has to change for this, which is worth saying
explicitly: each side measures the erasure rate of the direction *it* sends
into, from its own acknowledgements, and publishes that to the model its own
code reads. An asymmetric path was never assumed and is handled by
construction.

**There is a policer as well as an erasure.** Both directions saturate at
about 14.4 Mbit/s delivered, and the loss above that is the excess being
dropped rather than the channel erasing. The two compose: a rate limit near 23
Mbit/s, and behind it a 38% erasure, so the most that can be delivered is
about 14.4 Mbit/s and the most useful goodput an ideal transport could extract
is the same number.

## Where the bulk throughput goes

That measurement makes the question answerable. Traced on the live link during
a 10 MB download (`QUEQIAO_LANE_TRACE=1`), in steady state:

- offered 26–28 Mbit/s, which is just above the policer,
- `maxbw` 15.4 Mbit/s, which is the delivered ceiling the probe measured,
- goodput 12–15 Mbit/s,
- `inflight` equal to pacing × smoothed RTT, so the sender is pacing-limited
  rather than window-limited,
- `issued=321` chunks against `source=10485966` bytes with `reissued=0`: the
  striping layer sends each chunk exactly once,
- `sent` 19.4 MB for 10.49 MB delivered, a ratio of 1.85 where the erasure
  alone costs 1/(1-0.38) = 1.61.

So in steady state the transport gets 85–90% of everything the path can
deliver, and there is no missing fifth to find in the data path. What the
whole-transfer average loses -- 8.5–9 Mbit/s against the steady state's 12–15
-- is spent at the two ends of the transfer: about a second ramping, and about
a second draining. That is where the remaining work is, and it is a different
problem from the one this document was written about.

It also settles the question of coding bulk. Measured live, with bulk forced
onto the coded path, three 10 MB downloads ran at 7.0, 6.1 and 5.0 Mbit/s
against 9.8, 9.0 and 7.7 on the stream. The arithmetic says why, and says it
is not a defect of the code: given a fixed offered rate, retransmission
delivers `offered x (1-p)` and a code delivers at best `offered / (1 +
p/(1-p))`, which is the same number. A code can match retransmission's
bandwidth and never beat it, so every point of parity margin is a point of
goodput -- and the margin is what buys the latency. Bulk stays on the stream;
small exchanges stay coded.

## What the window is worth, on the real link

The sliding window carries small and interactive traffic; bulk stays on the
stream for the reason above. So what it is worth is what a small exchange
costs, and that was measured directly: one flow held open, a 16-byte request
and a 2700-byte reply, one at a time, against the same server built twice --
once as it ships and once with coding disabled outright.

| | median | mean | p90 | max |
|---|---|---|---|---|
| coded | 290 ms | 364 ms | 340 ms | 2.68 s |
| uncoded | 908 ms | 1.22 s | 2.06 s | 10.3 s |
| coded (repeat) | 328 ms | 562 ms | 583 ms | 8.64 s |
| uncoded (repeat) | 696 ms | 1.10 s | 2.37 s | 7.92 s |

The median is one round trip on a path whose minimum is 245 ms, against two
and a half to three for retransmission, and the tail is four to six times
better. It also answers a question this document had been carrying: why a
small exchange cost about 3.5 round trips live where the emulator gave one.
The old figure of 1.099 s was measured with the block code, and it matches the
uncoded column -- the block code was buying almost nothing here, because a
block sealed for one small frame is one frame repeated, and repeating it is
what retransmission already does.

With the ramp fixes below, the same measurement tightened again: median
295-296 ms, p90 309-318 ms, maximum 349 ms.

## Opening a flow costs one round trip, or it costs everything

The transport pools one QUIC connection for control and initial streams and
moves classified bulk onto lanes of its own. That pooling was opt-in, and the
cost of it being off is the whole of flow initiation:

| | attempt 1 | attempt 2 | attempt 3 |
|---|---|---|---|
| unpooled | 0.645 s | 14.77 s | 1.111 s |
| pooled | 0.302 s | 0.292 s | 0.300 s |

Unpooled, every flow dials its own connection and pays a handshake across a
38% erasure channel -- the 14.77 s is one that lost packets. Pooled, a flow
costs one round trip and nothing else. Bulk measured the same either way, so
the reason it had been opt-in (bulk to a Reno peer) no longer holds, and it is
now the default.

Two things then had to follow it.

**A seeded rate is not a seeded window.** BBR derives both the rate it paces
at and the window it will fill from one bandwidth estimate, and only the rate
was being seeded from the shared model. Traced live on a path already measured
at 15 Mbit/s, a flow began with a 37 KB window and a 60 Mbit/s pacing rate,
and spent eight round trips doubling the window while the pacer waited on it.
Seeding the estimate moves both, which needs the path's round trip -- so the
shared model carries that now, and the coded path sizes its window from the
measured round trip rather than from a configured 300 ms.

**An estimate is forgotten by idling, not disproved by it.** The bandwidth
filter holds ten rounds, so a pooled connection carrying small exchanges keeps
only what those exchanges delivered. Making pooling the default exposed this
immediately: a download arriving on such a connection started from 0.4 Mbit/s
and took nineteen seconds to climb back to the 12 Mbit/s the path had all
along, where the same download on a fresh connection took nine. A connection
now keeps the peak it has measured and restores it when its pipe refills; if
the path really has narrowed, the filter's own ten-round window disproves it.
Re-measured on the case that produced it: 10.3 s, 13.9 s, 9.8 s against 25.0.

## What a short flow costs, and what it was costing

A browser opens a connection, asks for something small, and closes it. Every
such flow is new, so nothing about it is warm except the connection
underneath, and there is never any data behind its packets to prove one lost.
This is the case the whole transport is for, and it was costing a timer.

Measured across the emulated 300 ms path, every short flow cost 1.055 s. With
the erasure switched off entirely it still cost 1.055 s -- and a fixed cost on
a lossless path is not loss. It was a race. A flow opens without waiting for
the peer to acknowledge it, which is what makes opening one cost nothing, and
that means its first data frame can overtake the open that names it. The
demultiplexer dropped such a frame as "a frame for a flow nobody has claimed",
which reads like an unreliable substrate doing its job and was a race lost by
a few hundred microseconds. Recovery then fell to the reissue timer.

The connection now holds a frame for a flow it has not heard of yet and
delivers it when the flow claims its share, bounded at 256 frames, a megabyte
and two seconds so that a peer naming flows that never arrive cannot cost
memory.

| | before | after |
|---|---|---|
| emulated, 45% erasure | median 2.27 s, max 10.9 s | median 305 ms |
| live | median 0.29 s, a quarter above 1 s | 22 of 24 between 0.21 and 0.26 s |

Two further things were found on the way there. The reissue delay is now the
path's rather than a constant: it governs only the case where nothing follows
a chunk to prove it lost, and a second and a half was six round trips here. And
a coded path with no measurement of the direction it sends into borrows what it
measures of the direction it receives from -- a client that asks small
questions never sends enough to measure itself, and assuming the path is clean
is a worse assumption than assuming it resembles itself. The prewarm now also
runs at startup rather than only when the uplink changes, for the same reason.

## The sender is clocked by the receiver, so the receiver has to speak

A chunk completes when its bytes are acknowledged, a lane's admission frees
when its chunks complete, and nothing is issued until it does. The receiver,
though, only spoke when its contiguous point advanced -- so the one arrival
that proves a hole exists, a segment landing above it, was the one arrival it
said nothing about.

A single unrepaired chunk therefore stopped the sender dead. Traced live on a
10 MB download: no acknowledgement scheduled at all for the first five
seconds, the lane sitting with a full write-ahead window and an empty pipe,
8.5 MB of a 10 MB file held before anything was released, and the application
seeing delivery in lumps -- nothing for 1.6 s, then 3.3 MB at once.

The receiver now publishes its ranges and asks for an acknowledgement on any
arrival that leaves a hole. The sender acts on that evidence rather than a
timer: data acknowledged beyond a chunk is proof the chunk did not arrive, the
same inference a fast retransmit makes, at the layer where an unrepairable
coded symbol actually goes missing. After: acknowledgements throughout, and 28
Mbit/s held on the wire for the whole body of a transfer.

## What one flow gets, and what eight get

| | rate |
|---|---|
| one 10 MB transfer | 9.5-10 Mbit/s |
| eight at once | 12.1 Mbit/s aggregate, within 1% of each other |
| what the path delivers at 24 Mbit/s offered | 13.3 |

Eight flows reach the path's ceiling and share it evenly; one does not. The
difference is the tail: after the last chunk is issued, a transfer spends two
to three seconds with the wire rate decaying while the transport waits out
probe timeouts on the last losses, and with eight flows the others fill the
pipe meanwhile. Widening the per-lane write-ahead window from 512 KB to 2 MB
made no difference (8.19 s against 8.14 s over six paired transfers), so it is
not a commitment bound.

Probing the tail directly was tried twice and did not survive its own
measurement either time. First as a re-issue of the oldest outstanding chunks
each round trip: the tail moved from a mean of 3.56 s to 3.16 s with the
medians crossing the other way, which on a path this noisy is not a result.
Then, understanding that a copy written to the same reliable stream lands
behind the very hole it is meant to fill -- stream bytes are delivered in
order -- the copies were sent over the coded path instead, which delivers out
of order. Twelve paired transfers each way: 7.96 s against 7.73 s. Slightly
worse.

The reason is worth keeping, because it bounds what any tail mechanism can do
here. Filling the session's hole does not unblock the flow, because the bulk
of the data is on a stream and the stream's own hole still gates everything
QUIC carries behind it. Head-of-line blocking cannot be escaped by a side
channel while the main channel is ordered. The only escape is to carry bulk on
the coded path outright, and that is measured to cost more than it saves
(7.0/6.1/5.0 Mbit/s against 9.8/9.0/7.7). So the tail is a property of the
split, not a defect in it.

## Where the transport stands against the path

Measured on the live link on 2026-08-16, with the path itself measured the
same night by UDP sweep for comparison.

| | what the path gives | what the transport gets | |
|---|---|---|---|
| download, one flow | 13.3 Mbit/s | 9.5-10 | 73% |
| download, eight flows | 13.3 | 12.1 aggregate, within 1% of each other | 91% |
| upload, one flow | 14.5 | 11.3-11.7 | 81% |
| short flow, round trip | 210 ms | 210-260 ms | one round trip |

The two directions are not the same path. Downstream erases 45% of what it
carries at any rate and then policies at about 24 Mbit/s offered, so 13.3
Mbit/s is all that can be delivered. Upstream erases nothing measurable and
policies at 14.5. A transport that assumed symmetry would be wrong in both
directions at once.

Larger transfers behave: 100 MB completes in 67-107 s, memory returns
afterwards -- 198 MB resident during, 104 MB after three of them, with the
thread count unchanged -- and a 100 MB download **survives the server being
restarted underneath it**, completing intact because the session retains what
is unacknowledged and replays it on a new connection.

## What is not done

**The tail of a single bulk transfer.** It is understood and bounded above:
the transport waits out probe timeouts on the last losses, and no side channel
can fix it while the bulk rides an ordered stream. It costs a single flow
about a fifth of the path; concurrent flows fill each other's tails and reach
it. Escaping it means carrying bulk coded, which costs more than it saves at
today's erasure rate -- but that arithmetic turns on the rate, and a path that
erased less would change it.

Interleaving across blocks is now moot: the window interleaves continuously by
construction, and `Params.InterleaveDepth` has no separate meaning for it.

Lanes share a `PathModel` per endpoint pair: the erasure floor is pooled across
their samples, each lane is capped at its share of the bottleneck so their
probes cannot compound, and a joining lane starts from what the model already
knows rather than re-ramping. Live, four lanes improved from about 8.0 to 8.45
Mbit/s — still below the single lane's 10.0, which is the finding rather than a
defect, since the bottleneck is per endpoint pair. What changed is that using
lanes is no longer actively harmful, and the server now starts bulk flows on
one lane because of it.
