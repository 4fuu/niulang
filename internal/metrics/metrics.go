// Package metrics provides the small, dependency-free operational surface
// needed by queqiaod. It intentionally exports aggregate counters only: no
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
	// udpPathUnavailable counts comparative failures: QUIC either failed or
	// did not authenticate before TLS/TCP reached the same configured endpoint.
	// It is deliberately separate from fallbacks, which also includes flows
	// sent directly to TCP while UDP is in cooldown.
	udpPathUnavailable atomic.Uint64
	// endpointRaceFailures counts AUTO transport races in which neither QUIC
	// nor TLS/TCP reached the configured endpoint. Keeping this separate from
	// UDP failures tells an operator whether TCP was a usable (but degraded)
	// escape path or whether the endpoint was unreachable on both transports.
	endpointRaceFailures atomic.Uint64
	udpReconnects        atomic.Uint64
	udpRescueFailures    atomic.Uint64
	completionTimeouts   atomic.Uint64
	flowTimeouts         atomic.Uint64
	classTransitions     [3]atomic.Uint64
	// The rescue window is dropped rather than allowed to throttle the
	// application. That trade has to be visible: a flow that has evicted part
	// of its window will fail rather than recover if its lane dies, so a
	// rising count is the operator's warning that lane rescue is no longer
	// available for the affected traffic.
	replayBytesInUse atomic.Int64
	// bulkIsolations counts bulk flows moved off the shared control
	// connection, which is the mechanism that protects interactive latency.
	bulkIsolations atomic.Uint64
	// reinjections counts frames re-sent on a second lane because the first
	// was holding up the receiver. A rising count means striping is costing
	// duplicate capacity to keep the reorder span bounded.
	reinjections atomic.Uint64
	telemetryMu  sync.Mutex
	quicFlows    map[uint64]QUICObservation
}

type Snapshot struct {
	ActiveFlows, FlowsStarted, FlowsCompleted, FlowsFailed           int64
	BytesUp, BytesDown, LaneFailures, LaneReplacements, Fallbacks    uint64
	UDPPathUnavailable, EndpointTransportRaceFailures                uint64
	UDPAssociationReconnects, UDPAssociationRescueFailures           uint64
	CompletionTimeouts                                               uint64
	FlowTimeouts                                                     uint64
	ClassTransitions                                                 [3]uint64
	BulkIsolations, Reinjections                                     uint64
	ReplayBytesInUse                                                 int64
	QUICLanes                                                        int64
	QUICLatestRTT, QUICSmoothedRTT                                   time.Duration
	QUICBytesSent, QUICBytesReceived, QUICBytesLost, QUICPacketsLost uint64
	QUICControllerKind                                               string
	QUICControllerMode                                               uint32
	QUICControllerMaxBandwidth, QUICControllerPacingRate             uint64
	QUICControllerLatestSample, QUICControllerSamples                uint64
	QUICControllerLatestAckRate, QUICControllerLatestSendRate        uint64
	QUICControllerNonAppSamples, QUICControllerAppSamples            uint64
	QUICControllerStateMisses, QUICControllerZeroSamples             uint64
	QUICControllerRound                                              uint64
	QUICControllerCongestionWindow, QUICControllerBytesInFlight      uint64
	QUICControllerBytesLost, QUICControllerPacketsLost               uint64
	QUICControllerMinRTT                                             time.Duration
	QUICControllerInRecovery                                         bool
}

