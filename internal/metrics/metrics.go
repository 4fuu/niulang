// Package metrics provides the small, dependency-free operational surface
// needed by wanoptd. It intentionally exports aggregate counters only: no
// destinations, session IDs, secrets, or application payload are retained.
package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
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
	telemetryMu      sync.Mutex
	quicFlows        map[uint64]QUICObservation
}

type Snapshot struct {
	ActiveFlows, FlowsStarted, FlowsCompleted, FlowsFailed           int64
	BytesUp, BytesDown, LaneFailures, LaneReplacements, Fallbacks    uint64
	ClassTransitions                                                 [3]uint64
	QUICLanes                                                        int64
	QUICLatestRTT, QUICSmoothedRTT                                   time.Duration
	QUICBytesSent, QUICBytesReceived, QUICBytesLost, QUICPacketsLost uint64
}

// QUICObservation is a point-in-time aggregate over the lanes of one logical
// flow.  RTT is represented as a maximum so an operator can see the worst
// active lane without any user-controlled labels.  Byte and loss values are
// the sum of the QUIC connection counters at that point.
type QUICObservation struct {
	Lanes         int
	LatestRTT     time.Duration
	SmoothedRTT   time.Duration
	BytesSent     uint64
	BytesReceived uint64
	BytesLost     uint64
	PacketsLost   uint64
}

func New() *Registry { return &Registry{quicFlows: make(map[uint64]QUICObservation)} }

func (r *Registry) FlowStarted() { r.activeFlows.Add(1); r.flowsStarted.Add(1) }

func (r *Registry) FlowFinished(bytesUp, bytesDown uint64, failed bool) {
	// A flow can be torn down concurrently by the accept-limit, context
	// cancellation, and transport-failure paths.  A Load followed by Add is
	// not atomic as a pair and could make the exported gauge negative if two
	// teardown paths race.  Decrement with CAS and clamp at zero instead.
	for {
		active := r.activeFlows.Load()
		if active <= 0 || r.activeFlows.CompareAndSwap(active, active-1) {
			break
		}
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

func (r *Registry) ObserveQUIC(key uint64, o QUICObservation) {
	if key == 0 {
		return
	}
	if o.Lanes < 0 {
		o.Lanes = 0
	}
	if o.LatestRTT < 0 {
		o.LatestRTT = 0
	}
	if o.SmoothedRTT < 0 {
		o.SmoothedRTT = 0
	}
	r.telemetryMu.Lock()
	if r.quicFlows == nil {
		r.quicFlows = make(map[uint64]QUICObservation)
	}
	r.quicFlows[key] = o
	r.telemetryMu.Unlock()
}

func (r *Registry) RemoveQUIC(key uint64) {
	if key == 0 {
		return
	}
	r.telemetryMu.Lock()
	delete(r.quicFlows, key)
	r.telemetryMu.Unlock()
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
	r.telemetryMu.Lock()
	var quicLanes int64
	var latestRTT, smoothedRTT time.Duration
	var bytesSent, bytesReceived, bytesLost, packetsLost uint64
	for _, o := range r.quicFlows {
		quicLanes += int64(o.Lanes)
		if o.LatestRTT > latestRTT {
			latestRTT = o.LatestRTT
		}
		if o.SmoothedRTT > smoothedRTT {
			smoothedRTT = o.SmoothedRTT
		}
		bytesSent += o.BytesSent
		bytesReceived += o.BytesReceived
		bytesLost += o.BytesLost
		packetsLost += o.PacketsLost
	}
	r.telemetryMu.Unlock()
	s.QUICLanes = quicLanes
	s.QUICLatestRTT = latestRTT
	s.QUICSmoothedRTT = smoothedRTT
	s.QUICBytesSent = bytesSent
	s.QUICBytesReceived = bytesReceived
	s.QUICBytesLost = bytesLost
	s.QUICPacketsLost = packetsLost
	return s
}

// ServeHTTP emits a stable Prometheus-compatible exposition subset. Values
// are process-wide aggregates and contain no user-controlled labels.
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodHead)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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
	fmt.Fprintf(w, "wanopt_quic_lanes %d\n", s.QUICLanes)
	fmt.Fprintf(w, "wanopt_quic_latest_rtt_seconds %.9f\n", s.QUICLatestRTT.Seconds())
	fmt.Fprintf(w, "wanopt_quic_smoothed_rtt_seconds %.9f\n", s.QUICSmoothedRTT.Seconds())
	fmt.Fprintf(w, "wanopt_quic_bytes_sent %d\n", s.QUICBytesSent)
	fmt.Fprintf(w, "wanopt_quic_bytes_received %d\n", s.QUICBytesReceived)
	fmt.Fprintf(w, "wanopt_quic_bytes_lost %d\n", s.QUICBytesLost)
	fmt.Fprintf(w, "wanopt_quic_packets_lost %d\n", s.QUICPacketsLost)
	for i, value := range s.ClassTransitions {
		fmt.Fprintf(w, "wanopt_class_transitions_total{class=\"%d\"} %d\n", i, value)
	}
}
