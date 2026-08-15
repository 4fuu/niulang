package congestion

import (
	"sync/atomic"
	"unsafe"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"github.com/icourses-dev/wanopt/internal/lossmodel"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
)

// ErasureSender is BBR on a path that erases packets for reasons that have
// nothing to do with congestion.
//
// Measured on the China-US path this project targets, about 42% of packets are
// dropped independently of the sending rate: at 1 Mbit/s as readily as at 12,
// and ICMP loses 37% at five packets a second
// (docs/PATH-CHARACTER-20260813.md). Every loss-responsive controller gives
// the path away. Across the emulated channel, carrying QUIC datagrams:
//
//	default (Reno/Cubic)          0.13 Mbit/s
//	BBR                           0.39
//	BBR-TUIC                      1.36
//	Brutal, told 25 Mbit/s        13.89
//
// Only Brutal gets the path, and only because it ignores loss and paces at a
// rate a human typed in. This controller is meant to reach the same place
// without being told, which needs two corrections rather than one.
//
// The first is that channel loss must not read as congestion. Which loss is
// which is not a policy but a measurement: the erasure floor is the loss that
// does not respond to sending more slowly, and the excess above it is the part
// a sender caused. Only that excess is passed to BBR.
//
// The second is subtler and is why plain BBR collapses rather than merely
// under-shooting. BBR's bandwidth estimate is the rate that is *delivered*,
// while its pacer governs the rate that is *sent*. On a clean path those are
// the same number. On an erasure channel they differ by the arrival rate, and
// pacing a delivered-rate estimate makes the sending rate its own input:
// sending S delivers S(1-p), which becomes the next estimate, which is paced
// as S(1-p), which delivers S(1-p)^2. Every rate is a fixed point of that loop
// only in the sense that zero is; in practice it walks down to nothing, which
// is the 0.39 Mbit/s above. Dividing the pacing rate by the arrival rate
// restores the property BBR assumes, and the loop then converges on the
// bottleneck rather than on zero: in startup S grows by the gain each round
// until delivery stops growing, which happens exactly when S reaches the
// bottleneck.
//
// What it does not do is ignore congestion. Above the bottleneck the loss
// stops being memoryless -- the live path's loss runs grow from 1.7 packets to
// 5.7 -- the excess over the floor becomes positive, and BBR sees it and backs
// off.
type ErasureSender struct {
	inner *TUICBBRSender
	pacer *pacer

	// estimator watches which packet numbers come back. Packet numbers are the
	// transmission order, which is the only order in which loss is visible.
	estimator *lossmodel.Estimator

	// forward carries the fractional part of how many losses to pass through,
	// so a congestive share of a third forwards one loss in three rather than
	// rounding to none.
	forward float64

	// arrival is the last computed arrival rate, published for the pacer and
	// the congestion window, which run outside the callback that computes it.
	arrival atomic.Uint64 // arrival rate in parts per million

	suppressed atomic.Uint64
	passed     atomic.Uint64

	// path is shared with every other lane to the same endpoint pair, or nil
	// for a lane that is on its own. share is this lane's allowance of the
	// endpoint's bottleneck in bytes per second, zero while it is unknown.
	path  *pathmodel.PathModel
	share atomic.Uint64
}

const (
	// erasureMinArrival bounds the compensation. At an arrival rate of 0.15 the
	// sender already puts nearly seven packets on the wire per packet
	// delivered; below that the path is not one to push harder into, and an
	// unbounded divisor would turn a measurement error into a flood.
	erasureMinArrival = 0.15
	// erasureMinSamples is how many packet fates must be decided before the
	// floor is trusted enough to suppress anything. Until then every loss is
	// passed through, so an unmeasured path behaves exactly like BBR.
	erasureMinSamples = 100
	partsPerMillion   = 1e6
)

// NewErasureSender returns a controller for a path whose loss is mostly not
// congestion, deciding on its own. Use NewErasureSenderOn when more than one
// lane shares an endpoint pair.
func NewErasureSender(initialPacketSize quiccongestion.ByteCount) *ErasureSender {
	return NewErasureSenderOn(initialPacketSize, nil)
}

