package pep

import (
	"context"
	"time"
)

// Lane admission, and why there is no flow-level window above it.
//
// RFC 6356's linked increase used to sit here, bounding what a flow could
// commit across all its lanes so that a multipath flow took no more of a
// shared bottleneck than a single-path one would. It went when the fairness
// obligation did, and the lanes it coupled went after it: a flow's data is
// carried by one connection. See docs/DESIGN.md.
//
// What is left is per-lane, and is not a congestion window. The transport
// already has one of those, and two nested controllers would be a mistake.

const (
	// laneSampleInterval is how often lane congestion state is read. It is
	// well inside a round trip on the paths this targets, so a loss episode is
	// seen while it still means something.
	laneSampleInterval = 50 * time.Millisecond
)

// laneAdmission answers the scheduler's admission questions for one flow.
//
// A lane's allowance is what its own transport says the path can hold, which
// stops the sender committing bytes to a lane that cannot move them. There is
// no second limit: there used to be a flow-level window over several lanes,
// and there is no longer more than one lane to bound.
type laneAdmission struct {
	flow *multipathFlow
}

func (w *laneAdmission) Lane(laneID uint64, _ uint64) int {
	allowance := minLaneQueueBytes
	if lane := w.flow.laneByID(laneID); lane != nil {
		allowance = lane.windowBytes()
	}
	return allowance
}

// Total is zero: nothing binds a flow's lanes together, because a flow's data
// is on one of them and its transport governs it.
func (w *laneAdmission) Total() int { return 0 }

// laneByID returns a lane by identifier, or nil.
func (f *multipathFlow) laneByID(laneID uint64) *mpLane {
	f.lanesMu.RLock()
	defer f.lanesMu.RUnlock()
	return f.lanes[laneID]
}

// sampleLaneCongestion reads each lane's transport for the telemetry the path
// model and the lane trace are built from.
//
// Loss is read as a counter rather than an event because that is what QUIC
// exposes: any increase since the last sample is a congestion episode, and the
// controller's own per-lane rate limit collapses a burst of losses into one
// decrease. Reading it on a timer rather than hooking the transport keeps this
// dependency-free enough that a TCP rescue lane needs no special case.
func (f *multipathFlow) sampleLaneCongestion(ctx context.Context) {
	ticker := time.NewTicker(laneSampleInterval)
	defer ticker.Stop()
	lost := make(map[uint64]uint64)
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		case <-ticker.C:
		}
		seen := make(map[uint64]bool)
		for _, lane := range f.healthyLanes() {
			seen[lane.id] = true
			provider, ok := lane.fc.transport().(laneStatsProvider)
			if !ok {
				continue
			}
			stats := provider.transportStats()
			rtt := stats.smoothedRTT
			if rtt <= 0 {
				rtt = stats.latestRTT
			}
			lost[lane.id] = stats.packetsLost
			traceLane(f, lane.id, stats)
		}
		for id := range lost {
			if !seen[id] {
				delete(lost, id)
			}
		}
	}
}
