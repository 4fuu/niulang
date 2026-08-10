package congestion

import (
	"testing"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

func TestTUICMinMaxRetainsPeakForTenRounds(t *testing.T) {
	m := newTUICMinMax()
	m.updateMax(1, 100)
	m.updateMax(3, 120)
	m.updateMax(5, 160)
	m.updateMax(7, 100)
	m.updateMax(10, 100)
	if got := m.get(); got != 160 {
		t.Fatalf("peak was lost inside filter window: %d", got)
	}
	m.updateMax(16, 100)
	if got := m.get(); got != 100 {
		t.Fatalf("expired peak remained in filter: %d", got)
	}
}

func TestTUICBandwidthUsesMinimumOfSendAndACKRates(t *testing.T) {
	e := newTUICBandwidthEstimator()
	start := monotime.Now()
	e.onSent(start, 1200)
	e.onSent(start.Add(10*time.Millisecond), 1200)
	e.onAck(start.Add(210*time.Millisecond), 1200, 1, false)
	e.onSent(start.Add(220*time.Millisecond), 1200)
	e.onSent(start.Add(230*time.Millisecond), 1200)
	e.onAck(start.Add(410*time.Millisecond), 1200, 1, false)
	if got := e.estimate(); got == 0 || got > 130*1024 {
		t.Fatalf("unexpected first bandwidth estimate: %d", got)
	}
}

func TestTUICBandwidthAggregatesCoalescedAckBatch(t *testing.T) {
	e := newTUICBandwidthEstimator()
	start := monotime.Now()
	// Send at 1 MiB/s for one RTT, then acknowledge a 64 KiB burst in one
	// event. The aggregate event must produce a useful ACK slope; processing
	// each packet with the same timestamp would only account for one packet.
	for i := 0; i < 64; i++ {
		e.onSent(start.Add(time.Duration(i)*time.Millisecond), 1024)
	}
	e.onAckEvent(start.Add(264*time.Millisecond), 64*1024, 1, false)
	for i := 0; i < 64; i++ {
		e.onSent(start.Add(264*time.Millisecond+time.Duration(i)*time.Millisecond), 1024)
	}
	e.onAckEvent(start.Add(528*time.Millisecond), 64*1024, 2, false)
	if got := e.estimate(); got < 200*1024 {
		t.Fatalf("coalesced ACK batch was underestimated: %d B/s", got)
	}
}

func TestTUICPacketSamplerUsesPerPacketSendState(t *testing.T) {
	e := newTUICBandwidthEstimator()
	start := monotime.Now()
	var pn quiccongestion.PacketNumber
	first := make([]quiccongestion.AckedPacketInfo, 0, 50)
	for i := 0; i < 50; i++ {
		e.onSentPacket(start.Add(time.Duration(i)*time.Millisecond), pn, 1200, uint64(i)*1200, true)
		first = append(first, quiccongestion.AckedPacketInfo{PacketNumber: pn, BytesAcked: 1200})
		pn++
	}
	e.onAckBatch(start.Add(100*time.Millisecond), first, 1)
	if got := e.estimate(); got == 0 {
		t.Fatal("the initial flight did not seed a delivery-rate sample")
	}
	second := make([]quiccongestion.AckedPacketInfo, 0, 50)
	for i := 0; i < 50; i++ {
		e.onSentPacket(start.Add(101*time.Millisecond+time.Duration(i)*time.Millisecond), pn, 1200, uint64(50+i)*1200, true)
		second = append(second, quiccongestion.AckedPacketInfo{PacketNumber: pn, BytesAcked: 1200})
		pn++
	}
	e.onAckBatch(start.Add(200*time.Millisecond), second, 2)
	if got := e.estimate(); got < 500*1024 {
		t.Fatalf("packet-state sampler underestimated ACK batch: %d B/s", got)
	}
}

func TestTUICPacketSamplerRetainsSpuriousLossState(t *testing.T) {
	e := newTUICBandwidthEstimator()
	start := monotime.Now()
	e.onSentPacket(start, 0, 1200, 0, true)
	e.onSentPacket(start.Add(time.Millisecond), 1, 1200, 1200, true)
	e.onLost(0)
	if _, ok := e.packetStates[0]; !ok {
		t.Fatal("loss discarded delivery state before a possible spurious ACK")
	}
	result := e.onAckBatch(start.Add(100*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: 0, BytesAcked: 1200}}, 1)
	if !result.hasSample || e.estimate() == 0 || e.totalAcked != 1200 {
		t.Fatalf("spurious ACK did not repair the delivery model: sample=%v estimate=%d acked=%d", result.hasSample, e.estimate(), e.totalAcked)
	}
}