// NewErasureSenderOn returns a controller that pools its measurements with
// every other lane on the same path.
//
// Deciding alone is what made lanes cost more than they earn: each lane
// measures the erasure floor from only its own packets, and each discovers the
// bottleneck from only its own delivered rate, so the aggregate overshoots by
// however many lanes there are and the path's loss stops being memoryless.
func NewErasureSenderOn(initialPacketSize quiccongestion.ByteCount, path *pathmodel.PathModel) *ErasureSender {
	e := newErasureSender(initialPacketSize)
	e.path = path
	if path != nil {
		// Start where its siblings already are rather than at the initial
		// window. On a path that erases 40% of packets the ramp is the
		// expensive part, and a lane opened to replace one that died would
		// otherwise pay it again on a path nothing has forgotten.
		//
		// What is seeded is the delivered rate, compensated for the erasure --
		// which is what this sender puts on the wire to deliver it. BBR sizes
		// both its pacing and its window from that one number, so seeding it
		// moves both; seeding a pacing rate alone leaves the window at the
		// initial one and the pacer waiting on it.
		if state := path.Current(); state.Share > 0 {
			e.arrival.Store(uint64((1 - state.Floor) * partsPerMillion))
			e.share.Store(uint64(state.Share))
			e.inner.seedBandwidth(uint64(state.Share/e.arrivalRate()), state.RoundTrip)
		}
	}
	return e
}

func newErasureSender(initialPacketSize quiccongestion.ByteCount) *ErasureSender {
	e := &ErasureSender{
		inner: NewTUICBBRSender(initialPacketSize),
		// A reorder tolerance wide enough for QUIC's acknowledgement
		// aggregation: packets are acked in batches, and a batch boundary must
		// not read as a gap.
		estimator: lossmodel.New(lossmodel.Config{ReorderTolerance: 32}),
	}
	e.arrival.Store(partsPerMillion)
	e.pacer = newTUICPacer(e.bandwidth)
	return e
}

// bandwidth is the rate to put on the wire: BBR's estimate of what arrives,
// bounded by this lane's share of the endpoint pair's bottleneck, and divided
// by the fraction that arrives.
//
// The cap is what keeps lanes from compounding. Each lane's own estimate is
// what it alone is receiving, so four lanes each probing above their own
// estimate put four times the overshoot into one bottleneck; capping at the
// share means the aggregate on the wire is what a single sender would have put
// there.
func (e *ErasureSender) bandwidth() quiccongestion.ByteCount {
	delivered := float64(e.inner.bandwidth())
	if share := float64(e.share.Load()); share > 0 && share < delivered {
		delivered = share
	}
	return quiccongestion.ByteCount(delivered / e.arrivalRate())
}

func (e *ErasureSender) arrivalRate() float64 {
	rate := float64(e.arrival.Load()) / partsPerMillion
	if rate < erasureMinArrival {
		return erasureMinArrival
	}
	if rate > 1 {
		return 1
	}
	return rate
}

func (e *ErasureSender) SetRTTStatsProvider(provider quiccongestion.RTTStatsProvider) {
	e.inner.SetRTTStatsProvider(provider)
}

func (e *ErasureSender) TimeUntilSend(quiccongestion.ByteCount) monotime.Time {
	return e.pacer.timeUntilSend()
}

func (e *ErasureSender) HasPacingBudget(now monotime.Time) bool {
	return e.pacer.budget(now) >= e.inner.maxDatagramSize
}

func (e *ErasureSender) OnPacketSent(sentTime monotime.Time, bytesInFlight quiccongestion.ByteCount, number quiccongestion.PacketNumber, bytes quiccongestion.ByteCount, retransmittable bool) {
	e.pacer.sentPacket(sentTime, bytes)
	e.inner.OnPacketSent(sentTime, bytesInFlight, number, bytes, retransmittable)
}

// CanSend uses the compensated window for the same reason the pacer uses the
// compensated rate: the window bounds what is on the wire, and on an erasure
// channel what is on the wire is more than what will arrive.
func (e *ErasureSender) CanSend(bytesInFlight quiccongestion.ByteCount) bool {
	return bytesInFlight < e.GetCongestionWindow()
}

func (e *ErasureSender) GetCongestionWindow() quiccongestion.ByteCount {
	return quiccongestion.ByteCount(float64(e.inner.GetCongestionWindow()) / e.arrivalRate())
}

func (e *ErasureSender) MaybeExitSlowStart() { e.inner.MaybeExitSlowStart() }

func (e *ErasureSender) OnPacketAcked(number quiccongestion.PacketNumber, ackedBytes, priorInFlight quiccongestion.ByteCount, eventTime monotime.Time) {
	e.inner.OnPacketAcked(number, ackedBytes, priorInFlight, eventTime)
}

