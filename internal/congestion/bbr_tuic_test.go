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
	if sender.initialCwnd != 200*1200 {
		t.Fatalf("unexpected TUIC initial window: %d", sender.initialCwnd)
	}
	if got, want := sender.bandwidth(), quiccongestion.ByteCount(float64(sender.initialCwnd)/0.2); got != want {
		// With a 200-ms RTT, TUIC startup pacing is exactly IW/RTT. A
		// high-gain multiplication here would create an avoidable queue.
		t.Fatalf("startup pacing=%d, want IW/RTT=%d", got, want)
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
