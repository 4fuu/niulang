package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/pathsim"
)

func trial(stack string, flows int, mbits float64, complete bool) TrialRecord {
	return TrialRecord{Stack: stack, Flows: flows, MbitsPerSec: mbits, Complete: complete}
}

// A median taken over completed trials alone rewards a transport for giving
// up. In a 35%-burst-loss block the reference completed 7 of 12 trials and
// queqiao 10 of 12, and reporting only successes made the transport that
// finished the hard trials look slower than the one that abandoned them. The
// headline statistic therefore has to include failures at their partial rate.
func TestSummaryCountsFailedTrials(t *testing.T) {
	trials := []TrialRecord{
		// Gives up on the hard trials, but is fast on the easy ones.
		trial("baseline", 1, 8, true), trial("baseline", 1, 8, true),
		trial("baseline", 1, 0.2, false), trial("baseline", 1, 0.2, false),
		// Finishes everything, more slowly on the hard trials.
		trial("queqiao", 1, 7, true), trial("queqiao", 1, 7, true),
		trial("queqiao", 1, 3, true), trial("queqiao", 1, 3, true),
	}
	summaries := summarize(trials)
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want one per stack", len(summaries))
	}
	byStack := map[string]CellSummary{}
	for _, s := range summaries {
		byStack[s.Stack] = s
	}
	reference, subject := byStack["baseline"], byStack["queqiao"]
	if reference.MedianCompleteMbits <= subject.MedianCompleteMbits {
		t.Fatal("test data no longer reproduces the misleading completed-only comparison")
	}
	if subject.MedianMbits <= reference.MedianMbits {
		t.Fatalf("all-trial median: queqiao %.2f, reference %.2f; the transport that "+
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
		trial("queqiao", 1, 8, true), trial("queqiao", 1, 8, true),
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
		trial("queqiao", 1, 10, true), trial("queqiao", 1, 10, false),
	})
	if err := gateReport(summaries, 0.50); err == nil {
		t.Fatal("a completion-rate regression passed the gate")
	}
}

func TestGateRequiresBothStacks(t *testing.T) {
	summaries := summarize([]TrialRecord{trial("queqiao", 1, 10, true)})
	err := gateReport(summaries, 0.10)
	if err == nil || !strings.Contains(err.Error(), "both stacks") {
		t.Fatalf("gate error = %v, want a clear complaint about the missing reference", err)
	}
}

func TestGatePassesAtParity(t *testing.T) {
	summaries := summarize([]TrialRecord{
		trial("baseline", 1, 10, true), trial("baseline", 4, 20, true),
		trial("queqiao", 1, 10.5, true), trial("queqiao", 4, 19.5, true),
	})
	if err := gateReport(summaries, 0.10); err != nil {
		t.Fatalf("parity failed the gate: %v", err)
	}
}

