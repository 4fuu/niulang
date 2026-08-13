package pep

import (
	"testing"
	"time"
)

const testRTT = 200 * time.Millisecond

// bottleneck models what a path does when the sender commits more. `tolerated`
// is how many bandwidth-delay products it absorbs before dropping; `ambient` is
// loss it inflicts regardless, as a fraction of bytes.
//
// The distinction is the whole problem: a lossy link has ambient loss and
// infinite tolerance, a policer has a low tolerance and no ambient loss, and
// the level of loss alone cannot tell them apart.
func drive(c *laneCommit, tolerated, ambient float64, rounds int) float64 {
	now := time.Now()
	var sent, lost uint64
	const perRound = 250_000
	for i := 0; i < rounds; i++ {
		now = now.Add(testRTT)
		sent += perRound
		lost += uint64(ambient * perRound / 1200)
		if c.level() > tolerated {
			// Over-commitment is answered with drops proportional to the
			// excess, which is what a token bucket does.
			lost += uint64((c.level() - tolerated) * perRound / 1200 * 0.5)
		}
		c.observe(sent, lost, now, testRTT)
	}
	return c.level()
}

// The defect this replaces: with 1% ambient loss every sample reports a loss,
// and a search that retreats on any loss retreats to the floor. That cost four
// lanes their aggregation entirely, 18.3 Mbit/s against a fixed setting's 26.9.
func TestAmbientLossDoesNotDriveTheSearchDown(t *testing.T) {
	c := newLaneCommitCeiling(6.0)
	got := drive(c, 6.0, 0.01, 300)
	if got < initialCommitProducts {
		t.Fatalf("settled at %.2f products, below the %.2f it started from: "+
			"ambient loss is being read as over-commitment", got, initialCommitProducts)
	}
}

// A policer answers extra commitment with drops, so the search must stop near
// what it absorbs.
func TestSearchStopsWhereLossRespondsToCommitment(t *testing.T) {
	c := newLaneCommitCeiling(6.0)
	got := drive(c, 2.0, 0.0, 300)
	if got > 3.5 {
		t.Fatalf("settled at %.2f products on a bottleneck absorbing 2", got)
	}
}

// The hard case, and the one the mechanism does not yet solve: a path that is
// both lossy and shallow. Comparing loss rates was supposed to cancel the
// ambient part and leave the part that responds to commitment. It does not,
// because the response is small beside the ambient level -- and this is almost
// certainly why the live measurement behaves as it does, where four lanes over
// a policed path with 1% loss measured 16.4 to 18.4 Mbit/s with the search free
// against 26.9 pinned.
//
// Recorded as a test asserting the limitation rather than deleted, so that
// enabling upward exploration has to confront it.
func TestSearchCannotYetSeparateAmbientLossFromOverCommitment(t *testing.T) {
	lossyDeep := drive(newLaneCommitCeiling(6.0), 6.0, 0.01, 300)
	lossyShallow := drive(newLaneCommitCeiling(6.0), 2.0, 0.01, 300)
	if lossyDeep > lossyShallow*1.2 {
		t.Fatalf("deep settled at %.2f and shallow at %.2f: the search now "+
			"separates them under ambient loss, so this limitation is fixed and "+
			"the live signal should be retested with exploration enabled",
			lossyDeep, lossyShallow)
	}
}

// A clean deep buffer should be exploited.
func TestSearchGrowsOnACleanDeepPath(t *testing.T) {
	got := drive(newLaneCommitCeiling(6.0), 6.0, 0.0, 300)
	if got < 3.0 {
		t.Fatalf("settled at %.2f products on a clean path absorbing 6", got)
	}
}

// The shipped configuration pins the search: the ceiling equals the floor,
// because the live loss signal does not yet drive it the way the model does.
// This records that as a fact rather than leaving the tests above to imply the
// mechanism is active.
func TestShippedConfigurationPinsTheSearch(t *testing.T) {
	if maxCommitProducts != minCommitProducts {
		t.Skip("upward exploration has been enabled; this test documented it being off")
	}
	got := drive(newLaneCommit(), 6.0, 0.0, 300)
	if got != minCommitProducts {
		t.Fatalf("search moved to %.2f with the ceiling pinned to the floor", got)
	}
}

// The floor must hold: a lane still has to be able to keep itself busy.
func TestSearchNeverFallsBelowTheFloor(t *testing.T) {
	got := drive(newLaneCommitCeiling(6.0), 0.1, 0.05, 300)
	if got < minCommitProducts {
		t.Fatalf("settled at %.2f, below the floor", got)
	}
}
