package pep

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/metrics"
)

// statsConn is a lane transport that reports fixed QUIC connection statistics.
type statsConn struct {
	stats laneTransportStats
}

func (c *statsConn) Read([]byte) (int, error)           { return 0, io.EOF }
func (c *statsConn) Write(p []byte) (int, error)        { return len(p), nil }
func (c *statsConn) Close() error                       { return nil }
func (c *statsConn) transportStats() laneTransportStats { return c.stats }

// A flow's telemetry entry is removed when the flow ends, but the lane
// managers poll the same snapshot on a timer and stop only once the flow
// signals done. A publication from that window reinstates an entry which
// nothing will ever remove or refresh again, and since the registry reports
// round-trip time as a maximum over entries, one such entry pins the exported
// estimate at that flow's final measurement for the life of the process.
func TestFinishedFlowStopsPublishingQUICTelemetry(t *testing.T) {
	registry := metrics.New()
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	flow := newMultipathFlow(context.Background(), local, [16]byte{1}, 7, 0, 0, 0, nil, registry)
	flow.lanes[0] = &mpLane{id: 0, kind: TransportQUIC, fc: newFrameConn(&statsConn{
		stats: laneTransportStats{latestRTT: 900 * time.Millisecond, smoothedRTT: 800 * time.Millisecond},
	})}

	flow.snapshot()
	if got := registry.Snapshot(); got.QUICLanes != 1 || got.QUICSmoothedRTT != 800*time.Millisecond {
		t.Fatalf("live flow telemetry lanes=%d smoothed=%s, want 1 and 800ms", got.QUICLanes, got.QUICSmoothedRTT)
	}

	// What flow.run does while a lane manager is between its own done check
	// and its next snapshot.
	flow.finished.Store(true)
	registry.RemoveQUIC(flow.telemetryID)

	flow.snapshot()
	if got := registry.Snapshot(); got.QUICLanes != 0 || got.QUICSmoothedRTT != 0 {
		t.Fatalf("finished flow republished telemetry: lanes=%d smoothed=%s", got.QUICLanes, got.QUICSmoothedRTT)
	}
}
