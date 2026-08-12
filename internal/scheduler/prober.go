package scheduler

// LaneProbe decides when a bulk flow should try one more lane, by running a
// controlled experiment rather than reacting to rate changes.
//
// The distinction matters. Comparing this tick's goodput with the previous
// tick's measures whatever the path happened to do — a flow whose congestion
// window is still opening shows a large positive change with no lane added,
// and ordinary variance on a lossy path shows a negative one. Growing on that
// signal is a random walk that spends handshakes on paths that cannot reward
// them, which is strictly worse than never growing at all.
//
// So a probe here is an A/B test with a held baseline:
//
//	wait for the rate to settle at the current lane count   -> baseline
//	add one lane, wait for it to handshake and ramp         -> settle
//	compare                                                 -> keep, or stop
//
// The comparison is deliberately biased against striping. The baseline is the
// best rate observed at the current lane count, so a probe must beat what the
// flow was already achieving at its best, not its worst; and a probe that
// fails to clear the bar retires the search permanently for that flow rather
// than retrying. A path that does not reward striping therefore pays for
// exactly one probe, and a wrong answer costs one connection instead of a
// persistent regression against a single-lane transport.
type probeState int

const (
	probeWarmup   probeState = iota // discard the congestion window's opening ramp
	probeBaseline                   // measure the rate at the current lane count
	probeSettle                     // let a newly added lane handshake and ramp
	probeMeasure                    // measure the rate with that lane carrying traffic
)

type LaneProbe struct {
	cfg       ProbeConfig
	state     probeState
	skip      int
	window    []float64
	baseline  float64
	pending   bool
	awaited   int
	exhausted bool
	verdict   float64
}

// ProbeConfig bounds the experiment. The sample counts are in decision
// intervals.
type ProbeConfig struct {
	// MinGain is the fractional improvement a probed lane must produce to be
	// kept. It sits well above the sampling error of a WindowSamples-long mean
	// on purpose: the cost of a false positive (a persistent extra connection
	// that does not pay) is higher than the cost of a false negative (behaving
	// like a single-lane transport, which is the status quo).
	MinGain float64
	// WindowSamples is how many samples are averaged into each side of the
	// comparison. Individual interval samples are far too noisy to compare --
	// on a policed 25 Mbit/s path they range over 7 to 32 Mbit/s -- so the
	// experiment compares window means, which cuts the standard error by the
	// square root of this count.
	WindowSamples int
	// WarmupSamples are discarded before the first baseline, so the congestion
	// window's opening ramp is not averaged into it and credited to a lane.
	WarmupSamples int
	// SettleSamples are discarded after a lane is added, covering its handshake
	// and ramp before its contribution is measured.
	SettleSamples int
	// MinGoodput is the rate below which a sample is treated as a stall and
	// excluded, rather than dragging a window mean towards zero.
	MinGoodput float64
	// AwaitLimit bounds how many decisions a requested lane may stay
	// unanswered before the request is treated as declined.
	AwaitLimit int
}

// DefaultProbeConfig returns bounds tuned for a 500ms decision interval, which
// puts the first verdict about 5s after a flow is classified as bulk. That
// latency is the price of measuring rather than assuming: a transfer shorter
// than a few seconds stays on one lane, and a long one converges.
func DefaultProbeConfig() ProbeConfig {
	return ProbeConfig{
		MinGain:       0.15,
		WindowSamples: 3,
		WarmupSamples: 2,
		SettleSamples: 2,
		MinGoodput:    64 * 1024,
		AwaitLimit:    30,
	}
}

// NewLaneProbe returns a probe using cfg, falling back to defaults for unset
// fields.
func NewLaneProbe(cfg ProbeConfig) *LaneProbe {
	d := DefaultProbeConfig()
	if cfg.MinGain <= 0 {
		cfg.MinGain = d.MinGain
	}
	if cfg.WindowSamples <= 0 {
		cfg.WindowSamples = d.WindowSamples
	}
	if cfg.WarmupSamples <= 0 {
		cfg.WarmupSamples = d.WarmupSamples
	}
	if cfg.SettleSamples <= 0 {
		cfg.SettleSamples = d.SettleSamples
	}
	if cfg.MinGoodput <= 0 {
		cfg.MinGoodput = d.MinGoodput
	}
	if cfg.AwaitLimit <= 0 {
		cfg.AwaitLimit = d.AwaitLimit
	}
	return &LaneProbe{cfg: cfg, skip: cfg.WarmupSamples}
}

