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
// quic-go's public CongestionControlEx callback does not expose an explicit
// application-limited notification, so the controller derives the same
// congestion-window-headroom condition at each event and records that marker
// per packet. ACK receive timestamps are used when the QUIC fork supplies them
// and the congestion-event time otherwise.
type TUICBBRSender struct {
	rttStats        quiccongestion.RTTStatsProvider
	maxDatagramSize quiccongestion.ByteCount
	pacer           *pacer

	mode                 tuicBbrMode
	pacingGain           float64
	highGain             float64
	drainGain            float64
	cwndGain             float64
	highCwndGain         float64
	lastCycle            monotime.Time
	cycleOffset          uint8
	randomState          uint64
	initialCwnd          quiccongestion.ByteCount
	minCwnd              quiccongestion.ByteCount
	maxCwnd              quiccongestion.ByteCount
	exitProbeRTT         monotime.Time
	probeRTTRoundPassed  bool
	minRTT               time.Duration
	minRTTAt             monotime.Time
	exitingQuiescence    bool
	maxAckedPN           quiccongestion.PacketNumber
	maxSentPN            quiccongestion.PacketNumber
	endRecoveryPN        quiccongestion.PacketNumber
	cwnd                 quiccongestion.ByteCount
	roundEndPN           quiccongestion.PacketNumber
	round                uint64
	bwAtLastRound        uint64
	roundsNoGain         uint8
	fullBandwidth        bool
	lastSampleAppLimited bool
	lossEventsInRound    uint64
	bytesLostInRound     uint64

	estimator      tuicBandwidthEstimator
	ackedBytes     uint64
	ackAgg         tuicAckAggregation
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

type tuicAckAggregation struct {
	maxAckHeight tuicMinMax
	epochStart   monotime.Time
	epochBytes   uint64
}

const (
	tuicHighGain            = 2.885
	tuicDrainGain           = 1 / tuicHighGain
	tuicCwndGain            = 2.0
	tuicProbeRTTInterval    = 10 * time.Second
	tuicProbeRTTDuration    = 200 * time.Millisecond
	tuicStartupGrowth       = 1.25
	tuicStartupNoGainRounds = 3
	tuicStartupLossEvents   = 8
	tuicStartupLossFraction = 0.02
	tuicMaxBurstPackets     = 10
	// sing-quic's TUIC BBR uses the standard 32-packet initial window.
	// A previous port accidentally used 200 packets, creating a burst roughly
	// six times larger than TUIC's IW on a 200-ms path.  That burst is
	// especially damaging on the measured lossy China-US path: the first loss
	// event then dominates the delivery estimator and recovery window.
	tuicInitialPackets        = 32
	tuicDefaultMinRate uint64 = 64 * 1024
	tuicMaxRate        uint64 = 2 * 1024 * 1024 * 1024
)

var tuicPacingGains = [...]float64{1.25, 0.75, 1, 1, 1, 1, 1, 1}

var _ quiccongestion.CongestionControlEx = (*TUICBBRSender)(nil)

func NewTUICBBRSender(initialPacketSize quiccongestion.ByteCount) *TUICBBRSender {
	if initialPacketSize < quiccongestion.MinInitialPacketSize {
		initialPacketSize = quiccongestion.InitialPacketSize
	}
	initial := quiccongestion.ByteCount(tuicInitialPackets) * initialPacketSize
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
		// TUIC uses a 2.0 congestion-window gain in STARTUP.  The pacing gain
		// remains highGain; applying that factor to cwnd as well overfills the
		// initial flight before a delivery model exists.
		cwndGain:      tuicCwndGain,
		highCwndGain:  tuicCwndGain,
		maxAckedPN:    quiccongestion.PacketNumber(-1),
		maxSentPN:     quiccongestion.PacketNumber(-1),
		endRecoveryPN: quiccongestion.PacketNumber(-1),
		roundEndPN:    quiccongestion.PacketNumber(-1),
		estimator:     newTUICBandwidthEstimator(),
		ackAgg:        tuicAckAggregation{maxAckHeight: newTUICMinMax()},
		randomState:   uint64(monotime.Now()) ^ uint64(initialPacketSize)<<17,
		telemetry:     newTelemetryState("bbr-tuic"),
	}
	b.pacer = newTUICPacer(b.bandwidth)
	b.publishTelemetry()
	return b
}

