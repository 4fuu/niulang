package congestion

import (
	"testing"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

func TestWireSchedulerSharesOneRateAndRepaysConcurrentOvershoot(t *testing.T) {
	packetBytes := uint64(quiccongestion.InitialPacketSize)
	scheduler := NewWireScheduler(packetBytes*1000, 0)
	start := monotime.Now()
	for range maxBurstPackets {
		if overshoot := scheduler.charge(start, packetBytes, false); overshoot {
			t.Fatal("initial bounded burst reported overshoot")
		}
	}
	if deadline := scheduler.timeUntilSend(start, false); deadline.IsZero() {
		t.Fatal("aggregate scheduler admitted a packet beyond its bounded burst")
	}
	if overshoot := scheduler.charge(start, packetBytes, false); !overshoot {
		t.Fatal("concurrent packet outside the burst was not reported")
	}
	deadline := scheduler.timeUntilSend(start, false)
	if wait := deadline.Sub(start); wait < 1900*time.Microsecond || wait > 2100*time.Microsecond {
		t.Fatalf("overshoot debt = %v, want two packet intervals", wait)
	}
}

func TestWireSchedulerKeepsReserveUnavailableToBulk(t *testing.T) {
	scheduler := NewWireScheduler(1_200_000, 240_000)
	start := monotime.Now()
	// Offer bulk at the total rate for long enough to exhaust only its lower
	// service rate. The aggregate still has room for an interactive packet.
	for packet := range 60 {
		scheduler.charge(start.Add(time.Duration(packet)*time.Millisecond), 1200, true)
	}
	now := start.Add(59 * time.Millisecond)
	bulkDeadline := scheduler.timeUntilSend(now, true)
	interactiveDeadline := scheduler.timeUntilSend(now, false)
	if bulkDeadline.IsZero() {
		t.Fatal("bulk consumed its bounded share without pacing")
	}
	if !interactiveDeadline.IsZero() && interactiveDeadline >= bulkDeadline {
		t.Fatalf("interactive deadline %v did not precede bulk deadline %v",
			interactiveDeadline.Sub(start), bulkDeadline.Sub(start))
	}
	if wait := interactiveDeadline.Sub(now); !interactiveDeadline.IsZero() && wait > 1100*time.Microsecond {
		t.Fatalf("bulk consumed more than one aggregate packet slot: %v", wait)
	}
}

func TestWireCapSenderPreservesExtendedErasureEventsAndTelemetry(t *testing.T) {
	inner := NewErasureSender(1200)
	scheduler := NewWireScheduler(1_000_000, 100_000)
	sender := NewWireCapSender(inner, inner, scheduler, 1_000_000, 100_000, false)
	sender.OnCongestionEventEx(1200, monotime.Now(), []quiccongestion.AckedPacketInfo{{
		PacketNumber: 1, BytesAcked: 1200,
	}}, []quiccongestion.LostPacketInfo{{PacketNumber: 2, BytesLost: 1200}})
	sender.OnPacketSent(monotime.Now(), 0, 3, 1200, true)
	telemetry := sender.Telemetry()
	if telemetry.Kind != "erasure" || telemetry.WireCapRate != 1_000_000 ||
		telemetry.WireCapBulkRate != 900_000 || telemetry.WireCapBytes != 1200 {
		t.Fatalf("wire wrapper lost inner telemetry or accounting: %+v", telemetry)
	}
	sender.SetBulk(true)
	if telemetry = sender.Telemetry(); !telemetry.WireCapBulk {
		t.Fatal("wire wrapper did not switch connection class")
	}
}
