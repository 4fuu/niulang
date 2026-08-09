package pep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/icourses-dev/wanopt/internal/classifier"
	"github.com/icourses-dev/wanopt/internal/limiter"
	"github.com/icourses-dev/wanopt/internal/multipath"
	"github.com/icourses-dev/wanopt/internal/protocol"
)

const (
	maxLaneEvents       = 256
	maxReassemblyBytes  = 8 * 1024 * 1024
	maxReassemblyFrames = 4096
	maxLaneWriteQueue   = 64
	maxReplayBytes      = 8 * 1024 * 1024
	maxReplayFrames     = 4096
	laneReplacementWait = 15 * time.Second
)

type mpLane struct {
	id        uint64
	kind      TransportKind
	fc        *frameConn
	writeQ    chan protocol.Frame
	writeDone chan struct{}
	closed    atomic.Bool
	sent      atomic.Uint64
	recv      atomic.Uint64
}

type inboundEvent struct {
	lane  *mpLane
	frame protocol.Frame
}

// laneFailure is emitted once for a physical lane. The identity prevents a
// delayed error from an old lane being confused with a replacement failure.
type laneFailure struct {
	lane *mpLane
	err  error
}

type multipathFlow struct {
	ctx       context.Context
	inner     net.Conn
	sessionID [16]byte
	flowID    uint64
	chunkSize int
	budget    *limiter.Budget

	sendAckFlag uint16
	recvAckFlag uint16

	lanesMu      sync.RWMutex
	lanes        map[uint64]*mpLane
	laneCursorMu sync.Mutex
	laneCursor   int

	events   chan inboundEvent
	laneErr  chan laneFailure
	finalAck chan struct{}
	sendDone chan struct{}
	done     chan struct{}

	classifier  *classifier.Classifier
	started     time.Time
	bytesUp     atomic.Uint64
	bytesDown   atomic.Uint64
	class       atomic.Uint32
	finSequence atomic.Uint64
	lastPayload atomic.Int64
	closeOnce   sync.Once
	doneOnce    sync.Once
	finished    atomic.Bool
	nextJoinID  uint64

	replayMu     sync.Mutex
	replay       map[uint64]protocol.Frame
	replayNotify chan struct{}
	replayBytes  uint64
	acked        uint64
	highestSent  uint64
}

func newMultipathFlow(ctx context.Context, inner net.Conn, sessionID [16]byte, flowID uint64, chunkSize int, sendAckFlag, recvAckFlag uint16, budget *limiter.Budget) *multipathFlow {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	f := &multipathFlow{
		ctx: ctx, inner: inner, sessionID: sessionID, flowID: flowID, chunkSize: chunkSize, budget: budget,
		sendAckFlag: sendAckFlag, recvAckFlag: recvAckFlag,
		lanes: make(map[uint64]*mpLane), events: make(chan inboundEvent, maxLaneEvents), laneErr: make(chan laneFailure, maxLaneEvents),
		finalAck: make(chan struct{}, 1), sendDone: make(chan struct{}),
		done:       make(chan struct{}),
		classifier: classifier.New(classifier.DefaultConfig()), started: time.Now(),
		replay: make(map[uint64]protocol.Frame), replayNotify: make(chan struct{}, 1),
	}
	f.class.Store(uint32(protocol.ClassNew))
	return f
}

func (f *multipathFlow) addLane(lane *mpLane) error {
	if lane == nil || lane.fc == nil {
		return errors.New("invalid lane")
	}
	select {
	case <-f.done:
		return errors.New("flow is closed")
	default:
	}
	f.lanesMu.Lock()
	if _, exists := f.lanes[lane.id]; exists {
		f.lanesMu.Unlock()
		return errors.New("duplicate lane id")
	}
	if lane.writeQ == nil {
		lane.writeQ = make(chan protocol.Frame, maxLaneWriteQueue)
	}
	if lane.writeDone == nil {
		lane.writeDone = make(chan struct{})
	}
	f.lanes[lane.id] = lane
	if lane.id >= f.nextJoinID {
		f.nextJoinID = lane.id + 1
	}
	f.lanesMu.Unlock()
	go f.readLane(lane)
	go f.writeLane(lane)
	return nil
}

