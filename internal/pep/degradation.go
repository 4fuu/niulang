package pep

import (
	"math"
	"time"
)

const (
	degradationErasureThreshold = 0.55
	degradationDirectErasure    = 0.65
	degradationRateFraction     = 0.55
	degradationMinBaselineRate  = 64 * 1024
	degradationMinWindow        = 2 * time.Second
)

type degradationDecision struct {
	reason       string
	observedFor  time.Duration
	erasure      float64
	currentRate  uint64
	baselineRate uint64
	rtt          time.Duration
}

// degradationDetector compares a flow with its own healthy history. Absolute
// low throughput is not a transport failure: it can be an idle application or
// a slow destination. A switch requires sustained severe QUIC erasure or a
// no-progress interval while bytes remain in flight, a material collapse from
// the rate this flow already demonstrated, and a recently acknowledged probe
// over TCP to the same endpoint.
type degradationDetector struct {
	lastAt       time.Time
	lastBytes    uint64
	lastPackets  uint64
	lastLost     uint64
	lastProgress time.Time
	baselineRate float64
	badSince     time.Time
}

func (d *degradationDetector) observe(now time.Time, snapshot flowSnapshot, tcpHealthy bool) (degradationDecision, bool) {
	if d.lastAt.IsZero() {
		d.lastAt, d.lastBytes, d.lastProgress = now, snapshot.Bytes, now
		d.lastPackets, d.lastLost = snapshot.PacketsSent, snapshot.PacketsLost
		return degradationDecision{}, false
	}
	elapsed := now.Sub(d.lastAt)
	if elapsed <= 0 {
		return degradationDecision{}, false
	}
	delta := snapshot.Bytes - d.lastBytes
	packetDelta := snapshot.PacketsSent - d.lastPackets
	lostDelta := snapshot.PacketsLost - d.lastLost
	rate := float64(delta) / elapsed.Seconds()
	d.lastAt, d.lastBytes = now, snapshot.Bytes
	d.lastPackets, d.lastLost = snapshot.PacketsSent, snapshot.PacketsLost
	erasure := snapshot.Erasure
	if packetDelta > 0 {
		intervalLoss := math.Min(1, float64(lostDelta)/float64(packetDelta))
		if intervalLoss > erasure {
			erasure = intervalLoss
		}
	}
	if delta > 0 {
		d.lastProgress = now
	}
	// Learn only from a reasonably clean period. Updating the baseline after
	// QoS begins would teach the detector that the collapsed rate is normal.
	if erasure < 0.20 && rate >= degradationMinBaselineRate {
		if d.baselineRate == 0 {
			d.baselineRate = rate
		} else {
			d.baselineRate = 0.8*d.baselineRate + 0.2*rate
		}
	}
	window := degradationMinWindow
	if rttWindow := 4 * snapshot.CurrentRTT; rttWindow > window {
		window = rttWindow
	}
	severeErasure := erasure >= degradationErasureThreshold
	rateCollapsed := d.baselineRate >= degradationMinBaselineRate && rate <= degradationRateFraction*d.baselineRate
	stalled := snapshot.BytesInFlight > 0 && !d.lastProgress.IsZero() && now.Sub(d.lastProgress) >= window
	// The no-progress duration is already the full sustained observation
	// window. Requiring badSince to run for another window would double the
	// blackout delay without adding evidence.
	if stalled && tcpHealthy {
		decision := degradationDecision{
			reason: "sustained_no_progress", observedFor: now.Sub(d.lastProgress), erasure: erasure,
			currentRate: uint64(math.Max(0, rate)), baselineRate: uint64(math.Max(0, d.baselineRate)),
			rtt: snapshot.CurrentRTT,
		}
		d.badSince = time.Time{}
		return decision, true
	}
	bad := severeErasure && (rateCollapsed || erasure >= degradationDirectErasure)
	if !bad {
		d.badSince = time.Time{}
		return degradationDecision{}, false
	}
	if d.badSince.IsZero() {
		d.badSince = now
		return degradationDecision{}, false
	}
	// Keep QUIC evidence while the one standby is being claimed by another
	// flow. TCP health is mandatory at the decision point, but forcing every
	// concurrent flow to repeat the full observation window after the standby
	// manager replenishes would add avoidable seconds to a shared-path event.
	if !tcpHealthy || now.Sub(d.badSince) < window {
		return degradationDecision{}, false
	}
	reason := "sustained_erasure_and_rate_collapse"
	if !rateCollapsed {
		reason = "sustained_severe_erasure"
	}
	decision := degradationDecision{
		reason: reason, observedFor: now.Sub(d.badSince), erasure: erasure,
		currentRate: uint64(math.Max(0, rate)), baselineRate: uint64(math.Max(0, d.baselineRate)),
		rtt: snapshot.CurrentRTT,
	}
	d.badSince = time.Time{}
	return decision, true
}
