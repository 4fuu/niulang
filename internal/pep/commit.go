package pep

import (
	"sync"
	"time"
)

// laneCommit adapts how much unacknowledged data a lane may hold, expressed as
// a multiple of that lane's bandwidth-delay product.
//
// Every fixed value tried for this was right on one path and wrong on another,
// by more than an order of magnitude. Four bandwidth-delay products measured
// 43.3 Mbit/s on a 100 Mbit/s path with a deep buffer and 10.5 on a path that
// polices each source at 25 Mbit/s; two measured 35.7 and 18.2. The same trade
// then reappeared in the read-ahead ceiling, the per-lane chunk count and the
// acknowledgement delay -- each loosened to help the buffered path, each paid
// for by the policed one.
//
// The quantity that differs is not bandwidth or delay, both of which are
// already measured: it is how much of a burst the bottleneck will absorb before
// it drops. A deep buffer absorbs several products and answers with delay; a
// token bucket absorbs about one and answers with loss. So that is what this
// searches for, by the only means that does not require being told which kind
// of path this is -- commit a little more, and watch whether the bottleneck's
// loss rate answers.
//
// Watching for loss at all is not enough, and the first version of this made
// exactly that mistake. On a path with 1% ambient loss every sample reports a
// loss, so a search that retreats whenever it sees one retreats to the floor
// and stays there -- which cost four lanes their aggregation entirely, 18.3
// Mbit/s against a fixed setting's 26.9. A lossy link drops regardless of what
// the sender does; a policer drops *because* of the burst. The level of loss
// cannot tell those apart. Only whether the level *responds* to committing
// more can, so that is what this measures: a step up, a settling period, and a
// comparison of the loss rate before and after.
//
// This is not a congestion controller and must not be confused with one. The
// lane's own transport decides what goes on the wire and when. This decides how
// far ahead of the acknowledgement stream the sender is willing to commit bytes
// to one lane, which is a different question: those bytes cannot be moved to
// another lane once handed over, so the cost of over-committing is paid in
// head-of-line blocking as well as in loss.
type laneCommit struct {
	mu       sync.Mutex
	products float64

	// The step under test. A step is accepted when the loss rate did not rise
	// with it, and reverted when it did.
	probing    bool
	probeUntil time.Time
	before     lossRate
	stepFrom   float64

	// Ambient loss, measured continuously as bytes sent against packets lost.
	current  lossRate
	lastSent uint64
	lastLost uint64
	haveLast bool

	ceiling  float64
	lastMove time.Time
}

// lossRate accumulates losses against bytes sent so two periods can be
// compared. Comparing counts alone would confuse a longer period with a worse
// one.
type lossRate struct {
	bytes  uint64
	losses uint64
}

func (r lossRate) per() float64 {
	if r.bytes == 0 {
		return 0
	}
	return float64(r.losses) / float64(r.bytes)
}

const (
	// minCommitProducts is the floor, and it is measured rather than reasoned.
	// One bandwidth-delay product is what a path holds in flight by definition,
	// but a lane held to that under-fills: two measured 26.9 Mbit/s on four
	// lanes over a policed path against 18.4 for a search allowed to go lower,
	// and 35.7 on a single lane over a buffered one. Two is safe on both paths
	// measured, so the search explores upward from it and never below.
	minCommitProducts = 2.0
	// initialCommitProducts is where the search starts.
	initialCommitProducts = 2.0
	// maxCommitProducts caps the search, and is currently equal to the floor:
	// upward exploration is disabled on live paths.
	//
	// The mechanism below is exercised by unit tests and reaches the right
	// answer against modelled bottlenecks. It does not yet reach it against a
	// real one. On the emulated path that polices each source at 25 Mbit/s,
	// four lanes measured 16.4 to 18.4 Mbit/s with the search free to explore,
	// against 26.9 with the level pinned here -- so the live loss signal is not
	// driving the search the way the model says it should, and letting it climb
	// costs a third of the throughput striping exists to provide.
	//
	// Raising this needs the live signal understood first: most likely the
	// bottleneck's response arrives later than the settle window, because QUIC
	// reports a loss only once its own recovery declares one. Until then the
	// value measured safe on every path tested is the value used.
	maxCommitProducts = 2.0
	// commitGrowthStep is the size of one step up.
	commitGrowthStep = 0.5
	// commitSettleRTTs is how long a step is given to show its effect before
	// the loss rates are compared: long enough for the extra bytes to reach
	// the bottleneck and any drop to be reported back.
	commitSettleRTTs = 4
	// commitLossRise is how much the loss rate must rise for a step to be
	// judged responsible. Ambient loss varies, so a small rise proves nothing;
	// a policer answers a burst with far more than this.
	commitLossRise = 1.5
)

