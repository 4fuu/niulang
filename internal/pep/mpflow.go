package pep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/icourses-dev/wanopt/internal/classifier"
	"github.com/icourses-dev/wanopt/internal/limiter"
	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/mpcc"
	"github.com/icourses-dev/wanopt/internal/multipath"
	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/session"
	"github.com/icourses-dev/wanopt/internal/stripe"
)

var nextTelemetryID atomic.Uint64

const (
	maxLaneEvents = 256
	// The receiver's out-of-order capacity must be at least the sender's send
	// window, or an ordinary striped transfer can legitimately overflow it and
	// abort a healthy application flow. Bytes buffered out of order are always
	// bytes the sender has not had acknowledged, so sizing both from the same
	// constant makes overflow impossible between peers running this code, and
	// keeps a hostile peer bounded by the same per-flow figure.
	//
	// The receiver cannot instead apply backpressure here: every lane feeds one
	// ordered reassembler, so pausing consumption would also pause the lane
	// carrying the segment that would close the gap.
	maxReassemblyBytes = maxFlowReplayBytes
	// The byte bound above is what limits memory. This frame bound only stops
	// a peer using very small frames from turning the window into millions of
	// map entries.
	maxReassemblyFrames = 16384
	maxLaneWriteQueue   = 64
	// Keep a small part of the bounded queue available for interactive/new
	// frames even when a bulk producer has filled its queue. This is a hard
	// reservation, not an additional memory allowance: writeSlots still caps
	// the combined queues at maxLaneWriteQueue.
	maxLaneInteractiveReserve = 8
	maxLaneBulkQueue          = maxLaneWriteQueue - maxLaneInteractiveReserve
	// maxReplayBytes is the window a flow may hold without drawing on the
	// endpoint's shared replay budget. Growth above it is granted only while
	// the endpoint has spare accounted memory; see replaybudget.go.
	maxReplayBytes  = 8 * 1024 * 1024
	maxReplayFrames = 16384
	// Must exceed QUIC dead-path detection plus TCP handshake time. The
	// client normally detects a blackhole at ~15 s, then needs one scheduler
	// tick and a bounded TCP handshake before the replacement can arrive.
	laneReplacementWait = 45 * time.Second
	// Once both FIN directions are observed, no additional application bytes
	// can be delivered. This grace lets a healthy final ACK arrive, but bounds
	// retention when the peer closes its last lane at exactly that point.
	flowCompletionGrace = 5 * time.Second
	// A local EOF is ambiguous between TCP half-close and full application
	// close. Wait for the final ACK, then escalate only after an idle grace;
	// interactive sessions get more time for legitimate quiet periods.
	flowAbortGrace        = 5 * time.Second
	interactiveAbortGrace = 30 * time.Second
	remoteFinDrainGrace   = 500 * time.Millisecond
	// Once the peer FIN has proved the receive sequence complete, do not spend
	// the full lane-replacement window trying to deliver its final ACK. If the
	// local direction is also closing, the server completion tombstone can
	// absorb a lost ACK without keeping the application flow alive.
	finalAckWriteGrace = 2 * time.Second
	// These limits are deliberately long enough for quiet SSH and remote
	// desktop sessions, while preventing an abandoned authenticated flow from
	// retaining a destination socket and replay window forever.
	defaultFlowIdleTimeout = 30 * time.Minute
	defaultFlowMaxLifetime = 24 * time.Hour
)

var (
	errFlowIdleTimeout = errors.New("flow idle timeout")
	errFlowLifetime    = errors.New("flow lifetime exceeded")
)

type mpLane struct {
	id   uint64
	kind TransportKind
	fc   *frameConn
	// writeHook is an internal deterministic fault-injection point used by
	// integration tests. Production lanes leave it nil, so the data path has
	// only one predictable nil check before the real framed write.
	writeHook func(protocol.Frame) error
	// writeQ is the bounded bulk/data queue. Interactive and new-flow frames
	// use writeInteractiveQ when it is initialized. Keeping a separate queue
	// lets the writer avoid sitting behind a burst of bulk retransmissions.
	// writeSlots is a semaphore shared by both queues, so the total queued
	// frames remain bounded by maxLaneWriteQueue rather than by the sum of
	// both channel capacities.
	writeQ chan protocol.Frame
	// pulling guards against two workers on one lane.
	pulling           atomic.Bool
	writeInteractiveQ chan protocol.Frame
	writeSlots        chan struct{}
	writeDone         chan struct{}
	closed            atomic.Bool
	sent              atomic.Uint64
	recv              atomic.Uint64
	// nextFree is this lane's virtual transmit clock: the time at which it
	// would finish sending everything the scheduler has already assigned to
	// it. It is read and advanced under multipathFlow.schedMu.
	nextFree time.Time

	// rateMu guards a short-lived cache of the lane's transport statistics.
	// Reading them from QUIC on every frame would take the connection lock on
	// the hot path for information that changes on an RTT timescale.
	rateMu      sync.Mutex
	rateSampled time.Time
	rateBytes   float64       // estimated send rate in bytes per second
	rateRTT     time.Duration // smoothed round-trip time
}

// laneRateCacheTTL is far below one long-haul RTT, so the scheduler still
// reacts within a single congestion round while avoiding per-frame lock
// traffic on a fast local path.
const laneRateCacheTTL = 5 * time.Millisecond

// serializationTime is how long this lane needs to put payload bytes on the
// wire at its current estimated rate.
func (l *mpLane) serializationTime(payload int) time.Duration {
	rate, rtt := l.sendRate()
	if rate <= 0 {
		if rtt <= 0 {
			return 0
		}
		// The RTT is known but the controller exposes no rate. One chunk per
		// round trip is a deliberately pessimistic stand-in that still orders
		// lanes consistently against each other.
		rate = float64(defaultChunkSize) / rtt.Seconds()
	}
	return time.Duration(float64(payload) / rate * float64(time.Second))
}

// laneScheduleHorizon bounds how far ahead the scheduler will commit bytes to
// a single lane. Without it, a lane that stops draining would keep its virtual
// clock running away and stay unusable long after it recovered.
const laneScheduleHorizon = 2 * time.Second