func TestTUICAppLimitedMarkerIsPacketScoped(t *testing.T) {
	e := newTUICBandwidthEstimator()
	start := monotime.Now()
	e.onSentPacket(start, 0, 1200, 0, true)
	e.markAppLimited()
	e.onSentPacket(start.Add(time.Millisecond), 1, 1200, 1200, true)
	if e.packetStates[0].appLimited || !e.packetStates[1].appLimited {
		t.Fatal("app-limited marker was not captured at packet send time")
	}
	e.onAckBatch(start.Add(100*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: 0, BytesAcked: 1200}}, 1)
	if !e.appLimited {
		t.Fatal("ACK of a pre-marker packet ended the app-limited phase")
	}
	result := e.onAckBatch(start.Add(200*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: 1, BytesAcked: 1200}}, 2)
	if !result.lastAppLimited || e.appLimited {
		t.Fatalf("phase boundary was not preserved: sample_app_limited=%v phase=%v", result.lastAppLimited, e.appLimited)
	}
	e.onSentPacket(start.Add(201*time.Millisecond), 2, 1200, 0, true)
	if e.packetStates[2].appLimited {
		t.Fatal("packet after the phase boundary remained app-limited")
	}
}

func TestTUICFirstPacketStartsFirstRound(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 100 * time.Millisecond})
	start := monotime.Now()
	sender.OnPacketSent(start, 0, 0, 1200, true)
	sender.OnCongestionEventEx(1200, start.Add(100*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: 0, BytesAcked: 1200}}, nil)
	if sender.round != 1 {
		t.Fatalf("packet zero did not start the first BBR round: %d", sender.round)
	}
}

func TestTUICLossRecoveryPreservesRateModelAtQuarterLoss(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 200 * time.Millisecond})
	start := monotime.Now()
	var pn quiccongestion.PacketNumber
	for round := 0; round < 12; round++ {
		flightStart := start.Add(time.Duration(round) * 200 * time.Millisecond)
		var inFlight quiccongestion.ByteCount
		acked := make([]quiccongestion.AckedPacketInfo, 0, 24)
		lost := make([]quiccongestion.LostPacketInfo, 0, 8)
		for i := 0; i < 32; i++ {
			sent := flightStart.Add(time.Duration(i) * time.Millisecond)
			sender.OnPacketSent(sent, inFlight, pn, 1200, true)
			inFlight += 1200
			if i%4 == 0 {
				lost = append(lost, quiccongestion.LostPacketInfo{PacketNumber: pn, BytesLost: 1200})
			} else {
				acked = append(acked, quiccongestion.AckedPacketInfo{PacketNumber: pn, BytesAcked: 1200})
			}
			pn++
		}
		sender.OnCongestionEventEx(inFlight, flightStart.Add(200*time.Millisecond), acked, lost)
	}
	if got := sender.estimator.estimate(); got < 100*1024 {
		t.Fatalf("quarter-loss recovery collapsed the delivery model: %d B/s", got)
	}
	if got := sender.GetCongestionWindow(); got <= sender.minCwnd {
		t.Fatalf("quarter-loss recovery collapsed cwnd to floor: %d", got)
	}
}

