package fec

import (
	"bytes"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/lossmodel"
)

func livePath() Params {
	return Params{
		Class:           ClassBulk,
		ShardBytes:      1200,
		RateBytesPerSec: 25e6 / 8,
		RoundTrip:       300 * time.Millisecond,
		InterleaveDepth: 1,
		TargetResidual:  1e-3,
	}
}

// The channel measured on the live path, below its knee: 42% loss, independent.
func liveSnapshot() lossmodel.Snapshot {
	return lossmodel.Snapshot{
		Samples: 20000, Loss: 0.42, Floor: 0.42, Recent: 0.42,
		LossAfterArrival: 0.42, ArrivalAfterLoss: 0.58,
		MeanBurst: 1.72, BurstFactor: 1.0, Memoryless: true,
	}
}

// A plan is only worth anything if a code built to it actually survives the
// channel it was sized for. This runs the chosen code against the channel the
// snapshot describes and checks the residual it promised.
func TestTheChosenCodeSurvivesTheChannelItWasSizedFor(t *testing.T) {
	if testing.Short() {
		t.Skip("thousands of coded blocks at (124,256)")
	}
	plan := Choose(liveSnapshot(), livePath())
	if !plan.Code {
		t.Fatalf("no code chosen for a 42%% erasure channel: %+v", plan)
	}
	t.Logf("plan: (%d,%d) rate=%.3f overhead=%.2fx residual=%.2e",
		plan.K, plan.N, plan.Rate, plan.Overhead(), plan.Residual)

	codec, err := New(plan.K, plan.N)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	const draws = 4000
	var failures int
	for draw := 0; draw < draws; draw++ {
		shards := make([][]byte, plan.N)
		for i := range shards {
			shards[i] = make([]byte, 256)
		}
		for i := 0; i < plan.K; i++ {
			rng.Read(shards[i])
		}
		if err := codec.Encode(shards); err != nil {
			t.Fatal(err)
		}
		want := snapshot(shards[:plan.K])
		got, present := snapshot(shards), make([]bool, plan.N)
		for i := range present {
			present[i] = rng.Float64() >= 0.42
			if !present[i] {
				got[i] = nil
			}
		}
		switch err := codec.Reconstruct(got, present); err {
		case nil:
			for i := 0; i < plan.K; i++ {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("draw %d: shard %d wrong after repair", draw, i)
				}
			}
		case ErrTooFewShards:
			failures++
		default:
			t.Fatal(err)
		}
	}
	observed := float64(failures) / draws
	t.Logf("observed residual %.2e over %d blocks against a promised %.2e",
		observed, draws, plan.Residual)
	// The promise is an estimate, so it is held to an order of magnitude
	// rather than exactly; what must not happen is the code failing far more
	// often than it said it would.
	if target := livePath().TargetResidual; observed > 10*target {
		t.Fatalf("observed residual %.2e against a target of %.2e", observed, target)
	}
}

// Coding for the loss floor rather than the loss rate is the design's central
// claim. During a congestion episode the code must not inflate: the extra loss
// is a queue overflowing, and parity is more of what overflowed it.
func TestTheCodeIsSizedForTheFloorNotTheSpike(t *testing.T) {
	calm := liveSnapshot()
	congested := calm
	congested.Loss = 0.72
	congested.Recent = 0.72
	congested.Congestive = 0.30
	congested.BurstFactor = 1.6
	congested.MeanBurst = 5.7
	congested.Memoryless = false
	// The floor is unchanged: the channel did not get worse, the queue did.

	before := Choose(calm, livePath())
	during := Choose(congested, livePath())
	t.Logf("calm: (%d,%d) rate=%.3f; congested: (%d,%d) rate=%.3f",
		before.K, before.N, before.Rate, during.K, during.N, during.Rate)

	if !during.Code {
		t.Fatalf("coding abandoned during congestion: %+v", during)
	}
	if during.Overhead() > before.Overhead()*1.35 {
		t.Fatalf("overhead rose from %.2fx to %.2fx on congestion the sender should "+
			"be removing by slowing down, not coding around",
			before.Overhead(), during.Overhead())
	}
}

// A path that is barely lossy must not be made to carry parity. Every parity
// shard is bandwidth taken from data, and on a rare-loss path retransmission is
// cheaper than any code.
func TestACleanPathIsNotCoded(t *testing.T) {
	clean := liveSnapshot()
	clean.Loss, clean.Floor, clean.Recent = 0.001, 0.001, 0.001
	if plan := Choose(clean, livePath()); plan.Code {
		t.Fatalf("a 0.1%% loss path was coded at %.2fx overhead: %+v", plan.Overhead(), plan)
	}
}

// Correlated loss makes a block code weaker at the same loss rate, because a
// burst is one erasure event rather than several independent ones. A controller
// that ignored this would certify a rate the path defeats.
func TestClusteredLossCostsMoreParityThanIndependentLoss(t *testing.T) {
	independent := liveSnapshot()
	clustered := independent
	clustered.BurstFactor = 3
	clustered.MeanBurst = 5.2
	clustered.Memoryless = false

	a, b := Choose(independent, livePath()), Choose(clustered, livePath())
	t.Logf("independent: rate=%.3f overhead=%.2fx; clustered: rate=%.3f overhead=%.2fx",
		a.Rate, a.Overhead(), b.Rate, b.Overhead())
	if !a.Code || !b.Code {
		t.Fatalf("expected both to be coded: %+v %+v", a, b)
	}
	if b.Overhead() <= a.Overhead() {
		t.Fatalf("clustered loss chose overhead %.2fx, no more than independent loss at %.2fx",
			b.Overhead(), a.Overhead())
	}
}