// writeLane serializes data and close frames for one lane while allowing
// other lanes to make progress independently. The bounded queue prevents a
// slow lane from becoming an unbounded memory sink. ACK/PING/PONG writes may
// still call frameConn.Write directly; its mutex preserves frame integrity.
func (f *multipathFlow) writeLane(lane *mpLane) {
	defer close(lane.writeDone)
	for {
		var frame protocol.Frame
		select {
		case frame = <-lane.writeQ:
		case <-f.done:
			return
		case <-f.ctx.Done():
			return
		}
		if err := lane.fc.Write(frame); err != nil {
			f.failLane(lane, fmt.Errorf("lane %d write: %w", lane.id, err))
			return
		}
		if frame.Header.Type == protocol.TypeData {
			lane.sent.Add(uint64(len(frame.Payload)))
		}
	}
}

type flowSnapshot struct {
	Class        classifier.Class
	CurrentLanes int
	HealthyLanes int
	Bytes        uint64
	Elapsed      time.Duration
}

func (f *multipathFlow) snapshot() flowSnapshot {
	lanes := f.healthyLanes()
	return flowSnapshot{
		Class: classifier.Class(f.classifier.Class()), CurrentLanes: f.laneCount(), HealthyLanes: len(lanes),
		Bytes: f.bytesUp.Load() + f.bytesDown.Load(), Elapsed: time.Since(f.started),
	}
}

func (f *multipathFlow) laneCount() int {
	f.lanesMu.RLock()
	defer f.lanesMu.RUnlock()
	count := 0
	for _, lane := range f.lanes {
		if !lane.closed.Load() {
			count++
		}
	}
	return count
}

// retireOldestLane makes room for a replacement when the peer has observed a
// dead lane but the server-side socket is still half-open. It is only used at
// the configured lane cap; deleting the entry keeps the cap a real resource
// bound rather than allowing unbounded historical lane IDs.
func (f *multipathFlow) retireOldestLane() bool {
	f.lanesMu.Lock()
	var victim *mpLane
	for _, lane := range f.lanes {
		if lane.closed.Load() {
			continue
		}
		if victim == nil || lane.id < victim.id {
			victim = lane
		}
	}
	if victim == nil {
		f.lanesMu.Unlock()
		return false
	}
	delete(f.lanes, victim.id)
	victim.closed.Store(true)
	f.lanesMu.Unlock()
	if victim.fc != nil {
		_ = victim.fc.Close()
	}
	return true
}

func (f *multipathFlow) allocateJoinID() (uint64, error) {
	f.lanesMu.Lock()
	defer f.lanesMu.Unlock()
	for id := f.nextJoinID; id < 1<<20; id++ {
		if _, exists := f.lanes[id]; !exists {
			f.nextJoinID = id + 1
			return id, nil
		}
	}
	return 0, errors.New("unable to allocate lane id")
}

func (f *multipathFlow) readLane(lane *mpLane) {
	for {
		frame, err := lane.fc.Read()
		if err != nil {
			f.failLane(lane, fmt.Errorf("lane %d: %w", lane.id, err))
			return
		}
		if frame.Header.Type == protocol.TypeData {
			lane.recv.Add(uint64(len(frame.Payload)))
		}
		select {
		case f.events <- inboundEvent{lane: lane, frame: frame}:
		case <-f.ctx.Done():
			return
		}
	}
}

// failLane transitions a lane to failed exactly once, stops both of its I/O
// goroutines, and notifies the flow-level recovery coordinator. A failed lane
// is never selected again, even if a buffered write completes later.
func (f *multipathFlow) failLane(lane *mpLane, err error) {
	if lane == nil || !lane.closed.CompareAndSwap(false, true) {
		return
	}
	if lane.fc != nil {
		_ = lane.fc.Close()
	}
	if f.finished.Load() {
		return
	}
	select {
	case f.laneErr <- laneFailure{lane: lane, err: err}:
	default:
		// The lane is already marked failed. The coordinator also observes
		// current health directly, so coalescing notifications is safe.
	}
}

