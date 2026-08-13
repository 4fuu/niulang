package pep

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// laneTrace prints one line per lane per congestion sample when
// WANOPT_LANE_TRACE is set. It exists because every question this transport has
// had to answer -- why a window grew, why a bottleneck dropped, why one trial
// was twice another -- is a question about per-lane state over time, and a
// throughput number at the end of a transfer cannot answer any of them.
//
// It is off unless asked for, writes to stderr, and is not a metric: metrics
// are aggregated and this is deliberately raw.
var laneTrace atomic.Bool

func init() { laneTrace.Store(os.Getenv("WANOPT_LANE_TRACE") == "1") }

var traceStart = time.Now()

// traceLane records one lane's transport and scheduler state. The flow is
// identified by its address rather than its wire identifier because both
// endpoints share the latter, and in an in-process benchmark both endpoints
// trace to the same stream.
func traceLane(f *multipathFlow, laneID uint64, stats laneTransportStats) {
	if !laneTrace.Load() {
		return
	}
	window := 0
	if lane := f.laneByID(laneID); lane != nil {
		window = lane.windowBytes()
	}
	c := stats.controller
	var held, queued uint64
	var chunks, ready int
	var flowHeld uint64
	var issued, reissued, sourceBytes, reissuedBytes, laneFailures uint64
	if sched := f.scheduler.Load(); sched != nil {
		chunks, held, queued = sched.LaneOutstanding(laneID)
		ready, flowHeld = sched.Pending()
		st := sched.Stats()
		// issued against source bytes is the check that found a completed chunk
		// being sent twice: a 320-chunk object cannot be issued 481 times.
		issued, reissued = st.ChunksIssued, st.ChunksRetransmit
		sourceBytes, reissuedBytes = st.SourceBytes, st.BytesRetransmit
		laneFailures = st.LaneFailures
	}
	meanResidency, maxResidency := f.takeResidency()
	fmt.Fprintf(os.Stderr,
		"lane t=%.3f flow=%p lane=%d cwnd=%d inflight=%d minrtt=%.1fms srtt=%.1fms pacing=%d maxbw=%d round=%d appsamp=%d nonapp=%d window=%d held=%d queued=%d chunks=%d ready=%d flowheld=%d issued=%d reissued=%d source=%d breissued=%d lanefail=%d resid=%.0fms residmax=%.0fms acksin=%d acksout=%d acksched=%d ackwr=%.0fms sent=%d lost=%d mode=%d\n",
		time.Since(traceStart).Seconds(), f, laneID,
		c.CongestionWindow, c.BytesInFlight,
		float64(c.MinRTT.Microseconds())/1000, float64(stats.smoothedRTT.Microseconds())/1000,
		c.PacingRate, c.MaxBandwidth, c.Round, c.AppSamples, c.NonAppSamples,
		window, held, queued, chunks, ready, flowHeld, issued, reissued, sourceBytes, reissuedBytes, laneFailures,
		float64(meanResidency.Microseconds())/1000, float64(maxResidency.Microseconds())/1000,
		f.acksIn.Swap(0), f.acksOut.Swap(0), f.acksSched.Swap(0), float64(f.ackWriteNS.Swap(0))/1e6,
		stats.bytesSent, stats.packetsLost, c.Mode)
}

// residency records how long a chunk stays in the scheduler's window: from the
// moment a lane takes it to the moment the peer's range acknowledgement frees
// it. It is the denominator of a lane's throughput -- window divided by
// residency -- so it is the number that says whether a window is too small or
// the loop through it is too slow, and no throughput figure distinguishes
// those.
type residency struct {
	mu    sync.Mutex
	total time.Duration
	count int
	max   time.Duration
}

func (f *multipathFlow) observeResidency(d time.Duration) {
	if !laneTrace.Load() {
		return
	}
	f.residency.mu.Lock()
	f.residency.total += d
	f.residency.count++
	if d > f.residency.max {
		f.residency.max = d
	}
	f.residency.mu.Unlock()
}

// takeResidency returns the mean and maximum since the last call.
func (f *multipathFlow) takeResidency() (mean, max time.Duration) {
	f.residency.mu.Lock()
	defer f.residency.mu.Unlock()
	if f.residency.count > 0 {
		mean = f.residency.total / time.Duration(f.residency.count)
	}
	max = f.residency.max
	f.residency.total, f.residency.count, f.residency.max = 0, 0, 0
	return mean, max
}
