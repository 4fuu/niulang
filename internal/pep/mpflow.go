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
	"github.com/icourses-dev/wanopt/internal/multipath"
	"github.com/icourses-dev/wanopt/internal/protocol"
)

const (
	maxLaneEvents       = 256
	maxReassemblyBytes  = 8 * 1024 * 1024
	maxReassemblyFrames = 4096
)

type mpLane struct {
	id     uint64
	kind   TransportKind
	fc     *frameConn
	closed atomic.Bool
}

type inboundEvent struct {
	lane  *mpLane
	frame protocol.Frame
}

type multipathFlow struct {
	ctx       context.Context
	inner     net.Conn
	sessionID [16]byte
	flowID    uint64
	chunkSize int

	sendAckFlag uint16
	recvAckFlag uint16

	lanesMu    sync.RWMutex
	lanes      map[uint64]*mpLane
	laneCursor int

	events   chan inboundEvent
	laneErr  chan error
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
	finished    atomic.Bool
	nextJoinID  uint64
}

func newMultipathFlow(ctx context.Context, inner net.Conn, sessionID [16]byte, flowID uint64, chunkSize int, sendAckFlag, recvAckFlag uint16) *multipathFlow {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	f := &multipathFlow{
		ctx: ctx, inner: inner, sessionID: sessionID, flowID: flowID, chunkSize: chunkSize,
		sendAckFlag: sendAckFlag, recvAckFlag: recvAckFlag,
		lanes: make(map[uint64]*mpLane), events: make(chan inboundEvent, maxLaneEvents), laneErr: make(chan error, 1),
		finalAck: make(chan struct{}, 1), sendDone: make(chan struct{}),
		done:       make(chan struct{}),
		classifier: classifier.New(classifier.DefaultConfig()), started: time.Now(),
	}
	f.class.Store(uint32(protocol.ClassNew))
	return f
}

func (f *multipathFlow) addLane(lane *mpLane) error {
	if lane == nil || lane.fc == nil {
		return errors.New("invalid lane")
	}
	f.lanesMu.Lock()
	if _, exists := f.lanes[lane.id]; exists {
		f.lanesMu.Unlock()
		return errors.New("duplicate lane id")
	}
	f.lanes[lane.id] = lane
	if lane.id >= f.nextJoinID {
		f.nextJoinID = lane.id + 1
	}
	f.lanesMu.Unlock()
	go f.readLane(lane)
	return nil
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
	return len(f.lanes)
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
	defer lane.closed.Store(true)
	for {
		frame, err := lane.fc.Read()
		if err != nil {
			if f.finished.Load() {
				return
			}
			select {
			case f.laneErr <- fmt.Errorf("lane %d: %w", lane.id, err):
			default:
			}
			return
		}
		select {
		case f.events <- inboundEvent{lane: lane, frame: frame}:
		case <-f.ctx.Done():
			return
		}
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
	lane := lanes[f.laneCursor%len(lanes)]
	f.laneCursor = (f.laneCursor + 1) % len(lanes)
	return lane, nil
}

func (f *multipathFlow) run(ctx context.Context) (FlowStats, error) {
	defer close(f.done)
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
				return stats, err
			}
		case err := <-f.laneErr:
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			return stats, err
		case <-ctx.Done():
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			return stats, ctx.Err()
		}
	}
	f.closeAll()
	stats.Ended = time.Now()
	stats.BytesSent = f.bytesUp.Load()
	stats.BytesRead = f.bytesDown.Load()
	return stats, nil
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
			lane, err := f.chooseLane(bulk)
			if err != nil {
				return err
			}
			payload := append([]byte(nil), buf[:n]...)
			if err := lane.fc.Write(protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeData, SessionID: f.sessionID, FlowID: f.flowID,
				Sequence: sequence, Class: protocol.Class(f.class.Load()),
			}, Payload: payload}); err != nil {
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
			lane, err := f.chooseLane(false)
			if err != nil {
				return err
			}
			if err := lane.fc.Write(protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
				SessionID: f.sessionID, FlowID: f.flowID, Sequence: sequence, Class: protocol.Class(f.class.Load()),
			}}); err != nil {
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

func (f *multipathFlow) sendSequence(sequence uint64) {
	// The sequence is immutable after FIN and is read by the receive loop when
	// an ACK arrives. A channel would also work, but atomic storage avoids a
	// second synchronization point on every data frame.
	f.finSequence.Store(sequence)
}

func (f *multipathFlow) receiveInner(ctx context.Context) error {
	reassembler := multipath.NewReassembler(multipath.Config{MaxBufferedBytes: maxReassemblyBytes, MaxBufferedFrames: maxReassemblyFrames})
	remoteFin := false
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
					if err := event.lane.fc.Write(protocol.Frame{Header: protocol.Header{
						Version: protocol.Version, Type: protocol.TypeAck,
						Flags:     protocol.FlagAckFinal | f.recvAckFlag,
						SessionID: f.sessionID, FlowID: f.flowID, Sequence: reassembler.NextSequence(),
						Class: protocol.Class(f.class.Load()),
					}}); err != nil {
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
				if frame.Header.Flags&protocol.FlagAckFinal != 0 && frame.Header.Flags&f.sendAckFlag != 0 && frame.Header.Sequence == f.finSequence.Load() {
					select {
					case f.finalAck <- struct{}{}:
					default:
					}
					if remoteFin {
						return nil
					}
				}
			case protocol.TypeReset:
				if len(frame.Payload) > 1 {
					return fmt.Errorf("peer reset flow: %s", string(frame.Payload[1:]))
				}
				return errors.New("peer reset flow")
			case protocol.TypePing:
				if err := event.lane.fc.Write(protocol.Frame{Header: protocol.Header{
					Version: protocol.Version, Type: protocol.TypePong, SessionID: f.sessionID, FlowID: f.flowID,
					Sequence: reassembler.NextSequence(), Class: protocol.Class(f.class.Load()),
				}, Payload: frame.Payload}); err != nil {
					return err
				}
			case protocol.TypePong, protocol.TypeWindow:
			default:
				return fmt.Errorf("unexpected flow frame type %d", frame.Header.Type)
			}
		case err := <-f.laneErr:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *multipathFlow) observe(n int, up bool) bool {
	now := time.Now()
	f.lastPayload.Store(now.UnixNano())
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
}
