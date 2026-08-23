package pep

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/metrics"
	"github.com/bojieli/queqiao/internal/protocol"
)

// A gateway refusing session resumes looked healthy at the default log level,
// because the refusal was a Debug record: the only place the failure was
// diagnosable was the peer, which is where it is least useful. Every refusal
// reason must now reach an operator reading at info, and must be counted
// separately, because the three mean different things.
func TestLaneJoinRefusalsAreVisibleAndCountedByReason(t *testing.T) {
	owner := identity.Principal{ProviderID: "provider", AccountID: "account", DeviceID: "owner"}
	other := owner
	other.DeviceID = "other"

	for _, test := range []struct {
		name   string
		known  bool
		joiner identity.Principal
		laneID uint64
		flowID uint64
		reason metrics.LaneJoinRefusal
		level  string
	}{
		{name: "invalid identity", known: true, joiner: owner, laneID: 0, flowID: 1, reason: metrics.LaneJoinInvalidIdentity, level: "INFO"},
		{name: "unknown session", known: false, joiner: owner, laneID: 1, flowID: 1, reason: metrics.LaneJoinUnknownSession, level: "INFO"},
		{name: "flow mismatch", known: true, joiner: owner, laneID: 1, flowID: 9, reason: metrics.LaneJoinFlowMismatch, level: "WARN"},
		{name: "principal mismatch", known: true, joiner: other, laneID: 1, flowID: 1, reason: metrics.LaneJoinPrincipalMismatch, level: "WARN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			flow := newIsolationTestFlow(t, false)
			var records bytes.Buffer
			registry := metrics.New()
			server := &Server{
				cfg:      ServerConfig{Logger: slog.New(slog.NewJSONHandler(&records, &slog.HandlerOptions{Level: slog.LevelInfo}))},
				sessions: map[[16]byte]*serverFlow{},
				metrics:  registry,
			}
			if test.known {
				server.sessions[flow.sessionID] = newServerFlow(flow, owner, TransportTCP, 1)
			}
			local, remote := net.Pipe()
			t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
			request := protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeJoin, SessionID: flow.sessionID,
				FlowID: test.flowID, Class: protocol.ClassBulk,
			}}
			go server.handleLaneJoinOpen(context.Background(), local, newFrameConn(local), test.joiner, flow.sessionID, test.laneID, request)

			response, err := newFrameConn(remote).Read()
			if err != nil {
				t.Fatal(err)
			}
			if response.Header.Type != protocol.TypeReset {
				t.Fatalf("response = %d, want RESET", response.Header.Type)
			}
			if got := registry.Snapshot().LaneJoinRefusals[test.reason]; got != 1 {
				t.Fatalf("%s counter = %d, want 1", test.reason, got)
			}
			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(records.Bytes()), &record); err != nil {
				t.Fatalf("no record at info level: %v (%q)", err, records.String())
			}
			if record["msg"] != "lane join refused" || record["reason"] != test.reason.String() || record["level"] != test.level {
				t.Fatalf("record = %#v", record)
			}
		})
	}
}

// A storm must stay legible: the reason is written, the ones it stood in for
// are counted, and the count is reported rather than lost.
func TestLaneJoinRefusalLogSurvivesAStorm(t *testing.T) {
	var log refusalLog
	start := time.Now()
	write, suppressed := log.due(metrics.LaneJoinUnknownSession, start)
	if !write || suppressed != 0 {
		t.Fatalf("first refusal write=%t suppressed=%d, want true and 0", write, suppressed)
	}
	const storm = 500
	for i := 0; i < storm; i++ {
		if write, _ := log.due(metrics.LaneJoinUnknownSession, start.Add(time.Duration(i)*time.Millisecond)); write {
			t.Fatalf("refusal %d was written inside the interval", i)
		}
	}
	// A different reason is never suppressed by another one's storm.
	if write, _ := log.due(metrics.LaneJoinPrincipalMismatch, start.Add(time.Second)); !write {
		t.Fatal("a principal mismatch was hidden by an unknown-session storm")
	}
	write, suppressed = log.due(metrics.LaneJoinUnknownSession, start.Add(laneJoinRefusalLogInterval))
	if !write || suppressed != storm {
		t.Fatalf("after the interval write=%t suppressed=%d, want true and %d", write, suppressed, storm)
	}
	if write, suppressed = log.due(metrics.LaneJoinUnknownSession, start.Add(2*laneJoinRefusalLogInterval)); !write || suppressed != 0 {
		t.Fatalf("suppressed count survived being reported: write=%t suppressed=%d", write, suppressed)
	}
}
