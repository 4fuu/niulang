package scheduler

import "testing"

// feedUntilProbe supplies a steady rate until the probe requests a lane,
// covering the warmup and baseline windows. A request left unanswered stalls
// the probe by contract, so callers answer with Confirm or Cancel.
func feedUntilProbe(t *testing.T, p *LaneProbe, rate float64) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if grow, _ := p.Observe(rate); grow {
			return
		}
	}
	t.Fatal("probe never requested a lane at a steady rate")
}

const probeMB float64 = 1024 * 1024

// A flow whose congestion window is still opening raises its rate every
// interval with no lane added. That must not read as a reason to stripe: it is
// the single-lane transport working correctly.
func TestLaneProbeIgnoresCongestionWindowRamp(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	// The opening ramp must be discarded rather than averaged into a baseline:
	// a baseline taken during startup is low, so the next window looks like a
	// large gain and every path appears to reward striping.
	ramp := []float64{2 * probeMB, 4 * probeMB}
	for _, rate := range ramp {
		if grow, _ := p.Observe(rate); grow {
			t.Fatalf("probed during the opening ramp at %.0f B/s", rate)
		}
	}
	// Now at a steady 20MB/s. The baseline must reflect that, not the ramp.
	var grew bool
	for i := 0; i < 3; i++ {
		if g, _ := p.Observe(20 * probeMB); g {
			grew = true
		}
	}
	if !grew {
		t.Fatal("no probe after the ramp settled")
	}
	if p.baseline < 19*probeMB {
		t.Fatalf("baseline %.1fMB/s contaminated by the ramp", p.baseline/probeMB)
	}
}

// The whole point of the default: on a path where an added lane does not raise
// goodput, the search must stop after one probe rather than spending a
// handshake every interval forever.
func TestLaneProbeStopsAfterOneUnrewardedProbe(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	steady := 10 * probeMB
	probes := 0
	for i := 0; i < 40; i++ {
		// A shared bottleneck: the added lane splits the same capacity, so the
		// aggregate rate does not move.
		if grow, _ := p.Observe(steady); grow {
			probes++
			p.Confirm()
		}
	}
	if probes != 1 {
		t.Fatalf("spent %d probes on a path that does not reward striping, want exactly 1", probes)
	}
	if !p.Exhausted() {
		t.Fatal("probe should have retired the search")
	}
}

// On a path that polices each connection, each lane genuinely adds capacity,
// and the search should keep climbing.
func TestLaneProbeKeepsGrowingWhileLanesPay(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	rate := 10 * probeMB
	lanes, probes := 1, 0
	for i := 0; i < 40; i++ {
		grow, _ := p.Observe(rate)
		if grow && lanes < 4 { // the shipped ceiling
			probes++
			p.Confirm()
			lanes++
			rate = float64(lanes) * 10 * probeMB // per-flow policing: linear
		}
	}
	if probes < 3 {
		t.Fatalf("only %d probes on a path where every lane paid for itself", probes)
	}
}

// Emergent, and worth stating: because the bar is a fraction of the current
// rate, even perfectly linear scaling stops once one more lane out of n is
// worth less than MinGain -- around seven lanes at the default 15%. The
// configured ceiling binds first, but the search is self-limiting regardless.
func TestLaneProbeSelfLimitsOnDiminishingRelativeGain(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	rate := 10 * probeMB
	lanes := 1
	for i := 0; i < 200 && !p.Exhausted(); i++ {
		if grow, _ := p.Observe(rate); grow {
			p.Confirm()
			lanes++
			rate = float64(lanes) * 10 * probeMB
		}
	}
	if !p.Exhausted() {
		t.Fatal("search never converged under linear scaling")
	}
	if lanes < 5 || lanes > 9 {
		t.Fatalf("converged at %d lanes, want the ~1/MinGain neighbourhood", lanes)
	}
}

