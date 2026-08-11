package pep

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/protocol"
)

// ackCaptureConn is a deterministic stream writer used to exercise the
// asynchronous acknowledgement path without involving a QUIC implementation.
// protocol.WriteFrame emits a header and payload in separate writes, so the
// helper assembles both before publishing the decoded frame.
type ackCaptureConn struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	frames   chan protocol.Frame
	delay    time.Duration
	fail     error
	closed   chan struct{}
	closeOne sync.Once
}

func newAckCaptureConn(delay time.Duration, fail error) *ackCaptureConn {
	return &ackCaptureConn{frames: make(chan protocol.Frame, 16), delay: delay, fail: fail, closed: make(chan struct{})}
}

func (c *ackCaptureConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *ackCaptureConn) Write(p []byte) (int, error) {
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.fail != nil {
		return 0, c.fail
	}
	c.mu.Lock()
	_, _ = c.buf.Write(p)
	for c.buf.Len() >= protocol.HeaderSize {
		raw := append([]byte(nil), c.buf.Bytes()[:protocol.HeaderSize]...)
		h, err := protocol.DecodeHeader(raw, protocol.DefaultMaxPayload)
		if err != nil {
			c.mu.Unlock()
			return 0, err
		}
		total := protocol.HeaderSize + int(h.PayloadLen)
		if c.buf.Len() < total {
			break
		}
		frame := protocol.Frame{Header: h, Payload: append([]byte(nil), c.buf.Bytes()[protocol.HeaderSize:total]...)}
		c.buf.Next(total)
		select {
		case c.frames <- frame:
		default:
		}
	}
	c.mu.Unlock()
	return len(p), nil
}

func (c *ackCaptureConn) Close() error {
	c.closeOne.Do(func() { close(c.closed) })
	return nil
}

func newAckTestFlow(conn *ackCaptureConn) *multipathFlow {
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}), ackWake: make(chan struct{}, 1), ackErr: make(chan error, 1), laneErr: make(chan laneFailure, 1),
		lanes: make(map[uint64]*mpLane), sessionID: [16]byte{1}, flowID: 7, recvAckFlag: protocol.FlagAckDown,
	}
	flow.lanes[0] = &mpLane{id: 0, fc: newFrameConn(conn, protocol.DefaultMaxPayload)}
	return flow
}

func waitAckFrame(t *testing.T, conn *ackCaptureConn) protocol.Frame {
	t.Helper()
	select {
	case frame := <-conn.frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for acknowledgement frame")
		return protocol.Frame{}
	}
}

func TestACKLoopCoalescesToNewestSequence(t *testing.T) {
	conn := newAckCaptureConn(0, nil)
	flow := newAckTestFlow(conn)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { flow.ackLoop(ctx); close(finished) }()
	flow.scheduleACK(10)
	flow.scheduleACK(20)
	got := waitAckFrame(t, conn)
	if got.Header.Type != protocol.TypeAck || got.Header.Sequence != 20 || got.Header.Flags != protocol.FlagAckDown {
		t.Fatalf("coalesced ACK = type=%d sequence=%d flags=%x", got.Header.Type, got.Header.Sequence, got.Header.Flags)
	}
	select {
	case extra := <-conn.frames:
		t.Fatalf("coalescer emitted an unnecessary ACK: sequence=%d", extra.Header.Sequence)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("ACK loop did not stop after cancellation")
	}
}

