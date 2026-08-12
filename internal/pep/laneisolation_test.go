package pep

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
)

// isolationLane builds a lane with a usable frame connection, which addLane
// requires.
func isolationLane(t *testing.T, id uint64) *mpLane {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	return &mpLane{id: id, kind: TransportQUIC, fc: newFrameConn(local, protocol.DefaultMaxPayload)}
}

func newIsolationTestFlow(t *testing.T, reserveControl bool) *multipathFlow {
	t.Helper()
	inner, peer := net.Pipe()
	t.Cleanup(func() { _ = inner.Close(); _ = peer.Close() })
	flow := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, defaultChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, nil, nil)
	flow.reserveControlLane = reserveControl
	return flow
}

// --max-lanes bounds the lanes carrying bulk payload. A flow that negotiated
// the control-lane reservation keeps lane zero for interactive and rescue
// traffic in addition to that budget.
//
// Counting the control lane against the budget makes the server reject and
// close every joined bulk lane. The peer sees an immediate EOF, retries, and
// the flow churns through lanes instead of transferring: with one configured
// lane this stalled a 50 MiB transfer at under 3 MiB.
func TestServerAdmitsBulkLanesBesidesTheControlLane(t *testing.T) {
	flow := newIsolationTestFlow(t, true)
	if err := flow.addLane(isolationLane(t, 0)); err != nil {
		t.Fatal(err)
	}
	session := &serverFlow{flow: flow, maxLanes: serverLaneBudget(1, flow.reserveControlLane)}
	if err := session.addLane(isolationLane(t, 1)); err != nil {
		t.Fatalf("server refused the only bulk lane: %v", err)
	}
	if got := flow.laneCount(); got != 2 {
		t.Fatalf("lane count = %d, want the control lane plus one bulk lane", got)
	}
}

// Without the reservation there is no separate control lane, so the budget is
// exactly --max-lanes and a second lane must be refused.
func TestServerBudgetIsExactWithoutControlReservation(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	if err := flow.addLane(isolationLane(t, 0)); err != nil {
		t.Fatal(err)
	}
	session := &serverFlow{flow: flow, maxLanes: serverLaneBudget(1, flow.reserveControlLane)}
	// A lane over budget is admitted only by retiring an existing one, so the
	// total never exceeds the configured maximum.
	_ = session.addLane(isolationLane(t, 1))
	if got := flow.laneCount(); got > 1 {
		t.Fatalf("lane count = %d, want at most the configured maximum of 1", got)
	}
}

// A bulk flow must be able to leave the shared pooled connection even when only
// one bulk lane is configured: that isolation, not striping, is what keeps
// interactive traffic off a bulk congestion window.
func TestBulkIsolationAppliesAtOneConfiguredLane(t *testing.T) {
	for _, test := range []struct {
		name           string
		maxLanes       int
		reserveControl bool
		wantBulkBudget int
		wantReserve    int
	}{
		{"isolated bulk at one lane", 1, true, 1, 1},
		{"no reservation keeps one total lane", 1, false, 1, 0},
		{"striping budget is the bulk budget", 4, true, 4, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget, reserve := bulkLaneBudget(test.maxLanes, test.reserveControl)
			if budget != test.wantBulkBudget || reserve != test.wantReserve {
				t.Fatalf("budget=%d reserve=%d, want %d and %d", budget, reserve, test.wantBulkBudget, test.wantReserve)
			}
		})
	}
}

// Isolation costs a handshake and a fresh congestion window, so it must only
// happen while another flow actually shares the control connection. The count
// that decides this has to track pooled flows exactly, or a lone bulk transfer
// pays about 8% of its goodput for nothing.
func TestPooledFlowCountTracksOpenAndClose(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("lane-isolation-test-secret-32byt")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: packetConn.LocalAddr().String(), Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableQUIC: true, Logger: logger,
		HandshakeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: packetConn.LocalAddr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportQUIC, EnableQUICPool: true,
		MaxLanes: 1, InitialLanes: 1, Logger: logger, HandshakeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ServePacketConn(ctx, packetConn) }()

	if got := client.quicPoolActive.Load(); got != 0 {
		t.Fatalf("pooled flow count = %d before any flow, want 0", got)
	}
	first, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("first pooled flow: %v", err)
	}
	if got := client.quicPoolActive.Load(); got != 1 {
		t.Fatalf("pooled flow count = %d after one flow, want 1", got)
	}
	second, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("second pooled flow: %v", err)
	}
	if got := client.quicPoolActive.Load(); got != 2 {
		t.Fatalf("pooled flow count = %d after two flows, want 2", got)
	}
	_ = second.outer.Close()
	if got := client.quicPoolActive.Load(); got != 1 {
		t.Fatalf("pooled flow count = %d after one close, want 1", got)
	}
	// Closing twice must not double-decrement; the count decides a policy.
	_ = second.outer.Close()
	if got := client.quicPoolActive.Load(); got != 1 {
		t.Fatalf("pooled flow count = %d after a repeated close, want 1", got)
	}
	_ = first.outer.Close()
	if got := client.quicPoolActive.Load(); got != 0 {
		t.Fatalf("pooled flow count = %d after all closes, want 0", got)
	}
	client.closeQUICPool()
	cancel()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server shutdown timeout")
	}
}