func (l *mpLane) sendRate() (float64, time.Duration) {
	l.rateMu.Lock()
	defer l.rateMu.Unlock()
	now := time.Now()
	if !l.rateSampled.IsZero() && now.Sub(l.rateSampled) < laneRateCacheTTL {
		return l.rateBytes, l.rateRTT
	}
	if l.fc == nil {
		return 0, 0
	}
	provider, ok := l.fc.conn.(laneStatsProvider)
	if !ok {
		l.rateSampled = now
		return l.rateBytes, l.rateRTT
	}
	stats := provider.transportStats()
	rtt := stats.smoothedRTT
	if rtt <= 0 {
		rtt = stats.latestRTT
	}
	rate := float64(stats.controller.PacingRate)
	if rate <= 0 && stats.controller.CongestionWindow > 0 && rtt > 0 {
		rate = float64(stats.controller.CongestionWindow) / rtt.Seconds()
	}
	l.rateSampled, l.rateBytes, l.rateRTT = now, rate, rtt
	return rate, rtt
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
	metrics   *metrics.Registry
	logger    *slog.Logger

	sendAckFlag uint16
	recvAckFlag uint16

	lanesMu sync.RWMutex
	lanes   map[uint64]*mpLane
	// schedMu serializes lane selection so that choosing a lane and charging
	// it for the selected bytes is one atomic step.
	schedMu sync.Mutex

	events   chan inboundEvent
	laneErr  chan laneFailure
	finalAck chan struct{}
	sendDone chan struct{}
	done     chan struct{}
	ackWake  chan struct{}
	ackErr   chan error

	classifier *classifier.Classifier
	// reserveControlLane is negotiated for pooled flows. Lane 0 is the
	// authenticated/persistent control stream; once a joined lane exists,
	// bulk payloads prefer joined lanes so loss recovery for a large transfer
	// cannot monopolize the interactive connection. If no joined lane is
	// healthy, selection deliberately falls back to lane 0 for availability.
	reserveControlLane bool
	started            time.Time
	completionGrace    time.Duration
	bytesUp            atomic.Uint64
	bytesDown          atomic.Uint64
	class              atomic.Uint32
	// ackTrack answers "has this range arrived?", which is what clocks every
	// lane. scheduler and sendCtx let a lane joined mid-flow start carrying
	// data as soon as it is admitted.
	ackTrack  *ackTracker
	scheduler atomic.Pointer[stripe.Scheduler]
	sendCtx   atomic.Pointer[context.Context]
	// outstandingChunks are chunks written and not yet acknowledged. A single
	// watcher completes them as acknowledgements arrive, because they complete
	// out of order by design and a waiter goroutine per chunk would mean
	// hundreds a second on a fast flow.
	cc                *mpcc.Window
	chunkMu           sync.Mutex
	outstandingChunks []outstandingChunk
	finSequence       atomic.Uint64
	remoteFinSequence atomic.Uint64
	finSent           atomic.Bool
	remoteFinSeen     atomic.Bool
	localClosed       atomic.Bool
	remoteAbort       atomic.Bool
	localAbortSent    atomic.Bool
	laneFailures      atomic.Uint64
	// openAckPending is set only for the opt-in optimistic OPEN path. The
	// application may begin sending immediately, but the eventual OPEN_OK is
	// still required on the authenticated stream and is consumed by the flow
	// reader before ordinary data/control frames are accepted.
	openAckPending bool
	// helloAckPending is set when the flow's first lane pipelined HELLO with
	// OPEN and did not wait for HELLO_OK. The acknowledgement still has to
	// arrive and still has to be valid, but the caller no longer pays a
	// round trip for it. onHelloOK publishes the negotiated capabilities.
	helloAckPending bool
	onHelloOK       func(session.HelloOK)
	// ackRanges is set when the peer advertised that it can consume byte
	// ranges alongside the cumulative acknowledgement. It is only useful to a
	// striped flow, and must never be assumed of a peer that did not say so.
	ackRanges atomic.Bool
	// receivedRanges publishes what the reassembler currently holds out of
	// order, so the acknowledgement loop can report it without touching the
	// reassembler from another goroutine.
	rangesMu      sync.Mutex
	pendingRanges [][2]uint64
	ackSequence   atomic.Uint64
	ackClosing    atomic.Bool
	lastPayload   atomic.Int64
	lastActivity  atomic.Int64
	closeOnce     sync.Once
	doneOnce      sync.Once
	finished      atomic.Bool
	nextJoinID    uint64
	telemetryID   uint64
	baselineRTTNS atomic.Int64
	currentRTTNS  atomic.Int64
	idleTimeout   time.Duration
	maxLifetime   time.Duration

	replayStalls    atomic.Uint64
	replayStalled   atomic.Int64
	replayEvictions atomic.Uint64
	reinjections    atomic.Uint64

	replayMu sync.Mutex
	replay   map[uint64]protocol.Frame
	// replayOrder is the retention FIFO. Data frames are recorded in
	// increasing sequence order, so its front is always the oldest retained
	// frame; entries whose frame has already been acknowledged are skipped
	// when popped rather than removed eagerly.
	replayOrder  []uint64
	replayNotify chan struct{}
	replayBytes  uint64
	// replayEvictedThrough is the highest byte offset released from the
	// retention window to keep the application moving. Replay onto a
	// replacement lane is complete as long as the peer has since acknowledged
	// everything up to it; a sticky "unreplayable" flag would instead condemn
	// a flow forever for an eviction the peer went on to acknowledge, which
	// turned completed transfers into failures at teardown.
	replayEvictedThrough uint64
	// reinjectedThrough is the sequence past which the stall detector has
	// already re-sent, so a held head is duplicated once per position rather
	// than on every tick.
	reinjectedThrough uint64
	// replayLimit is this flow's current send window. It starts at the
	// unaccounted floor and grows from replayBudget while the endpoint has
	// spare accounted memory. replayGranted is what has been drawn from that
	// shared budget and must be returned when the flow ends.
	replayLimit   uint64
	replayGranted uint64
	replayBudget  *replayBudget
	acked         uint64
	highestSent   uint64
}

func newMultipathFlow(ctx context.Context, inner net.Conn, sessionID [16]byte, flowID uint64, chunkSize int, sendAckFlag, recvAckFlag uint16, budget *limiter.Budget, registry *metrics.Registry, loggers ...*slog.Logger) *multipathFlow {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	f := &multipathFlow{
		ctx: ctx, inner: inner, sessionID: sessionID, flowID: flowID, chunkSize: chunkSize, budget: budget, metrics: registry,
		sendAckFlag: sendAckFlag, recvAckFlag: recvAckFlag,
		lanes: make(map[uint64]*mpLane), events: make(chan inboundEvent, maxLaneEvents), laneErr: make(chan laneFailure, maxLaneEvents),
		finalAck: make(chan struct{}, 1), sendDone: make(chan struct{}),
		done: make(chan struct{}), ackWake: make(chan struct{}, 1), ackErr: make(chan error, 1),
		classifier: classifier.New(classifier.DefaultConfig()), started: time.Now(), completionGrace: flowCompletionGrace,
		replay: make(map[uint64]protocol.Frame), replayNotify: make(chan struct{}, 1),
		replayLimit: maxReplayBytes,
	}
	if len(loggers) > 0 && loggers[0] != nil {
		f.logger = loggers[0]
	}
	f.idleTimeout = defaultFlowIdleTimeout
	f.maxLifetime = defaultFlowMaxLifetime
	f.lastActivity.Store(f.started.UnixNano())
	f.telemetryID = nextTelemetryID.Add(1)
	f.class.Store(uint32(protocol.ClassNew))
	f.ackTrack = newAckTracker()
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
		lane.writeQ = make(chan protocol.Frame, maxLaneBulkQueue)
	}
	if lane.writeInteractiveQ == nil {
		lane.writeInteractiveQ = make(chan protocol.Frame, maxLaneWriteQueue)
	}
	if lane.writeSlots == nil {
		lane.writeSlots = make(chan struct{}, maxLaneWriteQueue)
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
	// A lane admitted while the flow is sending starts carrying data at once;
	// it does not wait for anything to notice it.
	if sched := f.scheduler.Load(); sched != nil {
		if ctx := f.sendCtx.Load(); ctx != nil {
			f.startLanePuller(*ctx, lane, sched)
		}
	}
	return nil
}

// writeLane serializes data and close frames for one lane while allowing
// other lanes to make progress independently. Interactive/new frames are
// always selected before queued bulk frames. A bulk frame already in the
// underlying write may finish, but a later interactive frame does not wait
// behind the rest of the bulk queue. ACK/PING/PONG writes may still call
// frameConn.Write directly; its mutex preserves frame integrity.
func (f *multipathFlow) writeLane(lane *mpLane) {
	defer close(lane.writeDone)
	for {
		frame, ok := nextLaneFrame(lane, f.done, f.ctx.Done())
		if !ok {
			return
		}
		if lane.writeSlots != nil {
			// A slot is released when the writer takes ownership of a frame.
			// The non-blocking receive is safe because every initialized queue
			// insertion acquires exactly one slot first.
			select {
			case <-lane.writeSlots:
			default:
			}
		}
		if lane.writeHook != nil {
			if err := lane.writeHook(frame); err != nil {
				f.failLane(lane, fmt.Errorf("lane %d injected write failure: %w", lane.id, err))
				return
			}
		}
		err := lane.fc.WriteContext(f.ctx, frame)
		if err != nil {
			f.failLane(lane, fmt.Errorf("lane %d write: %w", lane.id, err))
			return
		}
		if frame.Header.Type == protocol.TypeData {
			lane.sent.Add(uint64(len(frame.Payload)))
		}
	}
}

// nextLaneFrame gives the interactive queue strict preference without
// starving shutdown: check it once before allowing the bulk queue to win a
// select, then check both queues while waiting. The non-blocking first check
// closes the common race where both queues already have work.
func nextLaneFrame(lane *mpLane, done <-chan struct{}, ctxDone <-chan struct{}) (protocol.Frame, bool) {
	if lane.writeInteractiveQ != nil {
		select {
		case frame := <-lane.writeInteractiveQ:
			return frame, true
		default:
		}
	}
	for {
		select {
		case <-done:
			return protocol.Frame{}, false
		case <-ctxDone:
			return protocol.Frame{}, false
		default:
		}
		if lane.writeInteractiveQ != nil {
			select {
			case frame := <-lane.writeInteractiveQ:
				return frame, true
			default:
			}
		}
		select {
		case frame := <-lane.writeInteractiveQ:
			return frame, true
		case frame := <-lane.writeQ:
			return frame, true
		case <-done:
			return protocol.Frame{}, false
		case <-ctxDone:
			return protocol.Frame{}, false
		}
	}
}

type flowSnapshot struct {
	Class        classifier.Class
	CurrentLanes int
	HealthyLanes int
	Bytes        uint64
	BytesUp      uint64
	BytesDown    uint64
	Elapsed      time.Duration
	BaselineRTT  time.Duration
	CurrentRTT   time.Duration
}

