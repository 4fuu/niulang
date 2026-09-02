package pep

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/classifier"
	"github.com/4fuu/niulang/internal/identity"
	"github.com/4fuu/niulang/internal/protocol"
)

// isolationLane builds a lane with a usable frame connection, which addLane
// requires.
func isolationLane(t *testing.T, id uint64) *mpLane {
	return isolationLaneKind(t, id, TransportQUIC)
}

func isolationLaneKind(t *testing.T, id uint64, kind TransportKind) *mpLane {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	return &mpLane{id: id, kind: kind, fc: newFrameConn(local)}
}

func TestLaneJoinCannotCrossDevicePrincipal(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	owner := identity.Principal{ProviderID: "provider", AccountID: "account", DeviceID: "owner"}
	existingFlow := newServerFlow(flow, owner)
	server := &Server{cfg: ServerConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, sessions: map[[16]byte]*serverFlow{flow.sessionID: existingFlow}}
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	other := owner
	other.DeviceID = "other"
	request := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeJoin, SessionID: flow.sessionID,
		FlowID: flow.flowID, Class: protocol.ClassBulk,
	}}
	go server.handleLaneJoinOpen(context.Background(), local, newFrameConn(local), other, flow.sessionID, 1, request)
	response, err := newFrameConn(remote).Read()
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Type != protocol.TypeReset {
		t.Fatalf("cross-device lane join response = %d, want RESET", response.Header.Type)
	}
}

func TestActiveFlowClosesAfterDeviceRevocation(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	leaf, err := x509.ParseCertificate(clientCredentials.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	principal, err := identity.PrincipalFromCertificate(leaf)
	if err != nil {
		t.Fatal(err)
	}
	flow := newIsolationTestFlow(t, false)
	serverFlow := newServerFlow(flow, principal)
	server := &Server{cfg: ServerConfig{Credentials: serverCredentials}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.watchAuthorization(ctx, serverFlow)
	if err := serverCredentials.Store.RevokeDevice(principal.DeviceID, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-flow.doneChan():
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("active flow remained open after device revocation")
	}
}

func newIsolationTestFlow(t *testing.T, reserveControl bool) *multipathFlow {
	t.Helper()
	inner, peer := net.Pipe()
	t.Cleanup(func() { _ = inner.Close(); _ = peer.Close() })
	flow := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, defaultChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, nil, nil)
	flow.reserveControlLane = reserveControl
	return flow
}

func TestStagedJoinLaneIsInvisibleUntilAcknowledged(t *testing.T) {
	flow := newIsolationTestFlow(t, true)
	lane := isolationLane(t, 1)
	lane.staged = true
	lane.control = true
	if err := flow.addLane(lane); err != nil {
		t.Fatal(err)
	}
	if got := flow.laneCount(); got != 1 {
		t.Fatalf("staged lane consumes %d admission slots, want one", got)
	}
	if got := len(flow.healthyLanes()); got != 0 {
		t.Fatalf("%d staged lanes became schedulable before OPEN_OK", got)
	}
	if err := flow.activateLane(lane); err != nil {
		t.Fatal(err)
	}
	if got := len(flow.healthyLanes()); got != 1 {
		t.Fatalf("healthy lanes after OPEN_OK = %d, want one", got)
	}
}

// A flow's data lives on one lane. A flow that negotiated the control-lane
// reservation keeps lane zero for interactive and rescue traffic in addition
// to that one.
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
	session := &serverFlow{flow: flow, maxLanes: serverLaneBudget(flow.reserveControlLane)}
	if err := session.addLane(isolationLane(t, 1)); err != nil {
		t.Fatalf("server refused the only bulk lane: %v", err)
	}
	if got := flow.laneCount(); got != 2 {
		t.Fatalf("lane count = %d, want the control lane plus one bulk lane", got)
	}
}

