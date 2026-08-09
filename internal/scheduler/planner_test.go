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

func TestPlannerStartsBulkWithTwoLanes(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{CurrentLanes: 1, HealthyLanes: 8, UDPHealthy: true})
	if d.TargetLanes != 2 {
		t.Fatalf("target lanes = %d, want 2", d.TargetLanes)
	}
}

func TestPlannerRetiresLaneWhenLatencyBudgetExceeded(t *testing.T) {
	p := New(DefaultConfig())
	d := p.Decide(classifier.ClassBulk, Metrics{
		CurrentLanes: 4,
		HealthyLanes: 8,
		UDPHealthy:   true,
		MarginalGain: 0.50,
		BaselineRTT:  200 * time.Millisecond,
		CurrentRTT:   300 * time.Millisecond,
	})
	if d.TargetLanes != 3 {
		t.Fatalf("target lanes = %d, want 3", d.TargetLanes)
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
