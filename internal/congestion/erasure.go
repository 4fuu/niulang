package congestion

import (
	"sort"
	"sync/atomic"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"

	"github.com/bojieli/queqiao/internal/lossmodel"
	"github.com/bojieli/queqiao/internal/pathmodel"
)

var nextErasureMemberID atomic.Uint64

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

	// estimator watches the explicit packet outcomes QUIC reports, ordered by
	// packet number within each congestion event for the burst statistic.
	estimator *lossmodel.Estimator

	// forward carries the fractional part of how many losses to pass through,
	// so a congestive share of a third forwards one loss in three rather than
	// rounding to none.
	forward  float64
	outcomes []packetOutcome

	// floorTrusted remembers that this lane has seen evidence of an
	// independent channel. establishedFloor is the lowest such estimate seen
	// by this connection. A physical erasure floor is a lower envelope: queue
	// loss may raise an observation, but it cannot raise the channel floor.
	// A replacement connection gets a fresh estimator and can establish a
	// genuinely different floor after a path change.
	floorTrusted     bool
	establishedFloor float64

	// arrival is the last computed arrival rate, published for the pacer and
	// the congestion window, which run outside the callback that computes it.
	arrival atomic.Uint64 // arrival rate in parts per million

	suppressed atomic.Uint64
	passed     atomic.Uint64

	// path is shared with every other lane to the same endpoint pair, or nil
	// for a lane that is on its own. share is this lane's allowance of the
	// endpoint's bottleneck in bytes per second, zero while it is unknown.
	path   *pathmodel.PathModel
	share  atomic.Uint64
	member pathmodel.Member
}

type packetOutcome struct {
	number  quiccongestion.PacketNumber
	arrived bool
}

const (
	// erasureMinArrival bounds the compensation. At an arrival rate of 0.15 the
	// sender already puts nearly seven packets on the wire per packet
	// delivered; below that the path is not one to push harder into, and an
	// unbounded divisor would turn a measurement error into a flood.
	erasureMinArrival = 0.15
	// erasureMinSamples is how many packet fates must be decided before the
	// early independence checks may establish a floor. Until then every loss
	// is passed through, so an unmeasured path behaves exactly like BBR.
	erasureMinSamples = 100
	// erasureEarlyMaxFloor bounds only the pre-verdict bootstrap. Acting on a
	// tiny sample that says more than two thirds of the path vanished would
	// multiply the send rate by more than three, which is precisely the unsafe
	// response to a full startup queue. A formally established floor may be
	// higher and is still bounded by erasureMinArrival.
	erasureEarlyMaxFloor = 0.65
	partsPerMillion      = 1e6
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
		//
		// The seed and the cap are separate: a lane joining a path that is
		// already occupied takes both, and a lane that is alone takes the
		// seed only. Sharing one number for the two used to mean that the
		// only lane on a path capped itself at whatever the last one managed.
		state := path.Current()
		// The floor is path knowledge too, so use it for the replacement's first
		// pacing decision. Do not call it this connection's own established
		// evidence, though: a fresh connection is also the safe point at which a
		// changed physical path may establish a higher floor. The shared model
		// retains the inherited value while this sender reports zero weight, then
		// replaces it once this connection has trustworthy evidence of its own.
		if state.Floor > 0 {
			e.arrival.Store(uint64((1 - state.Floor) * partsPerMillion))
		}
		if state.Seed > 0 {
			e.arrival.Store(uint64((1 - state.Floor) * partsPerMillion))
			if state.Share > 0 {
				e.share.Store(uint64(state.Share))
			}
			e.inner.seedBandwidth(uint64(state.Seed/e.arrivalRate()), state.RoundTrip)
		}
	}
	return e
}

