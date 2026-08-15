package congestion

import (
	"math"
	"testing"
	"time"
)

// The floor is pooled so that four lanes reach a usable estimate on four
// lanes' worth of samples rather than each waiting for its own, and so that
// they agree. Lanes that disagree compensate differently and the aggregate
// stops being predictable.
func TestTheFloorIsPooledAcrossLanes(t *testing.T) {
	m := NewPathModel()
	// Three lanes with plenty of samples agree on 0.42; a fourth has just
	// started and has seen almost nothing.
	m.Report(1, 0.42, 5000, 1e6)
	m.Report(2, 0.42, 5000, 1e6)
	m.Report(3, 0.42, 5000, 1e6)
	floor, _ := m.Report(4, 0.05, 20, 1e6)

	if math.Abs(floor-0.42) > 0.01 {
		t.Fatalf("pooled floor %.3f, want the weight of the measured lanes at about 0.42", floor)
	}
	// And the new lane is handed that estimate rather than its own, so it does
	// not have to rediscover the path.
	if floor < 0.4 {
		t.Fatalf("a joining lane was left with its own uninformed floor of %.3f", floor)
	}
}

// The share is what stops lanes compounding: each is capped at the endpoint
// pair's bottleneck divided by the number of lanes, so the aggregate on the
// wire is what one sender would have put there.
func TestTheShareDividesTheBottleneck(t *testing.T) {
	m := NewPathModel()
	const perLane = 1e6 // bytes per second
	m.Report(1, 0.42, 5000, perLane)
	m.Report(2, 0.42, 5000, perLane)
	m.Report(3, 0.42, 5000, perLane)
	_, share := m.Report(4, 0.42, 5000, perLane)

	if m.Lanes() != 4 {
		t.Fatalf("model counts %d lanes, want 4", m.Lanes())
	}
	// The aggregate is 4 MB/s, so each lane's share is 1 MB/s.
	if math.Abs(share-perLane) > perLane*0.05 {
		t.Fatalf("share %.0f of a 4 MB/s aggregate over 4 lanes, want about %.0f", share, perLane)
	}
	// Four lanes each capped at their share put the same total on the wire as
	// one lane would have.
	if total := share * 4; math.Abs(total-4*perLane) > perLane*0.2 {
		t.Fatalf("four shares total %.0f, want the aggregate %.0f", total, 4*perLane)
	}
}

// A lane on its own must not be capped by a bottleneck nobody has measured.
func TestAnUnknownBottleneckDoesNotCap(t *testing.T) {
	m := NewPathModel()
	if _, share := m.Report(1, 0, 0, 0); share != 0 {
		t.Fatalf("share %.0f before any delivered rate was reported, want 0 for no cap", share)
	}
}

// A lane that stops sending stops counting, or its share is held against the
// lanes that are still working. There is no close hook on a congestion
// controller, so membership has to expire.
func TestAnIdleLaneStopsCounting(t *testing.T) {
	m := NewPathModel()
	m.Report(1, 0.42, 5000, 1e6)
	m.Report(2, 0.42, 5000, 1e6)
	if m.Lanes() != 2 {
		t.Fatalf("lanes = %d, want 2", m.Lanes())
	}

	m.mu.Lock()
	m.lanes[1].at = time.Now().Add(-2 * laneIdle)
	m.mu.Unlock()

	_, share := m.Report(2, 0.42, 5000, 1e6)
	if m.Lanes() != 1 {
		t.Fatalf("lanes = %d after one went idle, want 1", m.Lanes())
	}
	// The remaining lane now has the whole bottleneck rather than half.
	if share < 1.5e6 {
		t.Fatalf("share %.0f for the only live lane, want the whole aggregate", share)
	}
}

// Lanes to the same peer share; lanes to different peers must not, or one
// path's bottleneck would cap the other's.
func TestSharedPathIsPerPeer(t *testing.T) {
	a, b := SharedPath("peer-a"), SharedPath("peer-b")
	if a == b {
		t.Fatal("different peers were given the same model")
	}
	if again := SharedPath("peer-a"); again != a {
		t.Fatal("the same peer was given two models")
	}
}

// A shared controller must still work when it is the only lane, because that
// is the common case and the one measured fastest live.
func TestASingleSharedLaneIsNotPenalised(t *testing.T) {
	m := NewPathModel()
	const rate = 2e6
	_, share := m.Report(1, 0.42, 5000, rate)
	if share < rate*0.95 {
		t.Fatalf("a lone lane was capped at %.0f of its own %.0f", share, rate)
	}
}

// A lane joining a measured path must start where its siblings are, not at the
// initial window. On this channel the ramp is the expensive part, and a lane
// opened to replace one that died would otherwise repeat it every time.
func TestAJoiningLaneStartsFromWhatIsAlreadyKnown(t *testing.T) {
	m := NewPathModel()
	const perLane = 2e6
	m.Report(1, 0.42, 5000, perLane)
	m.Report(2, 0.42, 5000, perLane)

	floor, share := m.Current()
	if math.Abs(floor-0.42) > 0.01 {
		t.Fatalf("a joining lane is offered floor %.3f, want the measured 0.42", floor)
	}
	// Two lanes hold 4 MB/s between them; a third joining makes three shares.
	if want := 4 * perLane / 2 / 3; math.Abs(share-want) > want*0.1 {
		t.Fatalf("joining share %.0f, want about %.0f", share, want)
	}

	seeded := NewErasureSenderOn(1200, m)
	if seeded.Share() <= 0 {
		t.Fatal("a lane joining a measured path was given no share")
	}
	// And its pacer starts at that share rather than at the initial window.
	fresh := NewErasureSender(1200)
	if seeded.bandwidth() <= fresh.bandwidth() {
		t.Fatalf("seeded lane starts at %d, no better than an unseeded %d",
			seeded.bandwidth(), fresh.bandwidth())
	}
}
