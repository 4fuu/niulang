// Package stripe schedules one byte stream across several independent lanes by
// self-pacing: a lane is handed its next chunk when it finishes the last one.
//
// The design it replaces predicted. It estimated each lane's rate, computed
// which lane would deliver a frame soonest, and committed frames to that lane's
// queue ahead of time -- up to 56 frames, 1.75 MiB, on a queue the scheduler
// could not take back. Every byte of that commitment was a bet on an estimate.
// When the bet was wrong, the bytes were already numbered and sitting behind a
// lane that had slowed down, and the receiver could not deliver past them; the
// remedies for that (reinjecting a stalled head on a timer, a replay window
// sized to the whole reorder span) were all corrections for mis-prediction.
//
// Self-pacing needs no estimate. A lane asks for work when it has capacity, so
// a lane that is fast asks often and a lane that is throttled asks rarely; a
// lane that has stopped entirely asks never and is thereby excluded without
// anyone deciding to exclude it. The rate each lane gets is whatever it can
// actually carry, measured by the only instrument that cannot be wrong about
// it: the lane itself.
//
// Two properties follow, and both are the point:
//
//   - A lane's commitment is bounded by its window (chunks it has been handed
//     and not yet completed), not by a queue depth chosen in advance. Losing a
//     lane costs that much data, and the loss is recoverable because any chunk
//     may be re-issued on any other lane.
//   - Lanes are time-independent. Nothing about chunk N's placement constrains
//     chunk N+1, so there is no ordering to violate and no schedule to fall
//     behind. Order is reconstructed at the receiver from byte offsets, which
//     is where it belongs.
package stripe

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// ErrClosed reports that the scheduler has been closed.
var ErrClosed = errors.New("stripe: scheduler closed")

// Windows supplies the admission limits a lane must satisfy to be handed more
// work. Both are in bytes of unacknowledged data.
//
// Keeping this an interface is what stops congestion control leaking into the
// scheduler. The scheduler's job is to hand the oldest ready chunk to a lane
// that has room; deciding how much room a lane has is a different question,
// answered by the transport and by the flow's coupled controller.
type Windows interface {
	// Lane is how much this lane may hold. It bounds what the sender commits
	// to a path that may not be able to move it.
	Lane(laneID uint64) int
	// Total is how much the flow may hold across every lane. It is what stops
	// N lanes claiming N times a single connection's share.
	Total() int
}

// Config bounds the scheduler.
type Config struct {
	// ChunkSize is the largest chunk handed to a lane. It trades balance
	// granularity against per-chunk overhead: with C bytes per chunk, the most
	// any single lane can hold up is C bytes of the receiver's contiguous
	// point, so this is the head-of-line exposure of one stalled lane.
	ChunkSize int
	// LaneWindow caps how many chunks a lane may hold at once, as a backstop on
	// bookkeeping. The binding limit is normally the window in bytes the lane
	// passes to Next.
	//
	// A window of one would be purely self-paced but leaves the lane idle for a
	// round trip between finishing a chunk and being handed the next, so the
	// window has to cover the lane's bandwidth-delay product or the lane
	// under-runs. That number is a property of the lane, not of the scheduler,
	// which is why the lane supplies it: a lane that slows down has a smaller
	// product and its window shrinks with it, without the scheduler estimating
	// anything.
	LaneWindow int
	// MaxOutstanding bounds chunks held across all lanes, which bounds memory:
	// a chunk is retained until it completes, because it may need re-issuing.
	MaxOutstanding int
	// RetransmitAfter is how long a chunk may be outstanding on a lane before
	// it is also offered to another. Re-issuing duplicates bytes the receiver
	// discards, so this trades a little bandwidth for not waiting on a lane
	// that has gone quiet. Zero disables it.
	RetransmitAfter time.Duration
	// Windows supplies admission limits. When nil, only LaneWindow and
	// MaxOutstanding apply, which is the behaviour of a flow with no
	// congestion coupling.
	Windows Windows
	// Now is the clock, for tests.
	Now func() time.Time
}

