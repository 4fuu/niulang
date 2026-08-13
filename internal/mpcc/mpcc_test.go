package mpcc

import (
	"testing"
	"time"
)

const mib = 1024 * 1024

// bottleneck is a shared capacity, in bytes per round trip, and the lanes that
// contend for it. Two lanes on one bottleneck is a shared path; one lane each
// is a path that polices per connection.
type bottleneck struct {
	capacity float64
	lanes    []int
}

// simulate runs a congestion loop faithful enough to test the properties that
// matter. Each lane keeps its own Reno-like window, the flow's coupled window
// caps what may be offered in total, and a bottleneck whose lanes collectively
// overshoot signals congestion to all of them.
//
// The detail that matters, and that a cruder model gets wrong: a lane is never
// offered more than its own window allows. Splitting the flow's window equally
// across lanes would hand a poor lane work it cannot carry and make it report
// congestion forever, which is a bug in the simulation rather than a property
// of the algorithm.
func simulate(t *testing.T, w *Window, paths []bottleneck, rtt time.Duration, rounds int) int {
	t.Helper()
	now := time.Now()
	w.cfg.Now = func() time.Time { return now }

	laneCount := 0
	for _, p := range paths {
		for _, id := range p.lanes {
			if id+1 > laneCount {
				laneCount = id + 1
			}
		}
	}
	for round := 0; round < rounds; round++ {
		now = now.Add(rtt)
		for id := 0; id < laneCount; id++ {
			w.Observe(uint64(id), rtt)
		}
		// Offer work exactly as the real admission rule does: never more than
		// the lane's own window, never more than the flow's window in total.
		remaining := float64(w.Total())
		offered := make([]float64, laneCount)
		for id := 0; id < laneCount; id++ {
			take := float64(w.Lane(uint64(id)))
			if take > remaining {
				take = remaining
			}
			offered[id] = take
			remaining -= take
		}
		for _, p := range paths {
			var total float64
			for _, id := range p.lanes {
				total += offered[id]
			}
			congested := total > p.capacity
			for _, id := range p.lanes {
				if congested {
					w.Congestion(uint64(id))
					continue
				}
				if offered[id] > 0 {
					w.Acked(uint64(id), int(offered[id]))
				}
			}
		}
	}
	return w.Total()
}

// Property 2 of RFC 6356, and the one that makes striping safe to turn on: over
// a shared bottleneck, N lanes must not claim N times the capacity. The
// previous design had no coupling at all, and four uncoupled QUIC controllers
// over one bottleneck lost a transfer outright at 50 MiB.
func TestSharedBottleneckDoesNotScaleWithLaneCount(t *testing.T) {
	const capacity = 1 * mib
	const rtt = 200 * time.Millisecond

	one := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	single := simulate(t, one, []bottleneck{{capacity, []int{0}}}, rtt, 600)

	four := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	striped := simulate(t, four, []bottleneck{{capacity, []int{0, 1, 2, 3}}}, rtt, 600)

	if float64(striped) > 1.5*float64(single) {
		t.Fatalf("four lanes on one bottleneck settled at %d against one lane's %d: the flow is claiming extra shares",
			striped, single)
	}
}

// Property 1: striping must never do worse than the best single lane, or the
// common case pays for a feature that only helps the uncommon one.
func TestAggregateIsAtLeastTheBestSingleLane(t *testing.T) {
	const rtt = 200 * time.Millisecond

	one := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	single := simulate(t, one, []bottleneck{{2 * mib, []int{0}}}, rtt, 600)

	// The same good path plus a poor one on its own bottleneck. The poor lane
	// must not drag the flow below what the good lane alone achieved.
	two := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	mixed := simulate(t, two, []bottleneck{{2 * mib, []int{0}}, {64 * 1024, []int{1}}}, rtt, 600)

	if float64(mixed) < 0.8*float64(single) {
		t.Fatalf("adding a poor lane cut the window from %d to %d", single, mixed)
	}
}

