package congestion

import (
	"testing"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

type fakeRTT struct{ smoothed time.Duration }

func (f *fakeRTT) MinRTT() time.Duration                  { return f.smoothed }
func (f *fakeRTT) LatestRTT() time.Duration               { return f.smoothed }
func (f *fakeRTT) SmoothedRTT() time.Duration             { return f.smoothed }
func (f *fakeRTT) MeanDeviation() time.Duration           { return 0 }
func (f *fakeRTT) MaxAckDelay() time.Duration             { return 0 }
func (f *fakeRTT) PTO(bool) time.Duration                 { return 2 * f.smoothed }
func (f *fakeRTT) UpdateRTT(time.Duration, time.Duration) {}
func (f *fakeRTT) SetMaxAckDelay(time.Duration)           {}
func (f *fakeRTT) SetInitialRTT(rtt time.Duration)        { f.smoothed = rtt }

func TestAdaptiveSenderGrowsOnCleanDeliveryAndBacksOffOnLoss(t *testing.T) {
	sender := NewAdaptiveSender(1200, 64*1024, 8*1024*1024)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 200 * time.Millisecond})
	initialRate := sender.rateBps
	start := monotime.Now()

	sender.OnCongestionEventEx(0, start, []quiccongestion.AckedPacketInfo{{BytesAcked: 32 * 1024}}, nil)
	sender.OnCongestionEventEx(0, start.Add(100*time.Millisecond), []quiccongestion.AckedPacketInfo{{BytesAcked: 64 * 1024}}, nil)
	if sender.rateBps <= initialRate {
		t.Fatalf("clean delivery did not grow rate: initial=%v current=%v", initialRate, sender.rateBps)
	}
	if sender.GetCongestionWindow() < 4*1200 {
		t.Fatalf("congestion window below safety floor: %d", sender.GetCongestionWindow())
	}

	beforeLoss := sender.rateBps
	sender.OnCongestionEventEx(128*1024, start.Add(200*time.Millisecond), nil, []quiccongestion.LostPacketInfo{{BytesLost: 1200}})
	if sender.rateBps >= beforeLoss {
		t.Fatalf("loss did not reduce rate: before=%v after=%v", beforeLoss, sender.rateBps)
	}
	if sender.rateBps < sender.minRateBps {
		t.Fatalf("loss reduced rate below configured floor: %v", sender.rateBps)
	}
}

func TestAdaptiveSenderHonorsBounds(t *testing.T) {
	sender := NewAdaptiveSender(1200, 100_000, 200_000)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 100 * time.Millisecond})
	start := monotime.Now()
	for i := 0; i < 20; i++ {
		sender.OnCongestionEventEx(0, start.Add(time.Duration(i)*100*time.Millisecond), []quiccongestion.AckedPacketInfo{{BytesAcked: 256 * 1024}}, nil)
	}
	if sender.rateBps > 200_000 {
		t.Fatalf("rate exceeded maximum: %v", sender.rateBps)
	}
	for i := 0; i < 20; i++ {
		sender.OnCongestionEventEx(0, start.Add(time.Duration(20+i)*100*time.Millisecond), nil, []quiccongestion.LostPacketInfo{{BytesLost: 1200}})
	}
	if sender.rateBps < 100_000 {
		t.Fatalf("rate fell below minimum: %v", sender.rateBps)
	}
}

func TestBrutalSenderCompensatesForModerateLoss(t *testing.T) {
	sender := NewBrutalSender(1_000_000, false)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 200 * time.Millisecond})
	start := monotime.Now()
	acked := make([]quiccongestion.AckedPacketInfo, 90)
	lost := make([]quiccongestion.LostPacketInfo, 10)
	sender.OnCongestionEventEx(0, start, acked, lost)
	sender.OnCongestionEventEx(0, start.Add(time.Second), acked, lost)
	if sender.ackRate < 0.89 || sender.ackRate > 0.91 {
		t.Fatalf("unexpected ACK rate: %v", sender.ackRate)
	}
	if sender.bandwidth() <= sender.bps {
		t.Fatalf("loss compensation did not raise wire pacing rate: target=%d wire=%d", sender.bps, sender.bandwidth())
	}
}

