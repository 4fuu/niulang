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

func TestLaneSelectionPrefersValidatedLanes(t *testing.T) {
	flow := &multipathFlow{
		done: make(chan struct{}), chunkSize: defaultChunkSize,
		lanes: map[uint64]*mpLane{0: {id: 0}, 1: {id: 1}},
	}
	// Lane 1 has no round-trip sample, so its congestion window is still the
	// initial guess. Trusting it would move ordered data onto a path in slow
	// start; lane 0 is slower on paper but proven.
	laneRate(flow.lanes[0], 1<<20, 200*time.Millisecond)
	laneRate(flow.lanes[1], 1<<30, 0)
	lane, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if lane.id != 0 {
		t.Fatalf("selection chose unvalidated lane %d over the validated lane 0", lane.id)
	}
}

func TestLaneSelectionSplitsInProportionToRate(t *testing.T) {
	flow := &multipathFlow{
		done: make(chan struct{}), chunkSize: defaultChunkSize,
		lanes: map[uint64]*mpLane{0: {id: 0}, 1: {id: 1}},
	}
	// A lane four times as fast should be given roughly four times the bytes.
	// Equalizing arrival time is what in-order reassembly needs: assigning
	// work a slow lane cannot deliver on time stalls everything behind it.
	laneRate(flow.lanes[0], 4<<20, 100*time.Millisecond)
	laneRate(flow.lanes[1], 1<<20, 100*time.Millisecond)
	counts := map[uint64]int{}
	for range 200 {
		lane, err := flow.chooseLane(true)
		if err != nil {
			t.Fatal(err)
		}
		counts[lane.id]++
	}
	if counts[1] == 0 {
		t.Fatal("the slower lane received no traffic at all")
	}
	ratio := float64(counts[0]) / float64(counts[1])
	if ratio < 2.5 || ratio > 6 {
		t.Fatalf("fast/slow split was %d/%d (ratio %.2f), want roughly 4:1", counts[0], counts[1], ratio)
	}
}

// A lane that stops draining must not bank unbounded backlog, or it would stay
// unusable long after the path recovered.
func TestLaneScheduleHorizonBoundsBacklog(t *testing.T) {
	flow := &multipathFlow{
		done: make(chan struct{}), chunkSize: defaultChunkSize,
		lanes: map[uint64]*mpLane{0: {id: 0}},
	}
	lane := flow.lanes[0]
	laneRate(lane, 1024, 100*time.Millisecond)
	for range 500 {
		if _, err := flow.chooseLane(true); err != nil {
			t.Fatal(err)
		}
	}
	if backlog := time.Until(lane.nextFree); backlog > laneScheduleHorizon+time.Second {
		t.Fatalf("lane banked %s of backlog, want at most the %s horizon", backlog, laneScheduleHorizon)
	}
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
		id: 0, writeQ: make(chan protocol.Frame, 1), writeSlots: make(chan struct{}, 1),
		writeDone: make(chan struct{}),
	}
	idle := &mpLane{
		id: 1, writeQ: make(chan protocol.Frame, 4), writeSlots: make(chan struct{}, 4),
		writeDone: make(chan struct{}),
	}
	flow.lanes[0], flow.lanes[1] = full, idle
	// Make lane 0 both the preferred lane and already full.
	laneRate(full, 1<<30, time.Millisecond)
	laneRate(idle, 1<<20, 10*time.Millisecond)
	full.writeSlots <- struct{}{}
	full.writeQ <- protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeData}}

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
