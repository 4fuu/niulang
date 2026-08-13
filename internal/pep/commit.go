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
// of path this is -- commit a little more while nothing is lost, and give back
// sharply when something is.
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
	lastLoss time.Time
	lastGrow time.Time
}

const (
	// minCommitProducts is the floor. One bandwidth-delay product is what a
	// path can hold in flight by definition, so below this a lane cannot keep
	// itself busy however shallow the bottleneck.
	minCommitProducts = 1.0
	// initialCommitProducts starts where a path that tolerates nothing extra
	// still works, and lets the search find the rest.
	initialCommitProducts = 1.5
	// maxCommitProducts caps the search. Beyond this the commitment to a single
	// lane costs more in head-of-line blocking, when that lane stalls, than the
	// throughput it can buy.
	maxCommitProducts = 6.0
	// commitGrowthStep is how much the search adds per quiet round trip.
	// Reaching the ceiling from the floor takes about twenty round trips, four
	// seconds on a long-haul path, which is fast enough to matter within a
	// transfer and slow enough not to overshoot on a path that will not take
	// it.
	commitGrowthStep = 0.25
	// commitBackoff is the multiplicative decrease on loss. Sharper than the
	// growth is deliberate: over-committing on a shallow bottleneck is
	// expensive and mis-measuring it is cheap to redo.
	commitBackoff = 0.6
	// commitLossQuiet is how long after a loss the search waits before growing
	// again, in round trips. A loss episode spans a round trip, and growing
	// into the tail of one measures the wrong thing.
	commitLossQuiet = 2
)

func newLaneCommit() *laneCommit {
	return &laneCommit{products: initialCommitProducts}
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

// observe advances the search from one sample of the lane's loss counter.
//
// The caller reports whether the lane lost anything since the previous sample.
// That is the whole input: no distinction between a policer and a queue, and
// nothing about the path supplied in advance.
func (c *laneCommit) observe(lost bool, now time.Time, rtt time.Duration) {
	if rtt <= 0 {
		rtt = 200 * time.Millisecond
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if lost {
		// Rate-limit to one backoff per round trip: a loss episode is reported
		// by every packet in it, and backing off once per report would collapse
		// the window on a single burst.
		if c.lastLoss.IsZero() || now.Sub(c.lastLoss) >= rtt {
			c.products *= commitBackoff
			if c.products < minCommitProducts {
				c.products = minCommitProducts
			}
			c.lastLoss = now
		}
		return
	}
	if !c.lastLoss.IsZero() && now.Sub(c.lastLoss) < time.Duration(commitLossQuiet)*rtt {
		return
	}
	if !c.lastGrow.IsZero() && now.Sub(c.lastGrow) < rtt {
		return
	}
	c.lastGrow = now
	if c.products < maxCommitProducts {
		c.products += commitGrowthStep
		if c.products > maxCommitProducts {
			c.products = maxCommitProducts
		}
	}
}

// products reports the current multiple, for tests and telemetry.
func (c *laneCommit) current() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.products
}