func (f *multipathFlow) healthyLanes() []*mpLane {
	f.lanesMu.RLock()
	defer f.lanesMu.RUnlock()
	lanes := make([]*mpLane, 0, len(f.lanes))
	for _, lane := range f.lanes {
		if !lane.closed.Load() {
			lanes = append(lanes, lane)
		}
	}
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].id < lanes[j].id })
	return lanes
}

func (f *multipathFlow) chooseLane(bulk bool) (*mpLane, error) {
	lanes := f.healthyLanes()
	if len(lanes) == 0 {
		return nil, errors.New("no healthy lanes")
	}
	if !bulk || len(lanes) == 1 {
		return lanes[0], nil
	}
	f.laneCursorMu.Lock()
	defer f.laneCursorMu.Unlock()
	lane := lanes[f.laneCursor%len(lanes)]
	f.laneCursor = (f.laneCursor + 1) % len(lanes)
	return lane, nil
}

func (f *multipathFlow) run(ctx context.Context) (FlowStats, error) {
	defer f.signalDone()
	defer f.finished.Store(true)
	stats := FlowStats{Started: f.started}
	results := make(chan error, 2)
	go func() { results <- f.sendInner(ctx) }()
	go func() { results <- f.receiveInner(ctx) }()
	completed := 0
	for completed < 2 {
		select {
		case err := <-results:
			completed++
			if err != nil {
				f.closeAll()
				stats.Ended = time.Now()
				stats.BytesSent = f.bytesUp.Load()
				stats.BytesRead = f.bytesDown.Load()
				stats.LaneBytes = f.laneStats()
				return stats, err
			}
		case failure := <-f.laneErr:
			err := failure.err
			// A secondary lane can fail without invalidating the bytes already
			// delivered on the logical flow. Replay unacknowledged frames on a
			// surviving lane. If the last lane fails, or replay is impossible,
			// fail closed and let the caller retry the application flow.
			if len(f.healthyLanes()) == 0 {
				if waitErr := f.waitForHealthyLane(ctx, laneReplacementWait); waitErr != nil {
					err = fmt.Errorf("last lane failed (%v): %w", err, waitErr)
				}
			}
			if len(f.healthyLanes()) > 0 {
				if replayErr := f.replayPending(ctx); replayErr == nil {
					continue
				} else {
					err = fmt.Errorf("lane failed (%v), replay failed: %w", err, replayErr)
				}
			}
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			stats.LaneBytes = f.laneStats()
			return stats, err
		case <-ctx.Done():
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			stats.LaneBytes = f.laneStats()
			return stats, ctx.Err()
		}
	}
	f.closeAll()
	stats.Ended = time.Now()
	stats.BytesSent = f.bytesUp.Load()
	stats.BytesRead = f.bytesDown.Load()
	stats.LaneBytes = f.laneStats()
	return stats, nil
}

func (f *multipathFlow) signalDone() {
	if f.done != nil {
		f.doneOnce.Do(func() { close(f.done) })
	}
}

func (f *multipathFlow) laneStats() map[uint64]LaneStats {
	f.lanesMu.RLock()
	defer f.lanesMu.RUnlock()
	stats := make(map[uint64]LaneStats, len(f.lanes))
	for id, lane := range f.lanes {
		stats[id] = LaneStats{Kind: lane.kind, Sent: lane.sent.Load(), Received: lane.recv.Load()}
	}
	return stats
}

func (f *multipathFlow) doneChan() <-chan struct{} { return f.done }

