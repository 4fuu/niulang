// Package mpcc implements coupled congestion control for a flow striped over
// several lanes, following the Linked Increase Algorithm of RFC 6356.
//
// Independent congestion control per lane is the obvious arrangement and it is
// wrong. Each lane here is a QUIC connection with its own controller, so four
// lanes over one bottleneck claim four shares, overshoot together, and drive
// each other into loss. Measured on a 50 MiB transfer over an emulated shared
// path, that configuration lost a transfer outright and its worst trial ran at
// a quarter of a single lane's throughput. This is the problem MPTCP solved.
//
// The instrument is a single window W shared by every lane. It grows in
// proportion to what the whole flow achieves and shrinks when any lane reports
// congestion, which gives the three properties the algorithm was designed for:
//
//  1. the flow does at least as well as it would on its best single lane;
//  2. on a shared bottleneck it takes no more than a single flow would;
//  3. traffic moves off congested lanes onto uncongested ones.
//
// Property 2 is the one that makes striping safe to enable. Property 1 is what
// protects the common case, where striping has nothing to offer and must
// therefore cost nothing.
//
// W does not replace the lanes' own congestion control, which still paces the
// wire. It bounds what the application commits across lanes -- one window over
// several controllers, which is the arrangement MP-RDMA uses. Nesting a second
// controller under the first would be a mistake.
package mpcc

import (
	"math"
	"sync"
	"time"
)

const (
	// segment is the unit the algorithm counts in. RFC 6356 is written in
	// packets; working in bytes needs a packet size, and this is QUIC's
	// conservative datagram size.
	segment = 1200.0
	// decreaseFactor halves the window on congestion, as Reno and LIA do.
	decreaseFactor = 0.5
)

// Config bounds the window.
type Config struct {
	// Initial is the window before anything is known about the path.
	Initial int
	// Min is the floor. A flow must always be able to make progress, or a
	// single congestion episode could stall it permanently.
	Min int
	// Max caps the flow's total commitment, bounding memory and the damage a
	// runaway estimate could do.
	Max int
	// Now is the clock, for tests.
	Now func() time.Time
}

// DefaultConfig returns bounds for a long-haul path.
func DefaultConfig() Config {
	return Config{
		Initial: 256 * 1024,
		Min:     64 * 1024,
		Max:     32 * 1024 * 1024,
	}
}

