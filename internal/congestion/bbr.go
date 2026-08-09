package congestion

import (
	"math"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// BBRSender is an independently implemented, BBRv1-shaped QUIC controller.
//
// It is intentionally kept separate from AdaptiveSender: Adaptive is a
// conservative rate estimator, while this controller maintains the BBR
// STARTUP/DRAIN/PROBE_BW/PROBE_RTT model, a max delivery-rate filter, and a
// recovery window.  The model only consumes QUIC's public congestion-control
// callbacks; no application plaintext or destination metadata is involved.
//
// The implementation is based on the public BBR model (draft-cardwell-iccrg)
// and is an original Go implementation.  It is experimental until the real
// path campaign demonstrates an acceptable latency/loss trade-off; Reno stays
// the default and Adaptive remains available as a conservative alternative.
type BBRSender struct {
	rttStats        quiccongestion.RTTStatsProvider
	maxDatagramSize quiccongestion.ByteCount
	pacer           *pacer

	mode       bbrMode
	pacingGain float64
	cwndGain   float64

	initialCwnd quiccongestion.ByteCount
	minCwnd     quiccongestion.ByteCount
	maxCwnd     quiccongestion.ByteCount
	cwnd        quiccongestion.ByteCount

	bytesInFlight quiccongestion.ByteCount
	lastSent      quiccongestion.PacketNumber
	lastAcked     quiccongestion.PacketNumber
	lastRoundEnd  quiccongestion.PacketNumber
	roundStarted  bool
	round         uint64

	minRTT       time.Duration
	minRTTAt     monotime.Time
	lastDelivery monotime.Time
	maxBandwidth uint64
	bwSamples    [10]bwSample

	bwAtLastRound uint64
	roundsNoGain  uint8
	fullBandwidth bool

	cycleStart  monotime.Time
	cycleOffset uint8

	inRecovery      bool
	recoveryWindow  quiccongestion.ByteCount
	endRecoveryPN   quiccongestion.PacketNumber
	recoveryLosses  quiccongestion.ByteCount
	probeRTTExit    monotime.Time
	probeRTTStarted bool
}

type bbrMode uint8

const (
	bbrStartup bbrMode = iota
	bbrDrain
	bbrProbeBW
	bbrProbeRTT
)

type bwSample struct {
	round uint64
	bps   uint64
}

const (
	bbrHighGain                   = 2.885
	bbrDrainGain                  = 1 / bbrHighGain
	bbrCwndGain                   = 2.0
	bbrProbeRTTInterval           = 10 * time.Second
	bbrProbeRTTDuration           = 200 * time.Millisecond
	bbrStartupGrowth              = 1.25
	bbrStartupNoGainRounds        = 3
	bbrMinRate                    = 64 * 1024
	bbrMaxRate             uint64 = 2 * 1024 * 1024 * 1024
)

var bbrPacingGains = [...]float64{1.25, 0.75, 1, 1, 1, 1, 1, 1}

var _ quiccongestion.CongestionControlEx = (*BBRSender)(nil)

// NewBBRSender creates a BBR-shaped sender with a conservative 10-packet
// initial window. The initial window is capped by quic-go's safety maximum.
func NewBBRSender(initialPacketSize quiccongestion.ByteCount) *BBRSender {
	if initialPacketSize < quiccongestion.MinInitialPacketSize {
		initialPacketSize = quiccongestion.InitialPacketSize
	}
	initial := maxByteCount(10*initialPacketSize, 14720)
	maxCwnd := quiccongestion.MaxCongestionWindowPackets * initialPacketSize
	if initial > maxCwnd {
		initial = maxCwnd
	}
	b := &BBRSender{
		maxDatagramSize: initialPacketSize,
		initialCwnd:     initial,
		minCwnd:         4 * initialPacketSize,
		maxCwnd:         maxCwnd,
		cwnd:            initial,
		mode:            bbrStartup,
		pacingGain:      bbrHighGain,
		cwndGain:        bbrHighGain,
	}
	b.pacer = newPacer(b.bandwidth)
	return b
}

func (b *BBRSender) SetRTTStatsProvider(provider quiccongestion.RTTStatsProvider) {
	b.rttStats = provider
	b.refreshRTT(monotime.Now())
	b.updateWindow(0)
}

func (b *BBRSender) rtt() time.Duration {
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

func (b *BBRSender) refreshRTT(now monotime.Time) {
	measured := time.Duration(0)
	if b.rttStats != nil {
		measured = b.rttStats.MinRTT()
		if measured <= 0 {
			measured = b.rttStats.SmoothedRTT()
		}
		if measured <= 0 {
			measured = b.rttStats.LatestRTT()
		}
	}
	if measured > 0 && (b.minRTT <= 0 || measured < b.minRTT) {
		b.minRTT = measured
		b.minRTTAt = now
		b.updateWindow(0)
	}
}

func (b *BBRSender) bandwidth() quiccongestion.ByteCount {
	rate := b.maxBandwidth
	if rate == 0 {
		rtt := b.rtt()
		if rtt <= 0 {
			rtt = 200 * time.Millisecond
		}
		rate = uint64(float64(b.initialCwnd) / rtt.Seconds() * bbrHighGain)
	}
	if rate < bbrMinRate {
		rate = bbrMinRate
	}
	if rate > bbrMaxRate {
		rate = bbrMaxRate
	}
	paced := float64(rate) * b.pacingGain
	if math.IsNaN(paced) || math.IsInf(paced, 0) || paced < bbrMinRate {
		paced = bbrMinRate
	}
	if paced > float64(bbrMaxRate) {
		paced = float64(bbrMaxRate)
	}
	return quiccongestion.ByteCount(paced)
}

func (b *BBRSender) TimeUntilSend(_ quiccongestion.ByteCount) monotime.Time {
	return b.pacer.timeUntilSend()
}

func (b *BBRSender) HasPacingBudget(now monotime.Time) bool {
	return b.pacer.budget(now) >= b.maxDatagramSize
}

func (b *BBRSender) OnPacketSent(sentTime monotime.Time, bytesInFlight quiccongestion.ByteCount, number quiccongestion.PacketNumber, bytes quiccongestion.ByteCount, _ bool) {
	b.lastSent = number
	// bytesInFlight is the value before this packet is accounted for.
	if bytesInFlight > ^quiccongestion.ByteCount(0)-bytes {
		b.bytesInFlight = ^quiccongestion.ByteCount(0)
	} else {
		b.bytesInFlight = bytesInFlight + bytes
	}
	b.pacer.sentPacket(sentTime, bytes)
}

func (b *BBRSender) CanSend(bytesInFlight quiccongestion.ByteCount) bool {
	return bytesInFlight < b.GetCongestionWindow()
}

func (b *BBRSender) MaybeExitSlowStart() {}

func (b *BBRSender) OnPacketAcked(_ quiccongestion.PacketNumber, _ quiccongestion.ByteCount, priorInFlight quiccongestion.ByteCount, _ monotime.Time) {
	b.bytesInFlight = priorInFlight
}

func (b *BBRSender) OnCongestionEvent(number quiccongestion.PacketNumber, lostBytes quiccongestion.ByteCount, priorInFlight quiccongestion.ByteCount) {
	if lostBytes == 0 {
		return
	}
	now := monotime.Now()
	b.bytesInFlight = priorInFlight
	b.noteLoss(number, lostBytes, now)
	b.updateWindow(0)
}

func (b *BBRSender) OnCongestionEventEx(priorInFlight quiccongestion.ByteCount, eventTime monotime.Time, acked []quiccongestion.AckedPacketInfo, lost []quiccongestion.LostPacketInfo) {
	if eventTime.IsZero() {
		eventTime = monotime.Now()
	}
	b.bytesInFlight = priorInFlight
	var ackedBytes quiccongestion.ByteCount
	var largestAck quiccongestion.PacketNumber
	for _, p := range acked {
		if p.BytesAcked > ^quiccongestion.ByteCount(0)-ackedBytes {
			ackedBytes = ^quiccongestion.ByteCount(0)
		} else {
			ackedBytes += p.BytesAcked
		}
		if p.PacketNumber > largestAck {
			largestAck = p.PacketNumber
		}
	}
	if len(acked) > 0 {
		b.lastAcked = largestAck
		b.refreshRTT(eventTime)
		b.updateBandwidth(eventTime, uint64(ackedBytes))
		b.updateRound(largestAck)
	}
	for _, p := range lost {
		b.noteLoss(p.PacketNumber, p.BytesLost, eventTime)
	}
	if len(lost) > 0 {
		b.fullBandwidth = true
	}
	b.transition(eventTime, priorInFlight, len(lost) > 0)
	b.updateWindow(ackedBytes)
	b.bytesInFlight = priorInFlight
	if ackedBytes >= b.recoveryLosses {
		b.recoveryLosses = 0
	} else {
		b.recoveryLosses -= ackedBytes
	}
}

func (b *BBRSender) noteLoss(number quiccongestion.PacketNumber, lostBytes quiccongestion.ByteCount, now monotime.Time) {
	if lostBytes == 0 {
		return
	}
	if !b.inRecovery {
		b.inRecovery = true
		b.recoveryWindow = maxByteCount(b.minCwnd, b.bytesInFlight)
	}
	if number > b.endRecoveryPN {
		b.endRecoveryPN = b.lastSent
	}
	if b.recoveryWindow > lostBytes {
		b.recoveryWindow -= lostBytes
	} else {
		b.recoveryWindow = b.minCwnd
	}
	if ^quiccongestion.ByteCount(0)-b.recoveryLosses < lostBytes {
		b.recoveryLosses = ^quiccongestion.ByteCount(0)
	} else {
		b.recoveryLosses += lostBytes
	}
	if b.mode == bbrStartup {
		b.fullBandwidth = true
	}
	if b.cycleStart.IsZero() {
		b.cycleStart = now
	}
}

func (b *BBRSender) updateBandwidth(now monotime.Time, bytes uint64) {
	if bytes == 0 {
		return
	}
	if !b.lastDelivery.IsZero() {
		delta := now.Sub(b.lastDelivery)
		if delta < time.Millisecond {
			delta = time.Millisecond
		}
		sample := uint64(float64(bytes) / delta.Seconds())
		if sample > 0 {
			b.bwSamples[b.round%uint64(len(b.bwSamples))] = bwSample{round: b.round, bps: sample}
			b.recomputeBandwidth()
		}
	}
	b.lastDelivery = now
}

// recomputeBandwidth gives the delivery estimator a finite memory. Keeping a
// peak forever is unsafe on a path that changes from a fast to a congested
// regime: the controller would continue pacing at a stale rate indefinitely.
func (b *BBRSender) recomputeBandwidth() {
	var best uint64
	for _, sample := range b.bwSamples {
		if sample.bps == 0 || sample.round+uint64(len(b.bwSamples)) < b.round {
			continue
		}
		if sample.bps > best {
			best = sample.bps
		}
	}
	b.maxBandwidth = best
}

func (b *BBRSender) updateRound(largestAck quiccongestion.PacketNumber) {
	if !b.roundStarted {
		b.roundStarted = true
		b.round++
		b.lastRoundEnd = b.lastSent
		return
	}
	if largestAck > b.lastRoundEnd {
		b.round++
		b.lastRoundEnd = b.lastSent
		if b.maxBandwidth >= uint64(float64(b.bwAtLastRound)*bbrStartupGrowth) {
			b.bwAtLastRound = b.maxBandwidth
			b.roundsNoGain = 0
		} else if b.mode == bbrStartup {
			b.roundsNoGain++
		}
	}
}

func (b *BBRSender) transition(now monotime.Time, inFlight quiccongestion.ByteCount, hadLoss bool) {
	if b.mode == bbrStartup && (b.fullBandwidth || b.roundsNoGain >= bbrStartupNoGainRounds) {
		b.mode = bbrDrain
		b.pacingGain = bbrDrainGain
		b.cwndGain = bbrHighGain
	}
	if b.mode == bbrDrain && inFlight <= b.targetWindow(1) {
		b.mode = bbrProbeBW
		b.pacingGain = 1
		b.cwndGain = bbrCwndGain
		b.cycleStart = now
	}
	if b.mode == bbrProbeBW {
		if b.cycleStart.IsZero() {
			b.cycleStart = now
		}
		advance := now.Sub(b.cycleStart) >= b.rtt()
		if b.pacingGain > 1 && !hadLoss && inFlight < b.targetWindow(b.pacingGain) {
			advance = false
		}
		if b.pacingGain < 1 && inFlight <= b.targetWindow(1) {
			advance = true
		}
		if advance {
			b.cycleOffset = (b.cycleOffset + 1) % uint8(len(bbrPacingGains))
			b.cycleStart = now
			b.pacingGain = bbrPacingGains[b.cycleOffset]
		}
	}
	if !b.minRTTAt.IsZero() && now.Sub(b.minRTTAt) >= bbrProbeRTTInterval && b.mode != bbrProbeRTT {
		b.mode = bbrProbeRTT
		b.pacingGain = 1
		b.probeRTTExit = 0
		b.probeRTTStarted = true
	}
	if b.mode == bbrProbeRTT {
		if b.probeRTTExit.IsZero() && inFlight <= b.minCwnd+b.maxDatagramSize {
			b.probeRTTExit = now.Add(bbrProbeRTTDuration)
		}
		if !b.probeRTTExit.IsZero() && !now.Before(b.probeRTTExit) {
			if b.fullBandwidth {
				b.mode = bbrProbeBW
				b.pacingGain = 1
				b.cwndGain = bbrCwndGain
				b.cycleStart = now
			} else {
				b.mode = bbrStartup
				b.pacingGain = bbrHighGain
				b.cwndGain = bbrHighGain
			}
			b.minRTTAt = now
			b.probeRTTStarted = false
		}
	}
	if b.inRecovery && !hadLoss && b.lastAcked > b.endRecoveryPN {
		b.inRecovery = false
		b.recoveryWindow = 0
	}
}

func (b *BBRSender) targetWindow(gain float64) quiccongestion.ByteCount {
	rtt := b.rtt()
	if b.maxBandwidth == 0 || rtt <= 0 {
		return b.initialCwnd
	}
	target := float64(b.maxBandwidth) * rtt.Seconds() * gain
	if math.IsNaN(target) || math.IsInf(target, 0) || target < float64(b.minCwnd) {
		target = float64(b.minCwnd)
	}
	if target > float64(b.maxCwnd) {
		target = float64(b.maxCwnd)
	}
	return quiccongestion.ByteCount(target)
}

func (b *BBRSender) updateWindow(acked quiccongestion.ByteCount) {
	if b.mode == bbrProbeRTT {
		b.cwnd = b.minCwnd
	} else if b.fullBandwidth {
		target := b.targetWindow(b.cwndGain)
		if b.cwnd < target {
			b.cwnd = minByteCount(target, b.cwnd+acked)
		} else if b.cwnd > target && b.mode != bbrStartup {
			b.cwnd = target
		}
	} else {
		// Startup is ACK-clocked: grow by newly acknowledged bytes while the
		// delivery-rate filter is still finding the path capacity.
		b.cwnd = minByteCount(b.maxCwnd, maxByteCount(b.minCwnd, b.cwnd+acked))
	}
	if b.inRecovery && acked > 0 {
		if ^quiccongestion.ByteCount(0)-b.recoveryWindow < acked {
			b.recoveryWindow = ^quiccongestion.ByteCount(0)
		} else {
			b.recoveryWindow += acked
		}
		if b.recoveryWindow < b.minCwnd {
			b.recoveryWindow = b.minCwnd
		}
	}
	if b.cwnd < b.minCwnd {
		b.cwnd = b.minCwnd
	}
	if b.cwnd > b.maxCwnd {
		b.cwnd = b.maxCwnd
	}
}

func (b *BBRSender) OnRetransmissionTimeout(retransmitted bool) {
	if !retransmitted {
		return
	}
	b.inRecovery = true
	b.recoveryWindow = b.minCwnd
	b.cwnd = b.minCwnd
	b.maxBandwidth = 0
	b.roundsNoGain = 0
	b.fullBandwidth = false
	b.mode = bbrStartup
	b.pacingGain = bbrHighGain
	b.cwndGain = bbrHighGain
	b.lastDelivery = 0
	b.pacer = newPacer(b.bandwidth)
	b.pacer.setMaxDatagramSize(b.maxDatagramSize)
}

func (b *BBRSender) SetMaxDatagramSize(size quiccongestion.ByteCount) {
	if size <= 0 {
		return
	}
	b.maxDatagramSize = size
	b.minCwnd = 4 * size
	b.maxCwnd = quiccongestion.MaxCongestionWindowPackets * size
	if b.initialCwnd < b.minCwnd {
		b.initialCwnd = b.minCwnd
	}
	b.pacer.setMaxDatagramSize(size)
	b.updateWindow(0)
}

func (b *BBRSender) InSlowStart() bool { return b.mode == bbrStartup }
func (b *BBRSender) InRecovery() bool  { return b.inRecovery }
func (b *BBRSender) GetCongestionWindow() quiccongestion.ByteCount {
	window := b.cwnd
	if window < b.minCwnd {
		window = b.minCwnd
	}
	if b.inRecovery && b.recoveryWindow > 0 && window > b.recoveryWindow {
		window = b.recoveryWindow
	}
	return window
}
