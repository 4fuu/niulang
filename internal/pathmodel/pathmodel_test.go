package pathmodel

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

	if m.Members() != 4 {
		t.Fatalf("model counts %d lanes, want 4", m.Members())
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
	if m.Members() != 2 {
		t.Fatalf("members = %d, want 2", m.Members())
	}

	m.mu.Lock()
	m.members[1].at = time.Now().Add(-2 * memberIdle)
	m.mu.Unlock()

	_, share := m.Report(2, 0.42, 5000, 1e6)
	if m.Members() != 1 {
		t.Fatalf("members = %d after one went idle, want 1", m.Members())
	}
	// The remaining lane now has the whole bottleneck rather than half.
	if share < 1.5e6 {
		t.Fatalf("share %.0f for the only live lane, want the whole aggregate", share)
	}
}

// Lanes to the same peer share; lanes to different peers must not, or one
// path's bottleneck would cap the other's.
func TestSharedPathIsPerPeer(t *testing.T) {
	a, b := Shared("peer-a"), Shared("peer-b")
	if a == b {
		t.Fatal("different peers were given the same model")
	}
	if again := Shared("peer-a"); again != a {
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