func (e *ErasureSender) OnCongestionEvent(number quiccongestion.PacketNumber, lostBytes, priorInFlight quiccongestion.ByteCount) {
	e.inner.OnCongestionEvent(number, lostBytes, priorInFlight)
}

// OnCongestionEventEx is where the two regimes are separated. Everything that
// arrived updates the estimate of the channel; only the share of the losses
// that the channel does not explain is passed to BBR.
func (e *ErasureSender) OnCongestionEventEx(priorInFlight quiccongestion.ByteCount, eventTime monotime.Time, acked []quiccongestion.AckedPacketInfo, lost []quiccongestion.LostPacketInfo) {
	for _, packet := range acked {
		if packet.PacketNumber >= 0 {
			e.estimator.Observe(uint64(packet.PacketNumber))
		}
	}
	snapshot := e.estimator.Snapshot()
	floor := snapshot.Floor
	if e.path != nil {
		// Pool with the other lanes: the floor converges on all their samples
		// together, and the share is what stops their probes compounding.
		state := e.path.Report(pathmodel.Member(e.id()), floor, snapshot.Samples,
			float64(e.inner.bandwidth()), e.inner.minRoundTrip())
		floor = state.Floor
		e.share.Store(uint64(state.Share))
	}
	e.arrival.Store(uint64((1 - floor) * partsPerMillion))

	e.inner.OnCongestionEventEx(priorInFlight, eventTime, acked, e.congestive(snapshot, lost))
}

// congestive returns the share of these losses that the channel does not
// explain, in whole packets.
//
// The share is a fraction, and a fraction of a loss cannot be reported, so the
// remainder is carried rather than rounded away. Rounding down would suppress
// every loss whenever the congestive share stayed below a half, which is
// exactly the mild persistent congestion a controller most needs to see.
func (e *ErasureSender) congestive(snapshot lossmodel.Snapshot, lost []quiccongestion.LostPacketInfo) []quiccongestion.LostPacketInfo {
	if len(lost) == 0 {
		return lost
	}
	if snapshot.Decided < erasureMinSamples || snapshot.Recent <= 0 {
		e.passed.Add(uint64(len(lost)))
		return lost
	}
	share := 1.0
	if snapshot.Recent > snapshot.Floor {
		share = (snapshot.Recent - snapshot.Floor) / snapshot.Recent
	} else {
		share = 0
	}

	e.forward += share * float64(len(lost))
	pass := int(e.forward)
	if pass > len(lost) {
		pass = len(lost)
	}
	e.forward -= float64(pass)

	e.passed.Add(uint64(pass))
	e.suppressed.Add(uint64(len(lost) - pass))
	if pass == 0 {
		return nil
	}
	return lost[:pass]
}

func (e *ErasureSender) OnRetransmissionTimeout(retransmitted bool) {
	e.inner.OnRetransmissionTimeout(retransmitted)
}

func (e *ErasureSender) SetMaxDatagramSize(size quiccongestion.ByteCount) {
	e.inner.SetMaxDatagramSize(size)
	e.pacer.setMaxDatagramSize(size)
}

func (e *ErasureSender) InSlowStart() bool { return e.inner.InSlowStart() }
func (e *ErasureSender) InRecovery() bool  { return e.inner.InRecovery() }

// Telemetry reports the inner controller's state with this one's compensation
// applied, so a trace shows the rate actually being put on the wire.
func (e *ErasureSender) Telemetry() ControllerTelemetry {
	t := e.inner.Telemetry()
	t.Kind = "erasure"
	arrival := e.arrivalRate()
	t.ErasureFloor = 1 - arrival
	t.PacingRate = uint64(float64(t.PacingRate) / arrival)
	t.CongestionWindow = uint64(float64(t.CongestionWindow) / arrival)
	return t
}

// id identifies this lane within its shared model. The pointer is stable for
// the controller's lifetime and unique among live controllers, which is
// exactly what membership needs.
func (e *ErasureSender) id() uintptr {
	return uintptr(unsafe.Pointer(e))
}

// Share is this lane's allowance of the endpoint pair's bottleneck in bytes
// per second, or zero when it is deciding alone or the bottleneck is not yet
// known.
func (e *ErasureSender) Share() float64 { return float64(e.share.Load()) }

// Channel reports what the controller believes about the path and how much
// loss it has declined to treat as congestion.
func (e *ErasureSender) Channel() (lossmodel.Snapshot, uint64, uint64) {
	return e.estimator.Snapshot(), e.suppressed.Load(), e.passed.Load()
}
