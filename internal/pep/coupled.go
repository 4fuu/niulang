package pep

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"github.com/icourses-dev/wanopt/internal/mpcc"
)

// coupledCongestion enables RFC 6356 coupling across a flow's lanes.
//
// It is off by default, and the reason is measured rather than ideological.
// Coupling makes a striped flow claim no more than one connection's share of a
// shared bottleneck, which is the right thing to do to other people's traffic.
// It buys that with a flow-wide increase of one segment per round trip, so a
// flow needs thousands of round trips to grow into capacity that its lanes'
// own controllers find in a few -- and on the policed path this exists for,
// that is the difference between 37.5 Mbit/s and a single lane's 18.
//
// Both behaviours are therefore available and both are measured. What is not
// available is a claim that one of them is free.
var coupledCongestion atomic.Bool

func init() { coupledCongestion.Store(os.Getenv("WANOPT_COUPLED_CC") == "1") }

// SetCoupledCongestion selects coupled congestion control across lanes.
func SetCoupledCongestion(on bool) { coupledCongestion.Store(on) }

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
	cc   *mpcc.Window
}

func (w *laneAdmission) Lane(laneID uint64) int {
	allowance := minLaneWindowBytes
	if lane := w.flow.laneByID(laneID); lane != nil {
		allowance = lane.windowBytes()
	}
	if w.cc != nil {
		if coupled := w.cc.Lane(laneID); coupled > 0 && coupled < allowance {
			allowance = coupled
		}
	}
	return allowance
}

func (w *laneAdmission) Total() int {
	if w.cc == nil {
		// Zero means the flow window does not bind, leaving each lane governed
		// by its own transport. This is the uncoupled arrangement: fast, and
		// willing to take more than one share where lanes really do contend.
		return 0
	}
	return w.cc.Total()
}

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
func (f *multipathFlow) sampleLaneCongestion(ctx context.Context, cc *mpcc.Window) {
	if cc == nil {
		return
	}
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
			provider, ok := lane.fc.conn.(laneStatsProvider)
			if !ok {
				continue
			}
			stats := provider.transportStats()
			rtt := stats.smoothedRTT
			if rtt <= 0 {
				rtt = stats.latestRTT
			}
			cc.Observe(lane.id, rtt)
			previous, known := lost[lane.id]
			lost[lane.id] = stats.packetsLost
			if known && stats.packetsLost > previous {
				cc.Congestion(lane.id)
			}
		}
		for id := range lost {
			if !seen[id] {
				delete(lost, id)
				cc.Forget(id)
			}
		}
	}
}