func (c *Config) applyDefaults() {
	d := DefaultConfig()
	if c.Initial <= 0 {
		c.Initial = d.Initial
	}
	if c.Min <= 0 {
		c.Min = d.Min
	}
	if c.Max <= 0 {
		c.Max = d.Max
	}
	if c.Max < c.Min {
		c.Max = c.Min
	}
	if c.Initial < c.Min {
		c.Initial = c.Min
	}
	if c.Initial > c.Max {
		c.Initial = c.Max
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// laneWindow is one lane's congestion window, in bytes. These are the windows
// RFC 6356 couples; the flow's window is their sum.
type laneWindow struct {
	w            float64
	rtt          time.Duration
	lastDecrease time.Time
}

// Window holds a flow's coupled congestion state.
//
// Coupling lives in the increase and nowhere else. A lane that loses packets
// halves its own window, exactly as an ordinary connection would; what the
// coupling changes is how fast each lane is allowed to grow, so that the
// aggregate over a shared bottleneck matches one flow. Halving the whole
// flow's window whenever any lane reported loss -- the obvious shortcut -- is
// wrong in a way that only shows up with several healthy lanes: each lane's
// ordinary sawtooth then knocks down every other lane's window too, so N
// independently policed lanes are punished N times as often as one and the
// flow never grows into the capacity that striping exists to reach.
type Window struct {
	cfg Config

	mu    sync.Mutex
	lanes map[uint64]*laneWindow
}

// New returns a coupled window.
func New(cfg Config) *Window {
	cfg.applyDefaults()
	return &Window{cfg: cfg, lanes: make(map[uint64]*laneWindow)}
}

// Observe records a lane's round-trip time, which the coupling factor needs.
// A lane is created on first sight with the initial window.
func (w *Window) Observe(laneID uint64, rtt time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	lane := w.laneLocked(laneID)
	if rtt > 0 {
		lane.rtt = rtt
	}
}

func (w *Window) laneLocked(laneID uint64) *laneWindow {
	lane, ok := w.lanes[laneID]
	if !ok {
		lane = &laneWindow{w: float64(w.cfg.Initial)}
		w.lanes[laneID] = lane
	}
	return lane
}

// Forget drops a lane that has gone away, so its stale window stops counting
// towards the flow's total and towards the coupling factor.
func (w *Window) Forget(laneID uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.lanes, laneID)
}

// Acked grows a lane's window for bytes acknowledged on it.
//
// The increase is the smaller of two terms. The coupled term shares one flow's
// worth of aggressiveness across every lane, so lanes over a shared bottleneck
// do not add aggregate load. The second term is what an ordinary connection on
// that lane alone would do, and capping by it stops the coupling from ever
// making a lane more aggressive than a single connection would be.
func (w *Window) Acked(laneID uint64, bytes int) {
	if bytes <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	lane := w.laneLocked(laneID)
	if lane.w <= 0 {
		lane.w = float64(w.cfg.Min)
	}
	total := w.totalLocked()
	if total <= 0 {
		total = lane.w
	}
	b := float64(bytes)
	coupled := w.alphaLocked(total) * b * segment / total
	uncoupled := b * segment / lane.w
	increase := math.Min(coupled, uncoupled)
	if increase <= 0 || math.IsNaN(increase) || math.IsInf(increase, 0) {
		return
	}
	lane.w = math.Min(lane.w+increase, float64(w.cfg.Max))
}

// Congestion halves one lane's window when that lane reports loss or recovery.
//
// Decreases are rate-limited per lane to one per round trip. A loss episode is
// reported by every packet the lane loses, and halving once per report would
// collapse the window on a single burst -- on a path with correlated loss that
// turns one event into a stall.
func (w *Window) Congestion(laneID uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	lane := w.laneLocked(laneID)
	now := w.cfg.Now()
	guard := lane.rtt
	if guard <= 0 {
		guard = 100 * time.Millisecond
	}
	if !lane.lastDecrease.IsZero() && now.Sub(lane.lastDecrease) < guard {
		return
	}
	lane.lastDecrease = now
	lane.w = math.Max(lane.w*decreaseFactor, float64(w.cfg.Min))
}

// alphaLocked computes the coupling factor of RFC 6356:
//
//	alpha = W * max_i(w_i / rtt_i^2) / (sum_i(w_i / rtt_i))^2
//
// It is what makes the flow's aggregate increase match what a single connection
// would achieve on the best available lane, which is the whole basis of the
// fairness property.
func (w *Window) alphaLocked(total float64) float64 {
	var maxTerm, sumTerm float64
	for _, lane := range w.lanes {
		if lane.w <= 0 || lane.rtt <= 0 {
			continue
		}
		rtt := lane.rtt.Seconds()
		if term := lane.w / (rtt * rtt); term > maxTerm {
			maxTerm = term
		}
		sumTerm += lane.w / rtt
	}
	if maxTerm <= 0 || sumTerm <= 0 {
		// Nothing usable is known yet. Behaving like a single unmodified
		// connection is the safe default: it cannot be unfair, only slow.
		return 1
	}
	alpha := total * maxTerm / (sumTerm * sumTerm)
	if math.IsNaN(alpha) || math.IsInf(alpha, 0) || alpha <= 0 {
		return 1
	}
	return alpha
}

func (w *Window) totalLocked() float64 {
	var total float64
	for _, lane := range w.lanes {
		total += lane.w
	}
	return total
}

// Total returns the flow's window: how many unacknowledged bytes it may have
// outstanding across every lane.
func (w *Window) Total() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := w.totalLocked()
	if total < float64(w.cfg.Min) {
		return w.cfg.Min
	}
	return int(total)
}

// Lane returns one lane's window: how many unacknowledged bytes that lane may
// hold. It is the admission limit that keeps the sender from committing bytes
// to a lane the path cannot move.
func (w *Window) Lane(laneID uint64) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	lane, ok := w.lanes[laneID]
	if !ok {
		return w.cfg.Initial
	}
	if lane.w < float64(w.cfg.Min) {
		return w.cfg.Min
	}
	return int(lane.w)
}
