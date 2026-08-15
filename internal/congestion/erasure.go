package congestion

import (
	"sync/atomic"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"github.com/icourses-dev/wanopt/internal/lossmodel"
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
// congestion.
func NewErasureSender(initialPacketSize quiccongestion.ByteCount) *ErasureSender {
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
// divided by the fraction that arrives.
func (e *ErasureSender) bandwidth() quiccongestion.ByteCount {
	delivered := float64(e.inner.bandwidth())
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
	e.arrival.Store(uint64((1 - snapshot.Floor) * partsPerMillion))

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
	t.PacingRate = uint64(float64(t.PacingRate) / arrival)
	t.CongestionWindow = uint64(float64(t.CongestionWindow) / arrival)
	return t
}

// Channel reports what the controller believes about the path and how much
// loss it has declined to treat as congestion.
func (e *ErasureSender) Channel() (lossmodel.Snapshot, uint64, uint64) {
	return e.estimator.Snapshot(), e.suppressed.Load(), e.passed.Load()
}