func TestServerReplacesALaneWithTheSameRole(t *testing.T) {
	flow := newIsolationTestFlow(t, true)
	session := newServerFlow(flow, identity.Principal{})
	control := isolationLane(t, 0)
	control.control = true
	bulk := isolationLane(t, 1)
	if err := session.addLane(control); err != nil {
		t.Fatal(err)
	}
	if err := session.addLane(bulk); err != nil {
		t.Fatal(err)
	}

	bulkReplacement := isolationLane(t, 2)
	if err := session.addLane(bulkReplacement); err != nil {
		t.Fatalf("bulk replacement refused: %v", err)
	}
	if control.closed.Load() || !bulk.closed.Load() {
		t.Fatalf("bulk replacement retired control=%t bulk=%t, want false/true", control.closed.Load(), bulk.closed.Load())
	}

	controlReplacement := isolationLane(t, 3)
	controlReplacement.control = true
	if err := session.addLane(controlReplacement); err != nil {
		t.Fatalf("control replacement refused: %v", err)
	}
	if !control.closed.Load() || bulkReplacement.closed.Load() {
		t.Fatalf("control replacement retired old-control=%t bulk=%t, want true/false", control.closed.Load(), bulkReplacement.closed.Load())
	}
}

// Without the reservation there is no separate control lane, so the budget is
// exactly one and a second lane must be refused.
func TestServerBudgetIsExactWithoutControlReservation(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	if err := flow.addLane(isolationLane(t, 0)); err != nil {
		t.Fatal(err)
	}
	session := &serverFlow{flow: flow, maxLanes: serverLaneBudget(flow.reserveControlLane)}
	// A lane over budget is admitted only by retiring an existing one, so the
	// total never exceeds the configured maximum.
	_ = session.addLane(isolationLane(t, 1))
	if got := flow.laneCount(); got > 1 {
		t.Fatalf("lane count = %d, want at most the configured maximum of 1", got)
	}
}

