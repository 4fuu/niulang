package fec

import (
	"bytes"
	"math/rand"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/lossmodel"
)

// symbol builds a distinguishable source symbol of the given length.
func symbol(esi uint32, length int) []byte {
	v := make([]byte, length)
	for i := range v {
		v[i] = byte(int(esi)*7 + i*13 + 1)
	}
	return v
}

// run sends count symbols through a window code at the given repair rate,
// erasing each transmission with probability loss, and reports what the
// receiver ended up with.
func run(t *testing.T, count int, length int, capacity int, repairRate float64, loss float64, seed int64) (delivered map[uint32][]byte, lost map[uint32]bool) {
	t.Helper()
	random := rand.New(rand.NewSource(seed))
	encoder := NewWindowEncoder(capacity)
	decoder := NewWindowDecoder()
	delivered, lost = map[uint32][]byte{}, map[uint32]bool{}

	note := func(d Delivery) {
		for _, r := range d.Recovered {
			delivered[r.ESI] = r.Vector
		}
		for _, esi := range d.Lost {
			if _, ok := delivered[esi]; !ok {
				lost[esi] = true
			}
		}
	}

	var rid uint32
	credit := 0.0
	for i := 0; i < count; i++ {
		v := symbol(uint32(i), length)
		esi := encoder.Add(v)
		if random.Float64() >= loss {
			note(decoder.Source(esi, v))
			delivered[esi] = append([]byte(nil), v...)
		}
		for credit += repairRate; credit >= 1; credit-- {
			repair, ok := encoder.Repair(rid, 0)
			rid++
			if !ok {
				continue
			}
			if random.Float64() >= loss {
				note(decoder.Repair(repair))
			}
		}
	}
	// The tail: a transfer that stops still wants its last symbols covered, and
	// nothing further is coming to carry them.
	for i := 0; i < capacity; i++ {
		repair, ok := encoder.Repair(rid, 0)
		rid++
		if !ok {
			break
		}
		if random.Float64() >= loss {
			note(decoder.Repair(repair))
		}
	}
	return delivered, lost
}

// The first thing to establish is that the algebra is right: what the decoder
// reconstructs must be the symbol that was sent, byte for byte, and not merely
// something of the right length.
func TestWindowRecoversTheSymbolThatWasSent(t *testing.T) {
	encoder := NewWindowEncoder(16)
	decoder := NewWindowDecoder()

	var sent [][]byte
	for i := 0; i < 8; i++ {
		v := symbol(uint32(i), 200)
		sent = append(sent, v)
		encoder.Add(v)
	}
	// Every symbol arrives except two, which are covered by two repairs.
	var out []Delivery
	for i, v := range sent {
		if i == 3 || i == 5 {
			continue
		}
		out = append(out, decoder.Source(uint32(i), v))
	}
	for rid := uint32(0); rid < 2; rid++ {
		repair, ok := encoder.Repair(rid, 0)
		if !ok {
			t.Fatal("the encoder held a window but produced no repair")
		}
		out = append(out, decoder.Repair(repair))
	}

	recovered := map[uint32][]byte{}
	for _, d := range out {
		for _, r := range d.Recovered {
			recovered[r.ESI] = r.Vector
		}
	}
	for _, esi := range []uint32{3, 5} {
		got, ok := recovered[esi]
		if !ok {
			t.Fatalf("symbol %d was not recovered from two repairs covering it", esi)
		}
		if !bytes.Equal(got[:len(sent[esi])], sent[esi]) {
			t.Fatalf("symbol %d recovered as different bytes", esi)
		}
	}
}

// At the rate the path is measured to need, almost everything arrives.
//
// The rate is the erasure rate's own: to deliver one symbol where a fraction p
// is erased takes 1/(1-p) transmissions, so p/(1-p) repairs per symbol is the
// break-even and anything above it is margin. This asks for a fifth more than
// break-even at 42% loss and expects the residual to be small -- not zero,
// because a code that never fails costs unboundedly more than one that fails
// rarely, and the session above is built to re-issue what the code drops.
func TestWindowRecoversAtTheMeasuredRate(t *testing.T) {
	const loss = 0.42
	const count = 2000
	rate := loss / (1 - loss) * 1.2

	delivered, lost := run(t, count, 1000, 64, rate, loss, 7)
	if len(delivered) < count-len(lost) {
		t.Fatalf("%d delivered and %d lost do not account for %d symbols",
			len(delivered), len(lost), count)
	}
	residual := float64(len(lost)) / count
	t.Logf("%d of %d symbols lost (%.2f%%) at %.0f%% erasure with %.0f%% repair overhead",
		len(lost), count, residual*100, loss*100, rate*100)
	if residual > 0.02 {
		t.Errorf("residual %.3f is above 2%%; the window is not recovering what "+
			"its repair rate says it should", residual)
	}
	for esi, v := range delivered {
		if want := symbol(esi, 1000); !bytes.Equal(v[:len(want)], want) {
			t.Fatalf("symbol %d delivered as different bytes", esi)
		}
	}
}

