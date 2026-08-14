# Multipath transport: design

Status: implemented and measured. Supersedes the lane scheduler described in
`docs/PERFORMANCE-20260812.md`.

## 1. What this is for

One application TCP flow, carried over several independent encrypted lanes
between a client in China and a fixed US egress, without losing the latency
that interactive traffic needs.

The narrow claim is worth stating first, because it bounds everything else.
**Striping one flow only pays where a single connection is policed below the
path's capacity.** On the emulated path that polices each source address at
25 Mbit/s, 50 MiB over four lanes measures 53.0 Mbit/s against the TUIC-shaped
reference's 22.5, every transfer completing. On a shared 100 Mbit/s bottleneck
the same four lanes measure 60.6 against one lane's 58.7 -- which is the
*intended* result, not a disappointing one: four connections should not take
four shares of one pipe.

TUIC, the reference this is measured against, does not stripe at all: it opens
one bidirectional QUIC stream per proxied TCP connection and relays raw bytes.
Its multiplexing removes head-of-line blocking *between* flows, which is the
common case and is already solved. Splitting a *single* flow is the uncommon
case. A design that makes the uncommon case fast and the common case worse is
a bad trade, so the first requirement is that this must never be worse than one
lane.

## 2. What went wrong, and what it implies

Three measured failures constrain the design.

**Prediction was the wrong instrument.** The previous scheduler estimated each
lane's rate, computed which lane would deliver a frame soonest, and committed
frames to that lane's queue in advance -- up to 56 frames, 1.75 MiB, with
sequence numbers assigned before the lane was chosen. When the estimate was
wrong the bytes were already numbered and behind a lane that had slowed, and
the receiver could not deliver past them. Everything built to compensate --
reinjecting a stalled head on a timer, a replay window sized to the whole
reorder span, an unreplayable state that failed flows -- was a correction for
mis-prediction.

**Uncoupled congestion control was the wrong default.** Each lane is a QUIC
connection with its own BBR. Four of them on a shared bottleneck claim four
shares, overshoot together, and drive each other into loss. That is what the
50 MiB result above is: not a tuning problem but a structural one, and the
problem MPTCP solved with coupled congestion control.

**Buffered writes are not a pacing signal -- but a blocking write is.** A lane
worker that treats bytes accepted into a send buffer as progress is measuring
its own buffer. quic-go's stream write is not that: it returns only once the
packer has consumed the bytes, which requires congestion-window and pacer room.
The first version of this design read the observation as "the transport cannot
be trusted to clock a lane" and built an application-level acknowledgement loop
instead. That cost a full extra round trip in the admission path and was the
larger of the two mistakes; see 3.2 and 7.1.

## 3. Design

Four mechanisms. Each maps onto something MPTCP or MP-RDMA already does, and
nothing else is retained.

### 3.1 One sequence space, addressed by byte offset

Chunks carry the byte offset they occupy in the application stream, which is
MPTCP's data sequence number. The receiver reassembles by offset and delivers
contiguously. Nothing about a chunk's placement constrains any other chunk, so
lanes are independent in time and any chunk may travel any lane.

### 3.2 Self-pacing: lanes pull, the sender never pushes

A lane asks for a chunk when it has room. A fast lane asks often, a throttled
lane asks rarely, a dead lane never asks and is excluded without anything
deciding to exclude it. This is MPTCP's scheduler -- send on a subflow that has
congestion window space -- and it removes rate estimation from the data path
entirely.

"Room" is what the lane's transport has not yet taken. A chunk is *committed*
to a lane from the moment the lane takes it until the lane's QUIC stream write
returns, and that write returns only once quic-go's packer has consumed the
bytes into packets -- which it does only when the congestion window and the
pacer allow. So the transport's own congestion control is the clock, directly,
with nothing estimating anything.

