package pep

import (
	"context"
	"time"
)

// Lanes are deliberately not coupled.
//
// RFC 6356's linked increase exists to enforce "do no harm": a multipath flow
// must take no more of a shared bottleneck than a single-path flow would. That
// is the right property for a general-purpose transport on the public internet
// and it is the wrong one here. This transport exists because the paths it
// runs over are policed per connection rather than per endpoint pair, so its
// purpose is to claim more of the link than one connection is allowed, and a
// mechanism whose job is to prevent exactly that has nothing to contribute.
//
// The implementation that used to sit here is deleted rather than disabled, so
// nothing reads as though the property were merely switched off.

const (
	// laneSampleInterval is how often lane congestion state is read. It is
	// well inside a round trip on the paths this targets, so a loss episode is
	// seen while it still means something.
	laneSampleInterval = 50 * time.Millisecond
)

// laneAdmission answers the scheduler's admission questions for one flow.
//
// A lane's allowance is the smaller of what its own transport says the path can
// hold and what the coupled controller permits. The first stops the sender
// committing bytes to a lane that cannot move them; the second stops the lanes
// collectively claiming more than one connection's share.
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

// Total is zero: no flow-level window binds the lanes together. Each lane is
// governed by its own transport, which is the arrangement this transport wants
// -- see the note above on why the lanes are not coupled.
func (w *laneAdmission) Total() int { return 0 }

// laneByID returns a lane by identifier, or nil.
func (f *multipathFlow) laneByID(laneID uint64) *mpLane {
	f.lanesMu.RLock()
	defer f.lanesMu.RUnlock()
	return f.lanes[laneID]
}

// sampleLaneCongestion feeds the coupled controller from the lanes' own
// transports.
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
