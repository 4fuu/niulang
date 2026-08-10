// Package scheduler contains lane-allocation policy. Transport-specific
// congestion control remains below this layer.
package scheduler

import (
	"time"

	"github.com/icourses-dev/wanopt/internal/classifier"
)

type Config struct {
	MaxLanes             int
	InteractiveLanes     int
	BulkStartLanes       int
	MinimumMarginalGain  float64
	InteractiveRTTBudget time.Duration
}

func DefaultConfig() Config {
	return Config{
		MaxLanes:         8,
		InteractiveLanes: 1,
		// Start with one lane and let the adaptive manager add a lane only
		// after it has a measured marginal-gain sample. Immediate striping is
		// unsafe on paths where independent QUIC controllers amplify loss.
		BulkStartLanes:       1,
		MinimumMarginalGain:  0.10,
		InteractiveRTTBudget: 40 * time.Millisecond,
	}
}

type Metrics struct {
	CurrentLanes int
	HealthyLanes int
	// AvailableLanes is the number of additional lanes the transport policy
	// is willing to create. HealthyLanes remains the number currently usable;
	// separating them prevents the bulk bootstrap decision from getting stuck
	// at one lane.
	AvailableLanes int
	MarginalGain   float64
	BaselineRTT    time.Duration
	CurrentRTT     time.Duration
	UDPHealthy     bool
}

type Decision struct {
	TargetLanes int
	Class       classifier.Class
	Reason      string
}

type Planner struct{ cfg Config }

func New(cfg Config) *Planner {
	if cfg.MaxLanes < 1 || cfg.InteractiveLanes < 1 || cfg.BulkStartLanes < 1 {
		cfg = DefaultConfig()
	}
	if cfg.BulkStartLanes > cfg.MaxLanes {
		cfg.BulkStartLanes = cfg.MaxLanes
	}
	return &Planner{cfg: cfg}
}

func (p *Planner) Decide(class classifier.Class, m Metrics) Decision {
	current := m.CurrentLanes
	if current < 1 {
		current = 1
	}
	switch class {
	case classifier.ClassNew:
		return Decision{TargetLanes: 1, Class: class, Reason: "new-flow latency budget"}
	case classifier.ClassInteractive:
		target := p.cfg.InteractiveLanes
		if target > m.HealthyLanes && m.HealthyLanes > 0 {
			target = m.HealthyLanes
		}
		return Decision{TargetLanes: maxInt(1, target), Class: class, Reason: "protect interactive RTT"}
	case classifier.ClassBulk:
		potential := m.HealthyLanes
		if m.AvailableLanes > potential {
			potential = m.AvailableLanes
		}
		if !m.UDPHealthy {
			return Decision{TargetLanes: 1, Class: class, Reason: "bulk start constrained by healthy transport"}
		}
		if m.BaselineRTT > 0 && m.CurrentRTT > m.BaselineRTT+p.cfg.InteractiveRTTBudget {
			return Decision{TargetLanes: maxInt(1, current-1), Class: class, Reason: "retire lane: RTT budget exceeded"}
		}
		// A probe that reduces aggregate goodput is evidence that the path or
		// receiver is being harmed by striping. Retire one lane immediately;
		// holding the current target here would turn a temporary experiment into
		// a persistent single-flow performance regression.
		if current > 1 && m.MarginalGain < 0 {
			return Decision{TargetLanes: current - 1, Class: class, Reason: "retire lane: negative marginal gain"}
		}
		if potential < p.cfg.BulkStartLanes {
			return Decision{TargetLanes: 1, Class: class, Reason: "bulk start constrained by healthy transport"}
		}
		if current < p.cfg.BulkStartLanes {
			return Decision{TargetLanes: p.cfg.BulkStartLanes, Class: class, Reason: "bulk startup lanes"}
		}
		if current < p.cfg.MaxLanes && m.MarginalGain >= p.cfg.MinimumMarginalGain &&
			(m.BaselineRTT <= 0 || m.CurrentRTT <= m.BaselineRTT+p.cfg.InteractiveRTTBudget) {
			return Decision{TargetLanes: current + 1, Class: class, Reason: "marginal throughput gain within RTT budget"}
		}
		return Decision{TargetLanes: current, Class: class, Reason: "hold lanes: gain or latency guardrail"}
	default:
		return Decision{TargetLanes: 1, Class: classifier.ClassNew, Reason: "unknown class"}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