// The rate the window asks for has to be the rate the window needs.
//
// WindowRate is sized from a measured property -- that a window's repairs
// chain across neighbouring windows, so the code behaves like a block several
// times as long -- and a measured property is one that can drift as the
// decoder changes. This runs the actual code at the rate it asks for and
// checks the residual it actually gets, at the target it was sized for.
//
// It also checks the other side: a rate meaningfully below what it asks for
// should miss the target. A code that is comfortable at three quarters of its
// stated rate is one paying for a residual nobody asked for, which on this
// path is bandwidth taken from data.
func TestTheWindowRateIsWhatTheWindowNeeds(t *testing.T) {
	const loss, count = 0.42, 20000
	channel := lossmodel.Snapshot{
		Loss: loss, Floor: loss, Recent: loss,
		BurstFactor: 1, ArrivalAfterLoss: 1 - loss,
	}
	params := Params{
		Class: ClassBulk, ShardBytes: 1100, RateBytesPerSec: 2e6,
		RoundTrip: 300 * time.Millisecond, TargetResidual: 1e-3,
	}
	for _, capacity := range []int{16, 32, 64, 128} {
		rate := WindowRate(capacity, channel, params)
		// Averaged over several channels: a failure loses a run of symbols at
		// once, so one channel's residual is a coarse estimate of the rate's.
		residual, slack := 0.0, 0.0
		const channels = 4
		for seed := int64(0); seed < channels; seed++ {
			_, lost := run(t, count, 200, capacity, rate, loss, 13+seed)
			residual += float64(len(lost)) / count / channels
			_, generous := run(t, count, 200, capacity, rate*0.75, loss, 13+seed)
			slack += float64(len(generous)) / count / channels
		}
		t.Logf("capacity %3d: %.2f repairs per symbol gives a residual of %.4f, "+
			"and three quarters of it gives %.4f", capacity, rate, residual, slack)
		if residual > 2*params.TargetResidual {
			t.Errorf("capacity %d: the rate asks for %.2f repairs and delivers a "+
				"residual of %.4f against a target of %.4f", capacity, rate,
				residual, params.TargetResidual)
		}
		if slack <= params.TargetResidual {
			t.Errorf("capacity %d: three quarters of the rate still meets the "+
				"target (%.4f); the rate is buying a residual nobody asked for",
				capacity, slack)
		}
	}
}

// The tail of a burst is the one place a block's arithmetic still applies, and
// it has to hold against the window that actually sends it.
//
// When a producer stops, the symbols it has just sent have nothing following
// them to share parity with, so the repairs that protect them are sized by
// ShardsFor -- a block's question, asked of a run with nothing after it. This
// runs that case as the coded path runs it: an isolated burst, the repairs
// ShardsFor buys, and nothing else on the wire.
func TestATailBurstSurvivesTheRepairsItIsGiven(t *testing.T) {
	const loss, trials = 0.42, 5000
	channel := lossmodel.Snapshot{
		Loss: loss, Floor: loss, Recent: loss,
		BurstFactor: 1, ArrivalAfterLoss: 1 - loss,
	}
	params := Params{
		Class: ClassBulk, ShardBytes: 1100, RateBytesPerSec: 2e6,
		RoundTrip: 300 * time.Millisecond, TargetResidual: 1e-3,
	}
	for _, burst := range []int{1, 4, 16, 32} {
		total, ok := ShardsFor(burst, channel, params)
		if !ok {
			t.Fatalf("no block size for a burst of %d at %.0f%% erasure", burst, loss*100)
		}
		residual := smallTransfer(burst, total-burst, trials, loss)
		t.Logf("a burst of %2d symbols gets %2d repairs and loses one %.4f of the time",
			burst, total-burst, residual)
		// The estimate is an estimate, so it is held to an order of magnitude
		// rather than exactly; what must not happen is a tail failing far more
		// often than the parity it was given says it should.
		if residual > 10*params.TargetResidual {
			t.Errorf("a burst of %d with %d repairs lost a symbol %.4f of the time, "+
				"against a target of %.4f", burst, total-burst, residual, params.TargetResidual)
		}
	}
}