func TestScheduleACKDoesNotBlockOnSlowWrite(t *testing.T) {
	conn := newAckCaptureConn(250*time.Millisecond, nil)
	flow := newAckTestFlow(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go flow.ackLoop(ctx)
	started := time.Now()
	flow.scheduleACK(1)
	if elapsed := time.Since(started); elapsed > 20*time.Millisecond {
		t.Fatalf("scheduleACK blocked for %s on a slow transport write", elapsed)
	}
}

func TestACKLoopWriteFailureReachesLaneRecovery(t *testing.T) {
	want := errors.New("injected ACK write failure")
	conn := newAckCaptureConn(0, want)
	flow := newAckTestFlow(conn)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go flow.ackLoop(ctx)
	flow.scheduleACK(1)
	select {
	case failure := <-flow.laneErr:
		if !errors.Is(failure.err, want) {
			t.Fatalf("lane recovery error = %v, want %v", failure.err, want)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK write failure was not reported")
	}
}

func TestFinalACKSuppressesLaterNonFinalACK(t *testing.T) {
	conn := newAckCaptureConn(0, nil)
	flow := newAckTestFlow(conn)
	flow.ackClosing.Store(true)
	flow.scheduleACK(100)
	select {
	case <-conn.frames:
		t.Fatal("non-final ACK was emitted after final ACK state")
	case <-time.After(20 * time.Millisecond):
	}
}

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

func TestLaneFailureCountIsMonotonicAndIdempotent(t *testing.T) {
	conn := newAckCaptureConn(0, nil)
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}), laneErr: make(chan laneFailure, 1),
		lanes: make(map[uint64]*mpLane),
	}
	lane := &mpLane{id: 1, fc: newFrameConn(conn, protocol.DefaultMaxPayload)}
	flow.failLane(lane, errors.New("first"))
	flow.failLane(lane, errors.New("duplicate"))
	if got := flow.laneFailureCount(); got != 1 {
		t.Fatalf("lane failure count = %d, want one transition", got)
	}
	select {
	case <-flow.laneErr:
	default:
		t.Fatal("lane failure was not published")
	}
}

func TestLaneWriterPrioritizesInteractiveFrames(t *testing.T) {
	conn := newAckCaptureConn(0, nil)
	flow := &multipathFlow{
		ctx:     context.Background(),
		done:    make(chan struct{}),
		laneErr: make(chan laneFailure, 1),
	}
	lane := &mpLane{
		id:                1,
		fc:                newFrameConn(conn, protocol.DefaultMaxPayload),
		writeQ:            make(chan protocol.Frame, maxLaneWriteQueue),
		writeInteractiveQ: make(chan protocol.Frame, maxLaneWriteQueue),
		writeSlots:        make(chan struct{}, maxLaneWriteQueue),
		writeDone:         make(chan struct{}),
	}
	bulk := protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Class: protocol.ClassBulk}, Payload: []byte("bulk")}
	interactive := protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Class: protocol.ClassInteractive}, Payload: []byte("interactive")}
	for _, frame := range []protocol.Frame{bulk, interactive} {
		if err := flow.enqueueFrameClass(context.Background(), lane, frame, frame.Header.Class == protocol.ClassBulk); err != nil {
			t.Fatal(err)
		}
	}
	go flow.writeLane(lane)
	first := waitAckFrame(t, conn)
	second := waitAckFrame(t, conn)
	if string(first.Payload) != string(interactive.Payload) || string(second.Payload) != string(bulk.Payload) {
		t.Fatalf("writer order = %q then %q, want interactive then bulk", first.Payload, second.Payload)
	}
	close(flow.done)
	select {
	case <-lane.writeDone:
	case <-time.After(time.Second):
		t.Fatal("lane writer did not stop")
	}
}

func TestBulkSelectionReservesNegotiatedControlLane(t *testing.T) {
	flow := &multipathFlow{
		done: make(chan struct{}),
		lanes: map[uint64]*mpLane{
			0: {id: 0},
			1: {id: 1},
			2: {id: 2},
		},
		reserveControlLane: true,
	}

	control, err := flow.chooseLane(false)
	if err != nil {
		t.Fatal(err)
	}
	if control.id != 0 {
		t.Fatalf("control selection chose lane %d, want lane 0", control.id)
	}
	first, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if first.id != 1 {
		t.Fatalf("bulk selection chose lane %d, want the first non-control lane", first.id)
	}
	// Selection is by estimated delivery time, not rotation: the next frame
	// moves to lane 2 only once lane 1 actually has a backlog. Rotating
	// unconditionally is what causes in-order reassembly to stall behind the
	// slower lane.
	flow.lanes[1].nextFree = time.Now().Add(500 * time.Millisecond)
	second, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if second.id != 2 {
		t.Fatalf("busy bulk selection chose lane %d, want the idle lane 2", second.id)
	}
	// The control lane stays excluded even when both bulk lanes are busy.
	flow.lanes[2].nextFree = time.Now().Add(time.Second)
	third, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if third.id != 1 {
		t.Fatalf("busy bulk selection chose lane %d, want the least backlogged bulk lane 1", third.id)
	}
}

