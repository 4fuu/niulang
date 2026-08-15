// Package pathmodel is the one place a path is measured.
//
// Everything that adapts to this project's path needs the same three
// quantities -- how much it erases, how much of that is congestion, and how
// fast it is -- and each of them used to be estimated separately by whatever
// needed it. A congestion controller measured the erasure floor from the
// packets it sent; an erasure code measured it again from the shards it
// received; a second lane measured it a third time from scratch. Three
// estimates of one number, each wrong until it converged, and each converging
// on its own traffic.
//
// The cost is not only duplication. An estimate that starts at zero is an
// estimate that says "this path is clean", and a code sized by it carries no
// parity. A sender that runs ahead of its own feedback therefore commits its
// whole window to the wire uncoded -- measured, that is the difference between
// 1.03 and 1.74 Mbit/s across the emulated channel, and it is not a tuning
// error but a consequence of asking each component to learn the path alone.
//
// So the path is measured once, per endpoint pair, and read by everyone. The
// congestion controller contributes what it learns from its own
// acknowledgements, which is the erasure rate of the direction it sends into;
// that is exactly the number an erasure code needs and would otherwise wait
// for the peer to tell it.
package pathmodel

import (
	"sync"
	"time"
)

// PathModel is what everything sending to one endpoint pair has in common,
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
	mu      sync.Mutex
	members map[Member]*report
	// aggregate is a windowed maximum of the summed delivered rate, which is
	// the endpoint pair's bottleneck as measured from this side.
	aggregate []bandwidthSample
	// striping is what a lane probe concluded about this path, which is a
	// property of the path and so outlives the flow that measured it.
	striping Striping
}

// Member identifies one contributor within a model. A pointer is stable for
// its owner's lifetime and unique among live owners, which is all membership
// needs.
type Member uintptr

type report struct {
	floor     float64
	samples   float64
	delivered float64
	roundTrip time.Duration
	at        time.Time
}

// State is what an endpoint pair has been measured to do, from the point of
// view of one contributor.
type State struct {
	// Floor is the erasure rate that does not respond to sending more slowly,
	// pooled across every lane's samples.
	Floor float64
	// Share is this contributor's allowance of the bottleneck in bytes per
	// second. Zero means the bottleneck is not yet known and the contributor
	// should not cap itself.
	Share float64
	// RoundTrip is the path's minimum round trip, which is the smallest any
	// lane has seen: a larger one is that lane's queueing, not the path.
	RoundTrip time.Duration
}

type bandwidthSample struct {
	rate float64
	at   time.Time
}

const (
	// memberIdle is how long a contributor may go without reporting before it
	// stops counting. It is several round trips on a long-haul path, so one
	// that is merely quiet is not evicted and made to rediscover the path.
	memberIdle = 5 * time.Second
	// bottleneckWindow is how long a delivered-rate sample stands. Long enough
	// to survive a probe cycle, short enough that a path which genuinely
	// narrowed is believed within a few seconds.
	bottleneckWindow = 10 * time.Second
)

// Striping is what a lane probe concluded about this path.
type Striping uint8

const (
	// StripingUnknown means no probe has concluded yet.
	StripingUnknown Striping = iota
	// StripingRewarded means a probe measured that a second lane raised the
	// aggregate.
	StripingRewarded
	// StripingRefused means a probe measured that it did not.
	StripingRefused
)

// RecordStriping remembers what a lane probe concluded, so the next flow does
// not repeat the experiment.
//
// Whether lanes help is a property of the path, not of a flow: it depends on
// whether the bottleneck is policed per connection or per endpoint pair, and
// that does not change between one download and the next. Without somewhere to
// put the answer, every flow re-runs the probe -- growing a lane, measuring
// that it did not help, retiring it, and growing again -- and pays for the
// experiment each time on a path that has already answered.
func (m *PathModel) RecordStriping(verdict Striping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.striping = verdict
}

// Striping is what is known about whether this path rewards lanes.
func (m *PathModel) Striping() Striping {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.striping
}