func TestBBRSenderBuildsDeliveryModelAndRecovers(t *testing.T) {
	sender := NewBBRSender(1200)
	rtt := &fakeRTT{smoothed: 200 * time.Millisecond}
	sender.SetRTTStatsProvider(rtt)
	start := monotime.Now()
	initial := sender.GetCongestionWindow()

	// Feed packet sends and ACK batches over several RTTs. The first ACK seeds
	// the delivery clock; subsequent batches create a real delivery-rate
	// sample and should grow both pacing and the ACK-clocked window.
	var pn quiccongestion.PacketNumber
	for round := 0; round < 8; round++ {
		for i := 0; i < 32; i++ {
			sender.OnPacketSent(start.Add(time.Duration(round)*200*time.Millisecond), sender.bytesInFlight, pn, 1200, true)
			pn++
		}
		sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(time.Duration(round+1)*200*time.Millisecond), []quiccongestion.AckedPacketInfo{{PacketNumber: pn - 1, BytesAcked: 32 * 1200}}, nil)
	}
	if sender.maxBandwidth == 0 {
		t.Fatal("BBR did not record a delivery-rate sample")
	}
	if sender.bandwidth() <= quiccongestion.ByteCount(initial) {
		t.Fatalf("BBR pacing model did not grow: initial window=%d pacing=%d", initial, sender.bandwidth())
	}
	if sender.bandwidth() < bbrMinRate {
		t.Fatalf("BBR pacing fell below safety floor: %d", sender.bandwidth())
	}

	before := sender.GetCongestionWindow()
	sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(3*time.Second), nil, []quiccongestion.LostPacketInfo{{PacketNumber: pn - 1, BytesLost: 1200}})
	if !sender.InRecovery() {
		t.Fatal("loss did not enter recovery")
	}
	if sender.GetCongestionWindow() > before {
		t.Fatalf("loss increased effective window: before=%d after=%d", before, sender.GetCongestionWindow())
	}

	// A clean ACK beyond the recovery boundary exits recovery without ever
	// dropping below the four-packet safety floor.
	sender.OnCongestionEventEx(sender.bytesInFlight, start.Add(4*time.Second), []quiccongestion.AckedPacketInfo{{PacketNumber: pn + 1, BytesAcked: 1200}}, nil)
	if sender.GetCongestionWindow() < sender.minCwnd {
		t.Fatalf("window below minimum after recovery: %d", sender.GetCongestionWindow())
	}
}

func TestBBRSenderTimeoutResetsToSafeStartup(t *testing.T) {
	sender := NewBBRSender(1200)
	sender.SetRTTStatsProvider(&fakeRTT{smoothed: 100 * time.Millisecond})
	sender.maxBandwidth = 10 * 1024 * 1024
	sender.fullBandwidth = true
	sender.mode = bbrProbeBW
	sender.cwnd = 2 * 1024 * 1024
	sender.OnRetransmissionTimeout(true)
	if sender.mode != bbrStartup || sender.fullBandwidth || sender.maxBandwidth != 0 {
		t.Fatalf("timeout did not reset BBR state: mode=%v full=%v bw=%d", sender.mode, sender.fullBandwidth, sender.maxBandwidth)
	}
	if sender.GetCongestionWindow() != sender.minCwnd {
		t.Fatalf("timeout window=%d, want minimum=%d", sender.GetCongestionWindow(), sender.minCwnd)
	}
}

func TestRateControllersDoNotSendAtWindowBoundary(t *testing.T) {
	adaptive := NewAdaptiveSender(1200, 64*1024, 1*1024*1024)
	brutal := NewBrutalSender(1*1024*1024, false)
	bbr := NewBBRSender(1200)
	for name, sender := range map[string]quiccongestion.CongestionControl{
		"adaptive": adaptive,
		"brutal":   brutal,
		"bbr":      bbr,
	} {
		window := sender.GetCongestionWindow()
		if sender.CanSend(window) {
			t.Errorf("%s admitted a packet at its congestion-window boundary", name)
		}
	}
}
