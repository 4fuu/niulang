package pep

import (
	"context"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
)

// laneRate installs a synthetic rate/RTT sample so lane selection can be
// exercised without a QUIC connection. sendRate caches these fields, and a
// fresh sample timestamp keeps the cache from being refreshed from a nil
// transport.
func laneRate(lane *mpLane, bytesPerSecond float64, rtt time.Duration) {
	lane.rateMu.Lock()
	lane.rateSampled, lane.rateBytes, lane.rateRTT = time.Now(), bytesPerSecond, rtt
	lane.rateMu.Unlock()
}

// Committing to one lane and then waiting for a queue slot lets a single lane
// throttle the whole flow while another sits idle, and the producer stops, so
// the scheduler never observes the imbalance it exists to correct.
func TestEnqueueSpillsToTheNextLaneWhenPreferredIsFull(t *testing.T) {
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}), laneErr: make(chan laneFailure, 4),
		chunkSize: defaultChunkSize, lanes: map[uint64]*mpLane{},
	}
	full := &mpLane{
		id: 0, writeQ: make(chan laneFrame, 1), writeSlots: make(chan struct{}, 1),
		writeDone: make(chan struct{}),
	}
	idle := &mpLane{
		id: 1, writeQ: make(chan laneFrame, 4), writeSlots: make(chan struct{}, 4),
		writeDone: make(chan struct{}),
	}
	flow.lanes[0], flow.lanes[1] = full, idle
	// Make lane 0 both the preferred lane and already full.
	laneRate(full, 1<<30, time.Millisecond)
	laneRate(idle, 1<<20, 10*time.Millisecond)
	full.writeSlots <- struct{}{}
	full.writeQ <- laneFrame{frame: protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeData}}}

	frame := protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Class: protocol.ClassBulk},
		Payload: make([]byte, 16),
	}
	done := make(chan error, 1)
	go func() { done <- flow.enqueueOnHealthyLane(context.Background(), frame, true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("enqueue with a full preferred lane: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked on the full preferred lane instead of using the idle lane")
	}
	if len(idle.writeQ) != 1 {
		t.Fatalf("idle lane received %d frames, want the spilled frame", len(idle.writeQ))
	}
}
