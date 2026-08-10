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
	PacingRate       uint64
	CongestionWindow uint64
	BytesInFlight    uint64
	MinRTT           time.Duration
	InRecovery       bool
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
	pacingRate       atomic.Uint64
	congestionWindow atomic.Uint64
	bytesInFlight    atomic.Uint64
	minRTTNS         atomic.Int64
	inRecovery       atomic.Bool
}

func newTelemetryState(kind string) telemetryState {
	return telemetryState{kind: kind}
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

func (t *telemetryState) snapshot() ControllerTelemetry {
	return ControllerTelemetry{
		Kind:             t.kind,
		Mode:             t.mode.Load(),
		MaxBandwidth:     t.maxBandwidth.Load(),
		PacingRate:       t.pacingRate.Load(),
		CongestionWindow: t.congestionWindow.Load(),
		BytesInFlight:    t.bytesInFlight.Load(),
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