// QUICObservation is a point-in-time aggregate over the lanes of one logical
// flow.  RTT is represented as a maximum so an operator can see the worst
// active lane without any user-controlled labels.  Byte and loss values are
// the sum of the QUIC connection counters at that point.
type QUICObservation struct {
	Lanes                      int
	LatestRTT                  time.Duration
	SmoothedRTT                time.Duration
	BytesSent                  uint64
	BytesReceived              uint64
	BytesLost                  uint64
	PacketsLost                uint64
	ControllerKind             string
	ControllerMode             uint32
	ControllerMaxBandwidth     uint64
	ControllerLatestSample     uint64
	ControllerLatestAckRate    uint64
	ControllerLatestSendRate   uint64
	ControllerSamples          uint64
	ControllerNonAppSamples    uint64
	ControllerAppSamples       uint64
	ControllerStateMisses      uint64
	ControllerZeroSamples      uint64
	ControllerRound            uint64
	ControllerPacingRate       uint64
	ControllerCongestionWindow uint64
	ControllerBytesInFlight    uint64
	ControllerBytesLost        uint64
	ControllerPacketsLost      uint64
	ControllerMinRTT           time.Duration
	ControllerInRecovery       bool
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
func (r *Registry) UDPPathUnavailable() {
	r.udpPathUnavailable.Add(1)
}
func (r *Registry) EndpointTransportRaceFailure() {
	r.endpointRaceFailures.Add(1)
}
func (r *Registry) UDPAssociationReconnect() {
	r.udpReconnects.Add(1)
}
func (r *Registry) UDPAssociationRescueFailure() {
	r.udpRescueFailures.Add(1)
}

// ReplayBytes tracks the endpoint's accounted rescue-window memory.
func (r *Registry) ReplayBytes(delta int64) {
	if r == nil || delta == 0 {
		return
	}
	if remaining := r.replayBytesInUse.Add(delta); remaining < 0 {
		r.replayBytesInUse.Store(0)
	}
}

// Reinjected records a frame re-sent on a second lane to unblock the receiver.
func (r *Registry) Reinjected() {
	if r == nil {
		return
	}
	r.reinjections.Add(1)
}

// BulkIsolated records a bulk flow moving off the shared control connection.
func (r *Registry) BulkIsolated() {
	if r == nil {
		return
	}
	r.bulkIsolations.Add(1)
}

func (r *Registry) CompletionTimeout() { r.completionTimeouts.Add(1) }
func (r *Registry) FlowTimeout()       { r.flowTimeouts.Add(1) }
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
		UDPPathUnavailable:            r.udpPathUnavailable.Load(),
		EndpointTransportRaceFailures: r.endpointRaceFailures.Load(),
		UDPAssociationReconnects:      r.udpReconnects.Load(),
		UDPAssociationRescueFailures:  r.udpRescueFailures.Load(),
		CompletionTimeouts:            r.completionTimeouts.Load(),
		FlowTimeouts:                  r.flowTimeouts.Load(),
		BulkIsolations:                r.bulkIsolations.Load(),
		Reinjections:                  r.reinjections.Load(),
		ReplayBytesInUse:              r.replayBytesInUse.Load(),
	}
	for i := range s.ClassTransitions {
		s.ClassTransitions[i] = r.classTransitions[i].Load()
	}
	r.telemetryMu.Lock()
	var quicLanes int64
	var latestRTT, smoothedRTT time.Duration
	var bytesSent, bytesReceived, bytesLost, packetsLost uint64
	var controllerKind string
	var controllerMode uint32
	var controllerMaxBandwidth, controllerPacingRate, controllerCwnd, controllerBytesInFlight uint64
	var controllerLatestSample, controllerSamples, controllerNonAppSamples, controllerAppSamples uint64
	var controllerLatestAckRate, controllerLatestSendRate uint64
	var controllerStateMisses, controllerZeroSamples, controllerRound uint64
	var controllerBytesLost, controllerPacketsLost uint64
	var controllerMinRTT time.Duration
	var controllerRecovery bool
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
		if o.ControllerKind != "" {
			if controllerKind == "" {
				controllerKind = o.ControllerKind
			} else if controllerKind != o.ControllerKind {
				controllerKind = "mixed"
			}
			if o.ControllerMode > controllerMode {
				controllerMode = o.ControllerMode
			}
			if o.ControllerMaxBandwidth > controllerMaxBandwidth {
				controllerMaxBandwidth = o.ControllerMaxBandwidth
			}
			if o.ControllerLatestSample > controllerLatestSample {
				controllerLatestSample = o.ControllerLatestSample
			}
			if o.ControllerLatestAckRate > controllerLatestAckRate {
				controllerLatestAckRate = o.ControllerLatestAckRate
			}
			if o.ControllerLatestSendRate > controllerLatestSendRate {
				controllerLatestSendRate = o.ControllerLatestSendRate
			}
			controllerSamples += o.ControllerSamples
			controllerNonAppSamples += o.ControllerNonAppSamples
			controllerAppSamples += o.ControllerAppSamples
			controllerStateMisses += o.ControllerStateMisses
			controllerZeroSamples += o.ControllerZeroSamples
			if o.ControllerRound > controllerRound {
				controllerRound = o.ControllerRound
			}
			if o.ControllerPacingRate > controllerPacingRate {
				controllerPacingRate = o.ControllerPacingRate
			}
			if o.ControllerCongestionWindow > controllerCwnd {
				controllerCwnd = o.ControllerCongestionWindow
			}
			if o.ControllerBytesInFlight > controllerBytesInFlight {
				controllerBytesInFlight = o.ControllerBytesInFlight
			}
			controllerBytesLost += o.ControllerBytesLost
			controllerPacketsLost += o.ControllerPacketsLost
			if o.ControllerMinRTT > controllerMinRTT {
				controllerMinRTT = o.ControllerMinRTT
			}
			controllerRecovery = controllerRecovery || o.ControllerInRecovery
		}
	}
	r.telemetryMu.Unlock()
	s.QUICLanes = quicLanes
	s.QUICLatestRTT = latestRTT
	s.QUICSmoothedRTT = smoothedRTT
	s.QUICBytesSent = bytesSent
	s.QUICBytesReceived = bytesReceived
	s.QUICBytesLost = bytesLost
	s.QUICPacketsLost = packetsLost
	s.QUICControllerKind = controllerKind
	s.QUICControllerMode = controllerMode
	s.QUICControllerMaxBandwidth = controllerMaxBandwidth
	s.QUICControllerLatestSample = controllerLatestSample
	s.QUICControllerLatestAckRate = controllerLatestAckRate
	s.QUICControllerLatestSendRate = controllerLatestSendRate
	s.QUICControllerSamples = controllerSamples
	s.QUICControllerNonAppSamples = controllerNonAppSamples
	s.QUICControllerAppSamples = controllerAppSamples
	s.QUICControllerStateMisses = controllerStateMisses
	s.QUICControllerZeroSamples = controllerZeroSamples
	s.QUICControllerRound = controllerRound
	s.QUICControllerPacingRate = controllerPacingRate
	s.QUICControllerCongestionWindow = controllerCwnd
	s.QUICControllerBytesInFlight = controllerBytesInFlight
	s.QUICControllerBytesLost = controllerBytesLost
	s.QUICControllerPacketsLost = controllerPacketsLost
	s.QUICControllerMinRTT = controllerMinRTT
	s.QUICControllerInRecovery = controllerRecovery
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
	fmt.Fprintf(w, "queqiao_active_flows %d\n", s.ActiveFlows)
	fmt.Fprintf(w, "queqiao_flows_started_total %d\n", s.FlowsStarted)
	fmt.Fprintf(w, "queqiao_flows_completed_total %d\n", s.FlowsCompleted)
	fmt.Fprintf(w, "queqiao_flows_failed_total %d\n", s.FlowsFailed)
	fmt.Fprintf(w, "queqiao_bytes_up_total %d\n", s.BytesUp)
	fmt.Fprintf(w, "queqiao_bytes_down_total %d\n", s.BytesDown)
	fmt.Fprintf(w, "queqiao_lane_failures_total %d\n", s.LaneFailures)
	fmt.Fprintf(w, "queqiao_lane_replacements_total %d\n", s.LaneReplacements)
	fmt.Fprintf(w, "queqiao_fallbacks_total %d\n", s.Fallbacks)
	fmt.Fprintf(w, "queqiao_udp_path_unavailable_total %d\n", s.UDPPathUnavailable)
	fmt.Fprintf(w, "queqiao_endpoint_transport_races_failed_total %d\n", s.EndpointTransportRaceFailures)
	fmt.Fprintf(w, "queqiao_udp_association_reconnects_total %d\n", s.UDPAssociationReconnects)
	fmt.Fprintf(w, "queqiao_udp_association_rescue_failures_total %d\n", s.UDPAssociationRescueFailures)
	fmt.Fprintf(w, "queqiao_completion_timeouts_total %d\n", s.CompletionTimeouts)
	fmt.Fprintf(w, "queqiao_flow_timeouts_total %d\n", s.FlowTimeouts)
	// A rising unreplayable count means lane rescue is no longer available for
	// the affected flows: their rescue window was dropped to keep the
	// application moving, so a lane failure now fails the flow.
	fmt.Fprintf(w, "queqiao_replay_bytes_in_use %d\n", s.ReplayBytesInUse)
	fmt.Fprintf(w, "queqiao_bulk_isolations_total %d\n", s.BulkIsolations)
	fmt.Fprintf(w, "queqiao_lane_reinjections_total %d\n", s.Reinjections)
	fmt.Fprintf(w, "queqiao_quic_lanes %d\n", s.QUICLanes)
	fmt.Fprintf(w, "queqiao_quic_latest_rtt_seconds %.9f\n", s.QUICLatestRTT.Seconds())
	fmt.Fprintf(w, "queqiao_quic_smoothed_rtt_seconds %.9f\n", s.QUICSmoothedRTT.Seconds())
	fmt.Fprintf(w, "queqiao_quic_bytes_sent %d\n", s.QUICBytesSent)
	fmt.Fprintf(w, "queqiao_quic_bytes_received %d\n", s.QUICBytesReceived)
	fmt.Fprintf(w, "queqiao_quic_bytes_lost %d\n", s.QUICBytesLost)
	fmt.Fprintf(w, "queqiao_quic_packets_lost %d\n", s.QUICPacketsLost)
	if s.QUICControllerKind != "" {
		fmt.Fprintf(w, "queqiao_quic_controller_kind{kind=\"%s\"} 1\n", s.QUICControllerKind)
	}
	fmt.Fprintf(w, "queqiao_quic_controller_mode %d\n", s.QUICControllerMode)
	fmt.Fprintf(w, "queqiao_quic_controller_max_bandwidth_bytes_per_second %d\n", s.QUICControllerMaxBandwidth)
	fmt.Fprintf(w, "queqiao_quic_controller_latest_sample_bytes_per_second %d\n", s.QUICControllerLatestSample)
	fmt.Fprintf(w, "queqiao_quic_controller_latest_ack_rate_bytes_per_second %d\n", s.QUICControllerLatestAckRate)
	fmt.Fprintf(w, "queqiao_quic_controller_latest_send_rate_bytes_per_second %d\n", s.QUICControllerLatestSendRate)
	fmt.Fprintf(w, "queqiao_quic_controller_samples_total %d\n", s.QUICControllerSamples)
	fmt.Fprintf(w, "queqiao_quic_controller_non_app_limited_samples_total %d\n", s.QUICControllerNonAppSamples)
	fmt.Fprintf(w, "queqiao_quic_controller_app_limited_samples_total %d\n", s.QUICControllerAppSamples)
	fmt.Fprintf(w, "queqiao_quic_controller_state_misses_total %d\n", s.QUICControllerStateMisses)
	fmt.Fprintf(w, "queqiao_quic_controller_zero_samples_total %d\n", s.QUICControllerZeroSamples)
	fmt.Fprintf(w, "queqiao_quic_controller_round %d\n", s.QUICControllerRound)
	fmt.Fprintf(w, "queqiao_quic_controller_pacing_rate_bytes_per_second %d\n", s.QUICControllerPacingRate)
	fmt.Fprintf(w, "queqiao_quic_controller_congestion_window_bytes %d\n", s.QUICControllerCongestionWindow)
	fmt.Fprintf(w, "queqiao_quic_controller_bytes_in_flight %d\n", s.QUICControllerBytesInFlight)
	fmt.Fprintf(w, "queqiao_quic_controller_bytes_lost %d\n", s.QUICControllerBytesLost)
	fmt.Fprintf(w, "queqiao_quic_controller_packets_lost %d\n", s.QUICControllerPacketsLost)
	fmt.Fprintf(w, "queqiao_quic_controller_min_rtt_seconds %.9f\n", s.QUICControllerMinRTT.Seconds())
	if s.QUICControllerInRecovery {
		fmt.Fprintln(w, "queqiao_quic_controller_in_recovery 1")
	} else {
		fmt.Fprintln(w, "queqiao_quic_controller_in_recovery 0")
	}
	for i, value := range s.ClassTransitions {
		fmt.Fprintf(w, "queqiao_class_transitions_total{class=\"%d\"} %d\n", i, value)
	}
}