// A bulk flow must be able to leave the shared pooled connection with one data
// lane: isolation, not striping, is what keeps interactive traffic off a bulk
// congestion window. Striping is deleted; this is the property that outlived
// it, and it is a latency argument rather than a capacity one.
func TestBulkIsolationAppliesAtOneConfiguredLane(t *testing.T) {
	for _, test := range []struct {
		name           string
		reserveControl bool
		wantBulkBudget int
		wantReserve    int
	}{
		{"isolated bulk keeps a control lane beside its own", true, 1, 1},
		{"no reservation keeps one total lane", false, 1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget, reserve := bulkLaneBudget(test.reserveControl)
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: packetConn.LocalAddr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: logger,
		HandshakeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: packetConn.LocalAddr().String(), Credentials: roots, EnableQUICPool: true, Logger: logger, HandshakeTimeout: time.Second,
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
	case <-time.After(3 * time.Second):
		t.Fatal("server shutdown timeout")
	}
}

func TestBulkIsolationCapacityCannotEscapeIntoDedicatedConnections(t *testing.T) {
	certificate, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: packetConn.LocalAddr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true},
		Logger:            logger, HandshakeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ServePacketConn(ctx, packetConn) }()

	var sockets atomic.Int64
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: packetConn.LocalAddr().String(),
		LocalAddress: "127.0.0.1", Credentials: roots,
		EnableQUICPool: true,
		DialTimeout:    2 * time.Second, HandshakeTimeout: 2 * time.Second,
		Logger: logger,
		SocketControl: func(network, _ string, _ syscall.RawConn) error {
			if strings.HasPrefix(network, "udp") {
				sockets.Add(1)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.memoryLimits.maxBulkConnections = 2

	occupied, err := client.reserveBulkConn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.close("test complete")
	if _, err := client.reserveBulkConn(ctx); !errors.Is(err, errBulkConnectionLimit) {
		t.Fatalf("second proactive bulk connection = %v, want capacity decision", err)
	}
	if got := sockets.Load(); got != 1 {
		t.Fatalf("capacity decision opened %d UDP sockets, want one", got)
	}
	recovery, err := client.reserveBulkConnMode(ctx, false)
	if err != nil {
		t.Fatalf("recovery was blocked by the proactive isolation limit: %v", err)
	}
	client.releaseBulkConn(recovery, true)
	client.memoryLimits.maxBulkConnections = 1

	const waiters = 16
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(laneID uint64) {
			defer wg.Done()
			_, laneErr := client.openJoinLane(ctx, TransportQUIC, [16]byte{1}, 1, laneID)
			errs <- laneErr
		}(uint64(i + 1))
	}
	wg.Wait()
	close(errs)
	for laneErr := range errs {
		if !errors.Is(laneErr, errBulkConnectionLimit) {
			t.Fatalf("over-budget lane = %v, want capacity decision", laneErr)
		}
	}
	if got := sockets.Load(); got != 2 {
		t.Fatalf("over-budget isolation opened %d UDP sockets, want only the isolation and recovery admissions", got)
	}

	client.closeQUICPool()
	cancel()
	select {
	case serveErr := <-serverErr:
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			t.Fatalf("server shutdown: %v", serveErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server shutdown timeout")
	}
}

func TestDedicatedBulkFallbackIsReservedForRecoverablePoolFailures(t *testing.T) {
	if dedicatedBulkFallbackAllowed(errBulkConnectionLimit) {
		t.Fatal("capacity decision was treated as a pool failure")
	}
	if dedicatedBulkFallbackAllowed(fmt.Errorf("wrapped: %w", errBulkConnectionLimit)) {
		t.Fatal("wrapped capacity decision was treated as a pool failure")
	}
	if dedicatedBulkFallbackAllowed(errBulkConnectionPending) {
		t.Fatal("pending decision was treated as a pool failure")
	}
	if dedicatedBulkFallbackAllowed(context.Canceled) || dedicatedBulkFallbackAllowed(context.DeadlineExceeded) {
		t.Fatal("expired caller started a dedicated fallback")
	}
	if !dedicatedBulkFallbackAllowed(errors.New("pooled stream unavailable")) {
		t.Fatal("recoverable pool failure did not retain dedicated fallback")
	}
}

func TestShortPooledCompletionMarksAndClearsMixedFanout(t *testing.T) {
	client := &Client{}
	client.quicPoolActive.Store(4)
	client.quicFanoutStarted.Store(time.Now().Add(-time.Second).UnixNano())
	client.noteControlPoolFlowClosed(time.Now().Add(-classifier.DefaultConfig().NewAge - time.Second))
	if client.shortFlowFanout.Load() {
		t.Fatal("an old bulk completion was classified as a mixed-resource fanout")
	}

	client.quicPoolActive.Store(4)
	client.quicFanoutStarted.Store(time.Now().Add(-time.Second).UnixNano())
	client.noteControlPoolFlowClosed(time.Now())
	if !client.shortFlowFanout.Load() {
		t.Fatal("a short completion with three active siblings did not protect the shared pool")
	}
	client.noteControlPoolFlowClosed(time.Time{})
	if !client.shortFlowFanout.Load() {
		t.Fatal("mixed-resource protection cleared while two flows remained")
	}
	client.noteControlPoolFlowClosed(time.Time{})
	if client.shortFlowFanout.Load() {
		t.Fatal("mixed-resource protection survived after fanout ended")
	}
}

func TestCompletionFromBeforeCurrentFanoutCannotClassifyIt(t *testing.T) {
	client := &Client{}
	client.quicPoolActive.Store(4)
	client.noteControlPoolFlowClosed(time.Now())
	if client.shortFlowFanout.Load() {
		t.Fatal("a completion classified fanout before an overlap boundary existed")
	}

	client.quicPoolActive.Store(4)
	started := time.Now().Add(-time.Second)
	client.quicFanoutStarted.Store(started.UnixNano())

	client.noteControlPoolFlowClosed(started.Add(-time.Millisecond))

	if client.shortFlowFanout.Load() {
		t.Fatal("a previous warm-up flow classified the following fanout as mixed")
	}
}

// The internal off state remains safe for tests and partial flow setup even
// though protocol v3 enables ranges on every established flow.
func TestRangesAreNotSentBeforeRangeTrackingIsEnabled(t *testing.T) {
	conn := newAckCaptureConn(0, nil)
	flow := newAckTestFlow(conn)
	flow.rangesMu.Lock()
	flow.pendingRanges = [][2]uint64{{100, 200}}
	flow.rangesMu.Unlock()

	if err := flow.writeACK(context.Background(), 50, protocol.FlagAckDown, false); err != nil {
		t.Fatal(err)
	}
	frame := waitAckFrame(t, conn)
	if frame.Header.Flags&protocol.FlagAckRanges != 0 || len(frame.Payload) != 0 {
		t.Fatalf("ranges were sent before range tracking was enabled: %+v", frame.Header)
	}

	flow.ackRanges.Store(true)
	if err := flow.writeACK(context.Background(), 50, protocol.FlagAckDown, false); err != nil {
		t.Fatal(err)
	}
	frame = waitAckFrame(t, conn)
	if frame.Header.Flags&protocol.FlagAckRanges == 0 || len(frame.Payload) != protocol.AckRangeSize {
		t.Fatalf("ranges were not sent to a capable peer: %+v", frame.Header)
	}
}
