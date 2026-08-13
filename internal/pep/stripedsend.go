package pep

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/icourses-dev/wanopt/internal/mpcc"
	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/stripe"
)

const (
	// maxLaneChunkWindow caps chunks outstanding on one lane. The byte window is
	// meant to be the binding limit and this only stops the bookkeeping growing
	// without bound, so it has to be far above what the byte window admits.
	//
	// It was 96, which sounds generous and is not: a chunk is whatever one read
	// from the application socket returned, often a few kilobytes rather than
	// the 32 KiB maximum, so 96 chunks can be under 800 KB. Against a 210ms
	// feedback loop that caps a lane near 30 Mbit/s regardless of the path, and
	// it is why the self-paced sender measured a third below the pushing one on
	// a single lane at 100 Mbit/s.
	maxLaneChunkWindow = 1024
	// maxFlowOutstandingChunks bounds chunks retained across every lane, which
	// bounds a flow's memory: a chunk is held until acknowledged because it may
	// have to be re-issued elsewhere.
	//
	// It has to clear the flow's bandwidth-delay product or it, rather than the
	// windows, becomes the limit. At 100 Mbit/s and 200ms one lane alone needs
	// 2.5 MB in flight and four need ten; the first value here was 4 MiB, and
	// on that path the self-paced sender measured a fifth below the pushing one
	// purely because the producer stopped reading. The real limit on commitment
	// is the per-lane window, which shrinks with the lane; this is the memory
	// ceiling behind it.
	maxFlowOutstandingChunks = 2048
	// chunkReissueDelay is how long a chunk may sit on one lane before it is
	// also offered to another.
	chunkReissueDelay = 1500 * time.Millisecond
	// chunkReissueInterval is how often the supervisor looks for chunks to
	// re-offer.
	chunkReissueInterval = 250 * time.Millisecond
	// laneEligibilityPoll is how long a lane waits before rechecking whether
	// the flow's class now lets it carry data. Classification changes at most
	// once or twice in a flow's life, so polling is cheaper than a broadcast.
	laneEligibilityPoll = 20 * time.Millisecond
	// minLaneWindowBytes is the window given to a lane with no usable rate
	// sample yet, and the floor for a lane whose rate has collapsed.
	minLaneWindowBytes = 128 * 1024
	// maxLaneWindowBytes caps what one lane may hold however fast it looks. Four
	// lanes at this bound is the flow's worst-case commitment, and it is why the
	// chunk ceiling above is not simply unbounded.
	maxLaneWindowBytes = 4 * 1024 * 1024
)

// windowBytes is how much unacknowledged data this lane will accept.
//
// It must cover the lane's bandwidth-delay product or the lane under-runs: it
// would finish a chunk and then sit idle for a round trip waiting to be handed
// the next one. Twice the product leaves room for the acknowledgement to be
// in flight while the lane keeps working.
//
// This estimate cannot misroute anything. It sets how deeply one lane
// pipelines, not which lane gets the data, so getting it wrong costs that
// lane's throughput and never puts bytes behind a lane that cannot move them.
// A lane whose rate collapses gets a smaller product and a smaller window
// without anything deciding to shrink it.
func (l *mpLane) windowBytes() int {
	// The lane's own congestion window is the right source, and the achieved
	// rate is not. Deriving the window from measured throughput is circular: if
	// the window is holding the lane back, the rate it measures is lower, which
	// lowers the window again. The congestion window says what the path will
	// accept rather than what this sender happened to achieve, which is also
	// what MPTCP means by a subflow's window.
	if cwnd := l.congestionWindow(); cwnd > 0 {
		window := 2 * cwnd
		if window < minLaneWindowBytes {
			return minLaneWindowBytes
		}
		if window > maxLaneWindowBytes {
			return maxLaneWindowBytes
		}
		return window
	}
	rate, rtt := l.sendRate()
	if rate <= 0 || rtt <= 0 {
		return minLaneWindowBytes
	}
	window := int(2 * rate * rtt.Seconds())
	if window < minLaneWindowBytes {
		return minLaneWindowBytes
	}
	if window > maxLaneWindowBytes {
		return maxLaneWindowBytes
	}
	return window
}

// congestionWindow reports what this lane's transport says the path will hold,
// or zero when the transport does not expose it.
func (l *mpLane) congestionWindow() int {
	if l == nil || l.fc == nil {
		return 0
	}
	provider, ok := l.fc.conn.(laneStatsProvider)
	if !ok {
		return 0
	}
	return int(provider.transportStats().controller.CongestionWindow)
}

// flowSource adapts the application connection to the scheduler's reader,
// keeping the classification and half-close handling that used to live in the
// send loop.
type flowSource struct{ flow *multipathFlow }

func (s *flowSource) Read(p []byte) (int, error) {
	n, err := s.flow.inner.Read(p)
	if n > 0 {
		s.flow.observe(n, true)
		s.flow.bytesUp.Add(uint64(n))
	}
	if err != nil {
		// HTTP clients often close a fully-consumed SOCKS socket without a TCP
		// half-close. Treat that as EOF while the logical flow is still live,
		// so the peer receives a normal FIN and releases its destination
		// connection.
		if expectedHalfCloseError(err) {
			err = io.EOF
		}
		if errors.Is(err, io.EOF) {
			s.flow.localClosed.Store(true)
		}
	}
	return n, err
}

