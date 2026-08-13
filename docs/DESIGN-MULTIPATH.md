# Multipath transport: design

Status: implemented and measured; single-lane parity reached.
Supersedes the lane scheduler described in `docs/PERFORMANCE-20260812.md`.

## 1. What this is for

One application TCP flow, carried over several independent encrypted lanes
between a client in China and a fixed US egress, without losing the latency
that interactive traffic needs.

The narrow claim is worth stating first, because it bounds everything else.
**Striping one flow only pays where a single connection is policed below the
path's capacity.** On the emulated path that polices each source address at
25 Mbit/s, four lanes measured 33.5 Mbit/s against a single lane's 20.4. On a
shared bottleneck it measured nothing, and at 50 MiB the fixed four-lane
configuration lost a transfer outright with a worst trial of 5.39 Mbit/s
against a single lane's 20.1.

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

**Buffered writes are not a pacing signal.** A QUIC stream with an 8 MiB
flow-control window accepts megabytes before it pushes back, so a lane worker
that treats a returned `Write` as progress measures its own send buffer and
concludes every lane is infinitely fast.

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

"Room" is defined by the windows in 3.3, and a chunk is outstanding until the
peer acknowledges its bytes. Acknowledgement, not a completed write, is what
frees room.

### 3.3 Coupled congestion control

This is the part the previous design lacked, and the reason it was unstable.

Two windows govern admission. A lane may take a chunk when **both** allow it:

    flow.outstanding + chunk <= W          (the coupled, flow-level window)
    lane.outstanding + chunk <= A(lane)    (the lane's own allowance)

**A(lane)** is the lane's QUIC congestion window less what QUIC already has in
flight. It answers "can this path hold more right now?" and is read from the
transport rather than estimated, so a lane whose path collapses stops being
handed work immediately. It exists to stop the sender committing bytes to a
lane's send buffer that the path cannot move.

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
| Per-lane 56-frame push queues (1.75 MiB) | The windows bound commitment |
| Replay buffer, budget, growth, eviction, unreplayable state | The scheduler retains unacknowledged chunks and re-issues them |
| Timer-based head reinjection with pressure heuristic | Reinjection is a scheduler property |
| Lane-count A/B probe, its exhaustion and RTT-collapse guards | A useless lane is never pulled from, so lane count stops being a dangerous decision |

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

Both senders exist in one binary (`WANOPT_SELF_PACED=0` selects the previous
one), so these are the same path, the same seed, and the same trials.

**Policed path, 25 Mbit/s per source, 50 MiB, four trials.** This is the case
striping exists for, at the size where the previous design's aggregation had
collapsed.

| Sender | 1 lane | 4 lanes | Completed |
| --- | ---: | ---: | --- |
| Self-pacing | 18.17 | **37.53** | 4/4 |
| Pushing | 21.15 | 21.05 | 4/4 |

The pushing scheduler aggregates nothing at this size: four lanes measure what
one lane measures. It reached 33.5 at 20 MiB and lost that entirely by 50 MiB,
which is the decay a longer transfer exposes -- more chances to commit bytes to
a lane that then slows, and each one costs the receiver's contiguous point.
Self-pacing holds 37.53 with a worst trial of 36.37, tighter than the old
design ever was and higher than it ever reached.

**Shared bottleneck, 100 Mbit/s, 50 MiB, three trials.** Every configuration
completed every trial. This path is where the self-paced sender was initially
much worse, and chasing that found three separate limits that were binding
instead of the windows:

| | 1 lane | 4 lanes |
| --- | ---: | ---: |
| Self-pacing, first measurement | 34.05 | 49.94 |
| after raising the read-ahead ceiling | 33.96 | 60.67 |
| after charging admission by real chunk size | 31.04 | -- |
| after raising the chunk-count cap | 36.76 | 56.96 |
| Pushing (control) | 43.2 - 45.9 | 58.5 - 61.1 |

- The read-ahead ceiling was 128 chunks, 4 MiB, while one lane at 100 Mbit/s
  and 200ms needs 2.5 MB in flight and four need ten. The producer stopped
  reading before the windows ever bound.
- Admission charged every chunk a nominal 32 KiB. A read from a TCP socket
  usually returns far less, so a 2 MB window held about 224 KB of real data.
- The per-lane chunk *count* cap was 96, which with small chunks is under
  800 KB -- about 30 Mbit/s against a 210ms feedback loop, whatever the path
  can do.

Four lanes still beat one here, and that is not a win: it is four connections
claiming more of one pipe than one would, which is what coupled congestion
control exists to prevent.

**The single-lane gap is closed, and the cause was the window depth.** A lane
was allowed two congestion windows of unacknowledged data, which would be right
if a chunk left the window as soon as the path delivered it. It does not: a
chunk holds window space from the moment it is handed to a lane until the
peer's acknowledgement returns -- the transport's own queueing delay, plus a
round trip, plus the acknowledgement delay. Two windows could not keep the pipe
full across that loop.

At four congestion windows, one lane at 100 Mbit/s over 20 MiB, three trials:

| Sender | Median | Worst |
| --- | ---: | ---: |
| Self-pacing | **43.27** | 41.61 |
| Pushing | 38.92 | 37.42 |

Neither CPU nor the transport queries were the cause, which two earlier fixes
had assumed: caching the congestion-window read and snapshotting the
acknowledged set were both worth doing and neither moved this number.

The cost of the deeper window is a proportionally deeper commitment to a lane
that may stall. It is bounded, and it shrinks with the lane's own congestion
window, so a lane whose path collapses still stops being handed work.

## 8. Phases

1. **Coupled window** as a standalone, unit-tested component: LIA increase,
   rate-limited decrease, alpha from per-lane state. Tested against the three
   properties in 3.3 with synthetic lanes.
2. **Admission** wired into the scheduler: both windows consulted, lane
   allowance read from QUIC transport stats.
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
