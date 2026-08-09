package pep

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
)

func TestLaneWriterStopsWhenFlowCompletes(t *testing.T) {
	flow := &multipathFlow{ctx: context.Background(), done: make(chan struct{}), laneErr: make(chan laneFailure, 1)}
	lane := &mpLane{
		writeQ:    make(chan protocol.Frame, 1),
		writeDone: make(chan struct{}),
	}
	go flow.writeLane(lane)
	close(flow.done)

	select {
	case <-lane.writeDone:
	case <-time.After(time.Second):
		t.Fatal("lane writer did not stop after flow completion")
	}
}

func TestReplayBufferAcknowledgesCumulativeSequence(t *testing.T) {
	flow := &multipathFlow{replay: make(map[uint64]protocol.Frame)}
	frames := []protocol.Frame{
		{Header: protocol.Header{Type: protocol.TypeData, Sequence: 0}, Payload: []byte("abc")},
		{Header: protocol.Header{Type: protocol.TypeData, Sequence: 3}, Payload: []byte("def")},
		{Header: protocol.Header{Type: protocol.TypeClose, Sequence: 6}},
	}
	for _, frame := range frames {
		if err := flow.recordReplay(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := flow.acknowledgeReplay(3, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := flow.replay[0]; ok {
		t.Fatal("cumulatively acknowledged frame was retained")
	}
	if len(flow.replay) != 2 || flow.replayBytes != 3 {
		t.Fatalf("unexpected replay state: frames=%d bytes=%d", len(flow.replay), flow.replayBytes)
	}
	if err := flow.acknowledgeReplay(6, true); err != nil {
		t.Fatal(err)
	}
	if len(flow.replay) != 0 || flow.replayBytes != 0 {
		t.Fatalf("final ACK did not clear replay state: frames=%d bytes=%d", len(flow.replay), flow.replayBytes)
	}
}

func TestReplayBufferRejectsInvalidBounds(t *testing.T) {
	flow := &multipathFlow{replay: make(map[uint64]protocol.Frame)}
	tooLarge := protocol.Frame{
		Header:  protocol.Header{Type: protocol.TypeData},
		Payload: []byte(strings.Repeat("x", maxReplayBytes+1)),
	}
	if err := flow.recordReplay(tooLarge); err == nil {
		t.Fatal("oversized replay frame was accepted")
	}
	if err := flow.recordReplay(protocol.Frame{Header: protocol.Header{Type: protocol.TypeData}, Payload: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := flow.acknowledgeReplay(2, false); err == nil {
		t.Fatal("ACK beyond sent data was accepted")
	}
}

func TestReplayPendingUsesSurvivingLane(t *testing.T) {
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}),
		lanes: make(map[uint64]*mpLane), replay: make(map[uint64]protocol.Frame),
	}
	lane := &mpLane{writeQ: make(chan protocol.Frame, 2), writeDone: make(chan struct{})}
	flow.lanes[1] = lane
	if err := flow.recordReplay(protocol.Frame{Header: protocol.Header{Type: protocol.TypeData, Sequence: 0}, Payload: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	if err := flow.replayPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-lane.writeQ:
		if string(got.Payload) != "abc" || got.Header.Sequence != 0 {
			t.Fatalf("unexpected replayed frame: %+v", got)
		}
	default:
		t.Fatal("pending frame was not replayed")
	}
}

func TestEnqueueFrameStopsWhenLaneWriterStops(t *testing.T) {
	flow := &multipathFlow{ctx: context.Background(), done: make(chan struct{}), laneErr: make(chan laneFailure, 1)}
	lane := &mpLane{
		writeQ:    make(chan protocol.Frame),
		writeDone: make(chan struct{}),
	}
	close(lane.writeDone)

	err := flow.enqueueFrame(context.Background(), lane, protocol.Frame{})
	if err == nil {
		t.Fatal("enqueue succeeded after lane writer stopped")
	}
}

func TestRetireLeastProductiveLaneKeepsControlLane(t *testing.T) {
	flow := &multipathFlow{lanes: map[uint64]*mpLane{
		0: {id: 0},
		1: {id: 1},
		2: {id: 2},
	}}
	flow.lanes[1].sent.Store(100)
	flow.lanes[2].sent.Store(10)
	if !flow.retireLeastProductiveLane() {
		t.Fatal("expected a non-control lane to be retired")
	}
	if _, ok := flow.lanes[2]; ok {
		t.Fatal("least productive lane remained attached")
	}
	if flow.lanes[0].closed.Load() {
		t.Fatal("control lane was retired")
	}
}
