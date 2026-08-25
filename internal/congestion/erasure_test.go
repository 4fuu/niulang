package congestion

import (
	"math/rand"
	"testing"
	"time"

	"github.com/apernet/quic-go/monotime"

	quiccongestion "github.com/apernet/quic-go/congestion"

	"github.com/bojieli/queqiao/internal/lossmodel"
	"github.com/bojieli/queqiao/internal/pathmodel"
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

func TestExplicitCongestionOutcomesDoNotInferAmbiguousPacketGaps(t *testing.T) {
	e := NewErasureSender(1200)
	acked := []quiccongestion.AckedPacketInfo{
		{PacketNumber: 1000, BytesAcked: 1200},
		{PacketNumber: 0, BytesAcked: 1200},
	}
	lost := []quiccongestion.LostPacketInfo{{PacketNumber: 500, BytesLost: 1200}}
	e.OnCongestionEventEx(0, monotime.Now(), acked, lost)
	snapshot := e.estimator.Snapshot()
	if snapshot.Decided != 3 {
		t.Fatalf("three explicit outcomes decided %d packet fates", snapshot.Decided)
	}
	if snapshot.Loss < 0.32 || snapshot.Loss > 0.34 {
		t.Fatalf("one explicit loss in three outcomes measured %.3f", snapshot.Loss)
	}
}

// A first flight can overrun a clean path's queue before the controller has
// found its bottleneck. Those losses arrive in runs: they are evidence of
// congestion, not evidence that the channel erases packets independently of
// rate. Trusting the partial-round minimum here creates positive feedback --
// the controller compensates for its own queue drops and sends still faster.
func TestClusteredStartupLossDoesNotBecomeAnErasureFloor(t *testing.T) {
	e := NewErasureSender(1200)
	var acked []quiccongestion.AckedPacketInfo
	var lost []quiccongestion.LostPacketInfo
	for pn := 0; pn < 1000; pn++ {
		if pn%100 < 50 {
			acked = append(acked, quiccongestion.AckedPacketInfo{
				PacketNumber: quiccongestion.PacketNumber(pn), BytesAcked: 1200,
			})
		} else {
			lost = append(lost, quiccongestion.LostPacketInfo{
				PacketNumber: quiccongestion.PacketNumber(pn), BytesLost: 1200,
			})
		}
	}
	e.OnCongestionEventEx(0, monotime.Now(), acked, lost)

	if snapshot := e.estimator.Snapshot(); snapshot.Floor < 0.4 || snapshot.Memoryless {
		t.Fatalf("test pattern is not an untrusted clustered floor: %+v", snapshot)
	}
	if got := e.arrivalRate(); got < 0.97 {
		t.Fatalf("clustered startup loss produced arrival rate %.3f, want no material compensation", got)
	}
}

// Gating the floor on evidence must not turn the erasure controller into plain
// BBR. Once enough independent losses have been observed, compensation should
// still converge on the channel's actual arrival rate.
func TestIndependentLossBecomesAnErasureFloor(t *testing.T) {
	e := NewErasureSender(1200)
	rng := rand.New(rand.NewSource(1))
	var acked []quiccongestion.AckedPacketInfo
	var lost []quiccongestion.LostPacketInfo
	for pn := 0; pn < 5000; pn++ {
		if rng.Float64() >= 0.42 {
			acked = append(acked, quiccongestion.AckedPacketInfo{
				PacketNumber: quiccongestion.PacketNumber(pn), BytesAcked: 1200,
			})
		} else {
			lost = append(lost, quiccongestion.LostPacketInfo{
				PacketNumber: quiccongestion.PacketNumber(pn), BytesLost: 1200,
			})
		}
	}
	e.OnCongestionEventEx(0, monotime.Now(), acked, lost)

	if got := e.arrivalRate(); got < 0.54 || got > 0.62 {
		t.Fatalf("independent 42%% loss produced arrival rate %.3f, want about 0.58", got)
	}
}

// A real erasure channel needs a conservative bootstrap before the formal
// memoryless verdict has enough transitions. Otherwise BBR backs away from the
// channel first and a short transfer spends most of its life recovering.
func TestIndependentLossBootstrapsBeforeTheMemorylessVerdict(t *testing.T) {
	e := NewErasureSender(1200)
	rng := rand.New(rand.NewSource(2))
	var acked []quiccongestion.AckedPacketInfo
	var lost []quiccongestion.LostPacketInfo
	for pn := 0; pn < 180; pn++ {
		if rng.Float64() >= 0.42 {
			acked = append(acked, quiccongestion.AckedPacketInfo{
				PacketNumber: quiccongestion.PacketNumber(pn), BytesAcked: 1200,
			})
		} else {
			lost = append(lost, quiccongestion.LostPacketInfo{
				PacketNumber: quiccongestion.PacketNumber(pn), BytesLost: 1200,
			})
		}
	}
	e.OnCongestionEventEx(0, monotime.Now(), acked, lost)

	if snapshot := e.estimator.Snapshot(); snapshot.Memoryless {
		t.Fatalf("test already reached the formal memoryless verdict: %+v", snapshot)
	}
	if got := e.arrivalRate(); got >= 0.9 || got < 0.5 {
		t.Fatalf("early independent loss produced arrival rate %.3f, want conservative compensation", got)
	}
}

func TestPartiallyClusteredLossDoesNotBootstrapCompensation(t *testing.T) {
	snapshot := lossmodel.Snapshot{
		Decided:          1000,
		Samples:          1000,
		Loss:             0.90,
		LossAfterArrival: 0.70,
		Floor:            0.88,
	}
	if floor, _ := conservativeErasureFloor(snapshot); floor != 0 {
		t.Fatalf("partially clustered loss produced erasure floor %.3f", floor)
	}
}

func TestExtremeLossRequiresTheFormalVerdict(t *testing.T) {
	snapshot := lossmodel.Snapshot{
		Decided:          1000,
		Samples:          1000,
		Loss:             0.85,
		LossAfterArrival: 0.84,
		BurstFactor:      1,
		Floor:            0.85,
	}
	if floor, _ := conservativeErasureFloor(snapshot); floor != 0 {
		t.Fatalf("unconfirmed extreme loss produced erasure floor %.3f", floor)
	}
	snapshot.Memoryless = true
	if floor, _ := conservativeErasureFloor(snapshot); floor != 0 {
		t.Fatalf("short formal verdict produced extreme floor %.3f", floor)
	}
	snapshot.Decided = 2 * lossmodel.DefaultRoundSamples
	if floor, _ := conservativeErasureFloor(snapshot); floor != snapshot.Floor {
		t.Fatalf("formal verdict produced floor %.3f, want %.3f", floor, snapshot.Floor)
	}
}

func TestAnEstablishedEarlyFloorSurvivesLaterClustering(t *testing.T) {
	e := NewErasureSender(1200)
	early := lossmodel.Snapshot{
		Decided:          120,
		Samples:          120,
		Loss:             0.42,
		LossAfterArrival: 0.41,
		Floor:            0.38,
	}
	if floor, _ := e.establishedErasureFloor(early); floor != early.LossAfterArrival {
		t.Fatalf("early independent floor = %.3f, want %.3f", floor, early.LossAfterArrival)
	}

	clustered := lossmodel.Snapshot{
		Decided:          1000,
		Samples:          1000,
		Loss:             0.75,
		LossAfterArrival: 0.40,
		Floor:            0.60,
	}
	if floor, _ := e.establishedErasureFloor(clustered); floor != early.LossAfterArrival {
		t.Fatalf("later clustering moved established floor to %.3f, want %.3f", floor, early.LossAfterArrival)
	}

	completed := clustered
	completed.Decided = lossmodel.DefaultRoundSamples
	completed.Floor = 0.41
	if floor, _ := e.establishedErasureFloor(completed); floor != completed.Floor {
		t.Fatalf("completed-round floor = %.3f, want %.3f", floor, completed.Floor)
	}
}

// A saturated path can keep every round congested long enough for the
// estimator's entire minimum window to rise. That observation is not a new
// physical erasure floor: compensating for it would add parity and pacing
// pressure to the queue that caused it.
func TestCompletedCongestedRoundsCannotRaiseAnEstablishedFloor(t *testing.T) {
	e := NewErasureSender(1200)
	established := lossmodel.Snapshot{
		Decided:          200,
		Samples:          200,
		Loss:             0.42,
		LossAfterArrival: 0.42,
		BurstFactor:      1,
		Floor:            0.42,
	}
	if floor, _ := e.establishedErasureFloor(established); floor != 0.42 {
		t.Fatalf("established floor = %.3f, want 0.420", floor)
	}

	overloaded := lossmodel.Snapshot{
		Decided:          10 * lossmodel.DefaultRoundSamples,
		Samples:          4 * lossmodel.DefaultRoundSamples,
		Loss:             0.72,
		LossAfterArrival: 0.50,
		BurstFactor:      1.4,
		Floor:            0.66,
	}
	if floor, _ := e.establishedErasureFloor(overloaded); floor != 0.42 {
		t.Fatalf("congested rounds raised established floor to %.3f, want 0.420", floor)
	}
}

func TestIndependentEvidenceCanLowerAnEstablishedFloor(t *testing.T) {
	e := NewErasureSender(1200)
	first := lossmodel.Snapshot{
		Decided:          200,
		Samples:          200,
		Loss:             0.42,
		LossAfterArrival: 0.42,
		BurstFactor:      1,
		Floor:            0.42,
	}
	e.establishedErasureFloor(first)

	cleaner := lossmodel.Snapshot{
		Decided:          2 * lossmodel.DefaultRoundSamples,
		Samples:          lossmodel.DefaultRoundSamples,
		Loss:             0.30,
		LossAfterArrival: 0.30,
		BurstFactor:      1,
		Floor:            0.30,
		Memoryless:       true,
	}
	if floor, _ := e.establishedErasureFloor(cleaner); floor != 0.30 {
		t.Fatalf("lower independent floor = %.3f, want 0.300", floor)
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

// A controller joining a path something else has already measured must start
// from what is known rather than at the initial window. On a channel that
// erases 40% of packets the ramp is the expensive part, and a lane opened to
// replace one that died would otherwise repeat it on a path nothing has
// forgotten.
func TestAJoiningSenderStartsFromWhatIsAlreadyKnown(t *testing.T) {
	model := pathmodel.NewPathModel()
	const perMember = 2e6
	model.Report(1, 0.42, 5000, 5000, perMember, 0)
	model.Report(2, 0.42, 5000, 5000, perMember, 0)

	seeded := NewErasureSenderOn(1200, model)
	if seeded.Share() <= 0 {
		t.Fatal("a sender joining a measured path was given no share")
	}
	fresh := NewErasureSender(1200)
	if seeded.bandwidth() <= fresh.bandwidth() {
		t.Fatalf("seeded sender starts at %d, no better than an unseeded %d",
			seeded.bandwidth(), fresh.bandwidth())
	}
}

func TestAJoiningSenderUsesButDoesNotClaimTheInheritedFloor(t *testing.T) {
	model := pathmodel.NewPathModel()
	model.Report(1, 0.42, 5000, 5000, 2e6, 250*time.Millisecond)

	seeded := NewErasureSenderOn(1200, model)
	if seeded.floorTrusted || seeded.establishedFloor != 0 {
		t.Fatalf("joining sender claimed trusted=%t local floor=%.3f before measuring",
			seeded.floorTrusted, seeded.establishedFloor)
	}
	if got := seeded.arrivalRate(); got < 0.579 || got > 0.581 {
		t.Fatalf("joining sender arrival rate = %.3f, want 0.580", got)
	}

	// Its first local sample is intentionally too small to establish a floor;
	// zero weight leaves the shared model's retained value intact.
	floor, samples := seeded.establishedErasureFloor(lossmodel.Snapshot{Decided: 12, Samples: 12})
	if floor != 0 || samples != 0 {
		t.Fatalf("first replacement report = floor %.3f samples %.0f, want no local verdict", floor, samples)
	}
	if got := model.Report(seeded.id(), floor, samples, 12, 0, 0).Floor; got != 0.42 {
		t.Fatalf("untrusted replacement report erased retained floor: %.3f", got)
	}

	// A new connection is the measurement generation boundary. If this path
	// really changed, its own independent evidence must be allowed to replace
	// the inherited value in either direction; treating the inheritance as a
	// local lower envelope would lock a worse path to the old rate forever.
	changed := lossmodel.Snapshot{
		Decided: 200, Samples: 200, Loss: 0.55, LossAfterArrival: 0.55,
		BurstFactor: 1, Floor: 0.55,
	}
	if floor, _ := seeded.establishedErasureFloor(changed); floor != 0.55 {
		t.Fatalf("new connection remained locked to inherited floor: %.3f", floor)
	}
}

// And the window has to be seeded too, not only the rate.
//
// BBR will not send beyond its congestion window however fast it is paced, so
// a sender that knows the path's rate and starts at the initial window spends
// its ramp doubling that window with the pacer idle behind it. Traced live,
// that was 37 KB against a 60 Mbit/s pacing rate, and eight round trips of
// doubling on a path already measured at 15 Mbit/s -- about two seconds of a
// ten-second transfer, paid again by every flow.
func TestAJoiningSenderStartsWithTheWindowItsRateImplies(t *testing.T) {
	const rate, roundTrip = 2e6, 250 * time.Millisecond
	model := pathmodel.NewPathModel()
	model.Report(1, 0.42, 5000, 5000, rate, roundTrip)

	seeded := NewErasureSenderOn(1200, model)
	fresh := NewErasureSender(1200)
	if seeded.GetCongestionWindow() <= fresh.GetCongestionWindow() {
		t.Fatalf("a sender joining a path measured at %.0f bytes/s and %v starts "+
			"with a %d-byte window, no better than an unseeded %d",
			rate, roundTrip, seeded.GetCongestionWindow(), fresh.GetCongestionWindow())
	}
	// The window it should hold is a round trip of what it will put on the
	// wire, which on an erasing path is more than what arrives.
	want := quiccongestion.ByteCount(rate / (1 - 0.42) * roundTrip.Seconds())
	if got := seeded.GetCongestionWindow(); got < want/2 || got > want*2 {
		t.Errorf("window %d against a bandwidth-delay product of %d", got, want)
	}

	// Without a measured round trip there is no window to compute, and the
	// sender must still start from the rate rather than refusing to start.
	blind := NewErasureSenderOn(1200, func() *pathmodel.PathModel {
		m := pathmodel.NewPathModel()
		m.Report(1, 0.42, 5000, 5000, rate, 0)
		return m
	}())
	if blind.bandwidth() <= NewErasureSender(1200).bandwidth() {
		t.Error("a sender given a rate but no round trip did not use the rate")
	}
}

// A connection whose pipe has emptied must not have to rediscover the path.
//
// The bandwidth filter holds ten rounds, so a connection that spends a while
// carrying small exchanges keeps only what those exchanges delivered. When a
// download then arrives on it, probing climbs a quarter per cycle from there:
// measured live, an estimate that had decayed to 0.4 Mbit/s took nineteen
// seconds to find the 12 Mbit/s the path had all along, on a connection that
// was pooled precisely so that using it again would be cheap.
func TestAnEmptiedPipeKeepsWhatItMeasured(t *testing.T) {
	sender := NewTUICBBRSender(1200)
	sender.minRTT = 250 * time.Millisecond
	const peak = 2 << 20

	// What the connection once measured, and then forgot as its filter aged
	// out over rounds of carrying almost nothing.
	sender.seedBandwidth(peak, sender.minRTT)
	sender.peakBandwidth = peak
	sender.estimator.maxFilter.reset()
	sender.estimator.maxFilter.updateMax(sender.round, monotime.Time(0), peak/32)
	if before := sender.estimator.estimate(); before >= peak {
		t.Fatalf("the filter still holds %d; this test is not testing decay", before)
	}

	// Work arrives on an empty pipe.
	sendTUICPacket(sender, monotime.Now(), 0, 1, 1200)

	if got := sender.estimator.estimate(); got < peak {
		t.Errorf("a connection that had measured %d bytes/s restarted from %d, "+
			"and will spend cycles climbing back to what it already knew", peak, got)
	}
}

// What the controller was charged is not what the path did, and until now only
// the charged figure left the process. On the live gateway that meant a path
// erasing a fifth of its downstream reported single-digit loss: the rest had
// been correctly reclassified as erasure and then dropped from the record, so
// no dashboard could show the erasure this transport exists to handle.
//
// The three counters have to satisfy observed = charged + suppressed, and
// observed has to be the one that tracks the channel.
func TestTheSenderReportsWhatThePathDidAndWhatItWasCharged(t *testing.T) {
	e := NewErasureSender(1200)

	// Establish a memoryless erasure floor the way a real channel does, then
	// keep feeding it at that rate.
	const erasure = 0.2
	rng := rand.New(rand.NewSource(7))
	number := quiccongestion.PacketNumber(0)
	observedLost := 0
	for round := 0; round < 40; round++ {
		var acked []quiccongestion.AckedPacketInfo
		var lost []quiccongestion.LostPacketInfo
		for i := 0; i < 500; i++ {
			if rng.Float64() < erasure {
				lost = append(lost, quiccongestion.LostPacketInfo{PacketNumber: number, BytesLost: 1200})
				observedLost++
			} else {
				acked = append(acked, quiccongestion.AckedPacketInfo{PacketNumber: number, BytesAcked: 1200})
			}
			number++
		}
		e.OnCongestionEventEx(0, monotime.Now(), acked, lost)
	}

	telemetry := e.Telemetry()
	_, suppressed, passed := e.Channel()
	t.Logf("observed=%d charged=%d suppressed=%d (channel lost %d of %d)",
		telemetry.PacketsLostObserved, telemetry.PacketsLost, telemetry.PacketsLostSuppressed,
		observedLost, number)

	if telemetry.PacketsLostObserved != telemetry.PacketsLost+telemetry.PacketsLostSuppressed {
		t.Fatalf("observed %d != charged %d + suppressed %d, so the three cannot be read together",
			telemetry.PacketsLostObserved, telemetry.PacketsLost, telemetry.PacketsLostSuppressed)
	}
	if telemetry.PacketsLost != passed || telemetry.PacketsLostSuppressed != suppressed {
		t.Fatalf("telemetry (charged=%d suppressed=%d) disagrees with the sender's own counters (%d/%d)",
			telemetry.PacketsLost, telemetry.PacketsLostSuppressed, passed, suppressed)
	}
	if telemetry.PacketsLostObserved != uint64(observedLost) {
		t.Fatalf("observed %d losses, the channel produced %d", telemetry.PacketsLostObserved, observedLost)
	}
	// The point of the split: a floor was established, so most of the loss was
	// correctly withheld from the controller, and the charged figure alone
	// would understate the channel by a wide margin.
	if telemetry.PacketsLostSuppressed == 0 {
		t.Fatal("nothing was suppressed on a 20% memoryless erasure channel; the floor never established")
	}
	if telemetry.PacketsLost >= telemetry.PacketsLostObserved/2 {
		t.Fatalf("charged %d of %d observed: the controller absorbed most of an erasure channel",
			telemetry.PacketsLost, telemetry.PacketsLostObserved)
	}
}

// A controller that classifies nothing must still answer the same question, or
// a dashboard cannot read one metric across the kinds this build ships.
func TestControllersThatDoNotClassifyReportObservedEqualToCharged(t *testing.T) {
	for name, sender := range map[string]interface {
		OnCongestionEventEx(quiccongestion.ByteCount, monotime.Time, []quiccongestion.AckedPacketInfo, []quiccongestion.LostPacketInfo)
		Telemetry() ControllerTelemetry
	}{
		"bbr-tuic": NewTUICBBRSender(1200),
		"brutal":   NewBrutalSender(1_000_000, false),
	} {
		t.Run(name, func(t *testing.T) {
			sender.OnCongestionEventEx(12_000, monotime.Now(), nil, losses(6))
			got := sender.Telemetry()
			if got.PacketsLostObserved != got.PacketsLost {
				t.Fatalf("observed %d against charged %d for a controller that classifies nothing",
					got.PacketsLostObserved, got.PacketsLost)
			}
			if got.PacketsLostSuppressed != 0 {
				t.Fatalf("suppressed %d for a controller that suppresses nothing", got.PacketsLostSuppressed)
			}
		})
	}
}