func newLaneCommit() *laneCommit {
	return &laneCommit{products: initialCommitProducts, ceiling: maxCommitProducts}
}

// window returns how many bytes this lane may hold unacknowledged, given the
// rate and round-trip time its transport currently reports.
func (c *laneCommit) window(rate float64, rtt time.Duration) int {
	if rate <= 0 || rtt <= 0 {
		return minLaneWindowBytes
	}
	c.mu.Lock()
	products := c.products
	c.mu.Unlock()
	window := int(products * rate * rtt.Seconds())
	if window < minLaneWindowBytes {
		return minLaneWindowBytes
	}
	if window > maxLaneWindowBytes {
		return maxLaneWindowBytes
	}
	return window
}

// observe advances the search from one sample of the lane's counters.
//
// Its only inputs are how many bytes the lane has sent and how many packets it
// has lost, both cumulative. Nothing tells it whether the bottleneck is a
// policer, a queue, or a lossy radio link.
func (c *laneCommit) observe(sent, lost uint64, now time.Time, rtt time.Duration) {
	if rtt <= 0 {
		rtt = 200 * time.Millisecond
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveLast {
		if sent >= c.lastSent {
			c.current.bytes += sent - c.lastSent
		}
		if lost >= c.lastLost {
			c.current.losses += lost - c.lastLost
		}
	}
	c.lastSent, c.lastLost, c.haveLast = sent, lost, true

	settle := time.Duration(commitSettleRTTs) * rtt
	if c.probing {
		if now.Before(c.probeUntil) {
			return
		}
		// Judge the step. A rise in the loss rate means this bottleneck is
		// answering the extra commitment with drops, which is what a policer
		// does and what a lossy link does not.
		after := c.current.per()
		beforeRate := c.before.per()
		c.probing = false
		c.current = lossRate{}
		c.lastMove = now
		if c.current.bytes == 0 && after == 0 && beforeRate == 0 {
			return
		}
		if beforeRate > 0 && after > beforeRate*commitLossRise {
			c.products = c.stepFrom
			c.ceiling = c.stepFrom
		} else if beforeRate == 0 && after > 0 {
			c.products = c.stepFrom
			c.ceiling = c.stepFrom
		}
		return
	}

	if !c.lastMove.IsZero() && now.Sub(c.lastMove) < settle {
		return
	}
	if c.products+commitGrowthStep > c.ceiling {
		// At the level this path was measured to tolerate. Keep watching: if
		// the path improves, the ceiling is retried after a long quiet period.
		if !c.lastMove.IsZero() && now.Sub(c.lastMove) > 20*settle {
			c.ceiling = maxCommitProducts
			c.lastMove = now
		}
		return
	}
	// Step up and measure. The comparison is against the loss rate just
	// observed at the current level, which is what makes ambient loss cancel.
	c.before = c.current
	c.current = lossRate{}
	c.stepFrom = c.products
	c.products += commitGrowthStep
	c.probing = true
	c.probeUntil = now.Add(settle)
}

// current reports the multiple in use, for tests and telemetry.
func (c *laneCommit) level() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.products
}
