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
