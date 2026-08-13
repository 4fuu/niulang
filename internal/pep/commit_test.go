package pep

import (
	"testing"
	"time"
)

const testRTT = 200 * time.Millisecond

// run drives the search with a bottleneck that drops whenever the commitment
// exceeds what it will absorb. The search is never told which kind of
// bottleneck it is facing; it only ever learns that something was lost.
func run(c *laneCommit, tolerated float64, rounds int) float64 {
	now := time.Now()
	for i := 0; i < rounds; i++ {
		now = now.Add(testRTT)
		c.observe(c.current() > tolerated, now, testRTT)
	}
	return c.current()
}

// A deep buffer absorbs several bandwidth-delay products and answers with
// delay, so the search should find most of them.
func TestSearchGrowsIntoADeepBuffer(t *testing.T) {
	c := newLaneCommit()
	got := run(c, 5.0, 200)
	if got < 3.5 {
		t.Fatalf("settled at %.2f products on a buffer that tolerates 5", got)
	}
}

// A token bucket absorbs about one product and answers with loss. Committing
// four to it measured 10.5 Mbit/s against two's 18.2, so the search must not
// go there.
func TestSearchStaysShallowOnAPolicer(t *testing.T) {
	c := newLaneCommit()
	got := run(c, 1.2, 200)
	if got > 2.0 {
		t.Fatalf("settled at %.2f products on a bottleneck that tolerates 1.2", got)
	}
	if got < minCommitProducts {
		t.Fatalf("settled at %.2f, below the floor a lane needs to stay busy", got)
	}
}

// The same code must reach both answers, which is the whole point: the path is
// not configuration.
func TestSearchSeparatesTheTwoPathsWithoutBeingTold(t *testing.T) {
	deep := run(newLaneCommit(), 5.0, 200)
	shallow := run(newLaneCommit(), 1.2, 200)
	if deep <= shallow*1.5 {
		t.Fatalf("deep buffer settled at %.2f and policer at %.2f: the search "+
			"cannot tell them apart", deep, shallow)
	}
}

// A path that degrades mid-transfer must be followed down.
func TestSearchBacksOffWhenAPathDegrades(t *testing.T) {
	c := newLaneCommit()
	high := run(c, 5.0, 100)
	low := run(c, 1.2, 100)
	if low >= high {
		t.Fatalf("commitment did not fall when the path degraded (%.2f -> %.2f)", high, low)
	}
	if low > 2.0 {
		t.Fatalf("settled at %.2f after degrading to a bottleneck tolerating 1.2", low)
	}
}

// A burst of loss reports from one episode must cost one backoff, not many.
func TestBackoffIsOncePerRoundTrip(t *testing.T) {
	c := newLaneCommit()
	now := time.Now()
	for i := 0; i < 40; i++ {
		c.observe(false, now, testRTT)
		now = now.Add(testRTT)
	}
	before := c.current()
	for i := 0; i < 20; i++ {
		c.observe(true, now, testRTT) // same instant: one episode
	}
	after := c.current()
	if after < before*commitBackoff*0.9 {
		t.Fatalf("one loss episode took the commitment from %.2f to %.2f", before, after)
	}
}
