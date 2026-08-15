package congestion

import (
	"testing"

	quiccongestion "github.com/apernet/quic-go/congestion"

	"github.com/icourses-dev/wanopt/internal/lossmodel"
)

func losses(n int) []quiccongestion.LostPacketInfo {
	out := make([]quiccongestion.LostPacketInfo, n)
	for i := range out {
		out[i] = quiccongestion.LostPacketInfo{PacketNumber: quiccongestion.PacketNumber(i), BytesLost: 1200}
	}
	return out
}

// Until the channel has been measured, every loss is congestion as far as this
// controller knows, and it must behave exactly like the BBR it wraps. Guessing
// early is how a clean path would be driven into collapse.
func TestUnmeasuredLossIsTreatedAsCongestion(t *testing.T) {
	e := NewErasureSender(1200)
	got := e.congestive(lossmodel.Snapshot{}, losses(10))
	if len(got) != 10 {
		t.Fatalf("forwarded %d of 10 losses before the channel was measured", len(got))
	}
}

// A channel whose loss is entirely its floor is not congested at any rate, and
// forwarding those losses is what costs the path: BBR-TUIC delivers 1.56
// Mbit/s where the same path carries 13.89.
func TestChannelLossIsNotForwarded(t *testing.T) {
	e := NewErasureSender(1200)
	snapshot := lossmodel.Snapshot{Decided: 5000, Floor: 0.42, Recent: 0.42}
	var forwarded int
	for i := 0; i < 100; i++ {
		forwarded += len(e.congestive(snapshot, losses(10)))
	}
	if forwarded != 0 {
		t.Fatalf("forwarded %d losses on a channel whose loss is all floor", forwarded)
	}
}

// Loss above the floor is the sender's own, and it must reach BBR or the
// controller has no congestion response at all.
func TestLossAboveTheFloorIsForwarded(t *testing.T) {
	e := NewErasureSender(1200)
	// Two thirds of this loss is above the floor.
	snapshot := lossmodel.Snapshot{Decided: 5000, Floor: 0.24, Recent: 0.72}
	var forwarded int
	for i := 0; i < 100; i++ {
		forwarded += len(e.congestive(snapshot, losses(10)))
	}
	if want := 1000 * 2 / 3; forwarded < want*9/10 || forwarded > want*11/10 {
		t.Fatalf("forwarded %d of 1000 losses, want about %d", forwarded, want)
	}
}

// A congestive share below a half must still be reported. Rounding it away
// would blind the controller to exactly the mild persistent congestion it most
// needs to see, so the fractional part is carried rather than dropped.
func TestASmallCongestiveShareIsNotRoundedAway(t *testing.T) {
	e := NewErasureSender(1200)
	// A tenth of the loss is congestive, and losses arrive one at a time.
	snapshot := lossmodel.Snapshot{Decided: 5000, Floor: 0.45, Recent: 0.5}
	var forwarded int
	for i := 0; i < 1000; i++ {
		forwarded += len(e.congestive(snapshot, losses(1)))
	}
	if forwarded < 80 || forwarded > 120 {
		t.Fatalf("forwarded %d of 1000 single losses at a tenth congestive, want about 100", forwarded)
	}
}

// The compensation is the other half of the design: BBR estimates the rate
// that arrives while its pacer governs the rate that is sent, and on an
// erasure channel those differ by the arrival rate. Without the correction the
// sending rate becomes its own input and walks down to nothing.
func TestThePacingRateIsCompensatedForErasure(t *testing.T) {
	e := NewErasureSender(1200)
	plain := e.inner.bandwidth()

	e.arrival.Store(uint64(0.58 * partsPerMillion))
	compensated := e.bandwidth()
	if want := float64(plain) / 0.58; float64(compensated) < want*0.95 || float64(compensated) > want*1.05 {
		t.Fatalf("compensated bandwidth %d against a delivered estimate of %d at 58%% arrival, want about %.0f",
			compensated, plain, want)
	}

	// A clean path must be left alone.
	e.arrival.Store(partsPerMillion)
	if got := e.bandwidth(); got != plain {
		t.Fatalf("bandwidth %d on a lossless path, want the uncorrected %d", got, plain)
	}
}

// The divisor is bounded. An arrival rate near zero would turn a measurement
// error into an unbounded send rate, which is the one failure mode worse than
// giving up the path.
func TestTheCompensationIsBounded(t *testing.T) {
	e := NewErasureSender(1200)
	e.arrival.Store(uint64(0.001 * partsPerMillion))
	if got := e.arrivalRate(); got != erasureMinArrival {
		t.Fatalf("arrival rate %v at a measured 0.001, want the floor of %v", got, erasureMinArrival)
	}
	plain := e.inner.bandwidth()
	if got := e.bandwidth(); float64(got) > float64(plain)/erasureMinArrival*1.01 {
		t.Fatalf("bandwidth %d exceeds the bounded compensation of %d", got, plain)
	}
}

// The congestion window is compensated for the same reason the rate is: it
// bounds what is on the wire, and on an erasure channel that is more than what
// will arrive.
func TestTheCongestionWindowIsCompensated(t *testing.T) {
	e := NewErasureSender(1200)
	e.arrival.Store(uint64(0.58 * partsPerMillion))
	inner := e.inner.GetCongestionWindow()
	outer := e.GetCongestionWindow()
	if want := float64(inner) / 0.58; float64(outer) < want*0.95 || float64(outer) > want*1.05 {
		t.Fatalf("compensated window %d against an inner %d, want about %.0f", outer, inner, want)
	}
	if !e.CanSend(inner) {
		t.Fatal("a sender at the inner window's limit should still have room on an erasure channel")
	}
	if e.CanSend(outer) {
		t.Fatal("a sender at the compensated window's limit should be blocked")
	}
}
