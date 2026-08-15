package scheduler

import (
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/classifier"
)

func TestPlannerProtectsInteractiveFlows(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassInteractive, Metrics{CurrentLanes: 4, HealthyLanes: 4})
	if d.TargetLanes != 1 {
		t.Fatalf("target lanes = %d, want 1", d.TargetLanes)
	}
}

func TestPlannerStartsBulkWithConfiguredLanes(t *testing.T) {
	p := New(Config{MaxLanes: 8, InteractiveLanes: 1, BulkStartLanes: 2, MinimumMarginalGain: 0.10, InteractiveRTTBudget: 40 * time.Millisecond})
	d := p.Decide(classifier.ClassBulk, Metrics{CurrentLanes: 1, HealthyLanes: 8, UDPHealthy: true})
	if d.TargetLanes != 2 {
		t.Fatalf("target lanes = %d, want 2", d.TargetLanes)
	}
}

// A bulk transfer fills the bottleneck queue and inflates its own RTT; that is
// the transport working, not a reason to retire a lane. Charging it the
// interactive latency budget made striping unreachable on every path with a
// buffer, because a 200ms path reaches 240ms within a second of the transfer
// starting.
func TestPlannerHoldsLanesUnderSelfInflictedQueueing(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{
		CurrentLanes: 4,
		HealthyLanes: 8,
		UDPHealthy:   true,
		BaselineRTT:  200 * time.Millisecond,
		CurrentRTT:   380 * time.Millisecond, // standing queue at a full bottleneck
	})
	if d.TargetLanes != 4 {
		t.Fatalf("target lanes = %d, want 4 held: %s", d.TargetLanes, d.Reason)
	}
}

// Collapse is still worth reacting to: delay several times the baseline means
// the queue is growing faster than it drains.
func TestPlannerRetiresLaneOnRTTCollapse(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{
		CurrentLanes: 4,
		HealthyLanes: 8,
		UDPHealthy:   true,
		MarginalGain: 0.50,
		BaselineRTT:  200 * time.Millisecond,
		CurrentRTT:   900 * time.Millisecond,
	})
	if d.TargetLanes != 3 {
		t.Fatalf("target lanes = %d, want 3: %s", d.TargetLanes, d.Reason)
	}
}

func TestPlannerRetiresLaneWhenMarginalGainIsNegative(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{
		CurrentLanes: 4, HealthyLanes: 4, AvailableLanes: 8,
		UDPHealthy: true, MarginalGain: -0.20,
		BaselineRTT: 200 * time.Millisecond, CurrentRTT: 205 * time.Millisecond,
	})
	if d.TargetLanes != 3 {
		t.Fatalf("target lanes = %d, want 3", d.TargetLanes)
	}
}

func TestPlannerHoldsOneLaneWhenLatencyBudgetExceeded(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{
		CurrentLanes: 1, HealthyLanes: 1, AvailableLanes: 8, UDPHealthy: true,
		BaselineRTT: 200 * time.Millisecond, CurrentRTT: 300 * time.Millisecond,
	})
	if d.TargetLanes != 1 {
		t.Fatalf("target lanes = %d, want 1", d.TargetLanes)
	}
}

func TestPlannerFallsBackToOneLaneWhenUDPUnhealthy(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{CurrentLanes: 4, HealthyLanes: 8, UDPHealthy: false})
	if d.TargetLanes != 1 {
		t.Fatalf("target lanes = %d, want 1", d.TargetLanes)
	}
}

// A probe that clears the gain bar has measured that this path rewards
// striping, so the next step is a doubling rather than another five-second
// experiment for one more lane. The first probe, which has nothing to compare
// against yet, still adds exactly one.
func TestConfirmedGainDoublesTheLaneTarget(t *testing.T) {
	p := New(Config{MaxLanes: 8, InteractiveLanes: 1, BulkStartLanes: 1, MinimumMarginalGain: 0.15})
	base := Metrics{CurrentLanes: 1, HealthyLanes: 1, AvailableLanes: 8, ProbeReady: true, UDPHealthy: true}

	speculative := p.Decide(classifier.ClassBulk, base)
	if speculative.TargetLanes != 2 {
		t.Fatalf("first probe asked for %d lanes, want 2", speculative.TargetLanes)
	}

	confirmed := base
	confirmed.CurrentLanes, confirmed.HealthyLanes, confirmed.MarginalGain = 2, 2, 0.4
	if got := p.Decide(classifier.ClassBulk, confirmed).TargetLanes; got != 4 {
		t.Fatalf("a probe measuring 40%% gain asked for %d lanes, want 4", got)
	}

	weak := base
	weak.CurrentLanes, weak.HealthyLanes, weak.MarginalGain = 2, 2, 0.05
	if got := p.Decide(classifier.ClassBulk, weak).TargetLanes; got != 3 {
		t.Fatalf("a probe below the gain bar asked for %d lanes, want 3", got)
	}

	capped := base
	capped.CurrentLanes, capped.HealthyLanes, capped.MarginalGain = 6, 6, 0.4
	if got := p.Decide(classifier.ClassBulk, capped).TargetLanes; got != 8 {
		t.Fatalf("doubling past the ceiling asked for %d lanes, want 8", got)
	}
}

