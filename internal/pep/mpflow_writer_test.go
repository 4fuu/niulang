package pep

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/protocol"
)

func TestCompletionWatchdogReleasesProvenCompleteFlow(t *testing.T) {
	inner, peer := net.Pipe()
	defer peer.Close()
	registry := metrics.New()
	flow := &multipathFlow{
		ctx: context.Background(), inner: inner, done: make(chan struct{}),
		lanes: make(map[uint64]*mpLane), metrics: registry, completionGrace: 10 * time.Millisecond,
	}
	stop := make(chan struct{})
	go flow.completionWatchdog(stop)
	flow.finSent.Store(true)
	flow.remoteFinSeen.Store(true)
	select {
	case <-flow.done:
	case <-time.After(time.Second):
		close(stop)
		t.Fatal("completion watchdog did not close proven-complete flow")
	}
	close(stop)
	if got := registry.Snapshot().CompletionTimeouts; got != 1 {
		t.Fatalf("completion watchdog metric=%d, want 1", got)
	}
}

func TestFlowIdleTimeoutReleasesResources(t *testing.T) {
	inner, peer := net.Pipe()
	defer peer.Close()
	registry := metrics.New()
	flow := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, 1024, protocol.FlagAckUp, protocol.FlagAckDown, nil, registry)
	flow.idleTimeout = 20 * time.Millisecond
	flow.maxLifetime = time.Second
	stats, err := flow.run(context.Background())
	if !errors.Is(err, errFlowIdleTimeout) {
		t.Fatalf("flow error = %v, want idle timeout", err)
	}
	if stats.Ended.IsZero() {
		t.Fatal("timed-out flow did not record an end time")
	}
	if got := registry.Snapshot(); got.FlowTimeouts != 1 || got.ActiveFlows != 0 {
		t.Fatalf("timeout metrics = %+v, want one timeout and no active flows", got)
	}
}

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

func TestExpectedCloseDoesNotCountAsLaneFailure(t *testing.T) {
	registry := metrics.New()
	flow := &multipathFlow{done: make(chan struct{}), metrics: registry}
	lane := &mpLane{id: 1}
	flow.finished.Store(true)
	flow.failLane(lane, errors.New("normal close"))
	if got := registry.Snapshot().LaneFailures; got != 0 {
		t.Fatalf("normal close exported as %d lane failures", got)
	}
}

func TestProvenCompleteFlowDoesNotWaitForLaneReplacementForFinalACK(t *testing.T) {
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}),
		lanes: make(map[uint64]*mpLane),
	}
	flow.finSent.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := flow.acknowledgeRemoteFIN(ctx, 0, false)
	if err != nil {
		t.Fatalf("proven-complete final ACK cleanup returned error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("final ACK cleanup took %s; lane replacement was not bounded", elapsed)
	}
}
