package fec

import (
	"bytes"
	"math/rand"
	"testing"
)

func makeShards(rng *rand.Rand, k, n, size int) (shards [][]byte, original [][]byte) {
	shards = make([][]byte, n)
	original = make([][]byte, n)
	for i := range shards {
		shards[i] = make([]byte, size)
	}
	for i := 0; i < k; i++ {
		rng.Read(shards[i])
	}
	return shards, original
}

func snapshot(shards [][]byte) [][]byte {
	out := make([][]byte, len(shards))
	for i, s := range shards {
		out[i] = append([]byte(nil), s...)
	}
	return out
}

// The MDS property is the whole reason for the Cauchy construction, and it is
// the property the rate controller's arithmetic assumes: any k of the n
// shards, never "k of a particular kind". This checks every subset for small
// codes, which is the only way to check "any".
func TestAnyKOfNReconstructs(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, code := range []struct{ k, n int }{{1, 3}, {2, 4}, {3, 5}, {4, 8}, {5, 9}, {6, 10}} {
		codec, err := New(code.k, code.n)
		if err != nil {
			t.Fatal(err)
		}
		shards, _ := makeShards(rng, code.k, code.n, 64)
		if err := codec.Encode(shards); err != nil {
			t.Fatal(err)
		}
		want := snapshot(shards)

		// Every subset of exactly k survivors.
		for mask := 0; mask < 1<<code.n; mask++ {
			if popcount(mask) != code.k {
				continue
			}
			got := snapshot(shards)
			present := make([]bool, code.n)
			for i := 0; i < code.n; i++ {
				present[i] = mask&(1<<i) != 0
				if !present[i] {
					got[i] = nil
				}
			}
			if err := codec.Reconstruct(got, present); err != nil {
				t.Fatalf("(%d,%d) survivors %b: %v", code.k, code.n, mask, err)
			}
			for i := 0; i < code.k; i++ {
				if !bytes.Equal(got[i], want[i]) {
					t.Fatalf("(%d,%d) survivors %b: data shard %d wrong", code.k, code.n, mask, i)
				}
			}
		}
	}
}

func popcount(x int) int {
	n := 0
	for ; x != 0; x &= x - 1 {
		n++
	}
	return n
}

// One shard fewer than k must fail cleanly rather than return wrong bytes. A
// code that silently produced garbage below its threshold would hand the
// application corrupt data, which is far worse than a hole.
func TestBelowThresholdFailsRatherThanGuesses(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	codec, err := New(4, 8)
	if err != nil {
		t.Fatal(err)
	}
	shards, _ := makeShards(rng, 4, 8, 100)
	if err := codec.Encode(shards); err != nil {
		t.Fatal(err)
	}
	present := []bool{true, true, true, false, false, false, false, false}
	if err := codec.Reconstruct(shards, present); err != ErrTooFewShards {
		t.Fatalf("reconstruct with k-1 shards returned %v, want ErrTooFewShards", err)
	}
}

// A receiver that lost nothing must do no work, because on a systematic code
// the data shards are the original bytes. This is the common case even at 42%
// loss once the code rate is right, so it has to be free.
func TestCompleteBlockIsUntouched(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	codec, err := New(8, 16)
	if err != nil {
		t.Fatal(err)
	}
	shards, _ := makeShards(rng, 8, 16, 256)
	if err := codec.Encode(shards); err != nil {
		t.Fatal(err)
	}
	want := snapshot(shards)
	present := make([]bool, 16)
	for i := range present {
		present[i] = true
	}
	if err := codec.Reconstruct(shards, present); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if !bytes.Equal(shards[i], want[i]) {
			t.Fatalf("shard %d changed on a complete block", i)
		}
	}
}

// The systematic property itself: the first k shards are the input, unmodified.
// Anything else would mean the receiver pays a decode for every block.
func TestEncodeLeavesDataShardsAlone(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	codec, err := New(6, 10)
	if err != nil {
		t.Fatal(err)
	}
	shards, _ := makeShards(rng, 6, 10, 128)
	want := snapshot(shards[:6])
	if err := codec.Encode(shards); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if !bytes.Equal(shards[i], want[i]) {
			t.Fatalf("encode modified data shard %d", i)
		}
	}
}