func newErasureSender(initialPacketSize quiccongestion.ByteCount) *ErasureSender {
	e := &ErasureSender{
		inner:  NewTUICBBRSender(initialPacketSize),
		member: pathmodel.Member(nextErasureMemberID.Add(1)),
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
	// QUIC has a separate packet-number space for Initial, Handshake and
	// 1-RTT packets, but this public callback omits the space. Inferring losses
	// from gaps therefore fabricates them when those numbers overlap. The
	// callback has already resolved every fate, so order that explicit batch
	// for the burst statistic and measure it directly. A handful of equal
	// numbers across spaces may be adjacent, but neither can become a
	// fictitious gap. Retaining the scratch slice avoids allocating on every
	// acknowledgement.
	e.outcomes = e.outcomes[:0]
	for _, packet := range acked {
		if packet.PacketNumber >= 0 {
			e.outcomes = append(e.outcomes, packetOutcome{number: packet.PacketNumber, arrived: true})
		}
	}
	for _, packet := range lost {
		if packet.PacketNumber >= 0 {
			e.outcomes = append(e.outcomes, packetOutcome{number: packet.PacketNumber})
		}
	}
	sort.Slice(e.outcomes, func(i, j int) bool { return e.outcomes[i].number < e.outcomes[j].number })
	for _, outcome := range e.outcomes {
		e.estimator.ObserveOutcome(outcome.arrived)
	}
	snapshot := e.estimator.Snapshot()
	floor, floorSamples := e.establishedErasureFloor(snapshot)
	if e.path != nil {
		// Pool with the other lanes: the floor converges on all their samples
		// together, and the share is what stops their probes compounding. An
		// untrusted local estimate contributes zero weight rather than diluting
		// a floor another lane has already established.
		state := e.path.Report(e.id(), floor, floorSamples, float64(snapshot.Decided),
			float64(e.inner.bandwidth()), e.inner.minRoundTrip())
		floor = state.Floor
		e.share.Store(uint64(state.Share))
	}
	e.arrival.Store(uint64((1 - floor) * partsPerMillion))

	// The pooled, established floor governs both compensation and which losses are
	// forwarded. Using the raw local floor here would still suppress startup
	// queue drops even though pacing correctly declined to compensate for them.
	snapshot.Floor = floor
	e.inner.OnCongestionEventEx(priorInFlight, eventTime, acked, e.congestive(snapshot, lost))
}

// establishedErasureFloor turns the stateless early discriminator into a
// stable measurement. Once an independent floor has been seen, a later burst
// of congestion must not make it vanish or raise it. The floor is a lower
// envelope for the lifetime of this connection: allowing a completed but
// congested measurement to increase it creates positive feedback, because
// both the pacer and FEC then add traffic to an already overloaded path.
//
// A lower independent measurement may refine the floor downward. If the
// physical path really changes to a higher erasure rate, lane recovery creates
// a new connection and therefore a new estimator; guessing upward inside a
// congested connection is the unsafe direction.
func (e *ErasureSender) establishedErasureFloor(snapshot lossmodel.Snapshot) (floor, samples float64) {
	candidate, _ := conservativeErasureFloor(snapshot)
	if candidate > 0 {
		// Before the first round, the conditional is the measurement that
		// passed the independence check; the partial-round loss rate can
		// already include a queue burst. Once the formal verdict is present,
		// its windowed floor is the stronger evidence.
		measured := candidate
		if snapshot.Memoryless {
			measured = snapshot.Floor
		}
		if measured <= 0 {
			measured = snapshot.Floor
		}
		if measured > 0 && (!e.floorTrusted || measured < e.establishedFloor) {
			e.establishedFloor = measured
		}
		e.floorTrusted = true
	}
	if !e.floorTrusted {
		return 0, 0
	}
	return e.establishedFloor, snapshot.Samples
}

// conservativeErasureFloor separates evidence of an independent channel from
// the first clustered queue drops of STARTUP.
//
// Waiting for the whole process to be declared memoryless is too late on the
// 42% channel this controller exists for: plain BBR has already backed away
// before that verdict and takes most of a short transfer to recover. The
// useful early statistic is P(loss | previous packet arrived). Congestion
// drops in runs, so this conditional is near zero even while a partial round's
// raw loss rate is large; an independent channel keeps it near the floor.
//
// Before the formal memoryless verdict, require that conditional to agree
// with the overall loss rate and require the observed burst length to agree as
// well. The conditional itself is then the conservative bootstrap. Once the
// verdict is available, the estimator's windowed minimum is authoritative.
func conservativeErasureFloor(snapshot lossmodel.Snapshot) (floor, samples float64) {
	if snapshot.Decided < erasureMinSamples || snapshot.Samples <= 0 {
		return 0, 0
	}
	// Even the formal transition test can briefly call an overflowing queue
	// memoryless when almost every packet in a small sample is lost. Do not
	// turn that extreme result into a three-to-sevenfold send-rate increase
	// until two complete floor rounds have given BBR time to drain and supply
	// a lower observation. Ordinary erasure floors use the early path below.
	if snapshot.Floor > erasureEarlyMaxFloor && snapshot.Decided < 2*lossmodel.DefaultRoundSamples {
		return 0, 0
	}
	if snapshot.Memoryless {
		return snapshot.Floor, snapshot.Samples
	}
	p := snapshot.LossAfterArrival
	if p <= 0 {
		return 0, 0
	}
	if snapshot.Loss > erasureEarlyMaxFloor || p > erasureEarlyMaxFloor {
		return 0, 0
	}
	// A conditional below the overall loss rate is evidence that losses are
	// clustered. Do not compensate until the two are close enough that the
	// early sample is consistent with independence. The formal verdict below
	// deliberately waits longer; this narrower bootstrap exists only to keep
	// BBR from yielding a real erasure channel before that verdict arrives.
	if p < 0.9*snapshot.Loss {
		return 0, 0
	}
	// The second, independent view of the same question is the length of loss
	// runs. A queue can briefly make the conditional probabilities look close
	// in a small sample; its longer bursts still distinguish it from a
	// memoryless channel.
	if snapshot.BurstFactor > 1.1 {
		return 0, 0
	}
	return p, snapshot.Samples
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

// id identifies this lane within its shared model. It avoids pointer-derived
// identity so a report never exposes an address, even inside the process.
func (e *ErasureSender) id() pathmodel.Member { return e.member }

// Share is this lane's allowance of the endpoint pair's bottleneck in bytes
// per second, or zero when it is deciding alone or the bottleneck is not yet
// known.
func (e *ErasureSender) Share() float64 { return float64(e.share.Load()) }

// Channel reports what the controller believes about the path and how much
// loss it has declined to treat as congestion.
func (e *ErasureSender) Channel() (lossmodel.Snapshot, uint64, uint64) {
	return e.estimator.Snapshot(), e.suppressed.Load(), e.passed.Load()
}