func TestTUICRateArithmeticSaturates(t *testing.T) {
	if got := rateFromDelta(^uint64(0), time.Nanosecond); got != ^uint64(0) {
		t.Fatalf("rate arithmetic wrapped: %d", got)
	}
	if got := saturatingByteAdd(maxCongestionByteCount-1, 2); got != maxCongestionByteCount {
		t.Fatalf("byte addition wrapped: %d", got)
	}
}

func TestTUICRecoverySaturatesOversizedLoss(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.recovery = tuicConservation
	sender.recoveryWindow = sender.initialCwnd
	sender.calculateRecoveryWindow(0, ^uint64(0), sender.initialCwnd)
	if sender.recoveryWindow < sender.minCwnd || sender.recoveryWindow > sender.initialCwnd {
		t.Fatalf("oversized loss corrupted recovery window: %d", sender.recoveryWindow)
	}
}

func TestTUICBBRSenderStartupAndRecovery(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 200 * time.Millisecond})
	if sender.initialCwnd != tuicInitialPackets*1200 {
		t.Fatalf("unexpected TUIC initial window: %d", sender.initialCwnd)
	}
	if sender.cwndGain != tuicCwndGain {
		t.Fatalf("unexpected TUIC startup cwnd gain: %v", sender.cwndGain)
	}
	if got, want := sender.bandwidth(), quiccongestion.ByteCount(float64(sender.initialCwnd)*tuicHighGain/0.2); got != want {
		// TUIC startup pacing is high_gain*IW/RTT. The high gain is applied
		// exactly once; the startup cwnd gain remains 2.0.
		t.Fatalf("startup pacing=%d, want high_gain*IW/RTT=%d", got, want)
	}
	start := monotime.Now()
	var pn quiccongestion.PacketNumber
	for round := 0; round < 8; round++ {
		for i := 0; i < 32; i++ {
			sent := start.Add(time.Duration(round)*200*time.Millisecond + time.Duration(i)*time.Millisecond)
			sender.OnPacketSent(sent, sender.bytesInFlight, pn, 1200, true)
			pn++
		}
		sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(time.Duration(round+1)*200*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: pn - 1, BytesAcked: 32 * 1200}}, nil)
	}
	if sender.estimator.estimate() == 0 {
		t.Fatal("TUIC-aligned estimator did not record bandwidth")
	}
	if sender.bandwidth() < quiccongestion.ByteCount(tuicDefaultMinRate) {
		t.Fatalf("pacing rate below safety floor: %d", sender.bandwidth())
	}
	before := sender.GetCongestionWindow()
	sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(3*time.Second), nil, []quiccongestion.LostPacketInfo{{PacketNumber: pn - 1, BytesLost: 1200}})
	if !sender.InRecovery() || sender.GetCongestionWindow() > before {
		t.Fatalf("loss did not bound recovery: recovery=%v cwnd=%d before=%d", sender.InRecovery(), sender.GetCongestionWindow(), before)
	}
}

func TestTUICBBRTimeoutReturnsToSafeStartup(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.fullBandwidth = true
	sender.mode = tuicBbrProbeBW
	sender.pacingRate = 10 * 1024 * 1024
	sender.cwnd = 2 * 1024 * 1024
	sender.OnRetransmissionTimeout(true)
	if sender.mode != tuicBbrStartup || sender.fullBandwidth || sender.pacingRate != 0 {
		t.Fatalf("timeout did not reset controller: mode=%v full=%v rate=%d", sender.mode, sender.fullBandwidth, sender.pacingRate)
	}
	if sender.GetCongestionWindow() != sender.minCwnd {
		t.Fatalf("timeout cwnd=%d, want %d", sender.GetCongestionWindow(), sender.minCwnd)
	}
}

func TestTUICTelemetryReportsControllerLoss(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.OnCongestionEventEx(2400, monotime.Now(), nil, []quiccongestion.LostPacketInfo{
		{PacketNumber: 1, BytesLost: 1200}, {PacketNumber: 2, BytesLost: 1200},
	})
	got := sender.Telemetry()
	if got.BytesLost != 2400 || got.PacketsLost != 2 {
		t.Fatalf("loss telemetry=%d bytes/%d packets, want 2400/2", got.BytesLost, got.PacketsLost)
	}
}