// sendInnerStriped carries the application stream to the peer by self-pacing:
// the scheduler holds chunks, and each lane takes one when it has room.
//
// Nothing here decides how to divide the stream between lanes. A lane that is
// fast finishes its chunk sooner, asks sooner, and carries more; a lane that is
// throttled asks rarely; a lane that has stopped never asks, and its chunks are
// re-offered to lanes that are still moving. The division is an outcome, not a
// policy.
func (f *multipathFlow) sendInnerStriped(ctx context.Context) (err error) {
	defer close(f.sendDone)

	var cc *mpcc.Window
	if coupledCongestion.Load() {
		cc = mpcc.New(mpcc.Config{})
	}
	sched := stripe.New(&flowSource{flow: f}, stripe.Config{
		ChunkSize:       f.chunkSize,
		LaneWindow:      maxLaneChunkWindow,
		MaxOutstanding:  maxFlowOutstandingChunks,
		RetransmitAfter: chunkReissueDelay,
		Windows:         &laneAdmission{flow: f, cc: cc},
	})
	defer sched.Close()
	f.cc = cc

	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.sendCtx.Store(&sendCtx)
	f.scheduler.Store(sched)
	defer f.scheduler.Store(nil)

	for _, lane := range f.healthyLanes() {
		f.startLanePuller(sendCtx, lane, sched)
	}
	go f.superviseChunks(sendCtx, sched)
	go f.watchChunkCompletion(sendCtx, sched)
	go f.sampleLaneCongestion(sendCtx, cc)

	select {
	case <-sched.Done():
	case <-sendCtx.Done():
		return sendCtx.Err()
	case <-f.done:
		if f.finSent.Load() && f.remoteFinSeen.Load() {
			return nil
		}
		return errors.New("flow closed before send completed")
	}
	if srcErr := sched.Err(); srcErr != nil {
		return srcErr
	}
	if f.remoteAbort.Load() {
		// The peer closed its full application socket; a local read error is a
		// consequence of that, not a transport failure.
		return nil
	}
	return f.sendFinal(ctx, sched.Stats().SourceBytes)
}

// superviseChunks re-offers chunks that a lane has gone quiet on. Self-pacing
// stops a slow lane being handed more work, but a chunk already on it needs
// somebody to notice, and the receiver cannot deliver past it meanwhile.
func (f *multipathFlow) superviseChunks(ctx context.Context, sched *stripe.Scheduler) {
	ticker := time.NewTicker(chunkReissueInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sched.Done():
			return
		case <-f.done:
			return
		case <-ticker.C:
			if n := sched.ReissueExpired(); n > 0 && f.metrics != nil {
				for i := 0; i < n; i++ {
					f.metrics.Reinjected()
				}
			}
		}
	}
}

// startLanePuller runs one worker for a lane. Lanes joined mid-flow get one
// when they are admitted, so a lane starts carrying data as soon as it exists.
func (f *multipathFlow) startLanePuller(ctx context.Context, lane *mpLane, sched *stripe.Scheduler) {
	if lane == nil || sched == nil {
		return
	}
	if !lane.pulling.CompareAndSwap(false, true) {
		return
	}
	go f.runLanePuller(ctx, lane, sched)
}

func (f *multipathFlow) runLanePuller(ctx context.Context, lane *mpLane, sched *stripe.Scheduler) {
	defer lane.pulling.Store(false)
	defer sched.RetireLane(lane.id)
	for {
		if lane.closed.Load() || f.doneChanClosed() {
			return
		}
		bulk := protocol.Class(f.class.Load()) == protocol.ClassBulk
		if !f.laneCarriesData(lane, bulk) {
			select {
			case <-time.After(laneEligibilityPoll):
				continue
			case <-ctx.Done():
				return
			case <-f.done:
				return
			}
		}
		chunk, err := sched.Next(ctx, lane.id, lane.windowBytes())
		if err != nil || chunk == nil {
			return
		}
		if err := f.sendChunk(ctx, lane, chunk, bulk); err != nil {
			sched.Fail(lane.id, chunk)
			return
		}
		// The chunk is now this lane's outstanding work, and it stays that way
		// until the peer acknowledges it. The lane does not block here: it
		// keeps pulling until its window is full of unacknowledged bytes, and
		// the window is what paces it. Waiting for each acknowledgement inline
		// would cap the lane at one chunk per round trip -- 32 KiB per 200ms,
		// about 1.3 Mbit/s -- however large its window was.
		f.trackChunk(lane.id, chunk)
	}
}

// outstandingChunk is a chunk written to a lane and not yet acknowledged.
type outstandingChunk struct {
	lane  uint64
	chunk *stripe.Chunk
}

