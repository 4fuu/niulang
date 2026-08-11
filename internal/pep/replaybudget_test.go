package pep

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
)

func TestReplayBudgetBoundsTotalGrants(t *testing.T) {
	budget := newReplayBudget(3 * minFlowReplayBytes)
	for i := range 3 {
		if !budget.acquire(minFlowReplayBytes) {
			t.Fatalf("grant %d was refused below the limit", i)
		}
	}
	if budget.acquire(minFlowReplayBytes) {
		t.Fatal("grant above the endpoint limit was accepted")
	}
	if got, want := budget.inUse(), int64(3*minFlowReplayBytes); got != want {
		t.Fatalf("in use = %d, want %d", got, want)
	}
	budget.release(minFlowReplayBytes)
	if !budget.acquire(minFlowReplayBytes) {
		t.Fatal("released capacity was not reusable")
	}
}

func TestReplayBudgetReleaseCannotUnderflow(t *testing.T) {
	budget := newReplayBudget(minFlowReplayBytes)
	budget.release(minFlowReplayBytes * 4)
	if got := budget.inUse(); got != 0 {
		t.Fatalf("in use = %d after an oversized release, want 0", got)
	}
	if !budget.acquire(minFlowReplayBytes) {
		t.Fatal("budget did not recover after an oversized release")
	}
}

// The replay window is the sender's send window. A window fixed below the
// path's bandwidth-delay product throttles the flow, so it has to grow, and
// the growth has to come from the accounted endpoint budget.
func TestReplayWindowGrowsFromEndpointBudget(t *testing.T) {
	inner, peer := net.Pipe()
	defer inner.Close()
	defer peer.Close()
	flow := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, defaultChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, nil, nil)
	flow.replayBudget = newReplayBudget(defaultReplayMemoryBytes)

	payload := make([]byte, defaultChunkSize)
	var sequence uint64
	// Fill well past the unaccounted floor so the window has to be extended.
	for sequence < uint64(maxReplayBytes)+uint64(2*defaultChunkSize) {
		frame := protocol.Frame{
			Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Sequence: sequence},
			Payload: payload,
		}
		if err := flow.recordReplay(frame); err != nil {
			t.Fatalf("record replay at sequence %d: %v", sequence, err)
		}
		sequence += uint64(len(payload))
	}
	if flow.replayLimit <= maxReplayBytes {
		t.Fatalf("replay window did not grow past the floor: limit=%d", flow.replayLimit)
	}
	if flow.replayGranted == 0 || flow.replayBudget.inUse() != int64(flow.replayGranted) {
		t.Fatalf("growth was not accounted: granted=%d in use=%d", flow.replayGranted, flow.replayBudget.inUse())
	}
	flow.releaseReplayBudget()
	if got := flow.replayBudget.inUse(); got != 0 {
		t.Fatalf("endpoint budget still holds %d bytes after release", got)
	}
	if flow.replayLimit != maxReplayBytes {
		t.Fatalf("replay window did not return to the floor: limit=%d", flow.replayLimit)
	}
}

// A flow must not be able to grow past its own cap even when the endpoint has
// spare budget, so one flow cannot consume the whole allowance.
func TestReplayWindowRespectsPerFlowCap(t *testing.T) {
	flow := &multipathFlow{replay: make(map[uint64]protocol.Frame), replayLimit: maxFlowReplayBytes}
	flow.replayBudget = newReplayBudget(defaultReplayMemoryBytes)
	flow.replayMu.Lock()
	flow.growReplayLimitLocked(minFlowReplayBytes)
	flow.replayMu.Unlock()
	if flow.replayLimit != maxFlowReplayBytes {
		t.Fatalf("replay window grew past the per-flow cap: limit=%d", flow.replayLimit)
	}
	if flow.replayBudget.inUse() != 0 {
		t.Fatalf("refused growth still drew %d bytes from the budget", flow.replayBudget.inUse())
	}
}

// The receiver's out-of-order capacity must cover the largest window a peer
// can hold; otherwise an ordinary striped transfer overflows it and a healthy
// application flow is aborted.
func TestReassemblyCapacityCoversPeerSendWindow(t *testing.T) {
	if maxReassemblyBytes < maxFlowReplayBytes {
		t.Fatalf("reassembly capacity %d is below the peer send window %d", maxReassemblyBytes, maxFlowReplayBytes)
	}
}

// The retained window is a rescue optimization, not a correctness requirement:
// QUIC already delivers reliably on the lane that carried a frame. Blocking the
// application when it fills couples forward progress to the reverse path's
// congestion state, which is what stalled live transfers at roughly the window
// mark. Recording must therefore always make progress.
func TestReplayWindowEvictsRatherThanBlocking(t *testing.T) {
	inner, peer := net.Pipe()
	defer inner.Close()
	defer peer.Close()
	flow := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, defaultChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, nil, nil)
	// An exhausted endpoint budget removes the option of growing the window,
	// which is the situation that used to block.
	flow.replayBudget = newReplayBudget(1)

	payload := make([]byte, defaultChunkSize)
	var sequence uint64
	done := make(chan error, 1)
	go func() {
		for range (maxReplayBytes / defaultChunkSize) + 64 {
			frame := protocol.Frame{
				Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Sequence: sequence},
				Payload: payload,
			}
			if err := flow.recordReplay(frame); err != nil {
				done <- err
				return
			}
			sequence += uint64(len(payload))
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recording past a full window: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recording blocked on a full replay window")
	}
	if flow.replayEvictions.Load() == 0 {
		t.Fatal("a full window did not evict anything")
	}
	if flow.replayable.Load() {
		t.Fatal("flow still claims to be replayable after eviction")
	}
	flow.replayMu.Lock()
	retained := flow.replayBytes
	flow.replayMu.Unlock()
	if retained > flow.replayLimit {
		t.Fatalf("retained %d bytes above the %d limit", retained, flow.replayLimit)
	}
}

// A flow that has dropped part of its unacknowledged window must fail rather
// than replay a gap onto a replacement lane.
func TestEvictedFlowStartsReplayable(t *testing.T) {
	inner, peer := net.Pipe()
	defer inner.Close()
	defer peer.Close()
	flow := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, defaultChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, nil, nil)
	if !flow.replayable.Load() {
		t.Fatal("a new flow must start replayable")
	}
}