func (f *multipathFlow) snapshot() flowSnapshot {
	lanes := f.healthyLanes()
	f.observeTransport(lanes)
	bytesUp, bytesDown := f.bytesUp.Load(), f.bytesDown.Load()
	return flowSnapshot{
		Class: classifier.Class(f.classifier.Class()), CurrentLanes: f.laneCount(), HealthyLanes: len(lanes),
		Bytes: bytesUp + bytesDown, BytesUp: bytesUp, BytesDown: bytesDown, Elapsed: time.Since(f.started),
		BaselineRTT: time.Duration(f.baselineRTTNS.Load()), CurrentRTT: time.Duration(f.currentRTTNS.Load()),
	}
}

func (f *multipathFlow) localAbortGrace() time.Duration {
	if classifier.Class(f.class.Load()) == classifier.ClassInteractive {
		return interactiveAbortGrace
	}
	return flowAbortGrace
}

func (f *multipathFlow) observeTransport(lanes []*mpLane) {
	var observation metrics.QUICObservation
	for _, lane := range lanes {
		provider, ok := lane.fc.conn.(laneStatsProvider)
		if !ok {
			continue
		}
		stats := provider.transportStats()
		observation.Lanes++
		if stats.latestRTT > observation.LatestRTT {
			observation.LatestRTT = stats.latestRTT
		}
		if stats.smoothedRTT > observation.SmoothedRTT {
			observation.SmoothedRTT = stats.smoothedRTT
		}
		observation.BytesSent += stats.bytesSent
		observation.BytesReceived += stats.bytesReceived
		observation.BytesLost += stats.bytesLost
		observation.PacketsLost += stats.packetsLost
		controller := stats.controller
		if controller.Kind != "" {
			if observation.ControllerKind == "" {
				observation.ControllerKind = controller.Kind
			} else if observation.ControllerKind != controller.Kind {
				observation.ControllerKind = "mixed"
			}
			if controller.Mode > observation.ControllerMode {
				observation.ControllerMode = controller.Mode
			}
			observation.ControllerMaxBandwidth += controller.MaxBandwidth
			if controller.LatestSample > observation.ControllerLatestSample {
				observation.ControllerLatestSample = controller.LatestSample
			}
			if controller.LatestAckRate > observation.ControllerLatestAckRate {
				observation.ControllerLatestAckRate = controller.LatestAckRate
			}
			if controller.LatestSendRate > observation.ControllerLatestSendRate {
				observation.ControllerLatestSendRate = controller.LatestSendRate
			}
			observation.ControllerSamples += controller.Samples
			observation.ControllerNonAppSamples += controller.NonAppSamples
			observation.ControllerAppSamples += controller.AppSamples
			observation.ControllerStateMisses += controller.StateMisses
			observation.ControllerZeroSamples += controller.ZeroSamples
			if controller.Round > observation.ControllerRound {
				observation.ControllerRound = controller.Round
			}
			observation.ControllerPacingRate += controller.PacingRate
			observation.ControllerCongestionWindow += controller.CongestionWindow
			observation.ControllerBytesInFlight += controller.BytesInFlight
			observation.ControllerBytesLost += controller.BytesLost
			observation.ControllerPacketsLost += controller.PacketsLost
			if controller.MinRTT > observation.ControllerMinRTT {
				observation.ControllerMinRTT = controller.MinRTT
			}
			observation.ControllerInRecovery = observation.ControllerInRecovery || controller.InRecovery
		}
	}
	if observation.SmoothedRTT > 0 {
		f.currentRTTNS.Store(observation.SmoothedRTT.Nanoseconds())
		f.baselineRTTNS.CompareAndSwap(0, observation.SmoothedRTT.Nanoseconds())
	}
	if f.metrics != nil {
		f.metrics.ObserveQUIC(f.telemetryID, observation)
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

// retireLeastProductiveLane removes one non-control lane.  It is used only
// after the scheduler has measured a negative marginal contribution or an
// RTT-budget violation.  The first lane is retained as the control/rescue
// lane so a reduction never strands the logical flow without a preferred
// path.
func (f *multipathFlow) retireLeastProductiveLane() bool {
	f.lanesMu.Lock()
	var victim *mpLane
	for _, lane := range f.lanes {
		if lane.closed.Load() || lane.id == 0 {
			continue
		}
		if victim == nil || lane.sent.Load()+lane.recv.Load() < victim.sent.Load()+victim.recv.Load() ||
			(lane.sent.Load()+lane.recv.Load() == victim.sent.Load()+victim.recv.Load() && lane.id > victim.id) {
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
	// Once both FIN directions are observed, a peer closing an outer lane is
	// the normal final-ACK/stream-close race, not a transport degradation.
	// The tombstone path retains the final sequence for any late replacement;
	// do not pollute lane-health metrics with this expected close.
	if f.finSent.Load() && f.remoteFinSeen.Load() {
		return
	}
	select {
	case <-f.done:
		return
	default:
	}
	if f.metrics != nil {
		f.metrics.LaneFailure()
	}
	// Hand back whatever this lane was carrying so another lane can finish it.
	// A lost lane costs its window, not the transfer.
	if sched := f.scheduler.Load(); sched != nil {
		sched.RetireLane(lane.id)
	}
	f.laneFailures.Add(1)
	if f.logger != nil {
		f.logger.Debug("multipath lane failed", "flow_id", f.flowID, "lane_id", lane.id, "transport", lane.kind, "error", err)
	}
	select {
	case f.laneErr <- laneFailure{lane: lane, err: err}:
	default:
		// The lane is already marked failed. The coordinator also observes
		// current health directly, so coalescing notifications is safe.
	}
}

func (f *multipathFlow) laneFailureCount() uint64 { return f.laneFailures.Load() }

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
	candidates, err := f.laneCandidates(bulk)
	if err != nil {
		return nil, err
	}
	lane := candidates[0]
	f.chargeLane(lane, f.chunkSize)
	return lane, nil
}

// laneCandidates returns the eligible lanes for a frame, best first. Callers
// that can fall back to a second choice use the whole list; callers that must
// commit to one lane take the head.
func (f *multipathFlow) laneCandidates(bulk bool) ([]*mpLane, error) {
	lanes := f.healthyLanes()
	if len(lanes) == 0 {
		return nil, errors.New("no healthy lanes")
	}
	if !bulk || len(lanes) == 1 {
		return lanes[:1], nil
	}
	if f.reserveControlLane {
		// Lane zero is the initial authenticated stream by protocol. Exclude
		// it only when at least one independent lane is healthy; retaining the
		// fallback is essential during join failure and lane recovery.
		bulkLanes := lanes[:0]
		for _, lane := range lanes {
			if lane.id != 0 {
				bulkLanes = append(bulkLanes, lane)
			}
		}
		if len(bulkLanes) > 0 {
			lanes = bulkLanes
		}
	}
	return f.orderLanesByArrival(lanes, f.chunkSize), nil
}

// orderLanesByArrival sorts lanes by when each would deliver the next frame.
//
// Round-robin striping is wrong for this transport. The receiver reassembles
// one ordered byte stream, so a frame placed on a slow lane blocks every later
// frame that already arrived on a fast one. Measured on the emulated 200 ms /
// 1% loss path, a two-lane round-robin flow ran 18% slower than the same flow
// on a single lane while carrying the same number of packets.
//
// The backlog cannot be read from the transport: a lane's writer returns as
// soon as bytes are copied into the QUIC stream's multi-megabyte send buffer,
// so the visible queue stays near zero and every lane looks idle. Each lane
// therefore carries its own virtual transmit clock, advanced by the
// serialization time of everything already assigned to it. Selecting the
// minimum of clock-plus-propagation both spreads bytes in proportion to
// measured lane rate and equalizes arrival times, which is exactly what
// in-order reassembly needs.
func (f *multipathFlow) orderLanesByArrival(lanes []*mpLane, payload int) []*mpLane {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	now := time.Now()
	type ranked struct {
		lane      *mpLane
		arrival   time.Time
		validated bool
	}
	order := make([]ranked, 0, len(lanes))
	for _, lane := range lanes {
		_, rtt := lane.sendRate()
		// A lane with no round-trip sample has not proven it can carry
		// anything: its congestion window is still the initial guess, so
		// trusting it would pull ordered data onto a path in slow start.
		// Rank any validated lane ahead of any unvalidated one.
		start := lane.nextFree
		if start.Before(now) {
			start = now
		}
		order = append(order, ranked{
			lane:      lane,
			arrival:   start.Add(lane.serializationTime(payload)).Add(rtt / 2),
			validated: rtt > 0,
		})
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].validated != order[j].validated {
			return order[i].validated
		}
		return order[i].arrival.Before(order[j].arrival)
	})
	ordered := make([]*mpLane, len(order))
	for i, entry := range order {
		ordered[i] = entry.lane
	}
	return ordered
}

// chargeLane advances a lane's virtual transmit clock for bytes just assigned
// to it. The horizon keeps a lane that stops draining from banking unbounded
// backlog and staying unusable long after it recovers.
func (f *multipathFlow) chargeLane(lane *mpLane, payload int) {
	f.schedMu.Lock()
	defer f.schedMu.Unlock()
	now := time.Now()
	start := lane.nextFree
	if start.Before(now) {
		start = now
	}
	lane.nextFree = start.Add(lane.serializationTime(payload))
	if horizon := now.Add(laneScheduleHorizon); lane.nextFree.After(horizon) {
		lane.nextFree = horizon
	}
}

func (f *multipathFlow) run(ctx context.Context) (FlowStats, error) {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	ackCtx, cancelACKs := context.WithCancel(runCtx)
	go f.ackLoop(ackCtx)
	telemetryStop := make(chan struct{})
	go f.telemetryLoop(telemetryStop)
	completionStop := make(chan struct{})
	go f.completionWatchdog(completionStop)
	limitsStop := make(chan struct{})
	limitErr := make(chan error, 1)
	go f.watchLimits(limitsStop, limitErr)
	defer f.signalDone()
	defer f.releaseReplayBudget()
	defer func() {
		cancelACKs()
		close(limitsStop)
		close(completionStop)
		close(telemetryStop)
		if f.metrics != nil {
			f.metrics.RemoveQUIC(f.telemetryID)
		}
	}()
	defer f.finished.Store(true)
	stats := FlowStats{Started: f.started}
	results := make(chan error, 2)
	go func() {
		if selfPacedSend.Load() {
			results <- f.sendInnerStriped(runCtx)
			return
		}
		results <- f.sendInner(runCtx)
	}()
	go func() { results <- f.receiveInner(runCtx) }()
	completed := 0
	for completed < 2 {
		select {
		case err := <-results:
			completed++
			if err != nil {
				// A destination can reset immediately after the client has
				// sent its FIN. Give the receive worker a short bounded window
				// to consume that in-flight FIN and emit the final ACK before
				// closing the lanes; otherwise a correct close is misclassified
				// as a failed flow and the client loses its completion signal.
				if expectedDestinationCloseError(err) {
					remoteFIN := f.remoteFinSeen.Load() || f.waitForRemoteFIN(ctx, remoteFinDrainGrace)
					if remoteFIN {
						// Keep the lane alive briefly after the final ACK write so
						// a 200 ms-class cross-Pacific RTT can deliver it before
						// the destination-reset cleanup closes the physical stream.
						drain := time.NewTimer(remoteFinDrainGrace)
						select {
						case <-drain.C:
						case <-ctx.Done():
							if !drain.Stop() {
								<-drain.C
							}
						}
						f.closeAll()
						stats.Ended = time.Now()
						stats.BytesSent = f.bytesUp.Load()
						stats.BytesRead = f.bytesDown.Load()
						stats.LaneBytes = f.laneStats()
						f.recordSendStalls(&stats)
						return stats, nil
					}
				}
				if err != nil {
					// Once the bounded remote-FIN drain above is exhausted, stop the
					// sibling worker before tearing down the physical lanes. Otherwise
					// a blocked application read can outlive the logical flow.
					cancelRun()
					f.closeAll()
					stats.Ended = time.Now()
					stats.BytesSent = f.bytesUp.Load()
					stats.BytesRead = f.bytesDown.Load()
					stats.LaneBytes = f.laneStats()
					f.recordSendStalls(&stats)
					return stats, err
				}
			}
		case err := <-limitErr:
			if err == nil {
				continue
			}
			if f.metrics != nil {
				f.metrics.FlowTimeout()
			}
			cancelRun()
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			stats.LaneBytes = f.laneStats()
			f.recordSendStalls(&stats)
			return stats, err
		case err := <-f.ackErr:
			if err == nil {
				continue
			}
			cancelRun()
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			stats.LaneBytes = f.laneStats()
			f.recordSendStalls(&stats)
			return stats, fmt.Errorf("cumulative acknowledgement: %w", err)
		case failure := <-f.laneErr:
			err := failure.err
			// Both FIN directions have already been observed. The application
			// bytes are complete, and a tombstone can replay a lost final ACK;
			// waiting the full lane-replacement grace here would leak an active
			// server flow after a normal peer close.
			if f.finSent.Load() && f.remoteFinSeen.Load() {
				f.closeAll()
				stats.Ended = time.Now()
				stats.BytesSent = f.bytesUp.Load()
				stats.BytesRead = f.bytesDown.Load()
				stats.LaneBytes = f.laneStats()
				f.recordSendStalls(&stats)
				return stats, nil
			}
			// A secondary lane can fail without invalidating the bytes already
			// delivered on the logical flow. Replay unacknowledged frames on a
			// surviving lane. If the last lane fails, or replay is impossible,
			// fail closed and let the caller retry the application flow.
			if len(f.healthyLanes()) == 0 {
				if waitErr := f.waitForHealthyLane(ctx, laneReplacementWait); waitErr != nil {
					err = fmt.Errorf("last lane failed (%v): %w", err, waitErr)
				}
			}
			if f.localAbortSent.Load() {
				f.closeAll()
				stats.Ended = time.Now()
				stats.BytesSent = f.bytesUp.Load()
				stats.BytesRead = f.bytesDown.Load()
				stats.LaneBytes = f.laneStats()
				f.recordSendStalls(&stats)
				return stats, nil
			}
			if len(f.healthyLanes()) > 0 {
				if !f.replayComplete() {
					// Bytes released from the retention window have still not
					// been acknowledged, so a replacement lane cannot be given
					// a complete stream. Failing closed is required: replaying
					// a gap would silently corrupt the flow.
					err = fmt.Errorf("lane failed (%v): flow is no longer replayable", err)
				} else if replayErr := f.replayPending(ctx); replayErr == nil {
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
			f.recordSendStalls(&stats)
			return stats, err
		case <-ctx.Done():
			cancelRun()
			f.closeAll()
			stats.Ended = time.Now()
			stats.BytesSent = f.bytesUp.Load()
			stats.BytesRead = f.bytesDown.Load()
			stats.LaneBytes = f.laneStats()
			f.recordSendStalls(&stats)
			return stats, ctx.Err()
		}
	}
	f.closeAll()
	stats.Ended = time.Now()
	stats.BytesSent = f.bytesUp.Load()
	stats.BytesRead = f.bytesDown.Load()
	stats.LaneBytes = f.laneStats()
	f.recordSendStalls(&stats)
	return stats, nil
}

// watchLimits turns silent or unbounded flows into explicit, observable
// failures.  It never closes the flow itself: run owns teardown so the
// timeout has the same bounded worker/lane cleanup path as any other error.
func (f *multipathFlow) watchLimits(stop <-chan struct{}, out chan<- error) {
	idle := f.idleTimeout
	lifetime := f.maxLifetime
	if idle <= 0 && lifetime <= 0 {
		return
	}
	interval := time.Second
	if idle > 0 && idle/4 < interval {
		interval = idle / 4
		if interval < 10*time.Millisecond {
			interval = 10 * time.Millisecond
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lifetimeTimer *time.Timer
	var lifetimeC <-chan time.Time
	if lifetime > 0 {
		lifetimeTimer = time.NewTimer(lifetime)
		lifetimeC = lifetimeTimer.C
		defer lifetimeTimer.Stop()
	}
	for {
		select {
		case <-ticker.C:
			if idle > 0 {
				last := f.lastActivity.Load()
				if last == 0 || time.Since(time.Unix(0, last)) >= idle {
					select {
					case out <- errFlowIdleTimeout:
					case <-stop:
					}
					return
				}
			}
		case <-lifetimeC:
			select {
			case out <- errFlowLifetime:
			case <-stop:
			}
			return
		case <-stop:
			return
		case <-f.ctx.Done():
			return
		}
	}
}

func (f *multipathFlow) waitForRemoteFIN(ctx context.Context, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if f.remoteFinSeen.Load() {
			return true
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			return f.remoteFinSeen.Load()
		case <-ctx.Done():
			return false
		case <-f.done:
			return f.remoteFinSeen.Load()
		}
	}
}

// completionWatchdog handles the one remaining shutdown race that transport
// recovery cannot solve: both application FINs are known, but the final ACK
// is lost while the last physical lane is closing. The FIN pair is the
// correctness boundary; after a small grace period it is safe to release all
// workers and lanes. Normal completion stops run before this timer fires.
func (f *multipathFlow) completionWatchdog(stop <-chan struct{}) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	grace := f.completionGrace
	if grace <= 0 {
		grace = flowCompletionGrace
	}
	var bothSince time.Time
	for {
		select {
		case <-ticker.C:
			if f.finSent.Load() && f.remoteFinSeen.Load() {
				if bothSince.IsZero() {
					bothSince = time.Now()
					continue
				}
				if time.Since(bothSince) >= grace {
					if f.metrics != nil {
						f.metrics.CompletionTimeout()
					}
					f.closeAll()
					return
				}
			} else {
				bothSince = time.Time{}
			}
		case <-stop:
			return
		case <-f.ctx.Done():
			return
		}
	}
}

func (f *multipathFlow) telemetryLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	// The stall detector runs far more often than telemetry: a head-of-line
	// segment held on a slow lane has to be re-sent within a round trip or so,
	// or the reorder span keeps growing.
	stall := time.NewTicker(reinjectInterval)
	defer stall.Stop()
	for {
		select {
		case <-ticker.C:
			f.observeTransport(f.healthyLanes())
		case <-stall.C:
			f.reinjectStalledHead()
		case <-stop:
			return
		case <-f.ctx.Done():
			return
		}
	}
}

const (
	// reinjectInterval is how often a striped flow checks whether its oldest
	// unacknowledged frame is holding up the receiver.
	reinjectInterval = 200 * time.Millisecond
	// reinjectPressure is the fraction of the retention window that must be
	// occupied before re-sending anything. Below it the window is doing its
	// job and a duplicate would only waste capacity.
	reinjectPressure = 2
)

// reinjectStalledHead re-sends the oldest unacknowledged frame on the lane
// most likely to deliver it first.
//
// A striped flow acknowledges one cumulative sequence, so the receiver's
// contiguous point sits behind whatever the slowest lane has not delivered.
// The sender's retention window then covers the whole reorder span rather than
// the bytes actually in flight; it fills, evicts bytes the peer never
// acknowledged, and the flow becomes unreplayable, after which any lane failure
// is fatal. Measured on a per-flow-policed path, four-lane transfers failed
// roughly one time in three that way.
//
// Re-sending the head bounds the span: the receiver already deduplicates a
// segment it holds or has passed, so a duplicate is harmless, and delivering
// the missing bytes over a fast lane lets the contiguous point advance and the
// window drain. This is the reinjection multipath TCP performs for the same
// reason.
func (f *multipathFlow) reinjectStalledHead() {
	if f.laneCount() < 2 || f.doneChanClosed() {
		return
	}
	f.replayMu.Lock()
	underPressure := f.replayBytes*uint64(reinjectPressure) >= f.replayLimit
	head, retained := f.oldestRetainedLocked()
	alreadySent := f.reinjectedThrough
	f.replayMu.Unlock()
	if !underPressure || !retained || head.Header.Sequence < alreadySent {
		return
	}

	// Choose by predicted arrival, and never re-send onto a lane that has not
	// proven it can carry anything.
	candidates, err := f.laneCandidates(true)
	if err != nil {
		return
	}
	lane := candidates[0]
	if _, rtt := lane.sendRate(); rtt <= 0 {
		return
	}
	if accepted, _ := f.tryEnqueueFrameClass(lane, head, true); !accepted {
		return
	}
	f.chargeLane(lane, len(head.Payload))
	f.replayMu.Lock()
	// Advance past this frame so a stalled head is re-sent once per position
	// rather than on every tick.
	f.reinjectedThrough = head.Header.Sequence + 1
	f.replayMu.Unlock()
	f.reinjections.Add(1)
	f.metrics.Reinjected()
}

// oldestRetainedLocked returns the lowest-sequence frame still held for
// replay. It must be called with replayMu held.
func (f *multipathFlow) oldestRetainedLocked() (protocol.Frame, bool) {
	for _, sequence := range f.replayOrder {
		if frame, ok := f.replay[sequence]; ok {
			if frame.Header.Type != protocol.TypeData {
				continue
			}
			copied := frame
			copied.Payload = append([]byte(nil), frame.Payload...)
			return copied, true
		}
	}
	return protocol.Frame{}, false
}

func (f *multipathFlow) signalDone() {
	if f.done != nil {
		f.doneOnce.Do(func() {
			close(f.done)
			if f.ackTrack != nil {
				f.ackTrack.Close()
			}
		})
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

func (f *multipathFlow) doneChanClosed() bool {
	select {
	case <-f.done:
		return true
	default:
		return false
	}
}

func (f *multipathFlow) sendInner(ctx context.Context) (err error) {
	defer close(f.sendDone)
	buf := make([]byte, f.chunkSize)
	var sequence uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		if f.remoteAbort.Load() {
			// The peer explicitly closed its full application socket. Any
			// concurrent inner read error is an expected consequence of the
			// destination close, not a transport failure.
			return nil
		}
		// HTTP clients often close a fully-consumed SOCKS socket without a
		// TCP half-close. Treat that local close as EOF while the logical flow
		// is still live, so the peer receives a normal FIN and can release its
		// destination connection. Context cancellation remains fatal via the
		// check above.
		if readErr != nil && expectedHalfCloseError(readErr) {
			f.localClosed.Store(true)
			readErr = io.EOF
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			// EOF is intentionally treated as a local-close hint. It may be a
			// half-close, so receiveInner keeps waiting for the peer FIN and
			// escalates only after its post-ACK idle grace.
			f.localClosed.Store(true)
			f.sendSequence(sequence)
			fin := protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
				SessionID: f.sessionID, FlowID: f.flowID, Sequence: sequence, Class: protocol.Class(f.class.Load()),
			}}
			if err := f.recordReplayContext(ctx, fin); err != nil {
				return err
			}
			// Publish the logical FIN before enqueueing it. A lane writer can
			// fail immediately (or a test/fault injector can drop the frame), and
			// a replacement lane may be admitted before this goroutine resumes.
			// Recovery must then know that the FIN is pending and replay it rather
			// than treating the flow as one-sided and opening another rescue.
			f.finSent.Store(true)
			if err := f.enqueueOnHealthyLane(ctx, fin, false); err != nil {
				return err
			}
			select {
			case <-f.finalAck:
				return nil
			case <-f.done:
				// Both FIN directions prove that all application bytes have
				// crossed the logical flow. A final ACK may be lost during the
				// normal last-lane close race; the completed-session tombstone
				// can replay it on a replacement lane, so do not retain this
				// worker indefinitely.
				if f.finSent.Load() && f.remoteFinSeen.Load() {
					return nil
				}
				return errors.New("flow closed before final acknowledgement")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func (f *multipathFlow) enqueueFrame(ctx context.Context, lane *mpLane, frame protocol.Frame) error {
	return f.enqueueFrameClass(ctx, lane, frame, frame.Header.Class == protocol.ClassBulk)
}

func (f *multipathFlow) enqueueFrameClass(ctx context.Context, lane *mpLane, frame protocol.Frame, bulk bool) error {
	if lane == nil || lane.closed.Load() {
		return errors.New("lane is closed")
	}
	queue := lane.writeQ
	if !bulk && lane.writeInteractiveQ != nil {
		queue = lane.writeInteractiveQ
	}
	if queue == nil {
		return errors.New("lane writer queue is unavailable")
	}
	acquired := false
	if lane.writeSlots != nil {
		select {
		case lane.writeSlots <- struct{}{}:
			acquired = true
		case <-lane.writeDone:
			f.failLane(lane, errors.New("lane writer stopped"))
			return errors.New("lane writer stopped")
		case <-f.done:
			return errors.New("flow is closed")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case queue <- frame:
		return nil
	case <-lane.writeDone:
		if acquired {
			<-lane.writeSlots
		}
		f.failLane(lane, errors.New("lane writer stopped"))
		return errors.New("lane writer stopped")
	case <-f.done:
		if acquired {
			<-lane.writeSlots
		}
		return errors.New("flow is closed")
	case <-ctx.Done():
		if acquired {
			<-lane.writeSlots
		}
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
		candidates, err := f.laneCandidates(bulk)
		if err == nil {
			// Try each lane in preference order without blocking. Committing to
			// the best lane and then waiting for one of its queue slots lets a
			// single lane throttle the whole flow while other lanes sit idle:
			// the producer stops, so no later frame is ever offered to them and
			// the scheduler never sees the imbalance it was meant to correct.
			// A lane that is full and a lane that is unusable are both simply
			// skipped here; tryEnqueueFrameClass has already transitioned an
			// unusable lane so the recovery coordinator can replace it.
			for _, lane := range candidates {
				if accepted, _ := f.tryEnqueueFrameClass(lane, frame, bulk); accepted {
					f.chargeLane(lane, len(frame.Payload))
					return nil
				}
			}
			// Every eligible lane is full, so the flow really is transport
			// limited. Wait on the best one.
			if err = f.enqueueFrameClass(ctx, candidates[0], frame, bulk); err == nil {
				f.chargeLane(candidates[0], len(frame.Payload))
				return nil
			}
		}
		if err := f.waitForHealthyLane(ctx, laneReplacementWait); err != nil {
			return err
		}
	}
}

// tryEnqueueFrameClass offers a frame to one lane without blocking. It reports
// accepted=false with a nil error when the lane is merely full, so the caller
// can move on to the next lane, and a non-nil error when the lane is unusable.
func (f *multipathFlow) tryEnqueueFrameClass(lane *mpLane, frame protocol.Frame, bulk bool) (bool, error) {
	if lane == nil || lane.closed.Load() {
		return false, errors.New("lane is closed")
	}
	queue := lane.writeQ
	if !bulk && lane.writeInteractiveQ != nil {
		queue = lane.writeInteractiveQ
	}
	if queue == nil {
		return false, errors.New("lane writer queue is unavailable")
	}
	acquired := false
	if lane.writeSlots != nil {
		select {
		case lane.writeSlots <- struct{}{}:
			acquired = true
		case <-lane.writeDone:
			f.failLane(lane, errors.New("lane writer stopped"))
			return false, errors.New("lane writer stopped")
		default:
			return false, nil
		}
	}
	select {
	case queue <- frame:
		return true, nil
	case <-lane.writeDone:
		if acquired {
			<-lane.writeSlots
		}
		f.failLane(lane, errors.New("lane writer stopped"))
		return false, errors.New("lane writer stopped")
	default:
		if acquired {
			<-lane.writeSlots
		}
		return false, nil
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
		if existing, exists := f.replay[frame.Header.Sequence]; exists {
			f.replayMu.Unlock()
			if len(existing.Payload) == len(frame.Payload) && existing.Header.Type == frame.Header.Type {
				// Re-sending a chunk on a second lane is ordinary recovery
				// under self-pacing, and the bytes are already retained. This
				// used to be an error, which meant every re-issue failed its
				// send, failed its chunk, and took the lane's worker down with
				// it -- so a flow lost lanes one at a time exactly when it was
				// trying to recover.
				return nil
			}
			return errors.New("conflicting replay sequence")
		}
		if len(frame.Payload) > maxFlowReplayBytes {
			f.replayMu.Unlock()
			return errors.New("replay frame exceeds buffer limit")
		}
		if len(f.replay)+1 <= maxReplayFrames && f.replayBytes+uint64(len(frame.Payload)) > f.replayLimit {
			// The window is the flow's send window. Grow it from the shared
			// endpoint budget rather than blocking, so a high bandwidth-delay
			// product path is not throttled by a fixed constant, and so the
			// total commitment across flows stays accounted.
			f.growReplayLimitLocked(uint64(len(frame.Payload)))
		}
		if len(f.replay)+1 <= maxReplayFrames && f.replayBytes+uint64(len(frame.Payload)) <= f.replayLimit {
			frame.Payload = append([]byte(nil), frame.Payload...)
			f.replay[frame.Header.Sequence] = frame
			f.replayOrder = append(f.replayOrder, frame.Header.Sequence)
			f.replayBytes += uint64(len(frame.Payload))
			end := frame.Header.Sequence + uint64(len(frame.Payload))
			if end > f.highestSent {
				f.highestSent = end
			}
			f.replayMu.Unlock()
			return nil
		}
		// The window is full and cannot grow. Waiting here throttles the
		// application, and on a lossy path that is exactly the wrong
		// behaviour: these frames are released by the peer's protocol
		// acknowledgements, which travel as ordinary stream data on the
		// reverse direction and are therefore subject to the reverse path's
		// congestion window. When that window collapses under heavy loss the
		// acknowledgements stall, the window fills, and a transfer that QUIC
		// is still delivering perfectly well grinds to a halt. Measured on the
		// live 30-50% loss path, this is what stopped transfers at roughly the
		// 8 MiB window mark.
		//
		// The retained window is a rescue optimization, not a correctness
		// requirement: QUIC already delivers reliably on the lane that carried
		// the frame. Drop the oldest entries instead and record that the flow
		// can no longer be replayed onto a replacement lane. A lane failure
		// then fails the flow, which is the same outcome an unreplayable flow
		// already had, without the throughput coupling.
		f.evictOldestReplayLocked(uint64(len(frame.Payload)))
		f.replayMu.Unlock()
	}
}

// evictOldestReplayLocked frees at least needed bytes by discarding the
// lowest-sequence retained frames. It must be called with replayMu held.
//
// Eviction runs once per frame while the window is full, which is exactly when
// the path is already struggling, so it must not be expensive. replayOrder is
// a FIFO of retained sequences: data frames are recorded in increasing
// sequence order, so the oldest is always at its front and eviction is
// amortized constant time. Sorting the whole window on every frame instead
// would put an O(n log n) cost on the hot path under loss.
func (f *multipathFlow) evictOldestReplayLocked(needed uint64) {
	freed := uint64(0)
	for len(f.replayOrder) > 0 {
		if freed >= needed && len(f.replay)+1 <= maxReplayFrames {
			return
		}
		sequence := f.replayOrder[0]
		f.replayOrder = f.replayOrder[1:]
		entry, retained := f.replay[sequence]
		if !retained {
			// Already released by an acknowledgement.
			continue
		}
		delete(f.replay, sequence)
		f.replayBytes -= uint64(len(entry.Payload))
		freed += uint64(len(entry.Payload))
		if end := sequence + uint64(len(entry.Payload)); end > f.replayEvictedThrough {
			f.replayEvictedThrough = end
		}
		first := f.replayEvictions.Add(1) == 1
		f.metrics.ReplayEvicted(1, first)
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
	if f.ackTrack != nil {
		f.ackTrack.Advance(sequence)
	}
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
	f.pruneReplayOrderLocked()
	f.replayMu.Unlock()
	select {
	case f.replayNotify <- struct{}{}:
	default:
	}
	return nil
}

// pruneReplayOrderLocked drops leading entries whose frame has already been
// released. Without it the retention FIFO would grow by one entry per frame
// for the life of a flow even when nothing is ever evicted, which on a
// long-lived bulk transfer is millions of entries. It must be called with
// replayMu held.
func (f *multipathFlow) pruneReplayOrderLocked() {
	pruned := 0
	for pruned < len(f.replayOrder) {
		if _, retained := f.replay[f.replayOrder[pruned]]; retained {
			break
		}
		pruned++
	}
	if pruned == 0 {
		return
	}
	f.replayOrder = f.replayOrder[pruned:]
	// Re-slicing alone leaves the released prefix reachable through the
	// backing array. Compact once the dead prefix dominates, so a long flow
	// cannot pin an ever-growing allocation.
	if cap(f.replayOrder) > 64 && len(f.replayOrder)*2 < cap(f.replayOrder) {
		compacted := make([]uint64, len(f.replayOrder))
		copy(compacted, f.replayOrder)
		f.replayOrder = compacted
	}
}

// noteSent records that bytes have been written without retaining them.
//
// Under self-pacing the scheduler already holds every unacknowledged chunk and
// can re-issue it on any healthy lane, so retaining a second copy in the replay
// window buys nothing and costs a great deal: the two have different limits, so
// the scheduler outruns the window, the window evicts, the flow is marked
// unreplayable, and the next lane failure kills a transfer the scheduler could
// have finished. The acknowledgement path still needs to know how far the
// stream has been written, which is all this records.
func (f *multipathFlow) noteSent(sequence uint64, n int) {
	f.replayMu.Lock()
	defer f.replayMu.Unlock()
	if end := sequence + uint64(n); end > f.highestSent {
		f.highestSent = end
	}
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
		if err := f.enqueueOnHealthyLane(ctx, frame, frame.Header.Type == protocol.TypeData && frame.Header.Class == protocol.ClassBulk); err != nil {
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
			if err := lane.fc.WriteContext(ctx, frame); err != nil {
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
	var payload []byte
	// A final acknowledgement proves everything arrived, so ranges would add
	// nothing; and a peer that did not advertise support must never see them.
	if !final && f.ackRanges.Load() {
		if encoded, err := protocol.EncodeAckRanges(f.takeReceivedRanges(sequence)); err == nil && len(encoded) > 0 {
			payload = encoded
			flags |= protocol.FlagAckRanges
		}
	}
	frame := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeAck, Flags: flags,
		SessionID: f.sessionID, FlowID: f.flowID, Sequence: sequence,
		Class: protocol.Class(f.class.Load()),
	}, Payload: payload}
	// ACKs are cumulative and their state is replayable.  Do not wait for the
	// full lane-replacement timeout when every current lane rejects one: the
	// flow coordinator must observe the failure immediately so a replacement
	// can be admitted and the latest ACK/FIN state replayed there.
	lanes := f.healthyLanes()
	if len(lanes) == 0 {
		return errors.New("no healthy lane for acknowledgement")
	}
	var lastErr error
	for _, lane := range lanes {
		if err := lane.fc.WriteContext(ctx, frame); err == nil {
			return nil
		} else {
			lastErr = err
			f.failLane(lane, fmt.Errorf("lane %d acknowledgement write: %w", lane.id, err))
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all lanes rejected acknowledgement")
	}
	return lastErr
}

// A protocol ACK is a window-release message layered above a reliable
// transport, not a loss-recovery signal: QUIC already retransmits. Its rate
// should therefore follow how fast the sender's replay window is being
// consumed, not how often the receiver happens to read.
//
// Acknowledging every 2 ms sent thousands of tiny frames up the reverse
// direction of a download. On a path losing 40% of packets that is actively
// harmful: the reverse stream is ordered, so a lost ACK frame blocks the ones
// behind it, and the retransmissions consume the client's congestion window
// and delay QUIC's own acknowledgements, which is the feedback the sender's
// congestion controller runs on.
//
// Acknowledge instead once a meaningful part of the window has been consumed,
// or after a bounded delay, whichever comes first. The delay stays far below
// one long-haul round trip, and it cannot hold up application bytes in any
// case: a half-close is acknowledged by the separate immediate final-ACK path,
// so this delay only defers releasing replay-window space. The byte threshold
// stays far below the smallest replay window so a sender never runs out of
// window waiting for one.
const (
	// Under self-pacing an acknowledgement is not just bookkeeping: it is what
	// frees window space for the next chunk, so its latency adds directly to
	// the flow's effective round trip. At 200ms RTT the old 50ms coalescing
	// delay was a fifth of the loop, and the self-paced sender measured about
	// 12% below the pushing one largely because of it. TCP acknowledges every
	// couple of segments for the same reason; a chunk here is 32 KiB, so two
	// chunks is the threshold and the timer is only a backstop. The reverse
	// path cost is a few dozen small frames a second.
	ackCoalesceDelay  = 10 * time.Millisecond
	ackBytesThreshold = 64 * 1024
)

// scheduleACK publishes the newest cumulative receive sequence without
// blocking application delivery on a control-frame write. QUIC already
// provides hop reliability; these protocol ACKs exist to bound the replay
// window and support cross-lane resume, so they can be cumulative and
// coalesced. A failed write transitions the lane and lets run's normal rescue
// coordinator replace it; an independent error is reported through ackErr.
func (f *multipathFlow) scheduleACK(sequence uint64) {
	if sequence == 0 || f.ackClosing.Load() {
		return
	}
	for {
		old := f.ackSequence.Load()
		if sequence <= old || f.ackSequence.CompareAndSwap(old, sequence) {
			break
		}
	}
	select {
	case f.ackWake <- struct{}{}:
	default:
	}
}

func (f *multipathFlow) ackLoop(ctx context.Context) {
	var sent uint64
	for {
		select {
		case <-f.ackWake:
		case <-ctx.Done():
			return
		case <-f.done:
			return
		}
		// Coalesce unless the sender's window is already being consumed fast
		// enough that waiting could stall it.
		if f.ackSequence.Load() < sent+ackBytesThreshold {
			timer := time.NewTimer(ackCoalesceDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-f.done:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		}
		for {
			if f.ackClosing.Load() {
				return
			}
			sequence := f.ackSequence.Load()
			if sequence <= sent {
				break
			}
			if err := f.writeACK(ctx, sequence, f.recvAckFlag, false); err != nil {
				// A failed ACK write already transitions the affected lane and
				// publishes laneErr. Let run perform its normal bounded rescue when
				// no lane remains; only surface an independent write error while a
				// healthy lane is still available.
				if ctx.Err() == nil && !f.doneChanClosed() && len(f.healthyLanes()) > 0 {
					select {
					case f.ackErr <- err:
					default:
					}
				}
				return
			}
			sent = sequence
			// If more bytes arrived during the write, immediately emit the
			// newer cumulative value. Otherwise return to the wake channel.
			if f.ackSequence.Load() <= sent {
				break
			}
		}
	}
}

func (f *multipathFlow) acknowledgeRemoteFIN(ctx context.Context, sequence uint64, abort bool) error {
	f.remoteFinSequence.Store(sequence)
	f.remoteFinSeen.Store(true)
	f.ackClosing.Store(true)
	if cw, ok := f.inner.(closeWriter); ok {
		if err := cw.CloseWrite(); err != nil && !expectedHalfCloseError(err) {
			return err
		}
	}
	ackCtx, cancel := context.WithTimeout(ctx, finalAckWriteGrace)
	err := f.writeACK(ackCtx, sequence, f.recvAckFlag, true)
	cancel()
	if err != nil {
		// The peer FIN and reassembly sequence prove that all inbound bytes are
		// complete. Once our side is also closing, failure to return the final
		// ACK is a cleanup race, not an application-data failure. The server
		// retains a bounded tombstone and can replay/absorb the close state.
		if f.finSent.Load() || f.localClosed.Load() || abort {
			return nil
		}
		return err
	}
	if abort {
		f.remoteAbort.Store(true)
		// No response bytes remain useful after an explicit full-close.
		_ = f.inner.Close()
	}
	return nil
}

func (f *multipathFlow) receiveInner(ctx context.Context) error {
	reassembler := multipath.NewReassembler(multipath.Config{MaxBufferedBytes: maxReassemblyBytes, MaxBufferedFrames: maxReassemblyFrames})
	remoteFin := false
	var lastAckSequence uint64
	var abortTimer *time.Timer
	var abortTimerC <-chan time.Time
	resetAbortTimer := func() {
		if abortTimer == nil {
			abortTimer = time.NewTimer(f.localAbortGrace())
		} else {
			if !abortTimer.Stop() {
				select {
				case <-abortTimer.C:
				default:
				}
			}
			abortTimer.Reset(f.localAbortGrace())
		}
		abortTimerC = abortTimer.C
	}
	defer func() {
		if abortTimer != nil {
			abortTimer.Stop()
		}
	}()
	for {
		select {
		case event := <-f.events:
			frame := event.frame
			// A pipelined HELLO_OK is session-scoped and carries flow 0, so it
			// is validated before the per-flow identity check below.
			if frame.Header.Type == protocol.TypeHelloOK {
				if !f.helloAckPending || frame.Header.SessionID != f.sessionID || frame.Header.FlowID != 0 {
					return errors.New("unexpected session acknowledgement")
				}
				var helloOK session.HelloOK
				if err := helloOK.UnmarshalBinary(frame.Payload); err != nil {
					return fmt.Errorf("decode session acknowledgement: %w", err)
				}
				f.helloAckPending = false
				// The peer's capabilities arrive after this flow started, so
				// adopt the range acknowledgement setting now rather than
				// leaving this flow on cumulative-only behavior.
				f.ackRanges.Store(helloOK.Capabilities&session.CapabilityAckRanges != 0)
				if f.onHelloOK != nil {
					f.onHelloOK(helloOK)
				}
				continue
			}
			// A session-scoped rejection of the pipelined HELLO also carries
			// flow 0. Report it as the authentication failure it is rather
			// than as a mismatched flow identity.
			if f.helloAckPending && frame.Header.Type == protocol.TypeReset && frame.Header.FlowID == 0 {
				return errors.New("server rejected session authentication")
			}
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
						f.publishReceivedRanges(reassembler)
						f.scheduleACK(next)
						lastAckSequence = next
					}
				}
				if closed {
					// A FIN may arrive on one lane before an earlier data
					// segment on another lane. Once that gap is filled,
					// Reassembler reports closed=true here; the FIN was valid
					// and must complete the normal ACK/half-close path.
					abort := f.remoteAbort.Load()
					if err := f.acknowledgeRemoteFIN(ctx, reassembler.NextSequence(), abort); err != nil {
						return err
					}
					remoteFin = true
					if abort {
						return nil
					}
					select {
					case <-f.sendDone:
						return nil
					default:
					}
				}
			case protocol.TypeClose:
				if frame.Header.Flags&protocol.FlagFin == 0 || len(frame.Payload) != 0 || frame.Header.Flags&protocol.FlagAckFinal != 0 || frame.Header.Flags&(protocol.FlagAckUp|protocol.FlagAckDown) != 0 {
					return errors.New("invalid flow close frame")
				}
				abort := frame.Header.Flags&protocol.FlagCloseAbort != 0
				if abort {
					// Preserve the abort intent if the FIN is buffered behind
					// out-of-order data; the contiguous data path above will
					// perform the actual final ACK and close.
					f.remoteAbort.Store(true)
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
					if abortTimer != nil {
						resetAbortTimer()
					}
				}
				if closed {
					if err := f.acknowledgeRemoteFIN(ctx, reassembler.NextSequence(), abort); err != nil {
						return err
					}
					if abort {
						f.remoteAbort.Store(true)
						// The application has explicitly closed its full socket;
						// no response bytes are still useful. Closing the inner
						// connection releases a keep-alive destination and unblocks
						// sendInner, which observes remoteAbort and exits cleanly.
						_ = f.inner.Close()
						return nil
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
					if frame.Header.Flags&protocol.FlagAckRanges != 0 {
						ranges, err := protocol.DecodeAckRanges(frame.Payload, frame.Header.Sequence)
						if err != nil {
							return fmt.Errorf("acknowledgement ranges: %w", err)
						}
						f.releaseAcknowledgedRanges(ranges)
						f.ackTrack.Add(ranges)
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
					if f.localClosed.Load() {
						resetAbortTimer()
					}
				} else {
					return errors.New("final acknowledgement sequence mismatch")
				}
			case protocol.TypeOpenOK:
				if !f.openAckPending || frame.Header.SessionID != f.sessionID || frame.Header.FlowID != f.flowID || len(frame.Payload) != 0 {
					return errors.New("unexpected flow open acknowledgement")
				}
				f.openAckPending = false
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
		case <-f.done:
			// closeAll is used by the completion watcher after both FIN
			// directions have been observed, and by fatal shutdown paths.
			// In the former case no additional frame is required; in the
			// latter run has already selected the original error and is only
			// draining this worker.
			if f.finSent.Load() && f.remoteFinSeen.Load() {
				return nil
			}
			return errors.New("flow closed")
		case <-abortTimerC:
			if f.localAbortSent.CompareAndSwap(false, true) {
				if len(f.healthyLanes()) == 0 {
					// The peer has already closed its transport after ACKing our
					// FIN. There is no lane on which to carry the escalation, and
					// the application has no remaining reader; release locally.
					f.closeAll()
					return nil
				}
				if err := f.writeControl(ctx, protocol.Frame{Header: protocol.Header{
					Version: protocol.Version, Type: protocol.TypeClose,
					Flags:     protocol.FlagFin | protocol.FlagCloseAbort,
					SessionID: f.sessionID, FlowID: f.flowID, Sequence: f.finSequence.Load(),
					Class: protocol.Class(f.class.Load()),
				}}, nil); err != nil {
					if f.localClosed.Load() {
						f.closeAll()
						return nil
					}
					return err
				}
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (f *multipathFlow) observe(n int, up bool) bool {
	now := time.Now()
	f.lastActivity.Store(now.UnixNano())
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
	oldClass := classifier.Class(f.class.Load())
	newClass := f.classifier.Observe(obs)
	f.class.Store(uint32(protocol.Class(newClass)))
	if f.metrics != nil && newClass != oldClass {
		f.metrics.ClassTransition(int(newClass))
	}
	return newClass == classifier.ClassBulk
}

func (f *multipathFlow) closeAll() {
	f.closeOnce.Do(func() {
		// Mark completion before closing physical lanes. Their reader goroutines
		// can observe the resulting EOF concurrently; those expected shutdown
		// errors must not be exported as transport failures.
		f.finished.Store(true)
		_ = f.inner.Close()
		f.lanesMu.RLock()
		defer f.lanesMu.RUnlock()
		for _, lane := range f.lanes {
			_ = lane.fc.Close()
		}
	})
	f.signalDone()
}

// recordSendStalls attaches the sender's replay-window stall accounting to a
// completion record.
func (f *multipathFlow) recordSendStalls(stats *FlowStats) {
	stats.SendStalls = f.replayStalls.Load()
	stats.SendStalled = time.Duration(f.replayStalled.Load())
	stats.ReplayEvictions = f.replayEvictions.Load()
}

// growReplayLimitLocked raises this flow's send window so it can cover the
// path's bandwidth-delay product, drawing the increase from the endpoint's
// shared replay budget. It must be called with replayMu held. Failing to grow
// is not an error: the caller then waits for acknowledgements, which is the
// previous fixed-window behavior and keeps memory bounded under load.
func (f *multipathFlow) growReplayLimitLocked(needed uint64) {
	if f.replayLimit >= maxFlowReplayBytes {
		return
	}
	step := uint64(minFlowReplayBytes)
	for step < needed {
		step *= 2
	}
	if f.replayLimit+step > maxFlowReplayBytes {
		step = maxFlowReplayBytes - f.replayLimit
	}
	if step == 0 || !f.replayBudget.acquire(int64(step)) {
		return
	}
	f.replayLimit += step
	f.replayGranted += step
	f.metrics.ReplayBytes(int64(step))
}

// releaseReplayBudget returns this flow's share of the endpoint replay budget.
// It is idempotent so both the normal completion path and an error path can
// call it.
func (f *multipathFlow) releaseReplayBudget() {
	f.replayMu.Lock()
	granted := f.replayGranted
	f.replayGranted = 0
	f.replayLimit = maxReplayBytes
	f.replayMu.Unlock()
	f.replayBudget.release(int64(granted))
	f.metrics.ReplayBytes(-int64(granted))
}

// replayComplete reports whether every byte the peer has not acknowledged is
// still retained, and so whether this flow can be moved onto a replacement
// lane without delivering a gap.
//
// Eviction alone does not condemn a flow. Frames are released oldest first,
// and the peer's cumulative acknowledgement keeps advancing over them; once it
// passes everything that was released, the retained window is complete again.
// Treating the first eviction as permanent turned transfers that had delivered
// every byte into failures when a lane closed at teardown.
func (f *multipathFlow) replayComplete() bool {
	if f.scheduler.Load() != nil {
		// A self-paced flow keeps every unacknowledged chunk in the scheduler
		// and hands the ones a dead lane was carrying to lanes that still
		// work, so a replacement lane never needs a gap filled from this
		// window.
		return true
	}
	f.replayMu.Lock()
	defer f.replayMu.Unlock()
	return f.acked >= f.replayEvictedThrough
}

// publishReceivedRanges snapshots what the reassembler holds out of order.
// The acknowledgement loop runs on its own goroutine and must not touch the
// reassembler, which belongs to the receive loop.
func (f *multipathFlow) publishReceivedRanges(reassembler *multipath.Reassembler) {
	if !f.ackRanges.Load() {
		return
	}
	ranges := reassembler.ReceivedRanges(protocol.MaxAckRanges)
	f.rangesMu.Lock()
	f.pendingRanges = ranges
	f.rangesMu.Unlock()
}

// takeReceivedRanges returns the published ranges that are still above the
// cumulative point this acknowledgement carries.
//
// The snapshot is taken when a segment is inserted, but the acknowledgement is
// coalesced and sent later with a newer cumulative sequence. Ranges the cursor
// has since passed are not merely redundant: the peer rejects an ACK whose
// ranges start below its cumulative point, which failed the flow outright.
func (f *multipathFlow) takeReceivedRanges(cumulative uint64) [][2]uint64 {
	f.rangesMu.Lock()
	defer f.rangesMu.Unlock()
	fresh := f.pendingRanges[:0]
	for _, r := range f.pendingRanges {
		if r[0] >= cumulative {
			fresh = append(fresh, r)
		}
	}
	f.pendingRanges = fresh
	return fresh
}

// releaseAcknowledgedRanges drops retained frames the peer has reported
// holding, even though they sit above the cumulative point.
//
// This is what keeps a striped flow's retention window proportional to the
// bytes actually outstanding rather than to the whole reorder span. A frame is
// released only when a reported range covers it completely; a partially
// covered frame still has to be replayable in full.
func (f *multipathFlow) releaseAcknowledgedRanges(ranges [][2]uint64) {
	if len(ranges) == 0 {
		return
	}
	f.replayMu.Lock()
	for start, frame := range f.replay {
		if frame.Header.Type != protocol.TypeData {
			continue
		}
		end := start + uint64(len(frame.Payload))
		for _, r := range ranges {
			if r[0] <= start && end <= r[1] {
				delete(f.replay, start)
				f.replayBytes -= uint64(len(frame.Payload))
				break
			}
		}
	}
	f.pruneReplayOrderLocked()
	f.replayMu.Unlock()
	select {
	case f.replayNotify <- struct{}{}:
	default:
	}
}