// A burst is what a block code is worst at, because a block's parity is spent
// on its own erasures and a burst inside one block cannot borrow from the
// next. A window has no boundary to be inside of: every repair emitted after
// the burst covers it, so the burst is repaired by parity that a block code
// would have committed elsewhere.
func TestWindowRepairsABurstNoBlockWouldHold(t *testing.T) {
	const capacity = 32
	encoder := NewWindowEncoder(capacity)
	decoder := NewWindowDecoder()

	sent := make([][]byte, 64)
	for i := range sent {
		sent[i] = symbol(uint32(i), 300)
	}

	burst := map[int]bool{}
	for i := 20; i < 28; i++ {
		burst[i] = true
	}

	recovered := map[uint32]bool{}
	note := func(d Delivery) {
		for _, r := range d.Recovered {
			recovered[r.ESI] = true
		}
	}
	var rid uint32
	for i, v := range sent {
		encoder.Add(v)
		if !burst[i] {
			note(decoder.Source(uint32(i), v))
		}
		// One repair every four symbols: a quarter of the rate, where the burst
		// is a quarter of the window. No single block of four could hold it.
		if i%4 == 3 {
			repair, ok := encoder.Repair(rid, 0)
			rid++
			if ok {
				note(decoder.Repair(repair))
			}
		}
	}
	for i := range burst {
		if !recovered[uint32(i)] {
			t.Fatalf("symbol %d of an eight-symbol burst was not recovered by the "+
				"repairs that followed it", i)
		}
	}
}

// Symbols of different lengths share one window, and a repair is only as long
// as the longest symbol it covers. Recovering a short symbol from a long
// repair must give back its own length, not the repair's.
func TestWindowCarriesSymbolsOfDifferentLengths(t *testing.T) {
	encoder := NewWindowEncoder(8)
	decoder := NewWindowDecoder()

	lengths := []int{10, 1000, 40, 700, 3, 900}
	sent := make([][]byte, len(lengths))
	for i, length := range lengths {
		sent[i] = symbol(uint32(i), length)
		encoder.Add(sent[i])
	}
	repair, ok := encoder.Repair(1, 0)
	if !ok {
		t.Fatal("no repair over a window of mixed lengths")
	}
	if len(repair.Vector) != 1000 {
		t.Fatalf("repair is %d bytes, want the longest symbol's 1000", len(repair.Vector))
	}

	// The short symbol is the one erased, and the repair covering it is long.
	var out []Delivery
	for i, v := range sent {
		if i == 4 {
			continue
		}
		out = append(out, decoder.Source(uint32(i), v))
	}
	out = append(out, decoder.Repair(repair))

	for _, d := range out {
		for _, r := range d.Recovered {
			if r.ESI != 4 {
				continue
			}
			// The recovered vector is padded to the repair's length; its own
			// bytes are the ones at the front, and the padding is zero.
			if !bytes.Equal(r.Vector[:3], sent[4]) {
				t.Fatalf("short symbol recovered as %v, want %v", r.Vector[:3], sent[4])
			}
			for _, b := range r.Vector[3:] {
				if b != 0 {
					t.Fatal("padding beyond a short symbol came back non-zero")
				}
			}
			return
		}
	}
	t.Fatal("the short symbol was not recovered from the long repair")
}

// A symbol that leaves the window unrecovered is lost, and the layer above is
// told once so it can re-issue rather than wait.
func TestSymbolsLeavingTheWindowAreReportedLostOnce(t *testing.T) {
	decoder := NewWindowDecoder()
	reported := map[uint32]int{}

	// One symbol is never sent; the rest walk the window past it.
	for i := uint32(0); i < 400; i++ {
		if i == 1 {
			continue
		}
		for _, esi := range decoder.Source(i, symbol(i, 100)).Lost {
			reported[esi]++
		}
	}
	if reported[1] != 1 {
		t.Fatalf("the missing symbol was reported lost %d times, want once", reported[1])
	}
	for esi, times := range reported {
		if esi != 1 {
			t.Fatalf("symbol %d was reported lost %d times though it arrived", esi, times)
		}
	}
}

// smallTransfer sends a handful of symbols with a fixed number of repairs and
// reports how often at least one symbol failed to arrive.
func smallTransfer(symbols, repairs, trials int, loss float64) float64 {
	failures := 0
	for trial := 0; trial < trials; trial++ {
		random := rand.New(rand.NewSource(int64(trial)))
		encoder := NewWindowEncoder(symbols)
		decoder := NewWindowDecoder()
		got := map[uint32]bool{}
		for i := 0; i < symbols; i++ {
			v := symbol(uint32(i), 64)
			esi := encoder.Add(v)
			if random.Float64() >= loss {
				decoder.Source(esi, v)
				got[esi] = true
			}
		}
		for rid := 0; rid < repairs; rid++ {
			repair, ok := encoder.Repair(uint32(rid), 0)
			if !ok {
				break
			}
			if random.Float64() >= loss {
				for _, r := range decoder.Repair(repair).Recovered {
					got[r.ESI] = true
				}
			}
		}
		if len(got) != symbols {
			failures++
		}
	}
	return float64(failures) / float64(trials)
}

