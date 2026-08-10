package congestion

import (
	"math/bits"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// tuicBandwidthEstimator is a bounded packet-state delivery sampler.  TUIC's
// BBR takes the minimum of an ACK-rate and a send-rate slope, then retains a
// maximum over a finite round window.  The important detail is that each
// acknowledged packet carries the cumulative send/ACK state captured when it
// was sent.  A cumulative event-only estimator loses that information under
// ACK coalescing and can mistake one delayed packet for the path bandwidth.
//
// The public quic-go callback does not currently populate ReceivedTime, so the
// sampler uses the congestion event time as the ACK time and automatically
// benefits from per-packet receive times when a future fork supplies them.
// Memory is bounded even if a peer stops acknowledging packets.
type tuicBandwidthEstimator struct {
	totalAcked uint64
	totalSent  uint64

	lastAckedSentTime  monotime.Time
	lastAckedAckTime   monotime.Time
	totalSentAtAck     uint64
	lastAckedPacket    quiccongestion.PacketNumber
	maxFilter          tuicMinMax
	ackedAtWindow      uint64
	packetStates       map[quiccongestion.PacketNumber]tuicPacketState
	legacyPrevAcked    uint64
	legacyPrevAckTime  monotime.Time
	legacyPrevSent     uint64
	legacyPrevSentTime monotime.Time
	legacyAckedTime    monotime.Time
	legacySentTime     monotime.Time
}

type tuicPacketState struct {
	sentTime          monotime.Time
	totalSentAtSend   uint64
	totalAckedAtSend  uint64
	lastAckedSentTime monotime.Time
	lastAckedAckTime  monotime.Time
}

const tuicMaxSendStates = 8192

func newTUICBandwidthEstimator() tuicBandwidthEstimator {
	return tuicBandwidthEstimator{
		maxFilter:    newTUICMinMax(),
		packetStates: make(map[quiccongestion.PacketNumber]tuicPacketState),
	}
}

// onSentPacket records the cumulative state at the time a congestion
// controlled packet is sent. Non-retransmittable packets are intentionally
// excluded from delivery-rate samples, matching QUIC BBR practice.
func (e *tuicBandwidthEstimator) onSentPacket(now monotime.Time, number quiccongestion.PacketNumber, bytes, bytesInFlight uint64, retransmittable bool) {
	if !retransmittable || bytes == 0 {
		return
	}
	e.totalSent = satAddUint64(e.totalSent, bytes)
	// TUIC's sampler establishes an A0/S0 point whenever a new flight starts.
	// Without this point the entire first congestion window produces no
	// delivery sample. On a lossy long-RTT path the second flight may already
	// be recovery-limited, permanently seeding BBR at only a few packets per
	// RTT. The first packet itself has a zero send interval and therefore uses
	// the ACK slope; later packets in the same flight obtain both slopes.
	if bytesInFlight == 0 {
		e.lastAckedAckTime = now
		e.lastAckedSentTime = now
		e.totalSentAtAck = e.totalSent
	}
	if len(e.packetStates) >= tuicMaxSendStates {
		e.pruneStates()
	}
	e.packetStates[number] = tuicPacketState{
		sentTime:          now,
		totalSentAtSend:   e.totalSent,
		totalAckedAtSend:  e.totalAcked,
		lastAckedSentTime: e.lastAckedSentTime,
		lastAckedAckTime:  e.lastAckedAckTime,
	}
}

// onLost removes packet state as soon as QUIC declares the packet lost. The
// loss callback is deliberately observational here; recovery and pacing are
// owned by the controller state machine.
func (e *tuicBandwidthEstimator) onLost(number quiccongestion.PacketNumber) {
	delete(e.packetStates, number)
}

// onAckBatch consumes one congestion event. ACK packets are processed in the
// order supplied by quic-go, but all contribute to one cumulative ACK clock.
// A packet's captured state supplies the preceding A0/S0 points, avoiding the
// zero-duration samples caused by calling an estimator once per packet at one
// event timestamp.
func (e *tuicBandwidthEstimator) onAckBatch(eventTime monotime.Time, acked []quiccongestion.AckedPacketInfo, round uint64, appLimited bool) {
	if eventTime.IsZero() {
		eventTime = monotime.Now()
	}
	var largestState tuicPacketState
	var largestPN quiccongestion.PacketNumber
	var haveLargest bool
	for _, packet := range acked {
		if packet.BytesAcked <= 0 {
			continue
		}
		bytes := uint64(packet.BytesAcked)
		e.totalAcked = satAddUint64(e.totalAcked, bytes)
		state, ok := e.packetStates[packet.PacketNumber]
		if !ok {
			continue
		}
		ackTime := eventTime
		if !packet.ReceivedTime.IsZero() {
			ackTime = packet.ReceivedTime
		}
		ackRate := uint64(0)
		if !state.lastAckedAckTime.IsZero() && ackTime.After(state.lastAckedAckTime) && e.totalAcked >= state.totalAckedAtSend {
			ackRate = rateFromDelta(e.totalAcked-state.totalAckedAtSend, ackTime.Sub(state.lastAckedAckTime))
		}
		sendRate := uint64(0)
		if !state.lastAckedSentTime.IsZero() && state.sentTime.After(state.lastAckedSentTime) && state.totalSentAtSend >= e.totalSentAtAck {
			sendRate = rateFromDelta(state.totalSentAtSend-e.totalSentAtAck, state.sentTime.Sub(state.lastAckedSentTime))
		}
		sample := ackRate
		if sendRate > 0 && (sample == 0 || sendRate < sample) {
			sample = sendRate
		}
		if sample > 0 && !appLimited {
			e.maxFilter.updateMax(round, sample)
		}
		if !haveLargest || packet.PacketNumber > largestPN {
			largestPN, largestState, haveLargest = packet.PacketNumber, state, true
		}
		delete(e.packetStates, packet.PacketNumber)
	}
	if haveLargest {
		ackTime := eventTime
		for _, packet := range acked {
			if packet.PacketNumber == largestPN && !packet.ReceivedTime.IsZero() {
				ackTime = packet.ReceivedTime
				break
			}
		}
		e.lastAckedSentTime = largestState.sentTime
		e.lastAckedAckTime = ackTime
		e.totalSentAtAck = largestState.totalSentAtSend
		e.lastAckedPacket = largestPN
	}
}

// onSent and onAck are retained as aggregate helpers for deterministic unit
// tests and callers that do not have packet numbers. Production QUIC callbacks
// use onSentPacket and onAckBatch above.
func (e *tuicBandwidthEstimator) onSent(now monotime.Time, bytes uint64) {
	e.legacyPrevSent = e.totalSent
	e.totalSent = satAddUint64(e.totalSent, bytes)
	e.legacyPrevSentTime = e.legacySentTime
	e.legacySentTime = now
}

func (e *tuicBandwidthEstimator) onAck(now monotime.Time, bytes, round uint64, appLimited bool) {
	if bytes == 0 {
		return
	}
	e.legacyPrevAcked = e.totalAcked
	e.totalAcked = satAddUint64(e.totalAcked, bytes)
	e.legacyPrevAckTime = e.legacyAckedTime
	e.legacyAckedTime = now
	if e.legacyPrevAckTime.IsZero() || e.legacyPrevSentTime.IsZero() {
		return
	}
	sendRate := rateFromDelta(e.totalSent-e.legacyPrevSent, e.legacySentTime.Sub(e.legacyPrevSentTime))
	ackRate := rateFromDelta(e.totalAcked-e.legacyPrevAcked, e.legacyAckedTime.Sub(e.legacyPrevAckTime))
	if !appLimited && sendRate > 0 && ackRate > 0 {
		e.maxFilter.updateMax(round, minUint64(sendRate, ackRate))
	}
}

func (e *tuicBandwidthEstimator) onAckEvent(now monotime.Time, bytes, round uint64, appLimited bool) {
	e.onAck(now, bytes, round, appLimited)
}

func (e *tuicBandwidthEstimator) pruneStates() {
	remove := tuicMaxSendStates / 4
	for i := 0; i < remove; i++ {
		var oldestPN quiccongestion.PacketNumber
		var oldest monotime.Time
		first := true
		for pn, state := range e.packetStates {
			if first || state.sentTime.Before(oldest) {
				oldestPN, oldest, first = pn, state.sentTime, false
			}
		}
		if first {
			return
		}
		delete(e.packetStates, oldestPN)
	}
}

func (e *tuicBandwidthEstimator) bytesAckedThisWindow() uint64 {
	if e.totalAcked < e.ackedAtWindow {
		return 0
	}
	return e.totalAcked - e.ackedAtWindow
}

func (e *tuicBandwidthEstimator) endAcks() { e.ackedAtWindow = e.totalAcked }

func (e *tuicBandwidthEstimator) estimate() uint64 { return e.maxFilter.get() }

func rateFromDelta(bytes uint64, elapsed time.Duration) uint64 {
	if bytes == 0 || elapsed <= 0 {
		return 0
	}
	ns := uint64(elapsed.Nanoseconds())
	if ns == 0 {
		return 0
	}
	// Divide a 128-bit product by a 64-bit duration. bits.Div64 reports
	// overflow when the quotient would not fit in uint64; saturating is safer
	// than wrapping a telemetry or pacing rate.
	hi, lo := bits.Mul64(bytes, 1_000_000_000)
	if hi >= ns {
		return ^uint64(0)
	}
	q, _ := bits.Div64(hi, lo, ns)
	return q
}

func satAddUint64(a, b uint64) uint64 {
	if ^uint64(0)-a < b {
		return ^uint64(0)
	}
	return a + b
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
