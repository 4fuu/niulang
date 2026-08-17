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
	m.Report(1, 0.42, 5000, 1e6, 0)
	m.Report(2, 0.42, 5000, 1e6, 0)
	m.Report(3, 0.42, 5000, 1e6, 0)
	floor := m.Report(4, 0.05, 20, 1e6, 0).Floor

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
	m.Report(1, 0.42, 5000, perLane, 0)
	m.Report(2, 0.42, 5000, perLane, 0)
	m.Report(3, 0.42, 5000, perLane, 0)
	share := m.Report(4, 0.42, 5000, perLane, 0).Share

	if m.Members() != 4 {
		t.Fatalf("model counts %d lanes, want 4", m.Members())
	}
	// The aggregate is 4 MB/s over four lanes, and each share carries the
	// probe gain, because a cap with no headroom is a cap computed from its
	// own effect.
	want := shareProbeGain * perLane
	if math.Abs(share-want) > want*0.05 {
		t.Fatalf("share %.0f of a 4 MB/s aggregate over 4 lanes, want about %.0f", share, want)
	}
	// Four lanes each capped at their share put on the wire what one BBR
	// sender probing on its own would have.
	if total := share * 4; math.Abs(total-shareProbeGain*4*perLane) > perLane*0.2 {
		t.Fatalf("four shares total %.0f, want %.0f", total, shareProbeGain*4*perLane)
	}
}

// A lane on its own must not be capped by a bottleneck nobody has measured.
func TestAnUnknownBottleneckDoesNotCap(t *testing.T) {
	m := NewPathModel()
	if share := m.Report(1, 0, 0, 0, 0).Share; share != 0 {
		t.Fatalf("share %.0f before any delivered rate was reported, want 0 for no cap", share)
	}
}

// A lane that stops sending stops counting, or its share is held against the
// lanes that are still working. There is no close hook on a congestion
// controller, so membership has to expire.
func TestAnIdleLaneStopsCounting(t *testing.T) {
	m := NewPathModel()
	m.Report(1, 0.42, 5000, 1e6, 0)
	m.Report(2, 0.42, 5000, 1e6, 0)
	if m.Members() != 2 {
		t.Fatalf("members = %d, want 2", m.Members())
	}

	m.mu.Lock()
	m.members[1].at = time.Now().Add(-2 * memberIdle)
	m.mu.Unlock()

	state := m.Report(2, 0.42, 5000, 1e6, 0)
	if m.Members() != 1 {
		t.Fatalf("members = %d after one went idle, want 1", m.Members())
	}
	// The remaining lane has the whole bottleneck rather than half, and being
	// alone it is no longer capped at all.
	if state.Seed < 1.5e6 {
		t.Fatalf("seed %.0f for the only live lane, want the whole aggregate", state.Seed)
	}
	if state.Share != 0 {
		t.Fatalf("share %.0f for a lane that is now alone, want no cap", state.Share)
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
	state := m.Report(1, 0.42, 5000, rate, 0)
	// Not capped at its own rate: not capped at all. A lone lane has nothing
	// to compound with, and a ceiling equal to what it has already delivered
	// is one it can never probe past.
	if state.Share != 0 {
		t.Fatalf("a lone lane was capped at %.0f, want no cap", state.Share)
	}
	// It still learns the path, so a lane replacing it can start from what it
	// measured rather than from the initial window.
	if state.Seed < rate*0.95 {
		t.Fatalf("seed %.0f from a lane delivering %.0f, want about the same", state.Seed, rate)
	}
}

// The share is measured from what the members deliver, and the members deliver
// what the share allows. Without headroom that is a loop whose only stable
// point is the rate it started at, and whose every disturbance moves it down:
// one application-limited interval lowers the windowed maximum, which lowers
// the share, which lowers what can be delivered, and nothing ever measures the
// path upward again.
//
// Live, that was a transport that ran at 143 Mbit/s for six 20-second windows
// and then moved no bytes at all until its process was restarted
// (docs/MEASUREMENTS-20260816.md). On the emulator it is one trial at 37
// Mbit/s followed by four at 0.8.
func TestTheShareDoesNotRatchetDown(t *testing.T) {
	const capacity = 8e6 // what the path would actually carry, bytes/second
	for _, lanes := range []int{1, 2, 4} {
		t.Run("", func(t *testing.T) {
			m := NewPathModel()
			// Each lane delivers what its own cap allows, which is the loop
			// the real controller runs: bandwidth() takes the smaller of its
			// estimate and its share.
			delivered := make([]float64, lanes)
			for i := range delivered {
				delivered[i] = capacity / float64(lanes)
			}
			// One application-limited interval: every lane briefly delivers a
			// tenth of what it could. This is a gap between requests, not a
			// property of the path.
			for i := range delivered {
				delivered[i] /= 10
			}
			var share float64
			for round := 0; round < 40; round++ {
				for i := 0; i < lanes; i++ {
					share = m.Report(Member(i+1), 0.1, 5000, delivered[i], 0).Share
					// The next round delivers what this one is allowed to,
					// bounded by the path itself.
					next := capacity / float64(lanes)
					if share > 0 && share < next {
						next = share
					}
					delivered[i] = next
				}
			}
			total := 0.0
			for _, d := range delivered {
				total += d
			}
			if total < capacity*0.9 {
				t.Fatalf("%d lane(s) settled at %.0f B/s after one idle interval, want the path's %.0f",
					lanes, total, capacity)
			}
		})
	}
}