func (f *multipathFlow) sendInner(ctx context.Context) (err error) {
	defer close(f.sendDone)
	buf := make([]byte, f.chunkSize)
	var sequence uint64
	for {
		n, readErr := f.inner.Read(buf)
		if n > 0 {
			bulk := f.observe(n, true)
			payload := append([]byte(nil), buf[:n]...)
			frame := protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeData, SessionID: f.sessionID, FlowID: f.flowID,
				Sequence: sequence, Class: protocol.Class(f.class.Load()),
			}, Payload: payload}
			if err := f.recordReplayContext(ctx, frame); err != nil {
				return err
			}
			if err := f.enqueueOnHealthyLane(ctx, frame, bulk); err != nil {
				return err
			}
			sequence += uint64(n)
			f.bytesUp.Add(uint64(n))
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			f.sendSequence(sequence)
			fin := protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
				SessionID: f.sessionID, FlowID: f.flowID, Sequence: sequence, Class: protocol.Class(f.class.Load()),
			}}
			if err := f.recordReplayContext(ctx, fin); err != nil {
				return err
			}
			if err := f.enqueueOnHealthyLane(ctx, fin, false); err != nil {
				return err
			}
			select {
			case <-f.finalAck:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (f *multipathFlow) enqueueFrame(ctx context.Context, lane *mpLane, frame protocol.Frame) error {
	if lane == nil || lane.closed.Load() {
		return errors.New("lane is closed")
	}
	select {
	case lane.writeQ <- frame:
		return nil
	case <-lane.writeDone:
		f.failLane(lane, errors.New("lane writer stopped"))
		return errors.New("lane writer stopped")
	case <-f.done:
		return errors.New("flow is closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *multipathFlow) enqueueOnHealthyLane(ctx context.Context, frame protocol.Frame, bulk bool) error {
	if f.budget != nil {
		interactive := !bulk
		if err := f.budget.Wait(ctx, len(frame.Payload), interactive); err != nil {
			return fmt.Errorf("aggregate pacing: %w", err)
		}
	}
	for {
		lane, err := f.chooseLane(bulk)
		if err == nil {
			if err = f.enqueueFrame(ctx, lane, frame); err == nil {
				return nil
			}
		}
		if err := f.waitForHealthyLane(ctx, laneReplacementWait); err != nil {
			return err
		}
	}
}

func (f *multipathFlow) waitForHealthyLane(ctx context.Context, timeout time.Duration) error {
	if len(f.healthyLanes()) > 0 {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if len(f.healthyLanes()) > 0 {
				return nil
			}
		case <-timer.C:
			return errors.New("lane replacement timeout")
		case <-f.done:
			return errors.New("flow closed while waiting for lane replacement")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *multipathFlow) recordReplay(frame protocol.Frame) error {
	return f.recordReplayContext(context.Background(), frame)
}

// recordReplayContext applies backpressure when the bounded replay window is
// full. Returning an error immediately would reset a healthy application flow
// merely because ACKs were delayed by the path; waiting is safe because the
// caller's context and the flow shutdown path both have explicit bounds.
func (f *multipathFlow) recordReplayContext(ctx context.Context, frame protocol.Frame) error {
	if frame.Header.Type != protocol.TypeData && frame.Header.Type != protocol.TypeClose {
		return errors.New("only data and close frames are replayable")
	}
	if frame.Header.Sequence > ^uint64(0)-uint64(len(frame.Payload)) {
		return errors.New("replay sequence overflow")
	}
	for {
		f.replayMu.Lock()
		if f.replay == nil {
			f.replay = make(map[uint64]protocol.Frame)
		}
		if _, exists := f.replay[frame.Header.Sequence]; exists {
			f.replayMu.Unlock()
			return errors.New("duplicate replay sequence")
		}
		if len(frame.Payload) > maxReplayBytes {
			f.replayMu.Unlock()
			return errors.New("replay frame exceeds buffer limit")
		}
		if len(f.replay)+1 <= maxReplayFrames && f.replayBytes+uint64(len(frame.Payload)) <= maxReplayBytes {
			frame.Payload = append([]byte(nil), frame.Payload...)
			f.replay[frame.Header.Sequence] = frame
			f.replayBytes += uint64(len(frame.Payload))
			end := frame.Header.Sequence + uint64(len(frame.Payload))
			if end > f.highestSent {
				f.highestSent = end
			}
			f.replayMu.Unlock()
			return nil
		}
		f.replayMu.Unlock()
		select {
		case <-f.replayNotify:
		case <-f.done:
			return errors.New("flow closed while waiting for replay space")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *multipathFlow) acknowledgeReplay(sequence uint64, final bool) error {
	f.replayMu.Lock()
	if sequence > f.highestSent {
		f.replayMu.Unlock()
		return fmt.Errorf("acknowledgement %d exceeds sent sequence %d", sequence, f.highestSent)
	}
	if sequence < f.acked {
		f.replayMu.Unlock()
		return nil // delayed ACK from a slower lane
	}
	f.acked = sequence
	for start, frame := range f.replay {
		end := start + uint64(len(frame.Payload))
		if frame.Header.Type == protocol.TypeData && end <= sequence {
			delete(f.replay, start)
			f.replayBytes -= uint64(len(frame.Payload))
		}
		if final && frame.Header.Type == protocol.TypeClose && start <= sequence {
			delete(f.replay, start)
		}
	}
	f.replayMu.Unlock()
	select {
	case f.replayNotify <- struct{}{}:
	default:
	}
	return nil
}

func (f *multipathFlow) replayPending(ctx context.Context) error {
	f.replayMu.Lock()
	sequences := make([]uint64, 0, len(f.replay))
	for sequence := range f.replay {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	frames := make([]protocol.Frame, 0, len(sequences))
	for _, sequence := range sequences {
		frame := f.replay[sequence]
		frame.Payload = append([]byte(nil), frame.Payload...)
		frames = append(frames, frame)
	}
	f.replayMu.Unlock()

	for _, frame := range frames {
		if err := f.enqueueOnHealthyLane(ctx, frame, frame.Header.Type == protocol.TypeData); err != nil {
			return err
		}
	}
	return nil
}

func (f *multipathFlow) sendSequence(sequence uint64) {
	// The sequence is immutable after FIN and is read by the receive loop when
	// an ACK arrives. A channel would also work, but atomic storage avoids a
	// second synchronization point on every data frame.
	f.finSequence.Store(sequence)
}

func (f *multipathFlow) writeControl(ctx context.Context, frame protocol.Frame, preferred *mpLane) error {
	var attempted map[uint64]bool
	if preferred != nil {
		attempted = map[uint64]bool{preferred.id: true}
	}
	for {
		lanes := f.healthyLanes()
		if len(lanes) == 0 {
			if err := f.waitForHealthyLane(ctx, laneReplacementWait); err != nil {
				return err
			}
			continue
		}
		for _, lane := range lanes {
			if attempted != nil && attempted[lane.id] {
				continue
			}
			if err := lane.fc.Write(frame); err != nil {
				f.failLane(lane, fmt.Errorf("lane %d control write: %w", lane.id, err))
				if attempted == nil {
					attempted = make(map[uint64]bool)
				}
				attempted[lane.id] = true
				continue
			}
			return nil
		}
		// Every lane in this pass failed. Start a new pass so a replacement
		// installed by the lane manager can carry the control frame.
		attempted = nil
	}
}

func (f *multipathFlow) writeACK(ctx context.Context, sequence uint64, direction uint16, final bool) error {
	flags := direction
	if final {
		flags |= protocol.FlagAckFinal
	}
	return f.writeControl(ctx, protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeAck, Flags: flags,
		SessionID: f.sessionID, FlowID: f.flowID, Sequence: sequence,
		Class: protocol.Class(f.class.Load()),
	}}, nil)
}

func (f *multipathFlow) receiveInner(ctx context.Context) error {
	reassembler := multipath.NewReassembler(multipath.Config{MaxBufferedBytes: maxReassemblyBytes, MaxBufferedFrames: maxReassemblyFrames})
	remoteFin := false
	var lastAckSequence uint64
	for {
		select {
		case event := <-f.events:
			frame := event.frame
			if frame.Header.SessionID != f.sessionID || frame.Header.FlowID != f.flowID {
				return errors.New("frame belongs to another session or flow")
			}
			switch frame.Header.Type {
			case protocol.TypeData:
				if remoteFin {
					return errors.New("data received after flow FIN")
				}
				out, closed, err := reassembler.Insert(multipath.Segment{Sequence: frame.Header.Sequence, Payload: frame.Payload})
				if err != nil {
					return err
				}
				if len(out) > 0 {
					if err := writeFull(f.inner, out); err != nil {
						return err
					}
					f.observe(len(out), false)
					f.bytesDown.Add(uint64(len(out)))
					if next := reassembler.NextSequence(); next > lastAckSequence {
						if err := f.writeACK(ctx, next, f.recvAckFlag, false); err != nil {
							return err
						}
						lastAckSequence = next
					}
				}
				if closed {
					return errors.New("reassembler closed without FIN frame")
				}
			case protocol.TypeClose:
				if frame.Header.Flags&protocol.FlagFin == 0 || len(frame.Payload) != 0 {
					return errors.New("invalid flow close frame")
				}
				out, closed, err := reassembler.Insert(multipath.Segment{Sequence: frame.Header.Sequence, Final: true})
				if err != nil {
					return err
				}
				if len(out) > 0 {
					if err := writeFull(f.inner, out); err != nil {
						return err
					}
					f.observe(len(out), false)
					f.bytesDown.Add(uint64(len(out)))
				}
				if closed {
					if cw, ok := f.inner.(closeWriter); ok {
						if err := cw.CloseWrite(); err != nil && !errors.Is(err, net.ErrClosed) {
							return err
						}
					}
					if err := f.writeACK(ctx, reassembler.NextSequence(), f.recvAckFlag, true); err != nil {
						return err
					}
					remoteFin = true
					select {
					case <-f.sendDone:
						return nil
					default:
					}
				}
			case protocol.TypeAck:
				if frame.Header.Flags&f.sendAckFlag == 0 {
					return errors.New("acknowledgement has wrong direction")
				}
				if frame.Header.Flags&protocol.FlagAckFinal == 0 {
					if err := f.acknowledgeReplay(frame.Header.Sequence, false); err != nil {
						return err
					}
					continue
				}
				if frame.Header.Sequence == f.finSequence.Load() {
					if err := f.acknowledgeReplay(frame.Header.Sequence, true); err != nil {
						return err
					}
					select {
					case f.finalAck <- struct{}{}:
					default:
					}
					if remoteFin {
						return nil
					}
				} else {
					return errors.New("final acknowledgement sequence mismatch")
				}
			case protocol.TypeReset:
				if len(frame.Payload) > 1 {
					return fmt.Errorf("peer reset flow: %s", string(frame.Payload[1:]))
				}
				return errors.New("peer reset flow")
			case protocol.TypePing:
				if err := f.writeControl(ctx, protocol.Frame{Header: protocol.Header{
					Version: protocol.Version, Type: protocol.TypePong, SessionID: f.sessionID, FlowID: f.flowID,
					Sequence: reassembler.NextSequence(), Class: protocol.Class(f.class.Load()),
				}, Payload: frame.Payload}, event.lane); err != nil {
					return err
				}
			case protocol.TypePong, protocol.TypeWindow:
			default:
				return fmt.Errorf("unexpected flow frame type %d", frame.Header.Type)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *multipathFlow) observe(n int, up bool) bool {
	now := time.Now()
	previousPayload := f.lastPayload.Swap(now.UnixNano())
	age := now.Sub(f.started)
	if age <= 0 {
		age = time.Nanosecond
	}
	upBytes := f.bytesUp.Load()
	downBytes := f.bytesDown.Load()
	if up {
		upBytes += uint64(n)
	} else {
		downBytes += uint64(n)
	}
	obs := classifier.Observation{
		BytesUp: upBytes, BytesDown: downBytes,
		UpRate: float64(upBytes) / age.Seconds(), DownRate: float64(downBytes) / age.Seconds(),
		Age: age, Bidirectional: upBytes > 0 && downBytes > 0,
		SinceLastPayload: func() time.Duration {
			if previousPayload == 0 {
				return age
			}
			return now.Sub(time.Unix(0, previousPayload))
		}(),
		SmallBidirectionalBursts: n <= 16*1024,
	}
	newClass := f.classifier.Observe(obs)
	f.class.Store(uint32(protocol.Class(newClass)))
	return newClass == classifier.ClassBulk
}

func (f *multipathFlow) closeAll() {
	f.closeOnce.Do(func() {
		_ = f.inner.Close()
		f.lanesMu.RLock()
		defer f.lanesMu.RUnlock()
		for _, lane := range f.lanes {
			_ = lane.fc.Close()
		}
	})
	f.signalDone()
}