// A lane that costs goodput must be reported as a negative verdict so the
// caller can suppress further growth, not merely as "no gain".
func TestLaneProbeReportsNegativeVerdict(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	steady := 10 * probeMB
	feedUntilProbe(t, p, steady)
	p.Confirm()
	var gain float64
	for i := 0; i < 6; i++ {
		// Striping made it worse, as it does when lanes share one bottleneck
		// and the extra congestion controllers amplify loss.
		if _, g := p.Observe(5 * probeMB); g != 0 {
			gain = g
			break
		}
	}
	if gain >= 0 {
		t.Fatalf("verdict %.2f, want a negative gain reported for a harmful lane", gain)
	}
}

// The baseline is the best rate seen at the current lane count, so a probe
// cannot be credited for merely recovering from a trough.
func TestLaneProbeComparesWindowMeans(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	// Averaging both sides is what makes the comparison meaningful under noise:
	// a swing that would dominate any single-sample comparison must not decide
	// the verdict.
	p.Observe(10 * probeMB) // warmup samples, discarded
	p.Observe(10 * probeMB)
	noisy := []float64{6 * probeMB, 14 * probeMB, 10 * probeMB} // mean 10
	for _, r := range noisy {
		p.Observe(r)
	}
	if !p.pending {
		t.Fatal("expected a probe once a baseline window completed")
	}
	if spread := relDiff(p.baseline, 10*probeMB); spread > 0.01 {
		t.Fatalf("baseline %.1fMB/s is not the window mean", p.baseline/probeMB)
	}
	p.Confirm()
	for i := 0; i < 2; i++ {
		p.Observe(30 * probeMB) // settle samples: discarded, not credited
	}
	var gain float64
	for _, r := range []float64{4 * probeMB, 16 * probeMB, 10 * probeMB} { // mean 10 again
		if _, g := p.Observe(r); g != 0 {
			gain = g
		}
	}
	if gain > 0.02 || gain < -0.02 {
		t.Fatalf("verdict %.2f, want ~0 when both window means are equal", gain)
	}
	if !p.Exhausted() {
		t.Fatal("a probe that did not clear the bar should retire the search")
	}
}

func relDiff(a, b float64) float64 {
	d := (a - b) / b
	if d < 0 {
		return -d
	}
	return d
}

// A stalled interval must not become a baseline, or the next real sample reads
// as an enormous gain and every path looks like it rewards striping.
func TestLaneProbeIgnoresStalledIntervals(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	for i := 0; i < 6; i++ {
		p.Observe(0)
	}
	if p.pending {
		t.Fatal("probed on the strength of stalled intervals")
	}
	if grow, gain := p.Observe(10 * probeMB); grow || gain != 0 {
		t.Fatalf("grow=%v gain=%.2f after a stall, want no growth signal", grow, gain)
	}
}

// The defect this guards: a probe request the caller cannot serve -- a policy
// veto, an exhausted lane budget, a peer that refused the join -- must not be
// judged. Measuring n lanes against n lanes yields noise, and one unlucky
// sample would retire the search on a path that rewards striping.
func TestLaneProbeCancelledRequestIsNotJudged(t *testing.T) {
	p := NewLaneProbe(ProbeConfig{})
	steady := 10 * probeMB
	feedUntilProbe(t, p, steady)
	p.Cancel() // the caller could not open the lane

	// Samples that would have looked like a catastrophic verdict if the probe
	// had assumed its request was served.
	for i := 0; i < 6; i++ {
		grow, gain := p.Observe(4 * probeMB)
		if gain != 0 {
			t.Fatalf("judged a lane that was never opened (gain %.2f)", gain)
		}
		if grow {
			p.Cancel() // the caller keeps declining
		}
	}
	if p.Exhausted() {
		t.Fatal("retired the search over a request that was never served")
	}
	// And it must still be willing to ask again once the rate settles.
	feedUntilProbe(t, p, steady)
}
