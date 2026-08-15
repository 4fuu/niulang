package congestion

import (
	"sync"
	"time"
)

// PathModel is what several lanes to the same endpoint pair have in common,
// which on this project's path is almost everything.
//
// Lanes were measured to be worth less than nothing here: one lane delivers
// about 11 Mbit/s live and four deliver about 8. The open-loop probe explains
// why -- the bottleneck is per endpoint pair rather than per 4-tuple, so lanes
// cannot multiply the share -- but it does not explain the loss, and the loss
// is what this type is for.
//
// Two things go wrong when each lane decides alone. Each measures the erasure
// floor from its own packets, so four lanes spend four times as long reaching a
// usable estimate and then disagree; and each discovers the bottleneck from its
// own delivered rate, so each probes above its own share and the aggregate
// overshoots by however many lanes there are. Past the knee the path's loss
// stops being memoryless, which costs more than the lanes were ever going to
// win.
//
// Sharing fixes both. The floor is pooled, so it converges on the sample count
// of every lane together. The bottleneck is discovered once for the endpoint
// pair, and each lane is capped at its share of it, so the aggregate on the
// wire is what one sender would have put there.
//
// Membership is by activity rather than registration. A congestion controller
// has no close hook to deregister in, and a lane that has stopped reporting has
// stopped sending, which is the same thing for this purpose.
type PathModel struct {
	mu    sync.Mutex
	lanes map[laneKey]*laneReport
	// aggregate is a windowed maximum of the summed delivered rate, which is
	// the endpoint pair's bottleneck as measured from this side.
	aggregate []bandwidthSample
}

type laneKey uintptr

type laneReport struct {
	floor     float64
	samples   float64
	delivered float64
	at        time.Time
}

type bandwidthSample struct {
	rate float64
	at   time.Time
}

const (
	// laneIdle is how long a lane may go without reporting before it stops
	// counting. It is several round trips on a long-haul path, so a lane that
	// is merely quiet is not evicted and made to rediscover the path.
	laneIdle = 5 * time.Second
	// bottleneckWindow is how long a delivered-rate sample stands. Long enough
	// to survive a probe cycle, short enough that a path which genuinely
	// narrowed is believed within a few seconds.
	bottleneckWindow = 10 * time.Second
)

// NewPathModel returns an empty model. Callers normally want SharedPath.
func NewPathModel() *PathModel {
	return &PathModel{lanes: make(map[laneKey]*laneReport)}
}

// Report records what one lane currently believes, and returns what the
// endpoint pair believes: the pooled erasure floor, and this lane's share of
// the bottleneck in bytes per second. A share of zero means the bottleneck is
// not yet known and the lane should not cap itself.
func (m *PathModel) Report(lane laneKey, floor, samples, delivered float64) (pooledFloor, share float64) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	report, ok := m.lanes[lane]
	if !ok {
		report = &laneReport{}
		m.lanes[lane] = report
	}
	report.floor, report.samples, report.delivered, report.at = floor, samples, delivered, now

	var weighted, weight, sum float64
	live := 0
	for key, other := range m.lanes {
		if now.Sub(other.at) > laneIdle {
			delete(m.lanes, key)
			continue
		}
		live++
		sum += other.delivered
		// A lane with few samples should not move the pooled estimate much,
		// which is also what lets a new lane join without disturbing it.
		weighted += other.floor * other.samples
		weight += other.samples
	}
	if weight > 0 {
		pooledFloor = weighted / weight
	} else {
		pooledFloor = floor
	}

	if sum > 0 {
		m.aggregate = append(m.aggregate, bandwidthSample{rate: sum, at: now})
	}
	bottleneck := 0.0
	kept := m.aggregate[:0]
	for _, sample := range m.aggregate {
		if now.Sub(sample.at) > bottleneckWindow {
			continue
		}
		kept = append(kept, sample)
		if sample.rate > bottleneck {
			bottleneck = sample.rate
		}
	}
	m.aggregate = kept

	if live > 0 && bottleneck > 0 {
		share = bottleneck / float64(live)
	}
	return pooledFloor, share
}

// Current is what the model already knows, without contributing to it: the
// pooled erasure floor and the share a lane joining now would be given.
//
// A lane that starts from nothing has to rediscover a path its siblings
// already measured, and on a channel that erases 40% of packets that discovery
// is expensive -- it is the same ramp that costs a loss-based controller the
// path in the first place. A replacement lane, opened because its predecessor
// died, would pay it every time.
func (m *PathModel) Current() (floor, share float64) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var weighted, weight, bottleneck float64
	live := 0
	for key, report := range m.lanes {
		if now.Sub(report.at) > laneIdle {
			delete(m.lanes, key)
			continue
		}
		live++
		weighted += report.floor * report.samples
		weight += report.samples
	}
	if weight > 0 {
		floor = weighted / weight
	}
	for _, sample := range m.aggregate {
		if now.Sub(sample.at) <= bottleneckWindow && sample.rate > bottleneck {
			bottleneck = sample.rate
		}
	}
	if bottleneck > 0 {
		// The joining lane counts too, or the first thing it does is take a
		// share sized for a path with one fewer lane on it.
		share = bottleneck / float64(live+1)
	}
	return floor, share
}

// Lanes is how many lanes are currently reporting.
func (m *PathModel) Lanes() int {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	live := 0
	for key, report := range m.lanes {
		if now.Sub(report.at) > laneIdle {
			delete(m.lanes, key)
			continue
		}
		live++
	}
	return live
}

// shared holds one model per endpoint pair. The map only ever grows by the
// number of distinct peers, and an idle model is a few words, so there is
// nothing to reclaim that would be worth the lifetime tracking.
var (
	sharedMu sync.Mutex
	shared   = make(map[string]*PathModel)
)

// SharedPath returns the model for an endpoint pair, creating it on first use.
// The key should identify the peer rather than the connection: lanes to the
// same peer are exactly the ones that must share.
func SharedPath(key string) *PathModel {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	model, ok := shared[key]
	if !ok {
		model = NewPathModel()
		shared[key] = model
	}
	return model
}
