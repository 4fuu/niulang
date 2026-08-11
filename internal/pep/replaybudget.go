package pep

import "sync/atomic"

// The replay window is wanopt's own reliability layer: a sender keeps every
// unacknowledged DATA frame so it can be replayed onto a replacement lane when
// the lane that carried it fails. It therefore behaves exactly like a send
// window, and a window smaller than the bandwidth-delay product throttles the
// flow no matter how much capacity the lanes have.
//
// Measured on the emulated 200 ms path with four lanes and per-flow policing,
// a fixed 8 MiB window left the sender blocked for 1.95 s of a 4.88 s
// transfer. The product it has to cover is larger than one QUIC connection's,
// because the feedback delay includes queueing on every lane.
//
// Sizing it per flow alone would trade that throttle for an unbounded memory
// commitment: a peer that stops acknowledging holds the whole window open, and
// a server admits thousands of sessions. The window is therefore bounded twice
// - once per flow, and once across all flows on the endpoint.
const (
	// maxFlowReplayBytes is the largest window a single flow may hold. At
	// 200 ms it covers well over a gigabit, so it is a memory bound rather
	// than a throughput bound.
	maxFlowReplayBytes = 32 * 1024 * 1024
	// minFlowReplayBytes is always available to a flow regardless of what
	// other flows are holding, so a busy endpoint degrades to the previous
	// behavior instead of stalling new transfers.
	minFlowReplayBytes = 1 * 1024 * 1024
	// defaultReplayMemoryBytes bounds the endpoint's total replay commitment.
	defaultReplayMemoryBytes = 256 * 1024 * 1024
)

// replayBudget accounts replay bytes actually retained across all flows on one
// endpoint. It tracks real retained bytes rather than reservations, so an idle
// flow costs nothing and the bound reflects genuine memory use.
type replayBudget struct {
	limit int64
	used  atomic.Int64
}

func newReplayBudget(limit int64) *replayBudget {
	if limit <= 0 {
		limit = defaultReplayMemoryBytes
	}
	return &replayBudget{limit: limit}
}

// acquire reserves n bytes above the caller's guaranteed floor. Bytes within
// the floor are never refused, which keeps one large transfer from preventing
// other flows from making progress at all.
func (b *replayBudget) acquire(n int64) bool {
	if b == nil || n <= 0 {
		return true
	}
	for {
		used := b.used.Load()
		if used+n > b.limit {
			return false
		}
		if b.used.CompareAndSwap(used, used+n) {
			return true
		}
	}
}

func (b *replayBudget) release(n int64) {
	if b == nil || n <= 0 {
		return
	}
	if remaining := b.used.Add(-n); remaining < 0 {
		b.used.Store(0)
	}
}

func (b *replayBudget) inUse() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}