// Observe records one interval goodput sample in bytes per second and reports
// whether the flow should add a lane now, along with the measured gain of the
// most recently completed probe. The gain is zero except on the decision that
// concludes a probe, so a caller can treat a negative value as evidence that
// striping harmed this path.
//
// A true return is a request, not a fact. The caller must answer it with
// Confirm or Cancel before the next Observe.
func (p *LaneProbe) Observe(goodput float64) (grow bool, gain float64) {
	if p.exhausted {
		return false, 0
	}
	if p.pending {
		// Opening a lane is not instantaneous -- on a saturated path the
		// handshake queues behind the flow's own data and can take seconds --
		// so the caller answers asynchronously. Samples taken while the answer
		// is outstanding belong to neither side of the comparison and are
		// discarded. A request that is never answered must not wedge the
		// search, so silence eventually counts as a decline.
		p.awaited++
		if p.awaited > p.cfg.AwaitLimit {
			p.Cancel()
		}
		return false, 0
	}
	if goodput < p.cfg.MinGoodput {
		// A stalled interval says nothing about lane count. Averaging it in
		// would understate whichever side of the comparison it landed on.
		return false, 0
	}
	if p.skip > 0 {
		p.skip--
		return false, 0
	}
	p.window = append(p.window, goodput)
	if len(p.window) < p.cfg.WindowSamples {
		return false, 0
	}
	mean := windowMean(p.window)
	p.window = p.window[:0]
	switch p.state {
	case probeWarmup, probeBaseline:
		p.baseline = mean
		p.pending = true
		return true, 0
	case probeMeasure:
		measured := 0.0
		if p.baseline > 0 {
			measured = (mean - p.baseline) / p.baseline
		}
		p.verdict = measured
		if measured >= p.cfg.MinGain {
			// The lane paid for itself. Hold the new plateau as the baseline
			// and immediately test whether another one does too.
			p.baseline = mean
			p.pending = true
			return true, measured
		}
		// This path does not reward striping. Stop spending handshakes on it.
		p.exhausted = true
		return false, measured
	}
	return false, 0
}

// Confirm reports that the lane the probe asked for was actually opened, which
// starts the settle-and-measure cycle.
//
// Nothing may judge a lane that does not exist. A caller can decline the
// request for reasons that have nothing to do with the path -- a policy veto, a
// lane budget, a peer that refused the join -- and if the probe assumed its
// request was honoured it would compare n lanes against n lanes, read the
// difference as noise, and retire the search on a path that would have
// rewarded it.
func (p *LaneProbe) Confirm() {
	if !p.pending {
		return
	}
	p.pending, p.awaited = false, 0
	p.state = probeSettle
	p.skip = p.cfg.SettleSamples
	p.window = p.window[:0]
	// The settle samples are discarded, and the window that follows is the
	// measurement.
	p.state = probeMeasure
}

// Cancel reports that the requested lane was not opened. The probe re-measures
// its baseline and may ask again; the unserved request is not evidence.
func (p *LaneProbe) Cancel() {
	if !p.pending {
		return
	}
	p.pending, p.awaited = false, 0
	p.state = probeBaseline
	p.window = p.window[:0]
}

// Exhausted reports whether the probe has concluded that this path does not
// reward more lanes.
func (p *LaneProbe) Exhausted() bool { return p.exhausted }

// Verdict returns the gain measured by the last completed probe.
func (p *LaneProbe) Verdict() float64 { return p.verdict }

// Stop retires the search, so a caller that learns growth is unsafe for other
// reasons does not keep paying for probes.
func (p *LaneProbe) Stop() { p.exhausted = true }

func windowMean(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	total := 0.0
	for _, s := range samples {
		total += s
	}
	return total / float64(len(samples))
}
