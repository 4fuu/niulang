package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func trial(stack string, flows int, mbits float64, complete bool) TrialRecord {
	return TrialRecord{Stack: stack, Flows: flows, MbitsPerSec: mbits, Complete: complete}
}

// A median taken over completed trials alone rewards a transport for giving
// up. In a 35%-burst-loss block the reference completed 7 of 12 trials and
// wanopt 10 of 12, and reporting only successes made the transport that
// finished the hard trials look slower than the one that abandoned them. The
// headline statistic therefore has to include failures at their partial rate.
func TestSummaryCountsFailedTrials(t *testing.T) {
	trials := []TrialRecord{
		// Gives up on the hard trials, but is fast on the easy ones.
		trial("baseline", 1, 8, true), trial("baseline", 1, 8, true),
		trial("baseline", 1, 0.2, false), trial("baseline", 1, 0.2, false),
		// Finishes everything, more slowly on the hard trials.
		trial("wanopt", 1, 7, true), trial("wanopt", 1, 7, true),
		trial("wanopt", 1, 3, true), trial("wanopt", 1, 3, true),
	}
	summaries := summarize(trials)
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want one per stack", len(summaries))
	}
	byStack := map[string]CellSummary{}
	for _, s := range summaries {
		byStack[s.Stack] = s
	}
	reference, subject := byStack["baseline"], byStack["wanopt"]
	if reference.MedianCompleteMbits <= subject.MedianCompleteMbits {
		t.Fatal("test data no longer reproduces the misleading completed-only comparison")
	}
	if subject.MedianMbits <= reference.MedianMbits {
		t.Fatalf("all-trial median: wanopt %.2f, reference %.2f; the transport that "+
			"finished every trial must not rank lower", subject.MedianMbits, reference.MedianMbits)
	}
	if reference.CompletionRate != 0.5 || subject.CompletionRate != 1 {
		t.Fatalf("completion rates = %.2f and %.2f, want 0.5 and 1", reference.CompletionRate, subject.CompletionRate)
	}
	if reference.WorstMbits != 0.2 {
		t.Fatalf("worst = %.2f, want the worst trial including failures", reference.WorstMbits)
	}
}

func TestGateFailsOnGoodputShortfall(t *testing.T) {
	summaries := summarize([]TrialRecord{
		trial("baseline", 1, 10, true), trial("baseline", 1, 10, true),
		trial("wanopt", 1, 8, true), trial("wanopt", 1, 8, true),
	})
	if err := gateReport(summaries, 0.10); err == nil {
		t.Fatal("a 20 percent shortfall passed a 10 percent tolerance")
	}
	if err := gateReport(summaries, 0.30); err != nil {
		t.Fatalf("a 20 percent shortfall failed a 30 percent tolerance: %v", err)
	}
}

// Completing fewer transfers is a regression even at identical goodput, so the
// gate must not be satisfiable by giving up on the hard trials.
func TestGateFailsOnCompletionShortfall(t *testing.T) {
	summaries := summarize([]TrialRecord{
		trial("baseline", 1, 10, true), trial("baseline", 1, 10, true),
		trial("wanopt", 1, 10, true), trial("wanopt", 1, 10, false),
	})
	if err := gateReport(summaries, 0.50); err == nil {
		t.Fatal("a completion-rate regression passed the gate")
	}
}

func TestGateRequiresBothStacks(t *testing.T) {
	summaries := summarize([]TrialRecord{trial("wanopt", 1, 10, true)})
	err := gateReport(summaries, 0.10)
	if err == nil || !strings.Contains(err.Error(), "both stacks") {
		t.Fatalf("gate error = %v, want a clear complaint about the missing reference", err)
	}
}

func TestGatePassesAtParity(t *testing.T) {
	summaries := summarize([]TrialRecord{
		trial("baseline", 1, 10, true), trial("baseline", 4, 20, true),
		trial("wanopt", 1, 10.5, true), trial("wanopt", 4, 19.5, true),
	})
	if err := gateReport(summaries, 0.10); err != nil {
		t.Fatalf("parity failed the gate: %v", err)
	}
}

func TestReportRoundTripsAsJSON(t *testing.T) {
	report := Report{
		Path:   PathReport{RTTMillis: 200, LossPercent: 1, RateMbits: 100},
		Trials: []TrialRecord{trial("wanopt", 1, 10, true)},
	}
	report.Summary = summarize(report.Trials)
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(path, report); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(path, report); err != nil {
		t.Fatalf("rewriting an existing report: %v", err)
	}
}

func TestMedianHandlesEvenAndEmpty(t *testing.T) {
	if got := median(nil); got != 0 {
		t.Fatalf("median of nothing = %v, want 0", got)
	}
	if got := median([]float64{1, 3}); got != 2 {
		t.Fatalf("median of two values = %v, want their midpoint", got)
	}
	if got := median([]float64{1, 2, 3}); got != 2 {
		t.Fatalf("median of three values = %v, want 2", got)
	}
}
