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
			// One record has to be readable on its own.
			if record["total"] != float64(1) {
				t.Fatalf("record total = %#v, want 1", record["total"])
			}
		})
	}
}

// A storm must stay legible: the reason is written, the ones it stood in for
// are counted, and the count is reported rather than lost.
func TestLaneJoinRefusalLogSurvivesAStorm(t *testing.T) {
	var log refusalLog
	start := time.Now()
	write, suppressed, total := log.due(metrics.LaneJoinUnknownSession, start)
	if !write || suppressed != 0 || total != 1 {
		t.Fatalf("first refusal write=%t suppressed=%d total=%d, want true, 0 and 1", write, suppressed, total)
	}
	const storm = 500
	for i := 0; i < storm; i++ {
		if write, _, _ := log.due(metrics.LaneJoinUnknownSession, start.Add(time.Duration(i)*time.Millisecond)); write {
			t.Fatalf("refusal %d was written inside the interval", i)
		}
	}
	// A different reason is never suppressed by another one's storm.
	if write, _, _ := log.due(metrics.LaneJoinPrincipalMismatch, start.Add(time.Second)); !write {
		t.Fatal("a principal mismatch was hidden by an unknown-session storm")
	}
	write, suppressed, total = log.due(metrics.LaneJoinUnknownSession, start.Add(refusalLogInterval))
	if !write || suppressed != storm || total != storm+2 {
		t.Fatalf("after the interval write=%t suppressed=%d total=%d, want true, %d and %d",
			write, suppressed, total, storm, storm+2)
	}
	if write, suppressed, _ = log.due(metrics.LaneJoinUnknownSession, start.Add(2*refusalLogInterval)); !write || suppressed != 0 {
		t.Fatalf("suppressed count survived being reported: write=%t suppressed=%d", write, suppressed)
	}
}

// A storm that stops must still say how big it was. The suppressed count is
// reported one record late, so on a gateway restart ninety-four refusals
// produced a single record claiming to stand for none of them; the total is
// what makes one record the whole story.
func TestARefusalRecordSaysHowManyThereHaveBeen(t *testing.T) {
	var log refusalLog
	start := time.Now()
	if _, _, total := log.due(metrics.LaneJoinUnknownSession, start); total != 1 {
		t.Fatalf("total = %d on the first refusal, want 1", total)
	}
	for i := 0; i < 93; i++ {
		log.due(metrics.LaneJoinUnknownSession, start.Add(time.Duration(i)*time.Millisecond))
	}
	// Nothing more arrives, so no further record is written and the suppressed
	// count is never reported. The record that was written must already carry
	// the count, which the next one confirms.
	_, _, total := log.due(metrics.LaneJoinUnknownSession, start.Add(refusalLogInterval))
	if total != 95 {
		t.Fatalf("total = %d, want every refusal counted", total)
	}
	if _, _, other := log.due(metrics.LaneJoinFlowMismatch, start); other != 1 {
		t.Fatalf("reasons share a total: flow mismatch = %d, want 1", other)
	}
}
