package pep

import (
	"testing"
	"time"
)

// A storm of attempts must leave one readable record rather than one line per
// attempt, and that record has to carry the size of the storm it stands for.
func TestEnrollmentLogSuppressesStormAndKeepsTotals(t *testing.T) {
	var log enrollmentLog
	start := time.Unix(1700000000, 0)
	write, suppressed, total := log.due("rejected", start)
	if !write || suppressed != 0 || total != 1 {
		t.Fatalf("first record write=%t suppressed=%d total=%d", write, suppressed, total)
	}
	for i := 1; i < 50; i++ {
		if write, _, _ := log.due("rejected", start.Add(time.Duration(i)*time.Millisecond)); write {
			t.Fatalf("attempt %d wrote inside the interval", i)
		}
	}
	write, suppressed, total = log.due("rejected", start.Add(enrollmentRecordInterval))
	if !write {
		t.Fatal("no record written after the interval elapsed")
	}
	if suppressed != 49 {
		t.Fatalf("suppressed=%d; want 49", suppressed)
	}
	if total != 51 {
		t.Fatalf("total=%d; want 51", total)
	}
}

// Outcomes are rate-limited apart, so an outage that repeats every second
// cannot bury the single rejection an operator is looking for.
func TestEnrollmentLogSeparatesOutcomes(t *testing.T) {
	var log enrollmentLog
	now := time.Unix(1700000000, 0)
	if write, _, _ := log.due("store_unavailable", now); !write {
		t.Fatal("first outage record was suppressed")
	}
	if write, _, _ := log.due("store_unavailable", now.Add(time.Second)); write {
		t.Fatal("second outage record inside the interval was written")
	}
	if write, _, total := log.due("rejected", now.Add(time.Second)); !write || total != 1 {
		t.Fatalf("a different outcome was suppressed by the outage: write=%t total=%d", write, total)
	}
	if write, _, _ := log.due(admissionRefused, now.Add(time.Second)); !write {
		t.Fatal("admission refusals share a budget with enrollment outcomes")
	}
}
