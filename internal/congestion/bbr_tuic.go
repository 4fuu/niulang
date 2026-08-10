package congestion

import (
	"math"
	"math/bits"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// TUICBBRSender is a Go port of the BBR controller used by TUIC's
// quinn-congestions fork. It is intentionally opt-in while it is being
// validated against the stock controller and the existing BBRSender.
//
// quic-go's public CongestionControlEx callback does not expose the
// application-limited bit or packet send timestamps. The port therefore uses
// cumulative send/ACK deltas (the same estimator used by TUIC) and a
// conservative idle-gap heuristic for app-limited epochs. This preserves the
// important ACK aggregation, round, recovery, and ProbeRTT behavior without
// reaching into quic-go internals.
type TUICBBRSender struct {
	rttStats        quiccongestion.RTTStatsProvider
	maxDatagramSize quiccongestion.ByteCount
	pacer           *pacer

	mode          tuicBbrMode
	pacingGain    float64
	highGain      float64
	drainGain     float64
	cwndGain      float64
	highCwndGain  float64
	lastCycle     monotime.Time
	cycleOffset   uint8
	randomState   uint64
	initialCwnd   quiccongestion.ByteCount
	minCwnd       quiccongestion.ByteCount
	maxCwnd       quiccongestion.ByteCount
	prevInFlight  quiccongestion.ByteCount
	exitProbeRTT  monotime.Time
	probeRTTStart monotime.Time
	minRTT        time.Duration
	lastSend      monotime.Time
	maxAckedPN    quiccongestion.PacketNumber
	maxSentPN     quiccongestion.PacketNumber
	endRecoveryPN quiccongestion.PacketNumber
	cwnd          quiccongestion.ByteCount
	roundEndPN    quiccongestion.PacketNumber
	round         uint64
	bwAtLastRound uint64
	roundsNoGain  uint8
	fullBandwidth bool

	estimator      tuicBandwidthEstimator
	ackedBytes     uint64
	ackAgg         tuicAckAggregation
	lossState      tuicLossState
	recovery       tuicRecoveryState
	recoveryWindow quiccongestion.ByteCount

	pacingRate    uint64
	bytesInFlight quiccongestion.ByteCount
	telemetry     telemetryState
}

type tuicBbrMode uint8

const (
	tuicBbrStartup tuicBbrMode = iota
	tuicBbrDrain
	tuicBbrProbeBW
	tuicBbrProbeRTT
)

type tuicRecoveryState uint8

const (
	tuicNotInRecovery tuicRecoveryState = iota
	tuicConservation
	tuicGrowth
)

func (r tuicRecoveryState) inRecovery() bool { return r != tuicNotInRecovery }

type tuicLossState struct{ lostBytes uint64 }

type tuicAckAggregation struct {
	maxAckHeight tuicMinMax
	epochStart   monotime.Time
	epochBytes   uint64
}

const (
	tuicHighGain                     = 2.885
	tuicDrainGain                    = 1 / tuicHighGain
	tuicCwndGain                     = 2.0
	tuicProbeRTTInterval             = 10 * time.Second
	tuicProbeRTTDuration             = 200 * time.Millisecond
	tuicStartupGrowth                = 1.25
	tuicStartupNoGainRounds          = 3
	tuicMaxInitialPackets            = 200
	tuicProbeRTTBDPMultiplier        = 0.75
	tuicDefaultMinRate        uint64 = 64 * 1024
	tuicMaxRate               uint64 = 2 * 1024 * 1024 * 1024
)

var tuicPacingGains = [...]float64{1.25, 0.75, 1, 1, 1, 1, 1, 1}

var _ quiccongestion.CongestionControlEx = (*TUICBBRSender)(nil)

func NewTUICBBRSender(initialPacketSize quiccongestion.ByteCount) *TUICBBRSender {
	if initialPacketSize < quiccongestion.MinInitialPacketSize {
		initialPacketSize = quiccongestion.InitialPacketSize
	}
	initial := quiccongestion.ByteCount(tuicMaxInitialPackets) * initialPacketSize
	minCwnd := 4 * initialPacketSize
	if initial < minCwnd {
		initial = minCwnd
	}
	maxCwnd := quiccongestion.MaxCongestionWindowPackets * initialPacketSize
	if initial > maxCwnd {
		initial = maxCwnd
	}
	b := &TUICBBRSender{
		maxDatagramSize: initialPacketSize,
		initialCwnd:     initial,
		minCwnd:         minCwnd,
		maxCwnd:         maxCwnd,
		cwnd:            initial,
		mode:            tuicBbrStartup,
		pacingGain:      tuicHighGain,
		highGain:        tuicHighGain,
		drainGain:       tuicDrainGain,
		cwndGain:        tuicHighGain,
		highCwndGain:    tuicCwndGain,
		estimator:       newTUICBandwidthEstimator(),
		ackAgg:          tuicAckAggregation{maxAckHeight: newTUICMinMax()},
		randomState:     uint64(monotime.Now()) ^ uint64(initialPacketSize)<<17,
		telemetry:       newTelemetryState("bbr-tuic"),
	}
	b.pacer = newPacer(b.bandwidth)
	b.publishTelemetry()
	return b
}

func (b *TUICBBRSender) SetRTTStatsProvider(provider quiccongestion.RTTStatsProvider) {
	b.rttStats = provider
	b.refreshRTT(monotime.Now())
	if b.pacingRate == 0 {
		b.pacingRate = b.initialPacingRate()
	}
	b.publishTelemetry()
}

func (b *TUICBBRSender) rtt() time.Duration {
	if b.minRTT > 0 {
		return b.minRTT
	}
	if b.rttStats != nil {
		if r := b.rttStats.SmoothedRTT(); r > 0 {
			return r
		}
		if r := b.rttStats.LatestRTT(); r > 0 {
			return r
		}
	}
	return 200 * time.Millisecond
}

func (b *TUICBBRSender) refreshRTT(now monotime.Time) {
	if b.rttStats == nil {
		return
	}
	measured := b.rttStats.MinRTT()
	if measured <= 0 {
		measured = b.rttStats.SmoothedRTT()
	}
	if measured <= 0 {
		measured = b.rttStats.LatestRTT()
	}
	if measured > 0 && (b.minRTT <= 0 || measured < b.minRTT) {
		b.minRTT = measured
		// Avoid entering ProbeRTT on the first ACK merely because the public
		// quic-go RTT provider has no explicit "first sample" flag.
		if b.probeRTTStart.IsZero() {
			b.probeRTTStart = now
		}
	}
}

func (b *TUICBBRSender) bandwidth() quiccongestion.ByteCount {
	rate := b.pacingRate
	if rate == 0 {
		// TUIC initializes pacing to IW/RTT once an RTT is available. A Go
		// controller must provide the same finite schedule to quic-go or the
		// first application packet could be delayed forever.
		rate = b.initialPacingRate()
	}
	if rate < tuicDefaultMinRate {
		rate = tuicDefaultMinRate
	}
	if rate > tuicMaxRate {
		rate = tuicMaxRate
	}
	b.telemetry.pacingRate.Store(rate)
	return quiccongestion.ByteCount(rate)
}

func (b *TUICBBRSender) initialPacingRate() uint64 {
	rtt := b.rtt()
	if rtt <= 0 {
		return tuicDefaultMinRate
	}
	rate := uint64(float64(b.initialCwnd) / rtt.Seconds())
	if rate < tuicDefaultMinRate {
		return tuicDefaultMinRate
	}
	if rate > tuicMaxRate {
		return tuicMaxRate
	}
	return rate
}

func (b *TUICBBRSender) TimeUntilSend(quiccongestion.ByteCount) monotime.Time {
	return b.pacer.timeUntilSend()
}

func (b *TUICBBRSender) HasPacingBudget(now monotime.Time) bool {
	return b.pacer.budget(now) >= b.maxDatagramSize
}

func (b *TUICBBRSender) OnPacketSent(sentTime monotime.Time, bytesInFlight quiccongestion.ByteCount, number quiccongestion.PacketNumber, bytes quiccongestion.ByteCount, _ bool) {
	b.maxSentPN = number
	b.lastSend = sentTime
	if bytesInFlight < 0 || bytes < 0 || bytesInFlight > maxCongestionByteCount-bytes {
		b.bytesInFlight = maxCongestionByteCount
	} else {
		b.bytesInFlight = bytesInFlight + bytes
	}
	b.estimator.onSent(sentTime, uint64(maxByteCount(0, bytes)))
	b.pacer.sentPacket(sentTime, bytes)
	b.publishTelemetry()
}

func (b *TUICBBRSender) CanSend(bytesInFlight quiccongestion.ByteCount) bool {
	return bytesInFlight < b.GetCongestionWindow()
}

func (b *TUICBBRSender) MaybeExitSlowStart() {}

func (b *TUICBBRSender) OnPacketAcked(_ quiccongestion.PacketNumber, _ quiccongestion.ByteCount, priorInFlight quiccongestion.ByteCount, _ monotime.Time) {
	b.bytesInFlight = priorInFlight
	b.publishTelemetry()
}

// quic-go calls this once for every newly detected lost packet and then calls
// OnCongestionEventEx with the complete loss batch. The extended callback is
// the single source of truth, so this method intentionally only updates the
// observational in-flight value and does not count loss twice.
func (b *TUICBBRSender) OnCongestionEvent(_ quiccongestion.PacketNumber, _ quiccongestion.ByteCount, priorInFlight quiccongestion.ByteCount) {
	b.bytesInFlight = priorInFlight
}

func (b *TUICBBRSender) OnCongestionEventEx(priorInFlight quiccongestion.ByteCount, eventTime monotime.Time, acked []quiccongestion.AckedPacketInfo, lost []quiccongestion.LostPacketInfo) {
	if eventTime.IsZero() {
		eventTime = monotime.Now()
	}
	ackedBytes := uint64(0)
	lostBytes := uint64(0)
	var largestAck quiccongestion.PacketNumber
	for _, p := range acked {
		if p.BytesAcked > 0 {
			ackedBytes = satAddUint64(ackedBytes, uint64(p.BytesAcked))
		}
		if p.PacketNumber > largestAck {
			largestAck = p.PacketNumber
		}
	}
	for _, p := range lost {
		if p.BytesLost > 0 {
			lostBytes = satAddUint64(lostBytes, uint64(p.BytesLost))
		}
	}
	b.bytesInFlight = saturatingRemaining(priorInFlight, ackedBytes, lostBytes)

	appLimited := b.appLimited(eventTime, priorInFlight, ackedBytes)
	for _, p := range acked {
		if p.BytesAcked > 0 {
			b.estimator.onAck(eventTime, uint64(p.BytesAcked), b.round, appLimited)
		}
	}
	b.ackedBytes = satAddUint64(b.ackedBytes, ackedBytes)
	if ackedBytes > 0 {
		b.refreshRTT(eventTime)
		b.maxAckedPN = largestAck
	}
	b.lossState.lostBytes = satAddUint64(b.lossState.lostBytes, lostBytes)

	bytesAckedWindow := b.estimator.bytesAckedThisWindow()
	excessAcked := b.ackAgg.update(bytesAckedWindow, eventTime, b.round, b.estimator.estimate())
	b.estimator.endAcks()

	isRoundStart := false
	if ackedBytes > 0 && largestAck > b.roundEndPN {
		isRoundStart = true
		b.roundEndPN = b.maxSentPN
		b.round++
	}
	b.updateRecoveryState(isRoundStart)
	if b.mode == tuicBbrProbeBW {
		b.updateGainCycle(eventTime, b.bytesInFlight)
	}
	if isRoundStart && !b.fullBandwidth {
		b.checkFullBandwidth(appLimited)
	}
	b.maybeExitStartupDrain(eventTime, b.bytesInFlight)
	b.maybeProbeRTT(eventTime, isRoundStart, b.bytesInFlight, appLimited)
	b.calculatePacingRate()
	b.calculateCwnd(bytesAckedWindow, excessAcked)
	b.calculateRecoveryWindow(bytesAckedWindow, lostBytes, b.bytesInFlight)
	b.prevInFlight = b.bytesInFlight
	b.lossState.lostBytes = 0
	b.publishTelemetry()
}

func saturatingRemaining(inFlight quiccongestion.ByteCount, acked, lost uint64) quiccongestion.ByteCount {
	if inFlight <= 0 {
		return 0
	}
	remaining := uint64(inFlight)
	if acked >= remaining {
		return 0
	}
	remaining -= acked
	if lost >= remaining {
		return 0
	}
	return quiccongestion.ByteCount(remaining - lost)
}

func saturatingByteAdd(a quiccongestion.ByteCount, b uint64) quiccongestion.ByteCount {
	if a < 0 {
		a = 0
	}
	max := uint64(maxCongestionByteCount)
	if uint64(a) >= max || b > max-uint64(a) {
		return maxCongestionByteCount
	}
	return a + quiccongestion.ByteCount(b)
}

func (b *TUICBBRSender) appLimited(now monotime.Time, priorInFlight quiccongestion.ByteCount, acked uint64) bool {
	if b.lastSend.IsZero() {
		return true
	}
	// The public quic-go API does not expose QUIC's application-limited marker.
	// Treat a drained flight followed by a substantial idle gap as app-limited;
	// continuous bulk traffic remains eligible for the max filter.
	idle := now.Sub(b.lastSend)
	threshold := b.rtt() / 2
	if threshold < 20*time.Millisecond {
		threshold = 20 * time.Millisecond
	}
	return acked == 0 || (priorInFlight <= b.maxDatagramSize && idle > threshold)
}

func (b *TUICBBRSender) updateRecoveryState(roundStart bool) {
	if b.lossState.lostBytes > 0 {
		b.endRecoveryPN = b.maxSentPN
	}
	switch b.recovery {
	case tuicNotInRecovery:
		if b.lossState.lostBytes > 0 {
			b.recovery = tuicConservation
			b.recoveryWindow = 0
			b.roundEndPN = b.maxSentPN
		}
	case tuicConservation, tuicGrowth:
		if b.recovery == tuicConservation && roundStart {
			b.recovery = tuicGrowth
		}
		if b.lossState.lostBytes == 0 && b.maxAckedPN > b.endRecoveryPN {
			b.recovery = tuicNotInRecovery
			b.recoveryWindow = 0
		}
	}
}

func (b *TUICBBRSender) updateGainCycle(now monotime.Time, inFlight quiccongestion.ByteCount) {
	advance := !b.lastCycle.IsZero() && now.Sub(b.lastCycle) > b.rtt()
	if b.pacingGain > 1 && b.lossState.lostBytes == 0 && b.prevInFlight < b.targetCwnd(b.pacingGain) {
		advance = false
	}
	if b.pacingGain < 1 && inFlight <= b.targetCwnd(1) {
		advance = true
	}
	if !advance {
		return
	}
	b.cycleOffset = (b.cycleOffset + 1) % uint8(len(tuicPacingGains))
	b.lastCycle = now
	// Preserve TUIC's drain-to-target behavior: remain at low gain until the
	// queue is actually drained instead of immediately switching to gain 1.
	if b.pacingGain < 1 && math.Abs(tuicPacingGains[b.cycleOffset]-1) < 1e-9 && inFlight > b.targetCwnd(1) {
		return
	}
	b.pacingGain = tuicPacingGains[b.cycleOffset]
}

func (b *TUICBBRSender) maybeExitStartupDrain(now monotime.Time, inFlight quiccongestion.ByteCount) {
	if b.mode == tuicBbrStartup && b.fullBandwidth {
		b.mode = tuicBbrDrain
		b.pacingGain = b.drainGain
		b.cwndGain = b.highCwndGain
	}
	if b.mode == tuicBbrDrain && inFlight <= b.targetCwnd(1) {
		b.mode = tuicBbrProbeBW
		b.cwndGain = tuicCwndGain
		b.lastCycle = now
		b.cycleOffset = b.nextCycleOffset()
		b.pacingGain = tuicPacingGains[b.cycleOffset]
	}
}

func (b *TUICBBRSender) nextCycleOffset() uint8 {
	// A per-controller xorshift state avoids synchronized ProbeBW cycles while
	// keeping the hot path allocation- and lock-free. The time/MTU seed is
	// different for independently created lanes; zero is repaired defensively.
	if b.randomState == 0 {
		b.randomState = uint64(monotime.Now()) ^ uint64(b.maxDatagramSize)<<19 ^ 0x9e3779b97f4a7c15
	}
	x := b.randomState
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	b.randomState = x
	return uint8(x % uint64(len(tuicPacingGains)))
}

func (b *TUICBBRSender) checkFullBandwidth(appLimited bool) {
	if appLimited {
		return
	}
	target := uint64(float64(b.bwAtLastRound) * tuicStartupGrowth)
	bw := b.estimator.estimate()
	if bw >= target {
		b.bwAtLastRound = bw
		b.roundsNoGain = 0
		b.ackAgg.maxAckHeight.reset()
		return
	}
	b.roundsNoGain++
	if b.roundsNoGain >= tuicStartupNoGainRounds || b.recovery.inRecovery() {
		b.fullBandwidth = true
	}
}

func (b *TUICBBRSender) maybeProbeRTT(now monotime.Time, roundStart bool, inFlight quiccongestion.ByteCount, appLimited bool) {
	if !appLimited && !b.probeRTTStart.IsZero() && now.Sub(b.probeRTTStart) > tuicProbeRTTInterval && b.mode != tuicBbrProbeRTT {
		b.mode = tuicBbrProbeRTT
		b.pacingGain = 1
		b.exitProbeRTT = 0
		b.probeRTTStart = now
	}
	if b.mode != tuicBbrProbeRTT {
		return
	}
	if b.exitProbeRTT.IsZero() && inFlight < b.probeRTTCwnd()+b.maxDatagramSize {
		b.exitProbeRTT = now.Add(tuicProbeRTTDuration)
	}
	if !b.exitProbeRTT.IsZero() && roundStart && !now.Before(b.exitProbeRTT) {
		if b.fullBandwidth {
			b.mode = tuicBbrProbeBW
			b.cwndGain = tuicCwndGain
			b.lastCycle = now
		} else {
			b.mode = tuicBbrStartup
			b.pacingGain = b.highGain
			b.cwndGain = b.highCwndGain
		}
	}
}

func (b *TUICBBRSender) targetCwnd(gain float64) quiccongestion.ByteCount {
	bw := b.estimator.estimate()
	if bw == 0 || b.minRTT <= 0 {
		return b.initialCwnd
	}
	bdp := float64(bw) * b.minRTT.Seconds() * gain
	if math.IsNaN(bdp) || math.IsInf(bdp, 0) || bdp < float64(b.minCwnd) {
		return b.minCwnd
	}
	if bdp > float64(b.maxCwnd) {
		return b.maxCwnd
	}
	return quiccongestion.ByteCount(bdp)
}

func (b *TUICBBRSender) probeRTTCwnd() quiccongestion.ByteCount {
	return b.targetCwnd(tuicProbeRTTBDPMultiplier)
}

func (b *TUICBBRSender) calculatePacingRate() {
	bw := b.estimator.estimate()
	if bw == 0 {
		return
	}
	target := uint64(float64(bw) * b.pacingGain)
	if b.fullBandwidth {
		b.pacingRate = target
	} else if b.pacingRate < target {
		b.pacingRate = target
	}
	if b.pacingRate > tuicMaxRate {
		b.pacingRate = tuicMaxRate
	}
}

func (b *TUICBBRSender) calculateCwnd(bytesAcked, excessAcked uint64) {
	if b.mode == tuicBbrProbeRTT {
		b.cwnd = b.probeRTTCwnd()
		return
	}
	target := b.targetCwnd(b.cwndGain)
	if b.fullBandwidth {
		target = minByteCount(b.maxCwnd, saturatingByteAdd(target, b.ackAgg.maxAckHeight.get()))
	} else {
		target = minByteCount(b.maxCwnd, saturatingByteAdd(target, excessAcked))
	}
	if b.fullBandwidth {
		b.cwnd = minByteCount(target, saturatingByteAdd(b.cwnd, bytesAcked))
	} else if b.cwnd < target || b.ackedBytes < uint64(b.initialCwnd) {
		b.cwnd = minByteCount(b.maxCwnd, saturatingByteAdd(b.cwnd, bytesAcked))
	}
	if b.cwnd < b.minCwnd {
		b.cwnd = b.minCwnd
	}
}

func (b *TUICBBRSender) calculateRecoveryWindow(bytesAcked, bytesLost uint64, inFlight quiccongestion.ByteCount) {
	if !b.recovery.inRecovery() {
		return
	}
	lost := maxCongestionByteCount
	if bytesLost < uint64(maxCongestionByteCount) {
		lost = quiccongestion.ByteCount(bytesLost)
	}
	if b.recoveryWindow == 0 {
		b.recoveryWindow = maxByteCount(b.minCwnd, saturatingByteAdd(inFlight, bytesAcked))
		return
	}
	if b.recoveryWindow >= lost {
		b.recoveryWindow -= lost
	} else {
		b.recoveryWindow = b.maxDatagramSize
	}
	if b.recovery == tuicGrowth {
		b.recoveryWindow = saturatingByteAdd(b.recoveryWindow, bytesAcked)
	}
	minimum := maxByteCount(b.minCwnd, saturatingByteAdd(inFlight, bytesAcked))
	if b.recoveryWindow < minimum {
		b.recoveryWindow = minimum
	}
}

func (b *TUICBBRSender) OnRetransmissionTimeout(retransmitted bool) {
	if !retransmitted {
		return
	}
	b.mode = tuicBbrStartup
	b.pacingGain = b.highGain
	b.cwndGain = b.highCwndGain
	b.fullBandwidth = false
	b.roundsNoGain = 0
	b.bwAtLastRound = 0
	b.pacingRate = 0
	b.cwnd = b.minCwnd
	b.recovery = tuicConservation
	b.recoveryWindow = b.minCwnd
	b.estimator = newTUICBandwidthEstimator()
	b.ackAgg = tuicAckAggregation{maxAckHeight: newTUICMinMax()}
	b.pacer = newPacer(b.bandwidth)
	b.pacer.setMaxDatagramSize(b.maxDatagramSize)
	b.publishTelemetry()
}

func (b *TUICBBRSender) SetMaxDatagramSize(size quiccongestion.ByteCount) {
	if size <= 0 {
		return
	}
	b.maxDatagramSize = size
	b.minCwnd = 4 * size
	b.maxCwnd = quiccongestion.MaxCongestionWindowPackets * size
	if b.initialCwnd < b.minCwnd {
		b.initialCwnd = b.minCwnd
	}
	if b.cwnd < b.minCwnd {
		b.cwnd = b.minCwnd
	}
	b.pacer.setMaxDatagramSize(size)
	b.publishTelemetry()
}

func (b *TUICBBRSender) InSlowStart() bool { return b.mode == tuicBbrStartup }
func (b *TUICBBRSender) InRecovery() bool  { return b.recovery.inRecovery() }

func (b *TUICBBRSender) GetCongestionWindow() quiccongestion.ByteCount {
	window := b.cwnd
	if b.mode == tuicBbrProbeRTT {
		window = b.probeRTTCwnd()
	}
	if window < b.minCwnd {
		window = b.minCwnd
	}
	if b.recovery.inRecovery() && b.mode != tuicBbrStartup && b.recoveryWindow > 0 && window > b.recoveryWindow {
		window = b.recoveryWindow
	}
	return window
}

func (b *TUICBBRSender) publishTelemetry() {
	mode := ControllerModeStartup
	switch b.mode {
	case tuicBbrDrain:
		mode = ControllerModeDrain
	case tuicBbrProbeBW:
		mode = ControllerModeProbeBW
	case tuicBbrProbeRTT:
		mode = ControllerModeProbeRTT
	}
	b.telemetry.update(mode, b.estimator.estimate(), uint64(b.bandwidth()), int64(b.GetCongestionWindow()), int64(b.bytesInFlight), b.minRTT, b.recovery.inRecovery())
}

func (b *TUICBBRSender) Telemetry() ControllerTelemetry { return b.telemetry.snapshot() }

func (a *tuicAckAggregation) update(newlyAcked uint64, now monotime.Time, round, bandwidth uint64) uint64 {
	if newlyAcked == 0 || bandwidth == 0 {
		a.epochBytes = newlyAcked
		a.epochStart = now
		return 0
	}
	var expected uint64
	if !a.epochStart.IsZero() && now.After(a.epochStart) {
		elapsedMicros := uint64(now.Sub(a.epochStart).Microseconds())
		hi, lo := bits.Mul64(bandwidth, elapsedMicros)
		if hi == 0 {
			expected = lo / 1_000_000
		} else {
			expected = ^uint64(0)
		}
	}
	if a.epochBytes <= expected {
		a.epochBytes = newlyAcked
		a.epochStart = now
		return 0
	}
	a.epochBytes = satAddUint64(a.epochBytes, newlyAcked)
	diff := a.epochBytes - expected
	a.maxAckHeight.updateMax(round, diff)
	return diff
}