func TestReportRoundTripsAsJSON(t *testing.T) {
	report := Report{
		SchemaVersion: 1,
		Arguments:     []string{"--rtt", "200", "--seed", "7"},
		Path: PathReport{
			RTTMillis: 200, LossPercent: 1, RateMbits: 100, Seed: 7,
			PolicerRefillMillis: 8, PolicerBurstBytes: 4000,
		},
		Trials: []TrialRecord{{
			Stack: "queqiao", Flows: 1, MbitsPerSec: 10, Complete: true,
			PathCounters: &PathCountersReport{
				Downstream: DirectionCountersReport{PacketsIn: 100, PacketsOut: 80, BottleneckDropped: 20},
			},
		}},
		Latency: []LatencyRecord{{Stack: "queqiao", Trial: 1, ColdMillis: 210, WarmMillis: 201, Complete: true}},
		UDP: []UDPRecord{{
			Stack: "queqiao", Trial: 1, Sent: 100, Received: 97, Lost: 3,
			DeliveryPercent: 97, P50Millis: 202, P95Millis: 240, MaxMillis: 281,
		}},
	}
	report.Summary = summarize(report.Trials)
	path := filepath.Join(t.TempDir(), "report.json")
	if err := writeReport(path, report); err != nil {
		t.Fatal(err)
	}
	if err := writeReport(path, report); err != nil {
		t.Fatalf("rewriting an existing report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 1 || decoded.Path.Seed != 7 || decoded.Path.PolicerRefillMillis != 8 ||
		len(decoded.Latency) != 1 || len(decoded.UDP) != 1 || decoded.UDP[0].Lost != 3 ||
		decoded.Trials[0].PathCounters.Downstream.BottleneckDropped != 20 {
		t.Fatalf("report lost reproducibility fields: %+v", decoded)
	}
}

func TestDescribePathRecordsPolicerAndApplicationBudgets(t *testing.T) {
	opts := options{
		rttMillis: 200, rateMbits: 25, policerRefill: 8 * time.Millisecond,
		policerBurst: 4000, brutalMbits: 24, aggregateMbits: 20,
		interactiveReserveMbits: 2, congestion: "brutal-no-comp", udpOnStream: true,
	}
	got := describePath(opts, pathsim.Config{
		PolicerRefillPeriod: opts.policerRefill, PolicerBurstBytes: opts.policerBurst,
	})
	if got.PolicerRefillMillis != 8 || got.PolicerBurstBytes != 4000 ||
		got.BrutalRateMbits != 24 || got.AggregateRateMbits != 20 || got.InteractiveReserveMbits != 2 || !got.UDPOnStream {
		t.Fatalf("path report omitted experiment controls: %+v", got)
	}
}

func TestDurationQuantilesIncludeTheDeliveredTail(t *testing.T) {
	p50, p95, max := durationQuantiles([]time.Duration{
		400 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond,
	})
	if p50 != 200 || p95 != 300 || max != 400 {
		t.Fatalf("duration quantiles = %.0f/%.0f/%.0f, want 200/300/400", p50, p95, max)
	}
	p50, p95, max = durationQuantiles(nil)
	if p50 != 0 || p95 != 0 || max != 0 {
		t.Fatalf("empty duration quantiles = %.0f/%.0f/%.0f, want zeroes", p50, p95, max)
	}
}

func TestDescribePathCountersSeparatesErasureFromBottleneckDrops(t *testing.T) {
	got := describePathCounters(
		pathsim.Stats{PacketsIn: 100, PacketsOut: 70, PacketsLost: 10, PacketsDropped: 20, BytesIn: 120_000, BytesOut: 84_000},
		pathsim.Stats{PacketsIn: 50, PacketsOut: 48, PacketsLost: 2, BytesIn: 60_000, BytesOut: 57_600},
	)
	if got.Upstream.PacketsErased != 10 || got.Upstream.BottleneckDropped != 20 ||
		got.Downstream.PacketsErased != 2 || got.Downstream.BottleneckDropped != 0 {
		t.Fatalf("path counters conflated erasure and bottleneck drops: %+v", got)
	}
}

func TestSourceProvenanceNamesTheCheckout(t *testing.T) {
	source := describeSource()
	if source.Revision == "" || source.CommitTime == "" {
		t.Fatalf("source provenance does not identify the checkout: %+v", source)
	}
	if source.GoVersion == "" || source.GOOS == "" || source.GOARCH == "" {
		t.Fatalf("source provenance does not identify the toolchain: %+v", source)
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

// A trial whose flow never started measures the path, not the transport's
// transfer behaviour, and carries no goodput. Counting it drags the median
// toward zero for whichever stack happened to run during a bad window, and
// makes the completion rate depend on setup luck.
func TestSetupFailuresAreSeparatedFromTransportFailures(t *testing.T) {
	trials := []TrialRecord{
		{Stack: "queqiao", Flows: 1, MbitsPerSec: 10, Complete: true},
		{Stack: "queqiao", Flows: 1, MbitsPerSec: 12, Complete: true},
		{Stack: "queqiao", Flows: 1, MbitsPerSec: 0, Note: "warmup: EOF"},
		{Stack: "queqiao", Flows: 1, MbitsPerSec: 0, Note: "setup: dial failed"},
	}
	summaries := summarize(trials)
	got := summaries[0]
	if got.SetupFailures != 2 {
		t.Fatalf("setup failures = %d, want 2", got.SetupFailures)
	}
	if got.CompletionRate != 1 {
		t.Fatalf("completion rate = %.2f, want 1 over the trials that actually ran", got.CompletionRate)
	}
	if got.MedianMbits != 11 {
		t.Fatalf("median = %.2f, want 11 with setup failures excluded", got.MedianMbits)
	}
	if got.WorstMbits != 10 {
		t.Fatalf("worst = %.2f, want the worst measured trial", got.WorstMbits)
	}
}

// A mid-transfer stall is a transport failure and must still count: it is
// exactly the behaviour the completion rate exists to expose.
func TestPartialTransferStillCountsAsFailure(t *testing.T) {
	trials := []TrialRecord{
		{Stack: "baseline", Flows: 1, MbitsPerSec: 8, Complete: true},
		{Stack: "baseline", Flows: 1, MbitsPerSec: 1, Note: "received 4118325 of 4194304 bytes"},
	}
	got := summarize(trials)[0]
	if got.SetupFailures != 0 {
		t.Fatalf("setup failures = %d, want a stalled transfer counted as a transport failure", got.SetupFailures)
	}
	if got.CompletionRate != 0.5 {
		t.Fatalf("completion rate = %.2f, want 0.5", got.CompletionRate)
	}
}

// Every trial failing setup must not produce a divide-by-zero or a fake
// completion rate.
func TestAllSetupFailuresDoNotPanic(t *testing.T) {
	got := summarize([]TrialRecord{
		{Stack: "queqiao", Flows: 1, Note: "warmup: EOF"},
		{Stack: "queqiao", Flows: 1, Note: "warmup: EOF"},
	})[0]
	if got.SetupFailures != 2 || got.CompletionRate != 0 || got.MedianMbits != 0 {
		t.Fatalf("all-setup-failure cell = %+v, want zeroes and two setup failures", got)
	}
}