func (f *multipathFlow) trackChunk(laneID uint64, chunk *stripe.Chunk) {
	f.chunkMu.Lock()
	f.outstandingChunks = append(f.outstandingChunks, outstandingChunk{lane: laneID, chunk: chunk})
	f.chunkMu.Unlock()
	// An empty final chunk is covered the moment it is tracked, so nudge the
	// watcher rather than leaving it until the next acknowledgement.
	if len(chunk.Data) == 0 {
		f.ackTrack.Touch()
	}
}

// watchChunkCompletion completes chunks as their bytes are acknowledged.
//
// One watcher per flow rather than one waiter per chunk: chunks complete out of
// order by design, and at 100 Mbit/s a 32 KiB chunk size would otherwise mean
// several hundred short-lived goroutines a second per flow.
func (f *multipathFlow) watchChunkCompletion(ctx context.Context, sched *stripe.Scheduler) {
	var gen uint64
	for {
		f.chunkMu.Lock()
		kept := f.outstandingChunks[:0]
		var completed []outstandingChunk
		for _, pending := range f.outstandingChunks {
			if f.ackTrack.Covered(pending.chunk.Offset, pending.chunk.End()) {
				completed = append(completed, pending)
				continue
			}
			kept = append(kept, pending)
		}
		f.outstandingChunks = kept
		f.chunkMu.Unlock()
		for _, done := range completed {
			sched.Complete(done.lane, done.chunk)
			if f.cc != nil {
				// Acknowledged bytes are what the coupled window grows on, so
				// it advances at the rate the path actually delivers.
				f.cc.Acked(done.lane, len(done.chunk.Data))
			}
		}

		var err error
		if gen, err = f.ackTrack.WaitChange(ctx, gen); err != nil {
			return
		}
	}
}

// laneCarriesData reports whether this lane may carry payload for the flow's
// current class, preserving the rule that only bulk flows stripe and that a
// reserved control lane keeps out of bulk.
func (f *multipathFlow) laneCarriesData(lane *mpLane, bulk bool) bool {
	candidates, err := f.laneCandidates(bulk)
	if err != nil {
		return false
	}
	for _, candidate := range candidates {
		if candidate.id == lane.id {
			return true
		}
	}
	return false
}

// sendChunk writes one chunk on one lane. The chunk's offset is its sequence
// number, so the receiver can place it without knowing which lane it came from
// or what order the lanes ran in.
func (f *multipathFlow) sendChunk(ctx context.Context, lane *mpLane, chunk *stripe.Chunk, bulk bool) error {
	if len(chunk.Data) == 0 {
		// An empty final chunk carries no bytes; the FIN that follows the
		// stream is what ends it.
		return nil
	}
	frame := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeData, SessionID: f.sessionID, FlowID: f.flowID,
		Sequence: chunk.Offset, Class: protocol.Class(f.class.Load()),
	}, Payload: chunk.Data}
	// Deliberately not retained here: the scheduler is holding this chunk until
	// it is acknowledged, and is the thing that will re-issue it if this lane
	// stops. Recording it again would put the same bytes under two limits.
	f.noteSent(chunk.Offset, len(chunk.Data))
	return f.enqueueFrameClass(ctx, lane, frame, bulk)
}

// sendFinal closes the outbound direction once every byte has been
// acknowledged, and waits for the peer's final acknowledgement.
func (f *multipathFlow) sendFinal(ctx context.Context, sequence uint64) error {
	f.localClosed.Store(true)
	f.sendSequence(sequence)
	fin := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
		SessionID: f.sessionID, FlowID: f.flowID, Sequence: sequence, Class: protocol.Class(f.class.Load()),
	}}
	if err := f.recordReplayContext(ctx, fin); err != nil {
		return err
	}
	// Publish the logical FIN before enqueueing it: a lane writer can fail
	// immediately, and recovery must know the FIN is pending so it replays it
	// rather than treating the flow as one-sided.
	f.finSent.Store(true)
	if err := f.enqueueOnHealthyLane(ctx, fin, false); err != nil {
		return err
	}
	select {
	case <-f.finalAck:
		return nil
	case <-f.done:
		// Both FIN directions prove every application byte crossed the flow. A
		// final ACK can be lost in the normal last-lane close race, and the
		// completed-session tombstone can replay it, so do not hold this
		// worker indefinitely.
		if f.finSent.Load() && f.remoteFinSeen.Load() {
			return nil
		}
		return errors.New("flow closed before final acknowledgement")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// selfPacedSend selects the self-pacing sender. It exists so the redesign can
// be measured against the scheduler it replaces on the same path and the same
// seed, rather than asserted; the pushing sender goes once the comparison is
// recorded.
var selfPacedSend atomic.Bool

// SetSelfPacedSend chooses the sender. It is process-wide and intended for
// benchmarks.
func SetSelfPacedSend(on bool) { selfPacedSend.Store(on) }

func init() {
	// The environment override exists so the two senders can be compared on
	// one binary, one path, and one seed. Anything else compares two builds.
	selfPacedSend.Store(os.Getenv("WANOPT_SELF_PACED") != "0")
}