// DefaultConfig returns bounds suitable for a long-haul path.
func DefaultConfig() Config {
	return Config{
		ChunkSize:       64 * 1024,
		LaneWindow:      4,
		MaxOutstanding:  64,
		RetransmitAfter: 2 * time.Second,
	}
}

func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.ChunkSize <= 0 {
		c.ChunkSize = d.ChunkSize
	}
	if c.LaneWindow <= 0 {
		c.LaneWindow = d.LaneWindow
	}
	if c.MaxOutstanding <= 0 {
		c.MaxOutstanding = d.MaxOutstanding
	}
	if c.MaxOutstanding < c.LaneWindow {
		c.MaxOutstanding = c.LaneWindow
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Chunk is a contiguous run of the source stream, addressed by byte offset.
// Offsets are what makes lanes independent: a chunk carries where it belongs,
// so it can travel any lane and arrive in any order.
type Chunk struct {
	Offset uint64
	Data   []byte
	// Final marks the chunk that ends the stream. It is delivered like any
	// other, so the end of the stream is ordered with respect to the data
	// rather than being a separate event that can overtake it.
	Final bool

	// urgent marks a chunk that came back from a lane which stalled or died.
	// Such a chunk is admitted to a lane even when that lane is at its window,
	// because it is almost certainly the one holding up the receiver's
	// contiguous point: every other lane is then full of chunks that cannot be
	// acknowledged until this one lands, so respecting the window here would
	// deadlock the flow with every lane waiting on a chunk none of them is
	// allowed to carry.
	urgent bool
}

// End returns the offset one past this chunk's last byte.
func (c *Chunk) End() uint64 { return c.Offset + uint64(len(c.Data)) }

type attempt struct {
	lane     uint64
	deadline time.Time
}

type outstanding struct {
	chunk    *Chunk
	attempts []attempt
}

// Stats reports what the scheduler did, for tests and metrics.
type Stats struct {
	ChunksIssued      uint64
	ChunksRetransmit  uint64
	ChunksCompleted   uint64
	BytesIssued       uint64
	BytesRetransmit   uint64
	LaneFailures      uint64
	SourceBytes       uint64
	PeakOutstanding   int
	CompletedByLaneID map[uint64]uint64
}

// Scheduler hands chunks of a source stream to lanes that ask for them.
//
// It is safe for concurrent use: one goroutine per lane calls Next, and the
// completion path may be called from anywhere.
type Scheduler struct {
	cfg Config

	mu       sync.Mutex
	ready    sync.Cond // a lane may have work, or space freed
	produced sync.Cond // the producer may read more

	src        io.Reader
	nextOffset uint64
	pending    []*Chunk
	live       map[uint64]*outstanding
	laneLoad   map[uint64]int
	laneBytes  map[uint64]uint64
	totalBytes uint64
	eof        bool
	srcErr     error
	closed     bool
	finished   chan struct{}
	stats      Stats
}

// New returns a scheduler that reads src and hands it out in chunks. It starts
// one goroutine to read ahead; Close stops it.
func New(src io.Reader, cfg Config) *Scheduler {
	cfg.applyDefaults()
	s := &Scheduler{
		cfg:       cfg,
		src:       src,
		live:      make(map[uint64]*outstanding),
		laneLoad:  make(map[uint64]int),
		laneBytes: make(map[uint64]uint64),
		finished:  make(chan struct{}),
	}
	s.stats.CompletedByLaneID = make(map[uint64]uint64)
	s.ready.L = &s.mu
	s.produced.L = &s.mu
	go s.produce()
	return s
}

// produce reads the source into ready chunks, staying within MaxOutstanding so
// a stalled transfer cannot buffer the whole stream in memory.
func (s *Scheduler) produce() {
	for {
		s.mu.Lock()
		for !s.closed && len(s.pending)+len(s.live) >= s.cfg.MaxOutstanding {
			s.produced.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		buf := make([]byte, s.cfg.ChunkSize)
		// One Read rather than ReadFull: a chunk is at most ChunkSize, not
		// exactly it. Waiting for a full buffer would hold a small write --
		// a request, an interactive turn -- until enough later bytes arrived
		// to fill the chunk, which is latency invented by the scheduler.
		n, err := s.src.Read(buf)

		s.mu.Lock()
		if n > 0 {
			chunk := &Chunk{Offset: s.nextOffset, Data: buf[:n]}
			s.nextOffset += uint64(n)
			s.stats.SourceBytes += uint64(n)
			s.pending = append(s.pending, chunk)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.markEOFLocked()
			} else {
				s.srcErr = err
				s.eof = true
			}
			s.signalIfDoneLocked()
			s.ready.Broadcast()
			s.mu.Unlock()
			return
		}
		s.ready.Broadcast()
		s.mu.Unlock()
	}
}

// markEOFLocked records the end of the stream. The final marker rides on the
// last chunk when there is one, so a receiver cannot see the end before the
// bytes that precede it.
func (s *Scheduler) markEOFLocked() {
	s.eof = true
	if len(s.pending) > 0 {
		s.pending[len(s.pending)-1].Final = true
		return
	}
	for _, out := range s.live {
		if out.chunk.End() == s.nextOffset {
			// The last data chunk is already in flight; it cannot be amended,
			// so the stream is ended by an empty final chunk at the tail.
			break
		}
	}
	s.pending = append(s.pending, &Chunk{Offset: s.nextOffset, Final: true})
}

// Next blocks until this lane should carry a chunk, and returns it. It returns
// nil with a nil error when the stream is finished, and ErrClosed after Close.
//
// This is the whole of the pacing policy: a lane calls it when it is ready for
// more work, so the lane's own speed decides its share. There is no rate
// estimate here, and no decision about which lane deserves the chunk -- the
// lane that asks first gets it, and a lane asks when it is free.
func (s *Scheduler) Next(ctx context.Context, laneID uint64, windowBytes int) (*Chunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stop := context.AfterFunc(ctx, func() { s.ready.Broadcast() })
	defer stop()

	for {
		if s.closed {
			return nil, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.laneLoad[laneID] < s.cfg.LaneWindow {
			if chunk := s.takeReadyLocked(laneID, s.hasRoomLocked(laneID, windowBytes)); chunk != nil {
				return chunk, nil
			}
			if err := s.finishedLocked(); err != nil {
				return nil, err
			} else if s.doneLocked() {
				return nil, nil
			}
		}
		s.ready.Wait()
	}
}

// takeReadyLocked assigns the oldest ready chunk to a lane. Oldest first
// matters: the receiver delivers contiguously, so the chunk nearest its
// contiguous point is the one whose absence is holding up delivery.
func (s *Scheduler) takeReadyLocked(laneID uint64, hasRoom bool) *Chunk {
	for i, chunk := range s.pending {
		if !hasRoom && !chunk.urgent {
			// The lane is full. Only recovery work jumps the window, and it is
			// always at the head of the ready set, so stopping here keeps the
			// scan cheap.
			return nil
		}
		if out, ok := s.live[chunk.Offset]; ok && laneHasAttempt(out, laneID) {
			// Never re-issue a chunk on the lane already carrying it: that is
			// the one lane whose failure it would not survive.
			continue
		}
		s.pending = append(s.pending[:i], s.pending[i+1:]...)
		out, exists := s.live[chunk.Offset]
		if !exists {
			out = &outstanding{chunk: chunk}
			s.live[chunk.Offset] = out
			s.stats.ChunksIssued++
			s.stats.BytesIssued += uint64(len(chunk.Data))
		} else {
			s.stats.ChunksRetransmit++
			s.stats.BytesRetransmit += uint64(len(chunk.Data))
		}
		var deadline time.Time
		if s.cfg.RetransmitAfter > 0 {
			deadline = s.cfg.Now().Add(s.cfg.RetransmitAfter)
		}
		out.attempts = append(out.attempts, attempt{lane: laneID, deadline: deadline})
		chunk.urgent = false
		s.laneLoad[laneID]++
		s.laneBytes[laneID] += uint64(len(chunk.Data))
		s.totalBytes += uint64(len(chunk.Data))
		if len(s.live) > s.stats.PeakOutstanding {
			s.stats.PeakOutstanding = len(s.live)
		}
		s.produced.Signal()
		return chunk
	}
	return nil
}

func laneHasAttempt(out *outstanding, laneID uint64) bool {
	for _, a := range out.attempts {
		if a.lane == laneID {
			return true
		}
	}
	return false
}

func (s *Scheduler) finishedLocked() error {
	if s.srcErr != nil && len(s.pending) == 0 && len(s.live) == 0 {
		return s.srcErr
	}
	return nil
}

func (s *Scheduler) doneLocked() bool {
	return s.eof && len(s.pending) == 0 && len(s.live) == 0
}

// Complete reports that a chunk arrived. It is idempotent and indifferent to
// which lane carried it: a chunk re-issued on a second lane is complete when
// either copy lands, and the other lane's attempt is released with it.
func (s *Scheduler) Complete(laneID uint64, chunk *Chunk) {
	if chunk == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.live[chunk.Offset]
	if !ok {
		return
	}
	for _, a := range out.attempts {
		s.releaseLaneLocked(a.lane, uint64(len(out.chunk.Data)))
	}
	delete(s.live, chunk.Offset)
	s.stats.ChunksCompleted++
	s.stats.CompletedByLaneID[laneID]++
	s.signalIfDoneLocked()
	s.produced.Signal()
	s.ready.Broadcast()
}

// Fail reports that a lane could not carry a chunk. The chunk returns to the
// ready set for any other lane to pick up, so a lane that breaks mid-transfer
// costs its window and nothing more.
func (s *Scheduler) Fail(laneID uint64, chunk *Chunk) {
	if chunk == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, ok := s.live[chunk.Offset]
	if !ok {
		return
	}
	s.stats.LaneFailures++
	s.dropAttemptLocked(out, laneID)
	if len(out.attempts) == 0 {
		delete(s.live, chunk.Offset)
		s.requeueLocked(chunk)
	}
	s.ready.Broadcast()
}

// RetireLane releases every chunk a lane was carrying, for a lane that has
// died rather than a single chunk that failed.
func (s *Scheduler) RetireLane(laneID uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for offset, out := range s.live {
		if !laneHasAttempt(out, laneID) {
			continue
		}
		s.stats.LaneFailures++
		s.dropAttemptLocked(out, laneID)
		if len(out.attempts) == 0 {
			delete(s.live, offset)
			s.requeueLocked(out.chunk)
		}
	}
	delete(s.laneLoad, laneID)
	delete(s.laneBytes, laneID)
	s.ready.Broadcast()
}

func (s *Scheduler) dropAttemptLocked(out *outstanding, laneID uint64) {
	for i, a := range out.attempts {
		if a.lane != laneID {
			continue
		}
		out.attempts = append(out.attempts[:i], out.attempts[i+1:]...)
		s.releaseLaneLocked(laneID, uint64(len(out.chunk.Data)))
		return
	}
}

func (s *Scheduler) releaseLaneLocked(laneID uint64, size uint64) {
	if s.laneLoad[laneID] > 0 {
		s.laneLoad[laneID]--
	}
	if s.laneBytes[laneID] >= size {
		s.laneBytes[laneID] -= size
	} else {
		size = s.laneBytes[laneID]
		s.laneBytes[laneID] = 0
	}
	if s.totalBytes >= size {
		s.totalBytes -= size
	} else {
		s.totalBytes = 0
	}
}

// hasRoomLocked reports whether a lane may be handed another chunk: it must fit
// both the lane's own window and the flow's.
//
// A lane holding nothing is always allowed one chunk. A window smaller than a
// chunk is otherwise able to stall a lane permanently, and a flow window that
// has collapsed must still make progress rather than deadlock.
func (s *Scheduler) hasRoomLocked(laneID uint64, windowBytes int) bool {
	// The escape hatch is deliberately keyed on the flow holding nothing, not
	// on this lane holding nothing. Per-lane, it would hand every lane a free
	// chunk and let the flow exceed its window by one chunk per lane -- which
	// is exactly the over-claiming the flow window exists to prevent.
	if s.totalBytes == 0 {
		return true
	}
	chunk := uint64(s.cfg.ChunkSize)
	if windowBytes > 0 && s.laneBytes[laneID]+chunk > uint64(windowBytes) {
		return false
	}
	if s.cfg.Windows != nil {
		if lane := s.cfg.Windows.Lane(laneID); lane > 0 && s.laneBytes[laneID]+chunk > uint64(lane) {
			return false
		}
		if total := s.cfg.Windows.Total(); total > 0 && s.totalBytes+chunk > uint64(total) {
			return false
		}
	}
	return true
}

// requeueLocked returns a chunk to the ready set in offset order, so the
// receiver's contiguous point is always what the next free lane works on.
func (s *Scheduler) requeueLocked(chunk *Chunk) {
	// A chunk only returns to the ready set because a lane stalled or failed,
	// which makes it the work most likely to be blocking delivery.
	chunk.urgent = true
	i := 0
	for i < len(s.pending) && s.pending[i].Offset < chunk.Offset {
		i++
	}
	s.pending = append(s.pending, nil)
	copy(s.pending[i+1:], s.pending[i:])
	s.pending[i] = chunk
}

// ReissueExpired offers any chunk that has been outstanding too long to
// another lane, without disturbing the attempt already in flight. A caller
// runs this periodically; it reports how many chunks it re-offered.
//
// This is what makes a throttled lane recoverable rather than fatal. The lane
// keeps its chunk -- it may yet deliver it -- but the chunk stops being that
// lane's exclusive responsibility.
func (s *Scheduler) ReissueExpired() int {
	if s.cfg.RetransmitAfter <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	reissued := 0
	for _, out := range s.live {
		if len(out.attempts) == 0 || len(out.attempts) > 1 {
			// Already being carried by more than one lane; adding a third is
			// unlikely to help and certainly costs bandwidth.
			continue
		}
		if out.attempts[0].deadline.IsZero() || now.Before(out.attempts[0].deadline) {
			continue
		}
		if s.pendingHas(out.chunk.Offset) {
			continue
		}
		// Push the deadline out so a chunk cannot be re-offered every tick
		// while both attempts are still in flight.
		out.attempts[0].deadline = now.Add(s.cfg.RetransmitAfter)
		s.requeueLocked(out.chunk)
		reissued++
	}
	if reissued > 0 {
		s.ready.Broadcast()
	}
	return reissued
}

func (s *Scheduler) pendingHas(offset uint64) bool {
	for _, c := range s.pending {
		if c.Offset == offset {
			return true
		}
	}
	return false
}

// Done is closed once every byte of the source has been delivered. A caller
// that must act at the end of the stream -- sending a FIN, for instance --
// waits on this rather than on the lanes, because which lane carried the last
// chunk is not knowable in advance and does not matter.
func (s *Scheduler) Done() <-chan struct{} { return s.finished }

// Err returns the source's read error, if the stream ended in one.
func (s *Scheduler) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.srcErr
}

func (s *Scheduler) signalIfDoneLocked() {
	if !s.doneLocked() {
		return
	}
	select {
	case <-s.finished:
	default:
		close(s.finished)
	}
}

// Stats returns a snapshot.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.stats
	out.CompletedByLaneID = make(map[uint64]uint64, len(s.stats.CompletedByLaneID))
	for k, v := range s.stats.CompletedByLaneID {
		out.CompletedByLaneID[k] = v
	}
	return out
}

// Close releases every waiter. Chunks in flight are abandoned.
func (s *Scheduler) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	s.ready.Broadcast()
	s.produced.Broadcast()
}