// NewPathModel returns an empty model. Callers normally want SharedPath.
func NewPathModel() *PathModel {
	return &PathModel{members: make(map[Member]*report)}
}

// Report records what one lane currently believes, and returns what the
// endpoint pair believes.
func (m *PathModel) Report(member Member, floor, samples, delivered float64, roundTrip time.Duration) State {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.members[member]
	if !ok {
		entry = &report{}
		m.members[member] = entry
	}
	entry.floor, entry.samples, entry.delivered, entry.at = floor, samples, delivered, now
	if roundTrip > 0 {
		entry.roundTrip = roundTrip
	}

	var state State
	var weighted, weight, sum float64
	live := 0
	for key, other := range m.members {
		if now.Sub(other.at) > memberIdle {
			delete(m.members, key)
			continue
		}
		live++
		sum += other.delivered
		// A lane with few samples should not move the pooled estimate much,
		// which is also what lets a new lane join without disturbing it.
		weighted += other.floor * other.samples
		weight += other.samples
		if other.roundTrip > 0 && (state.RoundTrip == 0 || other.roundTrip < state.RoundTrip) {
			state.RoundTrip = other.roundTrip
		}
	}
	if weight > 0 {
		state.Floor = weighted / weight
	} else {
		state.Floor = floor
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
		state.Share = bottleneck / float64(live)
	}
	return state
}

// Current is what the model already knows, without contributing to it: the
// pooled erasure floor and the share a lane joining now would be given.
//
// A lane that starts from nothing has to rediscover a path its siblings
// already measured, and on a channel that erases 40% of packets that discovery
// is expensive -- it is the same ramp that costs a loss-based controller the
// path in the first place. A replacement lane, opened because its predecessor
// died, would pay it every time.
func (m *PathModel) Current() State {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	var state State
	var weighted, weight, bottleneck float64
	live := 0
	for key, entry := range m.members {
		if now.Sub(entry.at) > memberIdle {
			delete(m.members, key)
			continue
		}
		live++
		weighted += entry.floor * entry.samples
		weight += entry.samples
		if entry.roundTrip > 0 && (state.RoundTrip == 0 || entry.roundTrip < state.RoundTrip) {
			state.RoundTrip = entry.roundTrip
		}
	}
	if weight > 0 {
		state.Floor = weighted / weight
	}
	for _, sample := range m.aggregate {
		if now.Sub(sample.at) <= bottleneckWindow && sample.rate > bottleneck {
			bottleneck = sample.rate
		}
	}
	if bottleneck > 0 {
		// The joining lane counts too, or the first thing it does is take a
		// share sized for a path with one fewer lane on it.
		state.Share = bottleneck / float64(live+1)
	}
	return state
}

// Members is how many contributors are currently reporting.
func (m *PathModel) Members() int {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	live := 0
	for key, entry := range m.members {
		if now.Sub(entry.at) > memberIdle {
			delete(m.members, key)
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

// Forget drops what is known about a path.
//
// A path that is gone should not be remembered indefinitely: the registry only
// grows by the number of distinct uplinks a machine has used, which is small,
// but a measurement kept past the network it describes is a confident wrong
// answer waiting to be read.
func Forget(key string) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	delete(shared, key)
}

// Reset drops every model.
//
// It exists for tests, and it exists because of what a path key is. A model is
// keyed by the endpoint pair, which is the right key for a real network and
// the wrong one for a process where every pair is loopback to loopback: two
// tests that could not affect each other on a real path share one model here,
// and the second inherits whatever the first measured. That has produced
// failures that look like flakes and are not -- a test whose channel erases
// 42% of packets, sizing its code from a floor a previous test measured on a
// clean one, and stalling until its deadline.
//
// A test that brings up its own endpoints starts from nothing by calling this.
func Reset() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	clear(shared)
}

// Shared returns the model for an endpoint pair, creating it on first use.
// The key should identify the peer rather than the connection: lanes to the
// same peer are exactly the ones that must share.
func Shared(key string) *PathModel {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	model, ok := shared[key]
	if !ok {
		model = NewPathModel()
		shared[key] = model
	}
	return model
}