func TestTUICTelemetryReportsSamplerDiagnostics(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 100 * time.Millisecond})
	start := monotime.Now()
	sender.OnPacketSent(start, 0, 0, 1200, true)
	sender.OnCongestionEventEx(1200, start.Add(100*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: 0, BytesAcked: 1200}}, nil)
	got := sender.Telemetry()
	if got.LatestSample == 0 || got.LatestAckRate == 0 || got.Samples == 0 || got.NonAppSamples == 0 || got.Round != 1 {
		t.Fatalf("incomplete sampler telemetry: %+v", got)
	}
	if got.AppSamples != 0 || got.StateMisses != 0 {
		t.Fatalf("unexpected sampler diagnostics: %+v", got)
	}
}

func TestTUICStartupLossDoesNotCollapseBeforeBandwidthModel(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 200 * time.Millisecond})
	start := monotime.Now()
	var pn quiccongestion.PacketNumber
	acked := make([]quiccongestion.AckedPacketInfo, 0, 5)
	lost := make([]quiccongestion.LostPacketInfo, 0, 195)
	for i := 0; i < 200; i++ {
		sender.OnPacketSent(start.Add(time.Duration(i)*time.Millisecond), sender.bytesInFlight, pn, 1200, true)
		if i%40 == 39 {
			acked = append(acked, quiccongestion.AckedPacketInfo{PacketNumber: pn, BytesAcked: 1200})
		} else {
			lost = append(lost, quiccongestion.LostPacketInfo{PacketNumber: pn, BytesLost: 1200})
		}
		pn++
	}
	sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(200*time.Millisecond), acked, lost)
	if sender.fullBandwidth {
		t.Fatal("startup loss prematurely declared the bottleneck model complete")
	}
	if sender.InRecovery() {
		t.Fatal("startup loss entered recovery before a delivery model existed")
	}
	if got := sender.GetCongestionWindow(); got <= sender.minCwnd {
		t.Fatalf("startup loss collapsed cwnd to safety floor: %d", got)
	}

	// Once the model is known, the same loss must still enter bounded
	// recovery; this guard prevents the startup exception from becoming an
	// unlimited loss-ignoring mode.
	sender.fullBandwidth = true
	sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(400*time.Millisecond), nil, []quiccongestion.LostPacketInfo{{PacketNumber: pn, BytesLost: 1200}})
	if !sender.InRecovery() {
		t.Fatal("loss after startup did not enter recovery")
	}
}

func TestTUICStartupDoesNotExitOnOnePostModelLoss(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.fullBandwidth = false
	sender.recovery = tuicConservation
	sender.roundsNoGain = 0
	sender.bwAtLastRound = 1
	sender.estimator.maxFilter.updateMax(1, 1)
	sender.checkFullBandwidth(false)
	if sender.fullBandwidth {
		t.Fatal("one recovery event prematurely exited startup")
	}
}

func TestTUICAppLimitedHysteresisPreservesBulkSamples(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 200 * time.Millisecond})
	start := monotime.Now()
	sender.lastSend = start
	if !sender.appLimited(start.Add(150*time.Millisecond), sender.maxDatagramSize, 1200) {
		t.Fatal("small pre-bulk burst was not classified as application-limited")
	}
	sender.estimator.totalSent = uint64(sender.initialCwnd)
	if sender.appLimited(start.Add(150*time.Millisecond), sender.maxDatagramSize, 1200) {
		t.Fatal("short loss-limited bulk ACK was incorrectly classified as application-limited")
	}
	if !sender.appLimited(start.Add(5*time.Second), sender.maxDatagramSize, 1200) {
		t.Fatal("long idle gap was not classified as application-limited")
	}
	if sender.appLimited(start.Add(150*time.Millisecond), sender.initialCwnd, 0) {
		t.Fatal("loss-only full bulk flight was incorrectly marked application-limited")
	}
}
