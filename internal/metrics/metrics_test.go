package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistryCountersAndHandler(t *testing.T) {
	r := New()
	r.FlowStarted()
	r.ClassTransition(2)
	r.LaneFailure()
	r.LaneReplacement()
	r.Fallback()
	r.UDPAssociationReconnect()
	r.UDPAssociationRescueFailure()
	r.FlowTimeout()
	r.ObserveQUIC(1, QUICObservation{
		Lanes: 2, LatestRTT: 250 * time.Millisecond, SmoothedRTT: 200 * time.Millisecond,
		BytesSent: 100, BytesReceived: 200, BytesLost: 3, PacketsLost: 1,
		ControllerKind: "bbr", ControllerMode: 3, ControllerMaxBandwidth: 1_000_000,
		ControllerLatestSample: 900_000, ControllerLatestAckRate: 1_100_000,
		ControllerLatestSendRate: 900_000, ControllerSamples: 12,
		ControllerNonAppSamples: 10, ControllerAppSamples: 2,
		ControllerStateMisses: 1, ControllerZeroSamples: 3, ControllerRound: 7,
		ControllerPacingRate: 1_250_000, ControllerCongestionWindow: 400_000,
		ControllerBytesInFlight: 200_000, ControllerMinRTT: 190 * time.Millisecond,
		ControllerInRecovery: true,
	})
	r.FlowFinished(10, 20, false)
	r.FlowStarted()
	r.FlowFinished(1, 2, true)
	s := r.Snapshot()
	if s.ActiveFlows != 0 || s.FlowsStarted != 2 || s.FlowsCompleted != 1 || s.FlowsFailed != 1 || s.BytesUp != 11 || s.BytesDown != 22 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "queqiao_lane_replacements_total 1") || !strings.Contains(rec.Body.String(), "queqiao_udp_association_reconnects_total 1") || !strings.Contains(rec.Body.String(), "queqiao_udp_association_rescue_failures_total 1") || !strings.Contains(rec.Body.String(), "queqiao_flow_timeouts_total 1") || !strings.Contains(rec.Body.String(), "queqiao_quic_smoothed_rtt_seconds 0.200000000") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_kind{kind=\"bbr\"} 1") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_latest_sample_bytes_per_second 900000") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_latest_ack_rate_bytes_per_second 1100000") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_non_app_limited_samples_total 10") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_state_misses_total 1") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_round 7") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_pacing_rate_bytes_per_second 1250000") || !strings.Contains(rec.Body.String(), "queqiao_quic_controller_in_recovery 1") {
		t.Fatalf("unexpected exposition: %s", rec.Body.String())
	}
}

func TestFlowFinishedNeverMakesActiveGaugeNegative(t *testing.T) {
	r := New()
	const workers = 64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			r.FlowFinished(0, 0, true)
		}()
	}
	wg.Wait()
	if got := r.Snapshot().ActiveFlows; got != 0 {
		t.Fatalf("active gauge = %d, want 0", got)
	}
}

func TestMetricsHandlerRejectsMutationMethods(t *testing.T) {
	r := New()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
	r.RemoveQUIC(1)
	if got := r.Snapshot().QUICLanes; got != 0 {
		t.Fatalf("removed QUIC telemetry still reports %d lanes", got)
	}
}

func TestZeroValueRegistryIsSafe(t *testing.T) {
	var r Registry
	r.ObserveQUIC(7, QUICObservation{Lanes: 1, SmoothedRTT: time.Millisecond})
	if got := r.Snapshot().QUICLanes; got != 1 {
		t.Fatalf("zero-value registry lanes = %d, want 1", got)
	}
	r.RemoveQUIC(7)
}

// The rescue window is dropped rather than allowed to throttle the
// application. That trade has to be visible: once a flow has evicted part of
// its window, a lane failure fails the flow instead of recovering it, so an
// operator needs to see it happening.
func TestReplayAndIsolationCountersAreExported(t *testing.T) {
	registry := New()
	registry.ReplayBytes(1024)
	registry.ReplayBytes(512)
	registry.ReplayBytes(-1024)
	registry.BulkIsolated()

	got := registry.Snapshot()
	if got.ReplayBytesInUse != 512 {
		t.Fatalf("replay bytes in use = %d, want 512", got.ReplayBytesInUse)
	}
	if got.BulkIsolations != 1 {
		t.Fatalf("bulk isolations = %d, want 1", got.BulkIsolations)
	}

	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		"queqiao_replay_bytes_in_use 512",
		"queqiao_bulk_isolations_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output is missing %q", want)
		}
	}
}

// Releasing more than was acquired must not drive the gauge negative, or the
// endpoint budget would appear to have capacity it does not have.
func TestReplayBytesGaugeCannotGoNegative(t *testing.T) {
	registry := New()
	registry.ReplayBytes(256)
	registry.ReplayBytes(-4096)
	if got := registry.Snapshot().ReplayBytesInUse; got != 0 {
		t.Fatalf("replay bytes in use = %d after an oversized release, want 0", got)
	}
}

// A nil registry is the "metrics disabled" case and must stay safe.
func TestNilRegistryIsSafe(t *testing.T) {
	var registry *Registry
	registry.ReplayBytes(10)
	registry.BulkIsolated()
}