func (b *TUICBBRSender) SetRTTStatsProvider(provider quiccongestion.RTTStatsProvider) {
	b.rttStats = provider
	b.publishTelemetry()
}

func (b *TUICBBRSender) rtt() time.Duration {
	if b.minRTT > 0 {
		return b.minRTT
	}
	if b.rttStats != nil {
		if r := b.rttStats.MinRTT(); r > 0 {
			return r
		}
	}
	return 100 * time.Millisecond
}

// refreshRTTSample mirrors TUIC's sampler-owned minimum RTT model. The
// provider's smoothed RTT is a safe pacing fallback, but it must not become a
// permanent min_rtt: doing so prevents ProbeRTT from correcting queueing bias.
// Returns true when an existing min_rtt expired at this event.
func (b *TUICBBRSender) refreshRTTSample(now monotime.Time, sample time.Duration) bool {
	if sample <= 0 {
		return false
	}
	expired := !b.minRTTAt.IsZero() && now.Sub(b.minRTTAt) >= tuicProbeRTTInterval
	if b.minRTT <= 0 || sample < b.minRTT || expired {
		b.minRTT = sample
		b.minRTTAt = now
	}
	return expired
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
	// TUIC's BBR starts at high_gain * IW / RTT.  The prior port omitted
	// high_gain, so after correcting IW from 200 packets to 32 it would seed
	// the delivery model at only one-third of TUIC's startup rate.  STARTUP
	// never decreases this value; the normal full-bandwidth and loss tests
	// still bound the transition to DRAIN/ProbeBW.
	rate := uint64(float64(b.initialCwnd) * b.highGain / rtt.Seconds())
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

func (b *TUICBBRSender) OnPacketSent(sentTime monotime.Time, bytesInFlight quiccongestion.ByteCount, number quiccongestion.PacketNumber, bytes quiccongestion.ByteCount, retransmittable bool) {
	b.maxSentPN = number
	// apNet/quic-go calls this hook after adding an ack-eliciting packet to its
	// connection flight. TUIC's sampler, however, takes the pre-send flight and
	// adds the packet itself when it snapshots send state. Convert at this
	// adapter boundary; otherwise every packet's sampler flight is one packet
	// too large and a first packet can never be recognized as opening a flight.
	preSendFlight := preSendBytesInFlight(bytesInFlight, bytes, retransmittable)
	if preSendFlight == 0 {
		b.exitingQuiescence = true
	}
	// Keep the controller telemetry in the same post-send units as quic-go.
	b.bytesInFlight = maxByteCount(0, bytesInFlight)
	b.estimator.onSentPacket(sentTime, number, uint64(maxByteCount(0, bytes)), uint64(preSendFlight), retransmittable)
	b.pacer.sentPacket(sentTime, bytes)
	b.publishTelemetry()
}

func preSendBytesInFlight(postSend, bytes quiccongestion.ByteCount, retransmittable bool) quiccongestion.ByteCount {
	if postSend <= 0 || !retransmittable || bytes <= 0 {
		return maxByteCount(0, postSend)
	}
	if postSend <= bytes {
		return 0
	}
	return postSend - bytes
}

func (b *TUICBBRSender) CanSend(bytesInFlight quiccongestion.ByteCount) bool {
	return bytesInFlight < b.GetCongestionWindow()
}

func (b *TUICBBRSender) MaybeExitSlowStart() {}

func (b *TUICBBRSender) OnPacketAcked(_ quiccongestion.PacketNumber, _ quiccongestion.ByteCount, _ quiccongestion.ByteCount, _ monotime.Time) {
}

// quic-go calls this once for every newly detected lost packet and then calls
// OnCongestionEventEx with the complete loss batch. The extended callback is
// the single source of truth, so the legacy callback deliberately does nothing.
func (b *TUICBBRSender) OnCongestionEvent(_ quiccongestion.PacketNumber, _ quiccongestion.ByteCount, _ quiccongestion.ByteCount) {
}

func (b *TUICBBRSender) OnCongestionEventEx(priorInFlight quiccongestion.ByteCount, eventTime monotime.Time, acked []quiccongestion.AckedPacketInfo, lost []quiccongestion.LostPacketInfo) {
	if eventTime.IsZero() {
		eventTime = monotime.Now()
	}
	hasLosses := len(lost) != 0
	ackedBytes := uint64(0)
	lostBytes := uint64(0)
	largestAck := quiccongestion.PacketNumber(-1)
	largestLoss := quiccongestion.PacketNumber(-1)
	lastLostState := tuicSendState{}
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
		state := b.estimator.onLost(p.PacketNumber)
		if state.valid && p.PacketNumber > largestLoss {
			largestLoss = p.PacketNumber
			lastLostState = state
		}
	}
	if hasLosses {
		b.telemetry.observeLoss(lostBytes, uint64(len(lost)))
	}
	b.bytesInFlight = saturatingRemaining(priorInFlight, ackedBytes, lostBytes)

	// Match TUIC's connection-level app-limited notification: if the sender
	// was not filling its current window, packets sent after this event belong
	// to an app-limited phase. The sampler records the marker per packet and
	// ends it only after an ACK for a later packet, rather than suppressing an
	// entire event based on an idle-gap guess.
	appLimited := b.appLimited(priorInFlight)
	if appLimited {
		b.estimator.markAppLimited()
	}

	// QUIC's round counter advances before the delivery sampler is evaluated.
	// Using the previous round here delayed the ten-round max filter by one
	// round and made sparse/loss-delayed ACKs expire valid peaks too early.
	isRoundStart := false
	if ackedBytes > 0 && largestAck > b.roundEndPN {
		isRoundStart = true
		b.roundEndPN = b.maxSentPN
		b.round++
	}
	// TUIC changes recovery only on events with an ACK. A loss-only callback
	// updates sampler/loss accounting, but waits for an ACK-clocked event before
	// entering or advancing packet conservation.
	if len(acked) != 0 {
		b.updateRecoveryState(largestAck, hasLosses, isRoundStart)
	}
	// Process the complete ACK batch as one sample. quic-go gives every packet
	// in this callback the same receive/event time; updating the estimator once
	// per packet would make all but the first packet have a zero ACK interval.
	sample := tuicAckSample{}
	if ackedBytes > 0 {
		sample = b.estimator.onAckBatch(eventTime, acked, b.round)
	}
	minRTTExpired := false
	if ackedBytes > 0 {
		// The sampler owns BBR's expiring minimum-RTT model. The RTT provider is
		// only a startup pacing fallback before a packet sample exists.
		if sample.minRTT > 0 {
			minRTTExpired = b.refreshRTTSample(eventTime, sample.minRTT)
		}
		b.maxAckedPN = largestAck
	}

	bytesAckedWindow := b.estimator.bytesAckedThisWindow()
	b.ackedBytes = satAddUint64(b.ackedBytes, bytesAckedWindow)
	excessAcked := uint64(0)
	if bytesAckedWindow > 0 {
		excessAcked = b.ackAgg.update(bytesAckedWindow, eventTime, b.round, b.estimator.estimate())
	}
	b.estimator.endAcks()
	lastSendState := sample.lastSendState
	if lastLostState.valid && (!lastSendState.valid || lastLostState.packetNumber > lastSendState.packetNumber) {
		lastSendState = lastLostState
	}
	if lastSendState.valid {
		b.lastSampleAppLimited = lastSendState.appLimited
	}
	if hasLosses {
		b.lossEventsInRound = satAddUint64(b.lossEventsInRound, 1)
		b.bytesLostInRound = satAddUint64(b.bytesLostInRound, lostBytes)
	}
	if b.mode == tuicBbrProbeBW {
		b.updateGainCycle(eventTime, priorInFlight, b.bytesInFlight, hasLosses)
	}
	if isRoundStart && !b.fullBandwidth {
		b.checkFullBandwidth(lastSendState)
	}
	b.maybeExitStartupDrain(eventTime, b.bytesInFlight)
	b.maybeProbeRTT(eventTime, isRoundStart, b.bytesInFlight, minRTTExpired)
	b.calculatePacingRate()
	b.calculateCwnd(bytesAckedWindow, excessAcked)
	// Native TUIC calculates recovery from the post-event in-flight remainder.
	// Adding bytesAcked in calculateRecoveryWindow then permits the newly
	// acknowledged amount while accounting for losses in the same event.
	b.calculateRecoveryWindow(bytesAckedWindow, lostBytes, b.bytesInFlight)
	if leastUnacked, ok := obsoletePacketNumber(acked, lost); ok {
		b.estimator.removeObsolete(leastUnacked)
	}
	if isRoundStart {
		b.lossEventsInRound = 0
		b.bytesLostInRound = 0
	}
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

// appLimited reports whether the sender had window it did not use, which marks
// the packets sent after this event as application-limited.
//
// The threshold is what makes this usable rather than a formality. Any unused
// window at all is the wrong test: a paced sender is below its congestion
// window at almost every acknowledgement, because the pacer is deliberately
// spacing packets out, so "in flight is below the window" marks nearly every
// packet application-limited. Application-limited samples are the ones the
// full-bandwidth test skips, so with that test the controller never counts a
// round without gain and never leaves startup at all -- measured on a path
// policing each source at 25 Mbit/s, it stayed in startup for an entire 20 MiB
// transfer, held 2.9 congestion windows in flight, and doubled the round trip
// with standing queue.
//
// Requiring a full burst of unused window separates the two: a sender that ran
// out of data leaves the whole window unused, while a paced one is short by at
// most the packets the pacer has not yet released. This is the rule the drain
// case already applied; it belongs in every mode.
func (b *TUICBBRSender) appLimited(priorInFlight quiccongestion.ByteCount) bool {
	window := b.GetCongestionWindow()
	if priorInFlight >= window {
		return false
	}
	return window-priorInFlight > tuicMaxBurstPackets*b.maxDatagramSize
}

func obsoletePacketNumber(acked []quiccongestion.AckedPacketInfo, lost []quiccongestion.LostPacketInfo) (quiccongestion.PacketNumber, bool) {
	var largest quiccongestion.PacketNumber = -1
	if len(acked) > 0 {
		for _, packet := range acked {
			if packet.PacketNumber > largest {
				largest = packet.PacketNumber
			}
		}
		if largest < 0 {
			return 0, false
		}
		if largest > 1 {
			return largest - 2, true
		}
		return 0, true
	}
	for _, packet := range lost {
		if packet.PacketNumber > largest {
			largest = packet.PacketNumber
		}
	}
	if largest < 0 {
		return 0, false
	}
	if largest == quiccongestion.PacketNumber(^uint64(0)>>1) {
		return largest, true
	}
	return largest + 1, true
}

func (b *TUICBBRSender) updateRecoveryState(lastAckedPacket quiccongestion.PacketNumber, hasLosses, roundStart bool) {
	// TUIC deliberately suppresses packet conservation until STARTUP has
	// established the bottleneck model. Mode alone is not sufficient: ProbeRTT
	// can occur before full bandwidth is known.
	if !b.fullBandwidth {
		return
	}
	if hasLosses {
		b.endRecoveryPN = b.maxSentPN
	}
	switch b.recovery {
	case tuicNotInRecovery:
		if hasLosses {
			b.recovery = tuicConservation
			b.recoveryWindow = 0
			b.roundEndPN = b.maxSentPN
		}
	case tuicConservation, tuicGrowth:
		if b.recovery == tuicConservation && roundStart {
			b.recovery = tuicGrowth
		}
		if !hasLosses && lastAckedPacket > b.endRecoveryPN {
			b.recovery = tuicNotInRecovery
		}
	}
}

func (b *TUICBBRSender) updateGainCycle(now monotime.Time, priorInFlight, inFlight quiccongestion.ByteCount, hasLosses bool) {
	advance := !b.lastCycle.IsZero() && now.Sub(b.lastCycle) > b.rtt()
	if b.pacingGain > 1 && !hasLosses && priorInFlight < b.targetCwnd(b.pacingGain) {
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
	offset := uint8(x % uint64(len(tuicPacingGains)-1))
	// TUIC starts ProbeBW at gain index 0 or 2..7. Index 1 is excluded so a
	// connection never enters the 0.75 drain phase without first probing.
	if offset >= 1 {
		offset++
	}
	return offset
}

func (b *TUICBBRSender) checkFullBandwidth(lastSendState tuicSendState) {
	if b.lastSampleAppLimited {
		return
	}
	target := uint64(float64(b.bwAtLastRound) * tuicStartupGrowth)
	bw := b.estimator.estimate()
	if bw >= target {
		b.bwAtLastRound = bw
		b.roundsNoGain = 0
		return
	}
	b.roundsNoGain++
	if b.roundsNoGain >= tuicStartupNoGainRounds || b.shouldExitStartupForLoss(lastSendState) {
		b.fullBandwidth = true
	}
}

func (b *TUICBBRSender) shouldExitStartupForLoss(lastSendState tuicSendState) bool {
	if b.lossEventsInRound < tuicStartupLossEvents || !lastSendState.valid || lastSendState.bytesInFlight == 0 || b.bytesLostInRound == 0 {
		return false
	}
	return float64(b.bytesLostInRound) > float64(lastSendState.bytesInFlight)*tuicStartupLossFraction
}

func (b *TUICBBRSender) maybeProbeRTT(now monotime.Time, roundStart bool, inFlight quiccongestion.ByteCount, minRTTExpired bool) {
	if minRTTExpired && !b.exitingQuiescence && b.mode != tuicBbrProbeRTT {
		b.mode = tuicBbrProbeRTT
		b.pacingGain = 1
		b.exitProbeRTT = 0
	}
	if b.mode != tuicBbrProbeRTT {
		b.exitingQuiescence = false
		return
	}
	// ProbeRTT samples are intentionally application-limited. Otherwise the
	// forced four-packet window can replace a valid bandwidth peak with the
	// artificial ProbeRTT delivery rate.
	b.estimator.markAppLimited()
	if b.exitProbeRTT.IsZero() && inFlight < b.probeRTTCwnd()+b.maxDatagramSize {
		b.exitProbeRTT = now.Add(tuicProbeRTTDuration)
		b.probeRTTRoundPassed = false
	}
	if !b.exitProbeRTT.IsZero() && roundStart {
		b.probeRTTRoundPassed = true
	}
	if !b.exitProbeRTT.IsZero() && b.probeRTTRoundPassed && !now.Before(b.exitProbeRTT) {
		b.minRTTAt = now
		if b.fullBandwidth {
			b.mode = tuicBbrProbeBW
			b.cwndGain = tuicCwndGain
			b.lastCycle = now
			b.cycleOffset = b.nextCycleOffset()
			b.pacingGain = tuicPacingGains[b.cycleOffset]
		} else {
			b.mode = tuicBbrStartup
			b.pacingGain = b.highGain
			b.cwndGain = b.highCwndGain
		}
	}
	b.exitingQuiescence = false
}

func (b *TUICBBRSender) targetCwnd(gain float64) quiccongestion.ByteCount {
	bw := b.estimator.estimate()
	if bw == 0 {
		target := float64(b.initialCwnd) * gain
		if target > float64(b.maxCwnd) {
			return b.maxCwnd
		}
		return maxByteCount(b.minCwnd, quiccongestion.ByteCount(target))
	}
	rtt := b.minRTT
	if rtt <= 0 {
		rtt = b.rtt()
	}
	bdp := float64(bw) * rtt.Seconds() * gain
	if math.IsNaN(bdp) || math.IsInf(bdp, 0) || bdp < float64(b.minCwnd) {
		return b.minCwnd
	}
	if bdp > float64(b.maxCwnd) {
		return b.maxCwnd
	}
	return quiccongestion.ByteCount(bdp)
}

func (b *TUICBBRSender) probeRTTCwnd() quiccongestion.ByteCount {
	return b.minCwnd
}

func (b *TUICBBRSender) calculatePacingRate() {
	bw := b.estimator.estimate()
	if bw == 0 {
		return
	}
	target := uint64(float64(bw) * b.pacingGain)
	if b.fullBandwidth {
		b.pacingRate = target
	} else {
		// The public startup rate is high_gain*IW/RTT while the stored rate is
		// still zero. On the first useful event with a provider RTT, native TUIC
		// seeds the stored model at IW/min_rtt before later ACKs grow it toward
		// the delivery estimate.
		if b.pacingRate == 0 && b.rttStats != nil {
			if rtt := b.rttStats.MinRTT(); rtt > 0 {
				b.pacingRate = rateFromDelta(uint64(b.initialCwnd), rtt)
				if b.pacingRate > tuicMaxRate {
					b.pacingRate = tuicMaxRate
				}
				return
			}
		}
		if b.pacingRate < target {
			b.pacingRate = target
		}
	}
	if b.pacingRate > tuicMaxRate {
		b.pacingRate = tuicMaxRate
	}
}

func (b *TUICBBRSender) calculateCwnd(bytesAcked, excessAcked uint64) {
	if b.mode == tuicBbrProbeRTT {
		return
	}
	target := b.targetCwnd(b.cwndGain)
	if b.fullBandwidth {
		target = minByteCount(b.maxCwnd, saturatingByteAdd(target, b.ackAgg.maxAckHeight.get()))
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
	// QUIC PTOs are probe events, not proof that the bottleneck model is
	// invalid. Native TUIC leaves BBR state intact and lets the ordinary ACK /
	// loss event update recovery. Resetting to a four-packet window here makes
	// one lossy-RTT PTO create a long throughput collapse.
	_ = retransmitted
}

func (b *TUICBBRSender) SetMaxDatagramSize(size quiccongestion.ByteCount) {
	if size <= 0 {
		return
	}
	oldSize := b.maxDatagramSize
	if oldSize <= 0 {
		oldSize = size
	}
	oldMinCwnd := b.minCwnd
	oldInitialCwnd := b.initialCwnd
	b.maxDatagramSize = size
	b.minCwnd = 4 * size
	b.maxCwnd = quiccongestion.MaxCongestionWindowPackets * size
	b.initialCwnd = scaleTUICWindow(oldInitialCwnd, oldSize, size)
	b.initialCwnd = minByteCount(b.maxCwnd, maxByteCount(b.minCwnd, b.initialCwnd))
	switch b.cwnd {
	case oldMinCwnd:
		b.cwnd = b.minCwnd
	case oldInitialCwnd:
		b.cwnd = b.initialCwnd
	default:
		b.cwnd = minByteCount(b.maxCwnd, maxByteCount(b.minCwnd, b.cwnd))
	}
	// Recovery is a byte budget accumulated from actual ACK/loss events, not a
	// packet-count setting. Preserve its byte value across an MTU increase and
	// only clamp it to the newly valid window range.
	b.recoveryWindow = minByteCount(b.maxCwnd, maxByteCount(b.minCwnd, b.recoveryWindow))
	b.pacer.setMaxDatagramSize(size)
	b.publishTelemetry()
}

func scaleTUICWindow(window, oldSize, newSize quiccongestion.ByteCount) quiccongestion.ByteCount {
	if window <= 0 || oldSize <= 0 || newSize <= 0 || oldSize == newSize {
		return window
	}
	if window > maxCongestionByteCount/newSize {
		return maxCongestionByteCount
	}
	return window * newSize / oldSize
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
	b.telemetry.updateSampler(
		b.estimator.latestSample,
		b.estimator.latestAckRate,
		b.estimator.latestSendRate,
		b.estimator.samples,
		b.estimator.nonAppSamples,
		b.estimator.appSamples,
		b.estimator.stateMisses,
		b.estimator.zeroSamples,
		b.round,
	)
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