// Lanes are a measurement, not a setting. The maximum is a ceiling on what
// evidence may buy, and evidence is what has to buy it: on a path whose
// bottleneck is shared -- which is what the open-loop probe found the
// China-US link to be -- a second lane divides one capacity and adds its own
// overhead, and four lanes measured 8 Mbit/s live where one measured 10.
//
// So a bulk flow that has measured nothing must stay where it started, however
// high the ceiling is set.
func TestLanesAreNotAddedWithoutMeasuredGain(t *testing.T) {
	planner := New(Config{MaxLanes: 8, BulkStartLanes: 1, InteractiveLanes: 1})
	steady := Metrics{
		CurrentLanes: 1, HealthyLanes: 1, AvailableLanes: 8, UDPHealthy: true,
		BaselineRTT: 300 * time.Millisecond, CurrentRTT: 320 * time.Millisecond,
		// Nothing has been measured: no probe has completed, so there is no
		// verdict either way.
		ProbeReady: false,
	}
	for round := 0; round < 5; round++ {
		decision := planner.Decide(classifier.ClassBulk, steady)
		if decision.TargetLanes != 1 {
			t.Fatalf("round %d: target %d lanes with a ceiling of 8 and no measured gain, want 1 (%s)",
				round, decision.TargetLanes, decision.Reason)
		}
		steady.CurrentLanes = decision.TargetLanes
	}
}

// And a path that does reward striping must get it, or the ceiling is the only
// thing deciding and the measurement is decoration.
func TestLanesAreAddedWhenGainIsMeasured(t *testing.T) {
	planner := New(Config{MaxLanes: 8, BulkStartLanes: 1, InteractiveLanes: 1})
	rewarding := Metrics{
		CurrentLanes: 1, HealthyLanes: 1, AvailableLanes: 8, UDPHealthy: true,
		BaselineRTT: 300 * time.Millisecond, CurrentRTT: 310 * time.Millisecond,
		ProbeReady: true, MarginalGain: 0.9,
	}
	decision := planner.Decide(classifier.ClassBulk, rewarding)
	if decision.TargetLanes <= 1 {
		t.Fatalf("target %d lanes on a path where a probe measured a 90%% gain (%s)",
			decision.TargetLanes, decision.Reason)
	}
}

// A path that has already answered must not be asked again.
//
// The first probe on a path is speculative -- there is nothing to compare
// until a second lane exists -- so on a path whose bottleneck is shared the
// flow would grow, measure that it did not help, retire, and grow again, every
// few seconds forever. The answer belongs to the path, and once it is known
// the search stops.
func TestAPathThatRefusesStripingIsNotProbedAgain(t *testing.T) {
	planner := New(Config{MaxLanes: 8, BulkStartLanes: 1, InteractiveLanes: 1})
	answered := Metrics{
		CurrentLanes: 2, HealthyLanes: 2, AvailableLanes: 8, UDPHealthy: true,
		BaselineRTT: 300 * time.Millisecond, CurrentRTT: 310 * time.Millisecond,
		// A probe is ready and would otherwise buy a lane.
		ProbeReady: true, MarginalGain: 0.5,
		// But this path has already been measured not to reward them.
		PathRefusesStriping: true,
	}
	decision := planner.Decide(classifier.ClassBulk, answered)
	if decision.TargetLanes != 1 {
		t.Fatalf("target %d lanes on a path measured not to reward striping, want 1 (%s)",
			decision.TargetLanes, decision.Reason)
	}
}