// The channel this is for drops about 42% of packets independently. A code
// sized for it has to survive that loss applied at random, not just at the
// worst case, and it has to do so repeatedly.
func TestSurvivesTheMeasuredChannel(t *testing.T) {
	const (
		k, n  = 24, 64 // rate 0.375, sized for 42% loss in the rate controller
		loss  = 0.42
		size  = 1200
		draws = 2000
	)
	rng := rand.New(rand.NewSource(5))
	codec, err := New(k, n)
	if err != nil {
		t.Fatal(err)
	}
	var failures int
	for draw := 0; draw < draws; draw++ {
		shards, _ := makeShards(rng, k, n, size)
		if err := codec.Encode(shards); err != nil {
			t.Fatal(err)
		}
		want := snapshot(shards[:k])
		present := make([]bool, n)
		for i := range present {
			present[i] = rng.Float64() >= loss
		}
		got := snapshot(shards)
		for i, ok := range present {
			if !ok {
				got[i] = nil
			}
		}
		err := codec.Reconstruct(got, present)
		if err == ErrTooFewShards {
			failures++
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < k; i++ {
			if !bytes.Equal(got[i], want[i]) {
				t.Fatalf("draw %d: data shard %d wrong after repair", draw, i)
			}
		}
	}
	// The binomial tail for 24 of 64 arrivals at 58% is well under a thousandth.
	if failures > draws/200 {
		t.Fatalf("%d of %d blocks unrecoverable at %.0f%% loss, want almost none",
			failures, draws, 100*loss)
	}
	t.Logf("(%d,%d) at %.0f%% independent loss: %d of %d blocks unrecoverable",
		k, n, 100*loss, failures, draws)
}

// Correlated loss is the other regime, and a block code has no defence against
// a burst that takes more than n-k of its shards. This records what the code
// can and cannot do, so the rate controller's decision to interleave, or to
// stop coding, rests on a measured fact rather than an assumption.
func TestABurstLongerThanTheParityIsUnrecoverable(t *testing.T) {
	rng := rand.New(rand.NewSource(6))
	codec, err := New(24, 64)
	if err != nil {
		t.Fatal(err)
	}
	shards, _ := makeShards(rng, 24, 64, 512)
	if err := codec.Encode(shards); err != nil {
		t.Fatal(err)
	}
	present := make([]bool, 64)
	for i := range present {
		present[i] = true
	}
	// 41 consecutive shards gone: one more than the 40 parity shards.
	for i := 5; i < 46; i++ {
		present[i] = false
		shards[i] = nil
	}
	if err := codec.Reconstruct(shards, present); err != ErrTooFewShards {
		t.Fatalf("a burst longer than the parity returned %v, want ErrTooFewShards", err)
	}
}

func TestRejectsImpossibleCodes(t *testing.T) {
	for _, code := range []struct{ k, n int }{{0, 4}, {-1, 4}, {5, 4}, {200, 300}} {
		if _, err := New(code.k, code.n); err == nil {
			t.Fatalf("New(%d,%d) succeeded, want an error", code.k, code.n)
		}
	}
}

func TestRejectsMismatchedShards(t *testing.T) {
	codec, err := New(2, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Encode([][]byte{make([]byte, 8), make([]byte, 9), make([]byte, 8), make([]byte, 8)}); err != ErrShardSize {
		t.Fatalf("mismatched shard lengths returned %v, want ErrShardSize", err)
	}
	if err := codec.Encode([][]byte{make([]byte, 8)}); err != ErrShardCount {
		t.Fatalf("wrong shard count returned %v, want ErrShardCount", err)
	}
}

// GF(256) has to be a field, or none of the above means anything.
func TestFieldArithmetic(t *testing.T) {
	for a := 1; a < 256; a++ {
		if got := mul(byte(a), inv(byte(a))); got != 1 {
			t.Fatalf("%d * inv(%d) = %d, want 1", a, a, got)
		}
		for b := 1; b < 256; b++ {
			if got := div(mul(byte(a), byte(b)), byte(b)); got != byte(a) {
				t.Fatalf("(%d * %d) / %d = %d, want %d", a, b, b, got, a)
			}
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	codec, err := New(24, 64)
	if err != nil {
		b.Fatal(err)
	}
	shards, _ := makeShards(rng, 24, 64, 1200)
	b.SetBytes(int64(24 * 1200))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := codec.Encode(shards); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReconstruct(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	codec, err := New(24, 64)
	if err != nil {
		b.Fatal(err)
	}
	shards, _ := makeShards(rng, 24, 64, 1200)
	if err := codec.Encode(shards); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(24 * 1200))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		got := snapshot(shards)
		present := make([]bool, 64)
		for j := range present {
			present[j] = rng.Float64() >= 0.42
		}
		for j, ok := range present {
			if !ok {
				got[j] = nil
			}
		}
		b.StartTimer()
		if err := codec.Reconstruct(got, present); err != nil && err != ErrTooFewShards {
			b.Fatal(err)
		}
	}
}
