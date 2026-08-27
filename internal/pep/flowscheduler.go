package pep

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/4fuu/niulang/internal/protocol"
)

const defaultFlowStartupBytes = 512 << 10

var errFlowSchedulingClosed = errors.New("flow scheduling closed")

// flowSchedulerConfig bounds how much one provider path may hand to its QUIC
// carriers concurrently. The bound is deliberately small: it is a scheduling
// window, not a byte-rate limit, and exists only to stop one bulk flow from
// filling every connection queue before a newly visible flow can be served.
type flowSchedulerConfig struct {
	startupBytes     int
	perFlow          int
	maxActive        int
	interactiveBurst int
	bulkAge          time.Duration
}

func defaultFlowSchedulerConfig() flowSchedulerConfig {
	return flowSchedulerConfig{
		startupBytes:     defaultFlowStartupBytes,
		perFlow:          2,
		maxActive:        8,
		interactiveBurst: 3,
		bulkAge:          250 * time.Millisecond,
	}
}

func (c flowSchedulerConfig) withDefaults() flowSchedulerConfig {
	defaults := defaultFlowSchedulerConfig()
	if c.startupBytes <= 0 {
		c.startupBytes = defaults.startupBytes
	}
	if c.perFlow <= 0 {
		c.perFlow = defaults.perFlow
	}
	if c.maxActive <= 0 {
		c.maxActive = defaults.maxActive
	}
	if c.interactiveBurst <= 0 {
		c.interactiveBurst = defaults.interactiveBurst
	}
	if c.bulkAge <= 0 {
		c.bulkAge = defaults.bulkAge
	}
	return c
}

// flowSchedulerSet owns one scheduler per stable provider-path identity. A
// pooled connection and later isolated lanes therefore coordinate even though
// they use different QUIC connections and ephemeral ports.
type flowSchedulerSet struct {
	mu    sync.Mutex
	cfg   flowSchedulerConfig
	paths map[string]*flowScheduler
}

func newFlowSchedulerSet(startupBytes int) *flowSchedulerSet {
	cfg := defaultFlowSchedulerConfig()
	if startupBytes > 0 {
		cfg.startupBytes = startupBytes
	}
	return &flowSchedulerSet{cfg: cfg, paths: make(map[string]*flowScheduler)}
}

func (s *flowSchedulerSet) acquire(ctx context.Context, stop <-chan struct{}, path string, flowID uint64, class protocol.Class, bytes int) (func(), error) {
	if s == nil || path == "" || bytes <= 0 {
		return nil, nil
	}
	select {
	case <-stop:
		return nil, errFlowSchedulingClosed
	default:
	}
	s.mu.Lock()
	select {
	case <-stop:
		s.mu.Unlock()
		return nil, errFlowSchedulingClosed
	default:
	}
	scheduler := s.paths[path]
	if scheduler == nil {
		scheduler = newFlowScheduler(s.cfg)
		s.paths[path] = scheduler
	}
	grant := scheduler.request(flowID, class, bytes)
	s.mu.Unlock()
	select {
	case <-stop:
		if scheduler.cancel(grant) {
			s.removeIfEmpty(path, scheduler)
		}
		return nil, errFlowSchedulingClosed
	default:
	}
	select {
	case <-grant.ready:
		// Flow teardown closes stop before reclaiming this flow's scheduler
		// state. Prefer that durable close signal when both it and an immediate
		// grant are ready, rather than reviving state after closeFlow's sweep.
		select {
		case <-stop:
			if scheduler.cancel(grant) {
				s.removeIfEmpty(path, scheduler)
			}
			return nil, errFlowSchedulingClosed
		default:
		}
		scheduler.mu.Lock()
		err := grant.done
		scheduler.mu.Unlock()
		if err != nil {
			if scheduler.empty() {
				s.removeIfEmpty(path, scheduler)
			}
			return nil, err
		}
		var once sync.Once
		return func() {
			once.Do(func() {
				if scheduler.release(grant) {
					s.removeIfEmpty(path, scheduler)
				}
			})
		}, nil
	case <-ctx.Done():
		if scheduler.cancel(grant) {
			s.removeIfEmpty(path, scheduler)
		}
		return nil, ctx.Err()
	case <-stop:
		if scheduler.cancel(grant) {
			s.removeIfEmpty(path, scheduler)
		}
		return nil, errFlowSchedulingClosed
	}
}

func (s *flowSchedulerSet) closeFlow(flowID uint64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	paths := make(map[string]*flowScheduler, len(s.paths))
	for path, scheduler := range s.paths {
		paths[path] = scheduler
	}
	s.mu.Unlock()
	for path, scheduler := range paths {
		if scheduler.closeFlow(flowID) {
			s.removeIfEmpty(path, scheduler)
		}
	}
}

func (s *flowSchedulerSet) removeIfEmpty(path string, scheduler *flowScheduler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths[path] == scheduler && scheduler.empty() {
		delete(s.paths, path)
	}
}

type flowScheduler struct {
	mu sync.Mutex

	cfg      flowSchedulerConfig
	states   map[uint64]*flowScheduleState
	high     []*flowGrant
	bulk     []*flowGrant
	active   int
	highRun  int
	lastFlow uint64
}