This is the correction that mattered most, and it took three wrong answers to
find. A completed *buffered* write is not a signal, which is true and is what
the original design said. The conclusion drawn from it -- that the peer's
application-level acknowledgement must be the signal instead -- is wrong, and
expensively so: that acknowledgement arrives a full round trip after the
transport already knew, so a window sized to keep the lane busy across it has to
cover two round trips rather than one. Every byte of that window is a byte
numbered into the application stream and committed to one lane, where no other
lane can take it back if that lane slows. Sizing it is not a tuning problem to
be solved with a better constant; it is a cost that should not be paid.

A blocking write has neither problem. It returns at the transport's clock, and
the queue in front of it is the only thing committed in advance -- bounded, in
3.3, at a fraction of the lane's congestion window.

The peer's acknowledgement is still needed, for a narrower job: a chunk must be
retained until the peer has it, because a lane that dies may not have delivered
what its transport accepted. That is a memory bound, not a throughput bound, and
it does not gate admission.

### 3.3 Admission, and coupled congestion control

Two limits govern admission. A lane may take a chunk when **both** allow it:

    flow.outstanding + chunk <= W          (the coupled, flow-level window)
    lane.uncollected  + chunk <= Q(lane)   (the lane's write-ahead queue)

**Q(lane)** is a quarter of the lane's QUIC congestion window, bounded to
[64 KiB, 512 KiB], and `lane.uncollected` is what the lane has been handed that
its transport has not yet taken. It is not a congestion window and must not be
read as one -- the transport already has one of those, and two nested
controllers would be a mistake. It is a jitter buffer: enough that the writer
always has the next chunk ready, and no more.

It cannot be zero. A QUIC sender that runs out of data marks its bandwidth
samples application-limited, BBR discards those samples, and its
full-bandwidth test skips those rounds -- so a lane starved even momentarily can
stop its controller ever leaving startup. Measured on a path policing each
source at 25 Mbit/s, that held a lane at 2.9 bandwidth-delay products for a
whole 20 MiB transfer and doubled the round trip with standing queue.

It also cannot be large, and this is the constraint the earlier design missed. A
lane's queue is head-of-line exposure: bytes already numbered into the
application stream that only this lane can deliver. A quarter of a congestion
window shrinks with a lane that slows, is bounded above so a lane with a very
large window cannot turn its queue into a buffer, and needs nothing configured.

**W** is the flow's shared window across all lanes, and it is what couples
them. It follows MPTCP's Linked Increase Algorithm (RFC 6356):

    on each acknowledged chunk of b bytes:
        W += min( alpha * b / W , b / W_of_that_lane )
    on a congestion signal from any lane:
        W = max( W / 2 , W_min )

with alpha computed from per-lane windows and round-trip times:

    alpha = W * max_i( w_i / rtt_i^2 ) / ( sum_i( w_i / rtt_i ) )^2

The three properties this buys are the reason to use a published algorithm
rather than invent one:

1. The flow does at least as well as it would on its best single lane.
2. On any shared bottleneck it takes no more than a single flow would.
3. It moves traffic off congested lanes onto uncongested ones.

Property 2 is what the 50 MiB failure needed. Property 1 is what protects the
common case. Property 3 is what makes a policed path -- where each lane has its
own uncongested allowance -- grow to the aggregate.

Congestion signals come from the lanes' QUIC controllers: a lane entering loss
recovery, or its congestion window shrinking. Decreases are rate-limited to one
per round trip so a single loss episode is not counted many times.

Note what this does *not* do: it does not replace QUIC's per-lane congestion
control, which still paces the wire. W bounds what the application commits
across lanes. Two nested controllers would be a mistake; one bound over several
controllers is the MP-RDMA arrangement -- a single connection-level window
distributed to whichever path has room.

### 3.4 Reinjection

A chunk outstanding on one lane past a deadline is offered to another lane as
well, and a chunk on a lane that dies is returned to the ready set. The
receiver discards the duplicate. This is MPTCP reinjection, and it is what makes
a throttled or broken path recoverable rather than fatal.

One rule is not obvious and was found by a deadlock: a chunk that comes back
from a stalled lane must be admitted **over** the windows. Every other lane is
by then full of chunks that cannot be acknowledged until the missing one lands,
so respecting the window would leave every lane waiting on work none of them is
allowed to take.

## 4. What is deleted

The measure of this design is how much it removes.

| Mechanism | Why it goes |
| --- | --- |
| Virtual transmit clock, predicted-arrival lane ranking | Replaced by pulling |
| Per-lane 56-frame push queues (1.75 MiB) | The write-ahead queue bounds commitment |
| Replay buffer, budget, growth, eviction, unreplayable state | The scheduler retains unacknowledged chunks and re-issues them |
| Timer-based head reinjection with pressure heuristic | Reinjection is a scheduler property |
| Lane-count A/B probe, its exhaustion and RTT-collapse guards | A useless lane is never pulled from, so lane count stops being a dangerous decision |
| Per-lane burst-tolerance search (`commit.go`) | It was scaling a quantity that had already run away; see 7.1 |
| The pushing sender itself, and the `WANOPT_SELF_PACED` switch | The comparison it existed for is recorded |
| Virtual transmit clock, lane ranking by predicted arrival | A pulling sender never chooses a lane, so an order has nothing to express |
| Frame-level replay window, its budget, eviction and "unreplayable" state | It held the same bytes the scheduler holds, under a second limit |
| `WINDOW`, `PING` and `PONG` frame types | Specified, never sent; QUIC already provides both properties |

That last one deserves a note. A whole measurement apparatus was built to decide
whether to add a lane, because under a pushing scheduler a bad lane actively
hurt. Under self-pacing with a coupled window, an added lane that cannot carry
anything simply receives nothing, and one that can is bounded by W from taking
more than the flow's fair share. Lane count becomes a resource decision, not a
performance gamble: open up to a cap, let the pacing sort it out.

## 5. Wire protocol

Unchanged. DATA frames already carry a byte offset as their sequence, and range
acknowledgements already report the byte ranges a receiver holds out of order.
Both are exactly what this design needs, so there is no new format, no new
capability negotiation, and no compatibility break.

## 6. Non-goals

- Replacing QUIC's per-lane congestion control.
- Striping interactive or short flows. They stay on one lane; the latency cost
  of reordering is real and the throughput gain is nil.
- Beating TUIC on a shared bottleneck. The honest target there is parity.

## 7. What the redesign measured

All figures below are `cmd/wanoptbench` against the seeded path emulator, both
stacks in one process, same path and same seed. "Reference" is
`internal/baseline`, the TUIC-shaped control: one authenticated QUIC connection,
one stream per relayed connection, unframed copying, on the same QUIC stack and
the same controller.

### 7.1 Where the aggregation regression came from

A 37.5 Mbit/s four-lane result on the policed path had stopped reproducing, and
the recorded explanation -- that a bottleneck's tolerance for a burst is a
property of the path that no constant can capture -- was wrong. Instrumenting
per-lane state over time (`WANOPT_LANE_TRACE=1`) found three separate faults,
none of which is about burst tolerance.

**The lane window was computed from quantities the window itself inflated.** It
was `2 x pacing_rate x smoothed_RTT`. BBR's pacing rate carries the startup gain
of 2.885, and the smoothed round trip carries the queueing delay that the window
had just created, so the product is a positive feedback loop: on a path policing
each source at 25 Mbit/s it reached 7.1 MB against that lane's true
bandwidth-delay product of 625 KB, and every path measured ran the per-lane
commitment to its 8 MiB ceiling. No search over "how much of a burst does this
bottleneck absorb" can succeed while the quantity being scaled is a runaway; the
search added in `internal/pep/commit.go` is deleted rather than fixed.

**Admission was clocked a round trip too late.** Even sized correctly, a window
released by the peer's application-level acknowledgement must cover two round
trips rather than one. Instrumenting chunk residency -- hand-off to release --
measured 600 to 1000 ms against a 200 ms path. That is why every constant wanted
to be large, and why every one of them cost the policed path: a large window is
a large per-lane commitment, and a striped flow pays for that in head-of-line
blocking. Admission now bounds only what the lane's transport has not yet taken
(3.2), so the acknowledgement is out of the throughput path entirely.

**The controller never left startup.** `bbr-tuic` classified the sender as
application-limited whenever bytes in flight were below the congestion window,
which for a *paced* sender is nearly every acknowledgement. The full-bandwidth
test skips application-limited rounds, so it never counted one: on the policed
path the controller stayed in STARTUP for an entire 20 MiB transfer, held 2.9
bandwidth-delay products in flight, and ran the round trip at 400 ms against the
path's 200. Requiring a full burst of unused window -- the rule the code already
applied in DRAIN -- fixes it; the transfer now reaches ProbeBW and the round trip
returns to 201 ms. This affected the reference equally, so it never showed up in
a wanopt-versus-reference comparison, which is exactly why it survived.

### 7.2 A fourth fault: a completed chunk stayed in the ready set

Re-offering a chunk to a second lane leaves it in two places at once: in flight
on the first lane, and waiting in the ready set for the second. If it then
arrived by the first route, nothing removed the copy still waiting -- so a later
lane picked it up and sent bytes the peer already had.

It hid from its own counter. A chunk taken while still in flight is recorded as
a re-issue; one taken after it completed is no longer in flight, so it was
recorded as a *new* issue. The re-issue counter therefore read zero throughout.
What gave it away was arithmetic: on a 264 ms path at 20% loss, a 320-chunk
object was issued 481 times, and the emulator counted 74% more packets crossing
the path than the reference needed for the same bytes.

`Complete` now drops the chunk from the ready set. At 20% loss that took wanopt
from 13.1 Mbit/s to 15.2 against the reference's 14.6, and the packet counts
match the reference's to within half a percent. It also closed most of the
concurrent-flow gap, from 4 and 8 flows at 59.2 Mbit/s against 62.1 and 68.3 to
61.6 and 68.8.

### 7.3 A fifth fault: isolation threw away the warmed-up lane

A bulk flow moves off the shared control connection so its congestion window
cannot queue interactive traffic. The implementation excluded lane 0 from bulk
the instant any joined lane existed -- including when the joined lane had just
been created and had a fresh congestion window, and including when nothing else
was using the control connection at all.

Measured on the policed path: a lane arrived five seconds into a 20 MiB
transfer, the flow abandoned a lane running at the full policed rate, and the
remaining quarter of the transfer took nearly half of its total time.

Isolation is now paid only while another flow is actually using the pooled
connection, tested per lane selection rather than once per flow. A flow alone on
the pool keeps the control lane; a flow sharing it yields within one chunk.

### 7.4 Result

The standard matrix (`scripts/bench_matrix.sh`, five trials per cell, every
transfer completing on both stacks), medians in Mbit/s:

| Block | Reference | wanopt |
| --- | ---: | ---: |
| 10 MiB, 200 ms, 0% loss | 37.94 | 38.05 |
| 10 MiB, 200 ms, 1% loss | 30.49 | **32.11** |
| 10 MiB, 200 ms, 3% loss | 29.19 | 29.24 |
| 10 MiB, 200 ms, 5% loss | 28.39 | 28.85 |
| 50 MiB steady state, 1% loss | 58.40 | 58.73 |
| 10 MiB x 4 concurrent flows | 62.18 | 61.66 |
| 10 MiB x 8 concurrent flows | 70.40 | 70.72 |
| 10 MiB, 264 ms, 10% loss | 17.92 | 17.58 |
| 10 MiB, 264 ms, 20% loss | 15.12 | **15.68** |
| 256 MiB, no impairment (datapath cost) | 879.94 | **905.94** |

Parity or better everywhere; the two cells below the reference (4 flows, 10%
loss) are inside the spread of repeated runs. `bench_matrix.sh --gate
--tolerance 0.10` passes, which is the first time it has.

The striping regime -- a path policing each source address at 25 Mbit/s, 200 ms,
1% loss:

| | Reference | wanopt |
| --- | ---: | ---: |
| 20 MiB, 1 lane | 20.70 | **22.26** |
| 20 MiB, 4 lanes | -- | **42.71** (1.92x one lane) |
| 50 MiB, 4 lanes | 22.49 | **53.03** (2.36x the reference) |
| 50 MiB, lane count searched, nothing configured | 22.23 | **33.19** |

The last row is the one that matters for a product: no `--lanes`, no
`--initial-lanes`, the client measuring its way there. It was 20.6 against the
reference's 20.4 before this work -- a search that was safe and bought nothing.
It reaches 33.2 now, and the gap to the pinned 53.0 is the search's own latency:
a probe costs a warm-up, a baseline, a settle and a measurement, so it converges
in seconds and a 15-second transfer spends a third of itself getting there. A
confirmed probe now doubles the target rather than adding one, which halves the
remaining distance; going further means making the measurement cheaper, not
making the policy bolder.

Interactive requests during a 50 MiB bulk transfer, 200 ms, 1% loss:

| | Reference | wanopt |
| --- | ---: | ---: |
| bulk goodput | 57.29 | 52.00 |
| interactive median | 323 ms | **208 ms** |
| interactive 95th percentile | 506 ms | **386 ms** |

208 ms is the idle round trip: interactive requests no longer queue behind bulk
at all. The 9% of bulk goodput is what isolation costs, and it is now charged
only while there is traffic to protect.

The 50 MiB shared-bottleneck case is the acceptance test in section 8: four
lanes measure 60.59 Mbit/s with 4/4 transfers completing and a worst trial of
57.74, against one lane's 58.73. It passes -- and it passes by *not* aggregating,
which is what a shared bottleneck should produce.

Before this work the same measurements were: policed one lane 19.1 against the
reference's 20.6, policed four lanes 18.0 to 30.0 across runs, 20% loss 13.1
against 14.8, four concurrent flows 59.2 against 62.1, and no reproducible
interactive advantage.

### 7.5 Lanes and the per-connection ceiling at high bandwidth-delay

At 200 ms with no rate limit, one QUIC connection plateaus well below the path.
Both stacks do it, and the reference's is a plain copy into a stream, so it is
not this project's scheduler. Four hundred MiB, so the congestion window's ramp
is amortised rather than dominating:

| | Mbit/s |
| --- | ---: |
| Reference, one connection | 209.90 |
| wanopt, one lane | 288.62 |
| wanopt, four lanes | **413.26** |

So parallelism does raise the ceiling for a single application flow -- 43% from
one lane to four -- but nothing like the fourfold a purely per-connection limit
would give. A striped flow is still one ordered byte stream: the lanes are
independent on the wire and share a reassembly point and an acknowledgement
loop, and that is what four lanes cannot divide.

The independent-flow case separates more cleanly. Four concurrent flows at
200 ms measure 196 Mbit/s through the reference, which multiplexes them onto one
connection, against 343 for wanopt, which moves classified bulk flows onto their
own. There the ceiling really is per connection and having more of them is worth
proportionally more.

What sets the single-connection ceiling is not established. It is not flow
control -- raising the stream receive window from 8 to 64 MiB changes nothing --
nor the emulator's rate limiter, nor its delivery queue, and the sender's own
trace shows a 17 MB congestion window with 6.5 MB in flight and no queueing at
all. It does not arise on the live path, which runs at about 10 Mbit/s, so it is
latent rather than pressing.

### 7.6 What is now the limit

On the policed path each lane still holds two bandwidth-delay products in flight
once it reaches ProbeBW, which is BBRv1's congestion-window gain and not
something this design chose. Four lanes reach 41.6 Mbit/s where the lanes'
combined policed allowance is 75; the gap is reordering and reassembly cost, and
it has not been decomposed.

Startup itself is still expensive in latency, and correctly so rather than
through a defect: the bandwidth estimate plateaus at round 8 and the controller
leaves startup at round 12, which is the published three-to-four rounds. But a
round during startup lasts 400 ms rather than 200, because BBR's own 2.885 gain
has filled the bottleneck with a bandwidth-delay product of queue -- so the
exit takes 1.6 s of wall clock, and a transfer shorter than that never sees the
drained state. This is BBRv1 behaviour, shared with the reference and with
native TUIC, not something this design introduced. Reducing it means a
different startup, not a different scheduler.

### 7.7 The live link, and what the emulator got wrong

Every figure above is emulated. Run against the real China-US path -- 263 ms
average round trip, 226 to 440 ms range, 5% loss, 48 ms of jitter -- the first
campaign said something the emulator never had: **wanopt lost 17 of 18
alternating rounds**, 5.42 Mbit/s against the reference's 8.85.

Disabling striping did not close it (4.34 against 8.84), so it was not the lane
machinery. Raising the per-lane write-ahead bound did not close it either (4.51
against 9.01). What closed it was reverting one change made in this work: the
application-limited test in `bbr-tuic`.

The sender's own trace showed why. Its bandwidth estimate crept from 80 KB/s to
288 KB/s over ten seconds and never left startup, and the sample counters
explain it: `nonapp` froze at 94 while `appsamp` climbed past 1600. Almost every
delivery-rate sample was being taken as valid, and on a path whose round trip
varies by a factor of two most of those samples are spuriously low. BBR's
application-limited marking is what keeps them out of the bandwidth filter.
Tightening that marking -- which on the emulator looked correct, and let the
controller leave startup instead of holding 2.9 congestion windows in flight --
removed the protection. With it restored, the same configuration measures 9.67
against 9.92.

The full campaign on the shipped configuration, fourteen alternating rounds:

| | Reference | wanopt |
| --- | ---: | ---: |
| mean | 10.59 Mbit/s | 10.24 Mbit/s |
| rounds ahead | 8 of 14 | 6 of 14 |

**Parity, not an advantage.** That is the honest live result, and it is the
number that should be believed over any emulated one in this document.

Two things follow. The first is that reverting cost nothing on the emulator --
23.32 policed against 20.70, 58.85 on four lanes, 44.46 shared, 206 ms
interactive median -- so the change never had emulated evidence for its
throughput either; it had a mechanism, and the mechanism was reasoned about on a
path with constant delay and independent loss. The second is that the emulator's
gap is now a known quantity rather than an assumption: it models neither RTT
variance nor burst loss, and both are what this transport meets in practice.

The emulator finding it was meant to fix is still real. On a constant path the
controller does stay in startup too long and holds a bandwidth-delay product of
standing queue. Fixing that needs something that does not also discard the live
path's noise, and that is not attempted here.

**The emulator has been improved, and it still does not reproduce this.** It
gained the impairment it most obviously lacked -- `DelayWander`, a correlated
random walk on the one-way delay, as against `DelayJitter`'s independent
per-packet draw. The distinction is the one that matters here: jitter reorders
and leaves the smoothed round trip near the minimum, while wander shifts a whole
flight together so the round trip varies and the minimum does not, which is what
the live path does. It is a real impairment -- on an otherwise identical path it
costs the transport 26% of its goodput, 13.84 Mbit/s to 10.27 -- and configured
to the live path's range it still scores the two controller variants within 2%
of each other, where live they differ by more than a factor of two.

So the emulator is now closer to the path and is still not a substitute for it.
That is worth stating plainly rather than presenting the improvement as a
validation: what remains unmodelled is at least the separate-process, separate-
machine timing of a real deployment, and whatever else the live path does that
this has not yet identified.

### 7.8 Scenarios outside the standard matrix

The matrix covers one shape of path. These are the others, four to six trials
each, medians in Mbit/s, reference then wanopt:

| Scenario | Reference | wanopt |
| --- | ---: | ---: |
| 20 ms, 1 Gbit/s, 100 MiB | 685.24 | **708.12** |
| 30 ms, 200 Mbit/s, 0.1% loss | 180.98 | 180.76 |
| 200 ms, 20 ms jitter (reordering) | 2.93 | **8.39** |
| 200 ms, 25% *upstream* loss, 32 MiB | 50.47 | **51.64** |
| 200 ms, 256 KiB bottleneck queue | 39.10 | **39.80** |
| 200 ms, 64 KiB object | 1.11 | **1.17** |

The jitter row is the largest margin anywhere in this document and deserves a
note, because it is structural rather than tuned. A 20 ms uniform jitter
reorders packets, and a transport that relays one QUIC stream per connection
pays for every reordering event with head-of-line blocking on that stream.
Reassembling by byte offset across chunks does not: a chunk that arrives out of
order is placed, not waited for. This is the same property that makes striping
possible, showing up on a single lane.

The reverse-loss row is worth stating too, because it is the case that should
have been this design's worst: a transport that layers its own acknowledgements
over QUIC puts application data on the return path, where 25% loss falls on it.
It is at parity, which it would not have been while admission depended on those
acknowledgements arriving.

**Correlated loss: what it was, and what is left.** At 35% loss in 10-packet
bursts, a flow that lost its lane spent 45 seconds trying to rejoin a session
the server did not have. The handshake itself often fails at this loss rate, so
the server had never registered the session at all -- and a client answered
`unknown session` twelve times over 34 seconds kept waiting anyway, because the
recovery path treated a permanent answer as a transient one. A session
identifier is random and is never reissued, so the first refusal is as
informative as the twelfth; it now ends the flow's replacement grace.

Twenty trials, 4 MiB each: wanopt completes 15 of the 16 transfers it starts,
the reference 8 of 13 -- the other five fail or truncate. Both stacks lose a
similar number of connections during setup, which is the path and not the
transport. Medians are indistinguishable at this loss rate, so completion is the
only comparison that means anything here.

What is left is that a lane still dies of QUIC's idle timeout mid-burst, and
each death costs a rejoin. Raising the idle timeout is not the answer: measured
at 30 seconds it made wanopt worse, because the same constant bounds how fast a
genuinely black-holed path falls back to TCP.

**The TCP rescue path is fixed.** The flake was not a data-path failure at all,
which is why it was hard to see: every application byte crossed in both
directions, and then neither endpoint could finish. The acknowledgement loop
returned on its first failed write, which happens exactly when the lane it was
using dies -- so a flow that lost a lane stopped acknowledging permanently and
never resumed on the replacement that arrived half a second later. The peer's
sender is clocked by those acknowledgements. The loop now waits for a
replacement lane and re-acknowledges from where the peer was last told; the test
passes twenty times consecutively and under `-race`, which it never had.

### 7.9 Corrections to the earlier record

- The "5.37 s lane authentication exchange" is not reproducible. Measured now:
  the secondary QUIC pool authenticates in 404 ms and the lane join completes in
  606 ms. The delay before a joined lane appears is the lane probe's own
  baseline window, which is a scheduling choice, not a server-side defect.
- `--per-flow-rate` used not to model a shallow token bucket: the per-source
  policer inherited the aggregate path's queue, so at `--rate 400
  --per-flow-rate 25` each lane got a 10 MB bucket -- three seconds of
  buffering. It now scales the bucket from its own rate. Re-measured on the
  corrected path the striping results are unchanged: 23.53 Mbit/s on one lane
  against the reference's 20.69, and 61.95 on four against 22.23. The bug was
  real and the conclusions drawn on it happened to survive it.

## 8. Phases

1. **Coupled window** as a standalone, unit-tested component: LIA increase,
   rate-limited decrease, alpha from per-lane state. Tested against the three
   properties in 3.3 with synthetic lanes.
2. **Admission** wired into the scheduler: the lane's write-ahead queue read
   from QUIC transport stats, released by the lane writer.
3. **Deletion** of everything in section 4.
4. **Re-measurement**, reported whether or not it favours the design:
   - single lane against native TUIC, 0/1/3% loss -- must be parity;
   - policed path, 1 and 4 lanes, 20 and 50 MiB -- must not lose transfers;
   - shared bottleneck, 1 and 4 lanes -- must not regress against one lane;
   - lane failure and TCP rescue under load;
   - interactive latency under bulk load.

The 50 MiB shared-bottleneck case is the acceptance test. A design that cannot
run four lanes there without losing a transfer has not solved the problem it
was built for.