// Interleaving is what converts a burst back into independent erasures, so
// depth has to buy back the rate that clustering cost.
func TestInterleavingRecoversTheRateThatClusteringCost(t *testing.T) {
	clustered := liveSnapshot()
	clustered.BurstFactor = 4
	clustered.Memoryless = false

	flat := Choose(clustered, livePath())
	deep := livePath()
	deep.InterleaveDepth = 4
	interleaved := Choose(clustered, deep)

	t.Logf("no interleave: rate=%.3f; depth 4: rate=%.3f", flat.Rate, interleaved.Rate)
	if interleaved.Rate <= flat.Rate {
		t.Fatalf("interleaving to depth 4 did not raise the code rate: %.3f against %.3f",
			interleaved.Rate, flat.Rate)
	}
	if interleaved.EffectiveBurst > 1.01 {
		t.Fatalf("effective burst %.2f at depth 4 against a burst factor of 4",
			interleaved.EffectiveBurst)
	}
}

// An interactive flow trades rate for a block short enough that a repair beats
// a retransmission by a wide margin. It must not be handed a block that takes
// most of a round trip to send.
func TestInteractiveFlowsGetShorterBlocks(t *testing.T) {
	bulk := Choose(liveSnapshot(), livePath())
	p := livePath()
	p.Class = ClassInteractive
	interactive := Choose(liveSnapshot(), p)

	t.Logf("bulk: (%d,%d); interactive: (%d,%d)", bulk.K, bulk.N, interactive.K, interactive.N)
	if interactive.N >= bulk.N {
		t.Fatalf("interactive block of %d shards is not shorter than the bulk %d",
			interactive.N, bulk.N)
	}
	// And it must still be a usable code, not a token one.
	if !interactive.Code || interactive.Rate < 0.2 {
		t.Fatalf("interactive plan is not usable: %+v", interactive)
	}
	span := time.Duration(float64(interactive.N) * float64(p.ShardBytes) / p.RateBytesPerSec * float64(time.Second))
	if span > p.RoundTrip/2 {
		t.Fatalf("interactive block takes %v of a %v round trip", span, p.RoundTrip)
	}
}

// The code rate can never exceed the channel's capacity, which is (1-p). A plan
// that claimed otherwise would be promising to deliver more than arrives.
func TestNoPlanExceedsChannelCapacity(t *testing.T) {
	for _, loss := range []float64{0.05, 0.2, 0.42, 0.6, 0.75} {
		s := liveSnapshot()
		s.Loss, s.Floor, s.Recent = loss, loss, loss
		s.ArrivalAfterLoss = 1 - loss
		s.MeanBurst = 1 / (1 - loss)
		plan := Choose(s, livePath())
		if !plan.Code {
			t.Logf("loss %.2f: not coded (%s)", loss, plan.Why)
			continue
		}
		t.Logf("loss %.2f: (%d,%d) rate=%.3f overhead=%.2fx residual=%.1e",
			loss, plan.K, plan.N, plan.Rate, plan.Overhead(), plan.Residual)
		if plan.Rate > 1-loss {
			t.Fatalf("loss %.2f: code rate %.3f exceeds the channel capacity %.3f",
				loss, plan.Rate, 1-loss)
		}
	}
}

// A channel too lossy to code at any usable rate must say so rather than
// returning a code that cannot work.
func TestAnImpossibleChannelIsReportedNotCoded(t *testing.T) {
	s := liveSnapshot()
	s.Loss, s.Floor, s.Recent = 0.97, 0.97, 0.97
	plan := Choose(s, livePath())
	if plan.Code {
		t.Fatalf("97%% loss produced a code: %+v", plan)
	}
	if plan.Why == "" {
		t.Fatal("no reason given for refusing to code")
	}
}

// The binomial tail is the arithmetic every plan rests on, so it is checked
// against values computed independently.
func TestBinomialTail(t *testing.T) {
	for _, test := range []struct {
		n    int
		q    float64
		k    int
		want float64
	}{
		{n: 10, q: 0.5, k: 1, want: 0.0009765625},  // P(X = 0)
		{n: 10, q: 0.5, k: 6, want: 0.623046875},   // P(X <= 5)
		{n: 1, q: 0.3, k: 1, want: 0.7},            // P(X = 0)
		{n: 64, q: 0.58, k: 0, want: 0},            // vacuous
		{n: 64, q: 0.58, k: 65, want: 1},           // certain
		{n: 20, q: 0.9, k: 20, want: 0.8784233454}, // P(X <= 19) = 1 - 0.9^20
	} {
		got := binomialTailBelow(test.n, test.q, test.k)
		if math.Abs(got-test.want) > 1e-9 {
			t.Errorf("binomialTailBelow(%d, %v, %d) = %.10f, want %.10f",
				test.n, test.q, test.k, got, test.want)
		}
	}
}
