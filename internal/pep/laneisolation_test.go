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

// A striped flow acknowledges one cumulative sequence, so the receiver's
// contiguous point sits behind whatever the slowest lane has not delivered.
// The sender's retention window then covers the whole reorder span rather than
// the bytes in flight, fills, and evicts bytes the peer never acknowledged,
// after which any lane failure is fatal. Re-sending the head on a faster lane
// is what bounds that span.
func TestStalledHeadIsReinjectedOnAnotherLane(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	// addLane starts the lane's writer, so the frame reaches the transport
	// rather than sitting in the queue. Capture what each lane actually sends.
	fastConn, slowConn := newAckCaptureConn(0, nil), newAckCaptureConn(0, nil)
	fast := &mpLane{id: 0, kind: TransportQUIC, fc: newFrameConn(fastConn, protocol.DefaultMaxPayload)}
	slow := &mpLane{id: 1, kind: TransportQUIC, fc: newFrameConn(slowConn, protocol.DefaultMaxPayload)}
	for _, lane := range []*mpLane{fast, slow} {
		if err := flow.addLane(lane); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = fastConn.Close(); _ = slowConn.Close() })
	laneRate(fast, 1<<20, 10*time.Millisecond)
	laneRate(slow, 1<<10, 500*time.Millisecond)

	// Fill the retention window past the pressure threshold.
	payload := make([]byte, defaultChunkSize)
	var sequence uint64
	for flowRetained(flow)*uint64(reinjectPressure) < flow.replayLimit {
		frame := protocol.Frame{
			Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Sequence: sequence},
			Payload: payload,
		}
		if err := flow.recordReplay(frame); err != nil {
			t.Fatalf("record at %d: %v", sequence, err)
		}
		sequence += uint64(len(payload))
	}

	flow.reinjectStalledHead()
	if got := flow.reinjections.Load(); got != 1 {
		t.Fatalf("reinjections = %d, want the stalled head re-sent once", got)
	}
	select {
	case frame := <-fastConn.frames:
		if frame.Header.Sequence != 0 || frame.Header.Type != protocol.TypeData {
			t.Fatalf("re-sent %+v, want the oldest retained data frame", frame.Header)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was re-sent on the faster lane")
	}
	select {
	case frame := <-slowConn.frames:
		t.Fatalf("the slow lane received %+v; re-sending there defeats the purpose", frame.Header)
	default:
	}

	// The same head must not be duplicated on every tick.
	flow.reinjectStalledHead()
	if got := flow.reinjections.Load(); got != 1 {
		t.Fatalf("reinjections = %d, want the head re-sent once per position", got)
	}
}

// A flow on one lane has nowhere to re-send, and duplicating onto the same
// lane would only waste capacity.
func TestSingleLaneFlowDoesNotReinject(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	if err := flow.addLane(isolationLane(t, 0)); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, defaultChunkSize)
	var sequence uint64
	for flowRetained(flow)*uint64(reinjectPressure) < flow.replayLimit {
		if err := flow.recordReplay(protocol.Frame{
			Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Sequence: sequence},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
		sequence += uint64(len(payload))
	}
	flow.reinjectStalledHead()
	if got := flow.reinjections.Load(); got != 0 {
		t.Fatalf("reinjections = %d on a single-lane flow, want none", got)
	}
}

// Below the pressure threshold the window is doing its job and a duplicate
// would only waste capacity.
func TestReinjectionWaitsForWindowPressure(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	for id := range uint64(2) {
		lane := isolationLane(t, id)
		if err := flow.addLane(lane); err != nil {
			t.Fatal(err)
		}
		laneRate(lane, 1<<20, 10*time.Millisecond)
	}
	if err := flow.recordReplay(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData},
		Payload: make([]byte, defaultChunkSize),
	}); err != nil {
		t.Fatal(err)
	}
	flow.reinjectStalledHead()
	if got := flow.reinjections.Load(); got != 0 {
		t.Fatalf("reinjections = %d with an almost empty window, want none", got)
	}
}

func flowRetained(f *multipathFlow) uint64 {
	f.replayMu.Lock()
	defer f.replayMu.Unlock()
	return f.replayBytes
}

// Releasing frames the peer reports holding is what keeps a striped flow's
// retention window proportional to the bytes actually outstanding rather than
// to the whole reorder span.
func TestRangeAcknowledgementReleasesRetainedFrames(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	payload := make([]byte, 1000)
	for sequence := uint64(0); sequence < 5000; sequence += 1000 {
		if err := flow.recordReplay(protocol.Frame{
			Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Sequence: sequence},
			Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	before := flowRetained(flow)

	// The peer holds [2000,4000) out of order, above a cumulative point of 0.
	flow.releaseAcknowledgedRanges([][2]uint64{{2000, 4000}})
	if got := flowRetained(flow); got != before-2000 {
		t.Fatalf("retained %d bytes, want %d after releasing two frames", got, before-2000)
	}
	flow.replayMu.Lock()
	_, stillThere := flow.replay[2000]
	_, partial := flow.replay[4000]
	flow.replayMu.Unlock()
	if stillThere {
		t.Fatal("a fully covered frame was retained")
	}
	if !partial {
		t.Fatal("a frame outside the reported range was released")
	}
}

// A frame only partly covered by a reported range must still be replayable in
// full, or a rescue would deliver a hole.
func TestPartiallyCoveredFrameIsNotReleased(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	if err := flow.recordReplay(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, Sequence: 0},
		Payload: make([]byte, 1000),
	}); err != nil {
		t.Fatal(err)
	}
	flow.releaseAcknowledgedRanges([][2]uint64{{0, 500}})
	if got := flowRetained(flow); got != 1000 {
		t.Fatalf("retained %d bytes, want the partly covered frame kept whole", got)
	}
}

// A peer that never advertised the capability must never be sent ranges.
func TestRangesAreNotSentToAPeerThatDidNotAdvertiseThem(t *testing.T) {
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
		t.Fatalf("ranges were sent to a peer that did not advertise support: %+v", frame.Header)
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

// The snapshot is taken at insert time but the acknowledgement is coalesced and
// sent later with a newer cumulative sequence. Ranges the cursor has passed are
// not merely redundant: the peer rejects an ACK whose ranges start below its
// cumulative point, which failed the flow outright.
func TestStaleRangesAreDroppedAgainstTheCumulativePoint(t *testing.T) {
	flow := newIsolationTestFlow(t, false)
	flow.rangesMu.Lock()
	flow.pendingRanges = [][2]uint64{{100, 200}, {300, 400}}
	flow.rangesMu.Unlock()
	got := flow.takeReceivedRanges(250)
	if len(got) != 1 || got[0] != [2]uint64{300, 400} {
		t.Fatalf("ranges = %v, want only those above the cumulative point", got)
	}
}
