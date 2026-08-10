package congestion

import (
	"math/bits"
	"time"

	"github.com/apernet/quic-go/monotime"
)

// tuicBandwidthEstimator is a direct Go equivalent of TUIC's
// quinn-congestions BandwidthEstimation. It takes the minimum of an ACK-rate
// and a send-rate delta, and retains the maximum over a finite round window.
// This is deliberately separate from BBRSender: the latter is retained as an
// experimental controller and has a different packet-sample model.
type tuicBandwidthEstimator struct {
	totalAcked     uint64
	prevTotalAcked uint64
	ackedTime      monotime.Time
	prevAckedTime  monotime.Time
	totalSent      uint64
	prevTotalSent  uint64
	sentTime       monotime.Time
	prevSentTime   monotime.Time
	maxFilter      tuicMinMax
	ackedAtWindow  uint64
}

func newTUICBandwidthEstimator() tuicBandwidthEstimator {
	return tuicBandwidthEstimator{maxFilter: newTUICMinMax()}
}

func (e *tuicBandwidthEstimator) onSent(now monotime.Time, bytes uint64) {
	e.prevTotalSent = e.totalSent
	e.totalSent = satAddUint64(e.totalSent, bytes)
	e.prevSentTime = e.sentTime
	e.sentTime = now
}

// onAckEvent records one congestion-control ACK event. quic-go can report
// many packets in one event (an ACK frame may cover an entire burst), so the
// caller must pass the aggregate byte count and a single event timestamp.
// Feeding those packets one at a time with the same timestamp creates zero
// duration samples after the first packet and systematically underestimates
// bandwidth on high-RTT paths.
func (e *tuicBandwidthEstimator) onAckEvent(now monotime.Time, bytes, round uint64, appLimited bool) {
	if bytes == 0 {
		return
	}
	e.prevTotalAcked = e.totalAcked
	e.totalAcked = satAddUint64(e.totalAcked, bytes)
	e.prevAckedTime = e.ackedTime
	e.ackedTime = now

	if e.prevSentTime.IsZero() {
		return
	}
	sendRate := uint64(0)
	if !e.sentTime.IsZero() && e.sentTime.After(e.prevSentTime) {
		sendRate = rateFromDelta(e.totalSent-e.prevTotalSent, e.sentTime.Sub(e.prevSentTime))
	}
	ackRate := uint64(0)
	if !e.prevAckedTime.IsZero() {
		ackRate = rateFromDelta(e.totalAcked-e.prevTotalAcked, now.Sub(e.prevAckedTime))
	}
	if sendRate == 0 || ackRate == 0 {
		return
	}
	if !appLimited {
		e.maxFilter.updateMax(round, minUint64(sendRate, ackRate))
	}
}

// onAck is retained as a small single-event convenience for package tests and
// future callers that already have an aggregate byte count.
func (e *tuicBandwidthEstimator) onAck(now monotime.Time, bytes, round uint64, appLimited bool) {
	e.onAckEvent(now, bytes, round, appLimited)
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