// Where the lanes really are independent -- a path that polices each connection
// separately, which is the case striping exists for -- the flow must grow into
// the aggregate.
//
// It does, and the horizon here is the finding: the coupled increase is one
// segment per round trip for the whole flow, by design, so acquiring four
// lanes' worth of capacity takes thousands of round trips. Measured against
// this model, four independently policed lanes need on the order of ten
// thousand round trips -- half an hour at 200ms -- to reach four times a single
// lane's window. That is correct LIA behaviour and it is far too slow for a
// transfer that lasts eight seconds, which is why the shipped design does not
// use this window to discover capacity. See TestConvergenceIsTooSlowForShortTransfers.
func TestIndependentLanesGrowToTheAggregate(t *testing.T) {
	const perLane = 1 * mib
	const rtt = 200 * time.Millisecond

	one := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	single := simulate(t, one, []bottleneck{{perLane, []int{0}}}, rtt, 10000)

	four := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	striped := simulate(t, four, []bottleneck{
		{perLane, []int{0}}, {perLane, []int{1}}, {perLane, []int{2}}, {perLane, []int{3}},
	}, rtt, 10000)

	if float64(striped) < 1.5*float64(single) {
		t.Fatalf("four independently policed lanes settled at %d against one lane's %d: no aggregation",
			striped, single)
	}
}

// The cost of the fairness property, stated as a test so it cannot be forgotten
// when reading the reassuring ones above. Within the length of a real transfer,
// the coupled window has barely moved.
func TestConvergenceIsTooSlowForShortTransfers(t *testing.T) {
	const perLane = 1 * mib
	const rtt = 200 * time.Millisecond
	// Forty round trips is roughly an eight-second transfer at this RTT.
	four := New(Config{Initial: 128 * 1024, Max: 64 * mib})
	short := simulate(t, four, []bottleneck{
		{perLane, []int{0}}, {perLane, []int{1}}, {perLane, []int{2}}, {perLane, []int{3}},
	}, rtt, 40)
	if float64(short) > 2.0*4*perLane {
		t.Fatalf("window %d already at the aggregate after 40 round trips; "+
			"the convergence limitation this documents may have been fixed", short)
	}
}

// A loss episode is reported once per lost packet. Halving on every report
// collapses the window on a single burst, which on a path with correlated loss
// turns one event into a stall.
func TestCongestionDecreaseIsRateLimited(t *testing.T) {
	now := time.Now()
	w := New(Config{Initial: 4 * mib, Min: 64 * 1024, Now: func() time.Time { return now }})
	w.Observe(1, 200*time.Millisecond)

	before := w.Total()
	for i := 0; i < 20; i++ {
		w.Congestion(1)
	}
	after := w.Total()
	if after < before/3 {
		t.Fatalf("window fell from %d to %d on one burst: decreases are not rate limited", before, after)
	}

	// A later episode, past the round trip, must still take effect.
	now = now.Add(500 * time.Millisecond)
	w.Congestion(1)
	if w.Total() >= after {
		t.Fatalf("window did not fall on a fresh congestion episode (%d -> %d)", after, w.Total())
	}
}

func TestWindowRespectsFloorAndCeiling(t *testing.T) {
	now := time.Now()
	w := New(Config{Initial: 1 * mib, Min: 128 * 1024, Max: 2 * mib, Now: func() time.Time { return now }})
	w.Observe(1, 100*time.Millisecond)
	for i := 0; i < 50; i++ {
		now = now.Add(time.Second)
		w.Congestion(1)
	}
	if w.Total() < 128*1024 {
		t.Fatalf("window %d fell below the floor", w.Total())
	}
	for i := 0; i < 10000; i++ {
		w.Acked(1, 64*1024)
	}
	if w.Total() > 2*mib {
		t.Fatalf("window %d exceeded the ceiling", w.Total())
	}
}

// With nothing known about the lanes, the flow must behave like a single
// ordinary connection: it cannot be unfair, only slow.
func TestUnobservedLanesFallBackToSingleFlowBehaviour(t *testing.T) {
	w := New(Config{Initial: 128 * 1024, Max: 8 * mib})
	if got := w.alphaLocked(128 * 1024); got != 1 {
		t.Fatalf("alpha = %v with no lane state, want 1", got)
	}
	for i := 0; i < 200; i++ {
		w.Acked(7, 32*1024)
	}
	if w.Total() <= 128*1024 {
		t.Fatal("window never grew for an unobserved lane")
	}
}

// A lane that goes away must stop influencing the coupling factor.
func TestForgetDropsLaneState(t *testing.T) {
	w := New(Config{})
	w.Observe(1, 100*time.Millisecond)
	w.Observe(2, 100*time.Millisecond)
	withTwo := w.alphaLocked(float64(w.Total()))
	w.Forget(2)
	withOne := w.alphaLocked(float64(w.Total()))
	if withTwo == withOne {
		t.Fatal("forgetting a lane did not change the coupling factor")
	}
}
