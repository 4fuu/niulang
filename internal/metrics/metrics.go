// Package metrics provides the small, dependency-free operational surface
// needed by wanoptd. It intentionally exports aggregate counters only: no
// destinations, session IDs, secrets, or application payload are retained.
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type Registry struct {
	activeFlows      atomic.Int64
	flowsStarted     atomic.Uint64
	flowsCompleted   atomic.Uint64
	flowsFailed      atomic.Uint64
	bytesUp          atomic.Uint64
	bytesDown        atomic.Uint64
	laneFailures     atomic.Uint64
	laneReplacements atomic.Uint64
	fallbacks        atomic.Uint64
	classTransitions [3]atomic.Uint64
}

type Snapshot struct {
	ActiveFlows, FlowsStarted, FlowsCompleted, FlowsFailed        int64
	BytesUp, BytesDown, LaneFailures, LaneReplacements, Fallbacks uint64
	ClassTransitions                                              [3]uint64
}

func New() *Registry { return &Registry{} }

func (r *Registry) FlowStarted() { r.activeFlows.Add(1); r.flowsStarted.Add(1) }

func (r *Registry) FlowFinished(bytesUp, bytesDown uint64, failed bool) {
	if r.activeFlows.Load() > 0 {
		r.activeFlows.Add(-1)
	}
	if failed {
		r.flowsFailed.Add(1)
	} else {
		r.flowsCompleted.Add(1)
	}
	r.bytesUp.Add(bytesUp)
	r.bytesDown.Add(bytesDown)
}

func (r *Registry) LaneFailure()     { r.laneFailures.Add(1) }
func (r *Registry) LaneReplacement() { r.laneReplacements.Add(1) }
func (r *Registry) Fallback()        { r.fallbacks.Add(1) }
func (r *Registry) ClassTransition(class int) {
	if class >= 0 && class < len(r.classTransitions) {
		r.classTransitions[class].Add(1)
	}
}

func (r *Registry) Snapshot() Snapshot {
	s := Snapshot{
		ActiveFlows: r.activeFlows.Load(), FlowsStarted: int64(r.flowsStarted.Load()),
		FlowsCompleted: int64(r.flowsCompleted.Load()), FlowsFailed: int64(r.flowsFailed.Load()),
		BytesUp: r.bytesUp.Load(), BytesDown: r.bytesDown.Load(), LaneFailures: r.laneFailures.Load(),
		LaneReplacements: r.laneReplacements.Load(), Fallbacks: r.fallbacks.Load(),
	}
	for i := range s.ClassTransitions {
		s.ClassTransitions[i] = r.classTransitions[i].Load()
	}
	return s
}

// ServeHTTP emits a stable Prometheus-compatible exposition subset. Values
// are process-wide aggregates and contain no user-controlled labels.
func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s := r.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "wanopt_active_flows %d\n", s.ActiveFlows)
	fmt.Fprintf(w, "wanopt_flows_started_total %d\n", s.FlowsStarted)
	fmt.Fprintf(w, "wanopt_flows_completed_total %d\n", s.FlowsCompleted)
	fmt.Fprintf(w, "wanopt_flows_failed_total %d\n", s.FlowsFailed)
	fmt.Fprintf(w, "wanopt_bytes_up_total %d\n", s.BytesUp)
	fmt.Fprintf(w, "wanopt_bytes_down_total %d\n", s.BytesDown)
	fmt.Fprintf(w, "wanopt_lane_failures_total %d\n", s.LaneFailures)
	fmt.Fprintf(w, "wanopt_lane_replacements_total %d\n", s.LaneReplacements)
	fmt.Fprintf(w, "wanopt_fallbacks_total %d\n", s.Fallbacks)
	for i, value := range s.ClassTransitions {
		fmt.Fprintf(w, "wanopt_class_transitions_total{class=\"%d\"} %d\n", i, value)
	}
}