// The code costs nothing beyond the transmissions it takes.
//
// k symbols are recoverable from any k of the k+r transmissions that carry
// them, so the only way a transfer fails is if fewer than k arrive at all.
// That is a property of the channel, not of the code, and this measures the
// difference between them: anything above the binomial tail is overhead the
// code itself is imposing -- a repair that turned out not to be independent,
// or an erasure it declined to use a repair on.
func TestTheCodeCostsNothingBeyondItsTransmissions(t *testing.T) {
	const loss = 0.42
	const symbols, repairs, trials = 8, 8, 20000

	measured := smallTransfer(symbols, repairs, trials, loss)
	channel := binomialTailBelow(symbols+repairs, 1-loss, symbols)
	t.Logf("%d symbols and %d repairs at %.0f%% erasure failed %.2f%% of the time; "+
		"the channel alone delivers too few %.2f%% of the time",
		symbols, repairs, loss*100, measured*100, channel*100)
	if measured > channel+0.01 {
		t.Errorf("residual %.4f against a channel bound of %.4f: the code is "+
			"losing symbols the transmissions that arrived could have recovered",
			measured, channel)
	}
}

// The window's advantage at small sizes: a few symbols share the repairs that
// cover all of them, where a code protecting each symbol alone has to send
// each one until one gets through.
//
// At 42% erasure a lone symbol needs eight transmissions for a residual near a
// thousandth, so eight symbols protected separately cost sixty-four. Sharing a
// window reaches the same residual for well under half of that, and this
// measures it rather than asserting it.
func TestWindowProtectsSmallTransfersCheaply(t *testing.T) {
	const loss = 0.42
	const symbols, trials = 8, 20000
	// What a lone symbol needs for a residual near a thousandth, times eight.
	isolated := 0
	for residual := 1.0; residual > 1e-3; residual *= loss {
		isolated++
	}
	isolated *= symbols

	shared := 0
	var residual float64
	for repairs := 0; repairs+symbols <= isolated; repairs++ {
		if residual = smallTransfer(symbols, repairs, trials, loss); residual <= 1e-3 {
			shared = symbols + repairs
			break
		}
	}
	if shared == 0 {
		t.Fatalf("a window over %d symbols never reached a thousandth residual "+
			"within %d transmissions (last %.4f)", symbols, isolated, residual)
	}
	t.Logf("%d symbols reach a %.4f residual in %d transmissions shared; protected "+
		"separately they would take %d", symbols, residual, shared, isolated)
	if shared*2 > isolated {
		t.Errorf("%d transmissions against %d: sharing a window should cost far "+
			"less than protecting each symbol alone", shared, isolated)
	}
}

// The window's oldest identifier is advanced by evicting one identifier at a
// time, because each eviction is a symbol the layer above has to be told will
// never arrive. The identifier is the peer's to choose, though, so the number
// of those steps is too: a single source symbol naming an ESI far ahead used
// to walk the whole distance, evicting and appending a Lost entry per step
// with the receiving path's lock held. It never returned.
func TestASymbolNamedFarAheadDoesNotWalkTheWindowToReachIt(t *testing.T) {
	d := NewWindowDecoder()
	d.Source(0, []byte("hello"))

	done := make(chan Delivery, 1)
	go func() { done <- d.Source(1<<30, []byte("far")) }()
	select {
	case delivery := <-done:
		// A window's worth of evictions is the most that can have anything to
		// report, because a window's worth is the most that can be held.
		if len(delivery.Lost) > maxDecoderWidth {
			t.Fatalf("a jump of 2^30 reported %d symbols lost, past the %d a window holds",
				len(delivery.Lost), maxDecoderWidth)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a source symbol 2^30 ahead of the window did not return")
	}

	// The window still works where it now is: the far symbol is inside it,
	// and one beside it is admitted rather than refused.
	if delivery := d.Source(1<<30+1, []byte("next")); len(delivery.Recovered) != 0 {
		t.Fatalf("a source symbol recovered %d others", len(delivery.Recovered))
	}
	if d.Discarded() != 0 {
		t.Fatalf("the symbol beside the jump was discarded")
	}
}
