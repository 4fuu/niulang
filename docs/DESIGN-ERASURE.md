# Transport for an erasure channel (2026-08-15)

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

**Acknowledgements queued behind the data they gate.** This is the largest and
is not yet fixed. The session's own ACK frames travel over the same coded lane
whose progress they release, so each one waits behind a block pipeline that is
waiting for it. Measured: the same channel carries 0.87 Mbit/s one-way and
0.008 Mbit/s when the reverse direction has to carry acknowledgements — a
factor of a hundred.

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

## What is not done

**Separating the control plane from the coded data plane.** This is the
hundred-fold fault above and the next thing to build. A lane should be a QUIC
connection carrying both a stream and datagrams: control, handshake and
acknowledgement frames on the stream, where they are immediate, reliable and
uncoded; bulk data on datagrams, where the code can see the erasures. Today a
lane is one or the other, so acknowledgements queue behind the blocks they
release. The pieces already exist — the protocol reserves a control lane, and
the connection already carries both — so this is a re-wiring rather than a new
mechanism.

**Emitting parity continuously rather than per block.** Fixing (k,n) when a
block is sealed means a sender must know the path before it commits data. The
shared path model makes the first guess a good one, but the structural answer
is a sliding-window code: keep the data symbols, and emit repair symbols on a
continuous schedule sized by the floor as it is currently measured. Then
redundancy always reflects what is known now, and running ahead of feedback
costs nothing.

**Live measurement of the coded lane.** Only the controller has been measured
on the real link.

Interleaving is modelled and sized for (`Params.InterleaveDepth` divides the
burst factor) but the sender does not yet spread shards across blocks, so
depth is 1 in practice. On a path whose above-knee loss clusters, implementing
it would buy back most of the rate that clustering costs.

Lanes now share a `PathModel` per endpoint pair: the erasure floor is pooled
across their samples, each lane is capped at its share of the bottleneck so
their probes cannot compound, and a joining lane starts from what the model
already knows rather than re-ramping. Live, four lanes improved from about 8.0
to 8.45 Mbit/s — still below the single lane's 10.0, which is the finding
rather than a defect, since the bottleneck is per endpoint pair. What changed
is that using lanes is no longer actively harmful.

The coded channel has not been measured on the live link -- only the
controller has. Its latency advantage is emulator evidence so far.