func TestBulkSelectionFallsBackToControlLane(t *testing.T) {
	flow := &multipathFlow{
		done: make(chan struct{}), lanes: map[uint64]*mpLane{0: {id: 0}}, reserveControlLane: true,
	}
	lane, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if lane.id != 0 {
		t.Fatalf("bulk fallback chose lane %d, want control lane 0", lane.id)
	}
}

func TestBulkSelectionKeepsAllLanesWithoutReservation(t *testing.T) {
	flow := &multipathFlow{
		done: make(chan struct{}), lanes: map[uint64]*mpLane{0: {id: 0}, 1: {id: 1}},
	}
	first, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if first.id != 0 {
		t.Fatalf("unreserved bulk selection chose lane %d, want lane 0", first.id)
	}
	flow.lanes[0].nextFree = time.Now().Add(500 * time.Millisecond)
	second, err := flow.chooseLane(true)
	if err != nil {
		t.Fatal(err)
	}
	if second.id != 1 {
		t.Fatalf("backlogged unreserved selection chose lane %d, want the idle lane 1", second.id)
	}
}

func TestBulkLanePrewarmRequiresAgeBytesAndDirectionality(t *testing.T) {
	tests := []struct {
		name     string
		snapshot flowSnapshot
		want     bool
	}{
		{name: "young", snapshot: flowSnapshot{Bytes: 256 * 1024, BytesUp: 1, BytesDown: 256 * 1024, Elapsed: 100 * time.Millisecond}},
		{name: "small", snapshot: flowSnapshot{Bytes: 32 * 1024, BytesUp: 1, BytesDown: 32 * 1024, Elapsed: time.Second}},
		{name: "balanced interactive", snapshot: flowSnapshot{Bytes: 256 * 1024, BytesUp: 128 * 1024, BytesDown: 128 * 1024, Elapsed: time.Second}},
		{name: "download", snapshot: flowSnapshot{Bytes: 256*1024 + 1024, BytesUp: 1024, BytesDown: 256 * 1024, Elapsed: time.Second}, want: true},
		{name: "upload", snapshot: flowSnapshot{Bytes: 256*1024 + 1024, BytesUp: 256 * 1024, BytesDown: 1024, Elapsed: time.Second}, want: true},
		{name: "one way", snapshot: flowSnapshot{Bytes: 64 * 1024, BytesDown: 64 * 1024, Elapsed: bulkLanePrewarmAge}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldPrewarmBulkLane(test.snapshot); got != test.want {
				t.Fatalf("prewarm = %t, want %t for %+v", got, test.want, test.snapshot)
			}
		})
	}
}

func TestLaneQueueHasGlobalBound(t *testing.T) {
	flow := &multipathFlow{ctx: context.Background(), done: make(chan struct{}), laneErr: make(chan laneFailure, 1)}
	lane := &mpLane{
		writeQ:            make(chan protocol.Frame, maxLaneWriteQueue),
		writeInteractiveQ: make(chan protocol.Frame, maxLaneWriteQueue),
		writeSlots:        make(chan struct{}, maxLaneWriteQueue),
		writeDone:         make(chan struct{}),
	}
	frame := protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Class: protocol.ClassBulk}, Payload: []byte("x")}
	for i := 0; i < maxLaneBulkQueue; i++ {
		if err := flow.enqueueFrameClass(context.Background(), lane, frame, true); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	// The reserved interactive slots remain available even when bulk is at
	// its queue limit. Fill those slots with high-priority frames, then verify
	// that the combined queue cannot exceed the global bound.
	interactive := frame
	interactive.Header.Class = protocol.ClassInteractive
	for i := 0; i < maxLaneInteractiveReserve; i++ {
		if err := flow.enqueueFrameClass(context.Background(), lane, interactive, false); err != nil {
			t.Fatalf("interactive enqueue %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := flow.enqueueFrameClass(ctx, lane, frame, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("overflow enqueue error = %v, want context deadline", err)
	}
	if got := len(lane.writeSlots); got != maxLaneWriteQueue {
		t.Fatalf("global queue slots used = %d, want %d", got, maxLaneWriteQueue)
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
	// A frame larger than any window the flow could ever be granted must be
	// rejected rather than waiting forever for space that cannot arrive.
	tooLarge := protocol.Frame{
		Header:  protocol.Header{Type: protocol.TypeData},
		Payload: []byte(strings.Repeat("x", maxFlowReplayBytes+1)),
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
