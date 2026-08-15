package congestion

import (
	"sync/atomic"
	"time"
)

// ControllerTelemetry is a read-only, point-in-time projection of a QUIC
// sender. It deliberately contains no destination or application metadata.
// Values are safe to read from the flow telemetry goroutine while quic-go is
// invoking the controller on its packet-processing goroutine.
type ControllerTelemetry struct {
	Kind             string
	Mode             uint32
	MaxBandwidth     uint64
	LatestSample     uint64
	LatestAckRate    uint64
	LatestSendRate   uint64
	Samples          uint64
	NonAppSamples    uint64
	AppSamples       uint64
	StateMisses      uint64
	ZeroSamples      uint64
	Round            uint64
	PacingRate       uint64
	CongestionWindow uint64
	BytesInFlight    uint64
	BytesLost        uint64
	PacketsLost      uint64
	MinRTT           time.Duration
	InRecovery       bool
	// ErasureFloor is the share of packets this path drops for reasons that
	// have nothing to do with sending rate, as the controller currently
	// believes it. Everything sized for the path is sized from this number --
	// the pacing compensation, the congestion window, and the erasure code's
	// rate -- so a trace that does not carry it cannot explain any of them.
	ErasureFloor float64
}

const (
	ControllerModeUnknown uint32 = iota
	ControllerModeStartup
	ControllerModeDrain
	ControllerModeProbeBW
	ControllerModeProbeRTT
	ControllerModeAdaptive
	ControllerModeBrutal
	ControllerModeStock
)

// TelemetryProvider is implemented by the optional controllers. The stock
// quic-go controller does not expose an equivalent public state projection,
// so its mode remains ControllerModeStock only when explicitly supplied by a
// transport adapter.
type TelemetryProvider interface {
	Telemetry() ControllerTelemetry
}

// telemetryState stores every mutable value atomically. Congestion-control
// callbacks are serialized by quic-go, but observation runs independently and
// must not introduce a data race or a lock on the packet hot path.
type telemetryState struct {
	kind             string
	mode             atomic.Uint32
	maxBandwidth     atomic.Uint64
	latestSample     atomic.Uint64
	latestAckRate    atomic.Uint64
	latestSendRate   atomic.Uint64
	samples          atomic.Uint64
	nonAppSamples    atomic.Uint64
	appSamples       atomic.Uint64
	stateMisses      atomic.Uint64
	zeroSamples      atomic.Uint64
	round            atomic.Uint64
	pacingRate       atomic.Uint64
	congestionWindow atomic.Uint64
	bytesInFlight    atomic.Uint64
	bytesLost        atomic.Uint64
	packetsLost      atomic.Uint64
	minRTTNS         atomic.Int64
	inRecovery       atomic.Bool
}

func newTelemetryState(kind string) telemetryState {
	return telemetryState{kind: kind}
}

// observeLoss records loss reported to an external congestion controller.
// quic-go's public controller interface doesn't provide a connection-stats
// handle, so custom controllers must retain their own authoritative counters.
func (t *telemetryState) observeLoss(bytes, packets uint64) {
	t.bytesLost.Add(bytes)
	t.packetsLost.Add(packets)
}

func (t *telemetryState) update(mode uint32, maxBandwidth, pacingRate uint64, congestionWindow, bytesInFlight int64, minRTT time.Duration, inRecovery bool) {
	if congestionWindow < 0 {
		congestionWindow = 0
	}
	if bytesInFlight < 0 {
		bytesInFlight = 0
	}
	if minRTT < 0 {
		minRTT = 0
	}
	t.mode.Store(mode)
	t.maxBandwidth.Store(maxBandwidth)
	t.pacingRate.Store(pacingRate)
	t.congestionWindow.Store(uint64(congestionWindow))
	t.bytesInFlight.Store(uint64(bytesInFlight))
	t.minRTTNS.Store(minRTT.Nanoseconds())
	t.inRecovery.Store(inRecovery)
}

// updateSampler publishes diagnostic delivery-sampler state. Controllers
// that do not use the TUIC packet sampler leave these values at zero.
func (t *telemetryState) updateSampler(latestSample, latestAckRate, latestSendRate, samples, nonAppSamples, appSamples, stateMisses, zeroSamples, round uint64) {
	t.latestSample.Store(latestSample)
	t.latestAckRate.Store(latestAckRate)
	t.latestSendRate.Store(latestSendRate)
	t.samples.Store(samples)
	t.nonAppSamples.Store(nonAppSamples)
	t.appSamples.Store(appSamples)
	t.stateMisses.Store(stateMisses)
	t.zeroSamples.Store(zeroSamples)
	t.round.Store(round)
}

func (t *telemetryState) snapshot() ControllerTelemetry {
	return ControllerTelemetry{
		Kind:             t.kind,
		Mode:             t.mode.Load(),
		MaxBandwidth:     t.maxBandwidth.Load(),
		LatestSample:     t.latestSample.Load(),
		LatestAckRate:    t.latestAckRate.Load(),
		LatestSendRate:   t.latestSendRate.Load(),
		Samples:          t.samples.Load(),
		NonAppSamples:    t.nonAppSamples.Load(),
		AppSamples:       t.appSamples.Load(),
		StateMisses:      t.stateMisses.Load(),
		ZeroSamples:      t.zeroSamples.Load(),
		Round:            t.round.Load(),
		PacingRate:       t.pacingRate.Load(),
		CongestionWindow: t.congestionWindow.Load(),
		BytesInFlight:    t.bytesInFlight.Load(),
		BytesLost:        t.bytesLost.Load(),
		PacketsLost:      t.packetsLost.Load(),
		MinRTT:           time.Duration(t.minRTTNS.Load()),
		InRecovery:       t.inRecovery.Load(),
	}
}

func controllerModeName(mode uint32) string {
	switch mode {
	case ControllerModeStartup:
		return "startup"
	case ControllerModeDrain:
		return "drain"
	case ControllerModeProbeBW:
		return "probe_bw"
	case ControllerModeProbeRTT:
		return "probe_rtt"
	case ControllerModeAdaptive:
		return "adaptive"
	case ControllerModeBrutal:
		return "brutal"
	case ControllerModeStock:
		return "stock"
	default:
		return "unknown"
	}
}
