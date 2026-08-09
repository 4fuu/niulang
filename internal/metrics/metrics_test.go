package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryCountersAndHandler(t *testing.T) {
	r := New()
	r.FlowStarted()
	r.ClassTransition(2)
	r.LaneFailure()
	r.LaneReplacement()
	r.Fallback()
	r.FlowFinished(10, 20, false)
	r.FlowStarted()
	r.FlowFinished(1, 2, true)
	s := r.Snapshot()
	if s.ActiveFlows != 0 || s.FlowsStarted != 2 || s.FlowsCompleted != 1 || s.FlowsFailed != 1 || s.BytesUp != 11 || s.BytesDown != 22 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "wanopt_lane_replacements_total 1") {
		t.Fatalf("unexpected exposition: %s", rec.Body.String())
	}
}