type flowGrant struct {
	flowID uint64
	bytes  int
	high   bool
	queued time.Time
	ready  chan struct{}

	granted bool
	done    error
	state   *flowScheduleState
}

type flowScheduleState struct {
	active          int
	pending         int
	startupAssigned int
	grants          map[*flowGrant]struct{}
}

func newFlowScheduler(cfg flowSchedulerConfig) *flowScheduler {
	return &flowScheduler{cfg: cfg.withDefaults(), states: make(map[uint64]*flowScheduleState)}
}

func (s *flowScheduler) request(flowID uint64, class protocol.Class, bytes int) *flowGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[flowID]
	if state == nil {
		state = &flowScheduleState{grants: make(map[*flowGrant]struct{})}
		s.states[flowID] = state
	}
	high := class == protocol.ClassNew || class == protocol.ClassInteractive
	if !high && state.startupAssigned < s.cfg.startupBytes {
		state.startupAssigned += bytes
		high = true
	}
	grant := &flowGrant{
		flowID: flowID, bytes: bytes, high: high, queued: time.Now(),
		ready: make(chan struct{}), state: state,
	}
	state.pending++
	if high {
		s.high = append(s.high, grant)
	} else {
		s.bulk = append(s.bulk, grant)
	}
	s.dispatchLocked()
	return grant
}

func (s *flowScheduler) dispatchLocked() {
	for s.active < s.cfg.maxActive {
		chooseBulk := len(s.bulk) > 0 && (len(s.high) == 0 || s.highRun >= s.cfg.interactiveBurst || time.Since(s.bulk[0].queued) >= s.cfg.bulkAge)
		var grant *flowGrant
		if chooseBulk {
			grant, s.bulk = s.takeEligibleLocked(s.bulk)
			if grant != nil {
				s.highRun = 0
			}
		}
		if grant == nil && len(s.high) > 0 {
			grant, s.high = s.takeEligibleLocked(s.high)
			if grant != nil {
				s.highRun++
			}
		}
		if grant == nil && len(s.bulk) > 0 {
			grant, s.bulk = s.takeEligibleLocked(s.bulk)
			if grant != nil {
				s.highRun = 0
			}
		}
		if grant == nil {
			return
		}
		grant.granted = true
		grant.state.pending--
		grant.state.active++
		grant.state.grants[grant] = struct{}{}
		s.active++
		s.lastFlow = grant.flowID
		close(grant.ready)
	}
}

func (s *flowScheduler) takeEligibleLocked(queue []*flowGrant) (*flowGrant, []*flowGrant) {
	fallback := -1
	selected := -1
	for i, grant := range queue {
		if grant.done != nil || grant.state.active >= s.cfg.perFlow {
			continue
		}
		if fallback < 0 {
			fallback = i
		}
		if grant.flowID != s.lastFlow {
			selected = i
			break
		}
	}
	if selected < 0 {
		selected = fallback
	}
	if selected < 0 {
		return nil, queue
	}
	grant := queue[selected]
	copy(queue[selected:], queue[selected+1:])
	queue[len(queue)-1] = nil
	return grant, queue[:len(queue)-1]
}

func (s *flowScheduler) release(grant *flowGrant) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if grant == nil || grant.done != nil {
		return s.emptyLocked()
	}
	grant.done = errFlowSchedulingClosed
	if grant.granted {
		grant.state.active--
		s.active--
		delete(grant.state.grants, grant)
	}
	s.dispatchLocked()
	return s.emptyLocked()
}

func (s *flowScheduler) cancel(grant *flowGrant) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if grant == nil || grant.done != nil {
		return s.emptyLocked()
	}
	grant.done = context.Canceled
	if grant.granted {
		grant.state.active--
		s.active--
		delete(grant.state.grants, grant)
	} else {
		grant.state.pending--
		s.high = removeFlowGrant(s.high, grant)
		s.bulk = removeFlowGrant(s.bulk, grant)
	}
	s.dispatchLocked()
	return s.emptyLocked()
}

func (s *flowScheduler) closeFlow(flowID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.states[flowID]
	if state == nil {
		return s.emptyLocked()
	}
	closePending := func(queue []*flowGrant) []*flowGrant {
		kept := queue[:0]
		for _, grant := range queue {
			if grant.flowID != flowID || grant.done != nil {
				kept = append(kept, grant)
				continue
			}
			grant.done = errFlowSchedulingClosed
			state.pending--
			close(grant.ready)
		}
		clear(queue[len(kept):])
		return kept
	}
	s.high = closePending(s.high)
	s.bulk = closePending(s.bulk)
	for grant := range state.grants {
		if grant.done == nil {
			grant.done = errFlowSchedulingClosed
			state.active--
			s.active--
		}
		delete(state.grants, grant)
	}
	delete(s.states, flowID)
	s.dispatchLocked()
	return s.emptyLocked()
}

func removeFlowGrant(queue []*flowGrant, target *flowGrant) []*flowGrant {
	for i, grant := range queue {
		if grant != target {
			continue
		}
		copy(queue[i:], queue[i+1:])
		queue[len(queue)-1] = nil
		return queue[:len(queue)-1]
	}
	return queue
}

func (s *flowScheduler) emptyLocked() bool {
	return s.active == 0 && len(s.high) == 0 && len(s.bulk) == 0 && len(s.states) == 0
}

func (s *flowScheduler) empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emptyLocked()
}
