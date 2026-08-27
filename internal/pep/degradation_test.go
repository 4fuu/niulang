package pep

import (
	"testing"
	"time"
)

func TestDegradationDetectorRequiresSustainedDifferentialEvidence(t *testing.T) {
	start := time.Unix(1, 0)
	detector := degradationDetector{}
	snapshot := flowSnapshot{Bytes: 0, CurrentRTT: 100 * time.Millisecond}
	if _, switched := detector.observe(start, snapshot, true); switched {
		t.Fatal("first sample switched")
	}
	// Teach the detector this flow's own healthy rate: 1 MiB/s.
	for i := 1; i <= 6; i++ {
		snapshot.Bytes += 512 * 1024
		if _, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, true); switched {
			t.Fatal("healthy traffic switched")
		}
	}
	// One lossy second is a burst, not a carrier decision.
	snapshot.Erasure = 0.80
	for i := 7; i <= 8; i++ {
		snapshot.Bytes += 32 * 1024
		if _, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, true); switched {
			t.Fatal("transient loss switched")
		}
	}
	// Recovery clears the pending evidence.
	snapshot.Erasure = 0.05
	snapshot.Bytes += 512 * 1024
	if _, switched := detector.observe(start.Add(4500*time.Millisecond), snapshot, true); switched {
		t.Fatal("recovered path switched")
	}
	// A later sustained 80% episode crosses the two-second hysteresis.
	snapshot.Erasure = 0.80
	for i := 10; i <= 15; i++ {
		snapshot.Bytes += 32 * 1024
		decision, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, true)
		if switched {
			if decision.reason != "sustained_erasure_and_rate_collapse" || decision.erasure != 0.80 {
				t.Fatalf("decision = %+v", decision)
			}
			return
		}
	}
	t.Fatal("sustained degradation did not switch")
}

func TestDegradationDetectorRequiresHealthyTCP(t *testing.T) {
	start := time.Unix(1, 0)
	detector := degradationDetector{}
	snapshot := flowSnapshot{CurrentRTT: 50 * time.Millisecond}
	detector.observe(start, snapshot, false)
	for i := 1; i <= 12; i++ {
		snapshot.Bytes += 512 * 1024
		detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, false)
	}
	snapshot.Erasure = 0.95
	for i := 13; i <= 24; i++ {
		if _, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, false); switched {
			t.Fatal("degraded QUIC switched without positive TCP evidence")
		}
	}
}

func TestDegradationDetectorTreatsSustainedHighErasureAsDirectEvidence(t *testing.T) {
	start := time.Unix(1, 0)
	detector := degradationDetector{}
	snapshot := flowSnapshot{CurrentRTT: 50 * time.Millisecond}
	detector.observe(start, snapshot, true)
	for i := 1; i <= 6; i++ {
		snapshot.Bytes += 512 * 1024
		detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, true)
	}
	// The application can retain more than half its rate through FEC even
	// while the carrier erases enough packets that proven-clean TCP is the
	// materially safer path. Direct evidence therefore does not also require
	// rate collapse once erasure crosses the high threshold.
	snapshot.Erasure = degradationDirectErasure + 0.01
	for i := 7; i <= 12; i++ {
		snapshot.Bytes += 512 * 1024
		decision, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, true)
		if switched {
			if decision.reason != "sustained_severe_erasure" {
				t.Fatalf("reason = %q", decision.reason)
			}
			return
		}
	}
	t.Fatal("sustained high erasure did not switch")
}

func TestDegradationDetectorRetainsEvidenceWhileStandbyReplenishes(t *testing.T) {
	start := time.Unix(1, 0)
	detector := degradationDetector{}
	snapshot := flowSnapshot{CurrentRTT: 50 * time.Millisecond}
	detector.observe(start, snapshot, false)
	for i := 1; i <= 6; i++ {
		snapshot.Bytes += 512 * 1024
		detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, false)
	}
	snapshot.Erasure = 0.90
	for i := 7; i <= 12; i++ {
		snapshot.Bytes += 16 * 1024
		if _, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, false); switched {
			t.Fatal("degraded QUIC switched without a healthy standby")
		}
	}
	// Another flow may have claimed the one hot connection while this flow
	// accumulated evidence. Once the manager replenishes it, this flow should
	// not pay a second observation window for the same shared-path event.
	snapshot.Bytes += 16 * 1024
	decision, switched := detector.observe(start.Add(6500*time.Millisecond), snapshot, true)
	if !switched || decision.observedFor < degradationMinWindow {
		t.Fatalf("replenished standby decision = %+v, switched = %t", decision, switched)
	}
}

func TestDegradationDetectorFindsACompleteBlackout(t *testing.T) {
	start := time.Unix(1, 0)
	detector := degradationDetector{}
	snapshot := flowSnapshot{CurrentRTT: 400 * time.Millisecond, BytesInFlight: 128 * 1024}
	detector.observe(start, snapshot, true)
	for i := 1; i <= 12; i++ {
		decision, switched := detector.observe(start.Add(time.Duration(i)*500*time.Millisecond), snapshot, true)
		if switched {
			if decision.reason != "sustained_no_progress" {
				t.Fatalf("reason = %q", decision.reason)
			}
			return
		}
	}
	t.Fatal("complete blackout did not switch")
}

func TestUDPPathProbeRequiresSustainedSilenceAndClearsOnEcho(t *testing.T) {
	start := time.Unix(1, 0)
	state := udpPathProbeState{}
	if !state.start(start) {
		t.Fatal("first probe was not started")
	}
	if state.start(start.Add(laneDecisionInterval)) {
		t.Fatal("a second probe started while the first was in flight")
	}
	if age, stalled := state.stalled(start.Add(time.Second), degradationMinWindow); stalled || age != time.Second {
		t.Fatalf("one-second silence: age=%v stalled=%t", age, stalled)
	}
	state.finish(false, time.Second, laneDecisionInterval)
	if age, stalled := state.stalled(start.Add(degradationMinWindow), degradationMinWindow); !stalled || age != degradationMinWindow {
		t.Fatalf("sustained silence: age=%v stalled=%t", age, stalled)
	}
	if !state.start(start.Add(degradationMinWindow)) {
		t.Fatal("retry probe was not started")
	}
	state.finish(true, 100*time.Millisecond, laneDecisionInterval)
	if age, stalled := state.stalled(start.Add(3*time.Second), degradationMinWindow); stalled || age != 0 {
		t.Fatalf("echo did not clear pending evidence: age=%v stalled=%t", age, stalled)
	}
}

func TestUDPPathProbeTreatsSlowEchoAsDegradationEvidence(t *testing.T) {
	start := time.Unix(1, 0)
	state := udpPathProbeState{}
	state.start(start)
	state.finish(true, 600*time.Millisecond, 500*time.Millisecond)
	if age, stalled := state.stalled(start.Add(degradationMinWindow), degradationMinWindow); !stalled || age != degradationMinWindow {
		t.Fatalf("slow echo cleared evidence: age=%v stalled=%t", age, stalled)
	}
}

func TestSustainedDegradationEntersUDPCooldownImmediately(t *testing.T) {
	now := time.Unix(1, 0)
	health := newUDPHealth(3, time.Minute)
	health.degraded(now)
	if health.allow(now.Add(30 * time.Second)) {
		t.Fatal("sustained degradation did not enter cooldown")
	}
	if !health.allow(now.Add(time.Minute)) {
		t.Fatal("sustained degradation outlived its cooldown")
	}
}
