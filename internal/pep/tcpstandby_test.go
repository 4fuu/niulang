package pep

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/identity"
	"github.com/4fuu/niulang/internal/protocol"
)

func TestStandbyHeartbeatScheduleUsesOneBoundedPhasePerSlot(t *testing.T) {
	origin := time.Unix(1_700_000_000, 0)
	generation := [16]byte{0x42, 0x17, 0xa9, 0x31, 0x55, 0xc3, 0x08, 0xfe, 0x91, 0x26, 0x73, 0xdd, 0x04, 0xb8, 0x6a, 0x5c}
	schedule := newStandbyHeartbeatSchedule(origin, generation)
	const slots = 10_000
	intervals := make(map[time.Duration]struct{}, slots-1)
	var previous time.Time
	var sum, sumSquares float64
	for slot := 1; slot <= slots; slot++ {
		deadline := schedule.nextSlot()
		slotStart := origin.Add(time.Duration(slot) * standbyHeartbeatInterval)
		phase := deadline.Sub(slotStart)
		if phase < 0 || phase > standbyHeartbeatPhaseMax {
			t.Fatalf("slot %d phase = %v, want 0..%v", slot, phase, standbyHeartbeatPhaseMax)
		}
		if slot > 1 {
			interval := deadline.Sub(previous)
			if interval < standbyHeartbeatInterval-standbyHeartbeatPhaseMax || interval > standbyHeartbeatInterval+standbyHeartbeatPhaseMax {
				t.Fatalf("slot %d interval = %v, want %v..%v", slot, interval, standbyHeartbeatInterval-standbyHeartbeatPhaseMax, standbyHeartbeatInterval+standbyHeartbeatPhaseMax)
			}
			intervals[interval] = struct{}{}
			seconds := interval.Seconds()
			sum += seconds
			sumSquares += seconds * seconds
		}
		previous = deadline
	}
	if schedule.slot != slots {
		t.Fatalf("scheduled slots = %d, want %d", schedule.slot, slots)
	}
	if len(intervals) < slots*9/10 {
		t.Fatalf("unique intervals = %d, want at least %d", len(intervals), slots*9/10)
	}
	count := float64(slots - 1)
	mean := sum / count
	standardDeviation := math.Sqrt(sumSquares/count - mean*mean)
	if standardDeviation < 35*time.Millisecond.Seconds() || standardDeviation > 45*time.Millisecond.Seconds() {
		t.Fatalf("interval standard deviation = %.6fs, want 0.035s..0.045s", standardDeviation)
	}
	t.Logf("fixed schedule: unique_intervals=1 standard_deviation=0s")
	t.Logf("phased schedule: samples=%d unique_intervals=%d mean=%.9fs standard_deviation=%.9fs", slots-1, len(intervals), mean, standardDeviation)

	otherGeneration := generation
	otherGeneration[0]++
	other := newStandbyHeartbeatSchedule(origin, otherGeneration)
	independent := false
	schedule = newStandbyHeartbeatSchedule(origin, generation)
	for range 32 {
		if !schedule.nextSlot().Equal(other.nextSlot()) {
			independent = true
			break
		}
	}
	if !independent {
		t.Fatal("different standby generations produced the same phase sequence")
	}
}

func TestStandbyHeartbeatScheduleRetainsOnePendingTickAndSkipsOlderSlots(t *testing.T) {
	origin := time.Unix(1_700_000_000, 0)
	schedule := newStandbyHeartbeatSchedule(origin, [16]byte{1})
	first := schedule.nextAfter(origin)
	pending := schedule.nextAfter(first)
	if schedule.slot != 2 || pending.Before(origin.Add(2*standbyHeartbeatInterval)) {
		t.Fatalf("pending deadline = %v in slot %d, want slot 2", pending, schedule.slot)
	}
	next := schedule.nextAfter(origin.Add(3500 * time.Millisecond))
	if schedule.slot != 4 {
		t.Fatalf("next slot = %d, want slot 4 after retaining slot 2 and dropping slot 3", schedule.slot)
	}
	slotStart := origin.Add(4 * standbyHeartbeatInterval)
	if next.Before(slotStart) || next.After(slotStart.Add(standbyHeartbeatPhaseMax)) {
		t.Fatalf("next deadline = %v, want fourth slot %v..%v", next, slotStart, slotStart.Add(standbyHeartbeatPhaseMax))
	}
}

func TestStandbyHeartbeatPhaseWaitStopsImmediately(t *testing.T) {
	newWaitingStandby := func() (*tcpStandby, net.Conn) {
		local, peer := net.Pipe()
		return &tcpStandby{
			outer: local, fc: newFrameConn(local), generation: [16]byte{1},
			lastAck: time.Now(), claimedCh: make(chan struct{}), finishedCh: make(chan struct{}),
		}, peer
	}
	newSchedule := func() standbyHeartbeatSchedule {
		schedule := newStandbyHeartbeatSchedule(time.Now(), [16]byte{1})
		schedule.interval = 250 * time.Millisecond
		schedule.phaseMax = 25 * time.Millisecond
		return schedule
	}

	t.Run("claim", func(t *testing.T) {
		standby, peer := newWaitingStandby()
		defer peer.Close()
		result := make(chan bool, 1)
		go func() {
			claimed, err := standby.maintainHeartbeats(context.Background(), newSchedule())
			result <- claimed && err == nil
		}()
		time.Sleep(10 * time.Millisecond)
		started := time.Now()
		if !standby.claim(started) {
			t.Fatal("standby claim failed")
		}
		select {
		case ok := <-result:
			if !ok {
				t.Fatal("heartbeat wait did not report the claim")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("claim waited for the heartbeat phase timer")
		}
		if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
			t.Fatalf("claim interruption took %v", elapsed)
		}
		standby.close()
	})

	t.Run("cancellation", func(t *testing.T) {
		standby, peer := newWaitingStandby()
		defer peer.Close()
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			claimed, err := standby.maintainHeartbeats(ctx, newSchedule())
			if claimed {
				result <- errors.New("heartbeat wait reported a claim")
				return
			}
			result <- err
		}()
		time.Sleep(10 * time.Millisecond)
		started := time.Now()
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("cancellation waited for the heartbeat phase timer")
		}
		if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
			t.Fatalf("cancellation interruption took %v", elapsed)
		}
		standby.close()
	})
}

func BenchmarkStandbyHeartbeatSchedule(b *testing.B) {
	schedule := newStandbyHeartbeatSchedule(time.Unix(1_700_000_000, 0), [16]byte{1, 2, 3, 4})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = schedule.nextSlot()
	}
}

func TestTCPStandbyRegistersHeartbeatsAndClaims(t *testing.T) {
	certificate, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: listener.Addr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: listener.Addr().String(),
		Credentials: roots, Transport: TransportAuto, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ServeListener(ctx, listener) }()

	standby, err := client.dialTCPStandby(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !client.publishTCPStandby(standby) {
		t.Fatal("standby was not published")
	}
	if !client.standbyReady(time.Now()) {
		t.Fatal("registered standby is not healthy")
	}
	if err := standby.heartbeat(); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	claimed := client.claimTCPStandby(time.Now())
	if claimed != standby || !standby.claimed {
		t.Fatal("standby was not claimed atomically")
	}
	standby.finishClaim()
	standby.close()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("server shutdown: %v", err)
	}
}

func TestTCPStandbyManagerReplenishesAfterAFlowClaimsIt(t *testing.T) {
	certificate, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: listener.Addr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: listener.Addr().String(),
		Credentials: roots, Transport: TransportAuto, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ServeListener(ctx, listener) }()
	go client.maintainTCPStandby(ctx)
	waitForStandby := func() {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for !client.standbyReady(time.Now()) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if !client.standbyReady(time.Now()) {
			t.Fatal("TCP standby manager did not become ready")
		}
	}
	waitForStandby()
	first := client.claimTCPStandby(time.Now())
	if first == nil {
		t.Fatal("first flow could not claim the standby")
	}
	first.close()
	first.finishClaim()
	waitForStandby()
	second := client.claimTCPStandby(time.Now())
	if second == nil || second == first {
		t.Fatal("second flow did not receive a replenished standby")
	}
	second.close()
	second.finishClaim()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("server shutdown: %v", err)
	}
}

func TestFailedTCPJoinAcknowledgementKeepsQUICAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	flow := newMultipathFlow(ctx, nil, [16]byte{1}, 1, 1024, 0, 0, nil, nil, nil)
	quicLocal, quicPeer := net.Pipe()
	defer quicPeer.Close()
	quic := &mpLane{id: 0, kind: TransportQUIC, fc: newFrameConn(quicLocal), control: true}
	if err := flow.addLane(quic); err != nil {
		t.Fatal(err)
	}
	serverFlow := newServerFlow(flow, identity.Principal{}, TransportQUIC, 1)
	tcpLocal, tcpPeer := net.Pipe()
	defer tcpPeer.Close()
	tcp := &mpLane{id: 1, kind: TransportTCP, fc: newFrameConn(tcpLocal), staged: true}
	wantErr := errors.New("ack write failed")
	if err := serverFlow.commitJoinedLane(tcp, func() error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("commit error = %v", err)
	}
	lanes := flow.healthyLanes()
	if len(lanes) != 1 || lanes[0] != quic || quic.closed.Load() {
		t.Fatalf("QUIC lanes after refused handoff = %#v", lanes)
	}
}

func TestOverflowAdmissionEchoesOnlyAuthenticatedPathProbe(t *testing.T) {
	certificate, roots := testCertificate(t)
	leaf, err := x509.ParseCertificate(roots.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	principal, err := identity.PrincipalFromCertificate(leaf)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{ListenAddr: "127.0.0.1:0", Credentials: certificate})
	if err != nil {
		t.Fatal(err)
	}
	probe := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeProbe,
		SessionID: [16]byte{1}, FlowID: 0, Sequence: 0, Class: protocol.ClassNew,
	}, Payload: []byte{1}}

	t.Run("probe", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		clientFrames := newFrameConn(clientConn)
		done := make(chan struct{})
		go func() {
			server.handleOverflowPathProbe(serverConn, principal, &quicAuthState{})
			close(done)
		}()
		if err := clientFrames.Write(probe); err != nil {
			t.Fatal(err)
		}
		echo, err := clientFrames.Read()
		if err != nil || echo.Header.Type != protocol.TypeProbe || echo.Header.SessionID != probe.Header.SessionID ||
			echo.Header.FlowID != 0 || echo.Header.Sequence != 0 || len(echo.Payload) != 1 || echo.Payload[0] != 1 {
			t.Fatalf("overflow probe echo = %+v, error = %v", echo, err)
		}
		_ = clientFrames.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("overflow probe handler did not stop")
		}
	})

	t.Run("open", func(t *testing.T) {
		serverConn, clientConn := net.Pipe()
		clientFrames := newFrameConn(clientConn)
		done := make(chan struct{})
		go func() {
			server.handleOverflowPathProbe(serverConn, principal, &quicAuthState{})
			close(done)
		}()
		open := probe
		open.Header.Type = protocol.TypeOpen
		open.Header.FlowID = 1
		if err := clientFrames.Write(open); err != nil {
			t.Fatal(err)
		}
		_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := clientFrames.Read(); err == nil {
			t.Fatal("overflow admission accepted destination OPEN")
		}
		_ = clientFrames.Close()
		<-done
	})
}

func TestRegisteredTCPStandbyActivatesAtomicHandoff(t *testing.T) {
	certificate, roots := testCertificate(t)
	leaf, err := x509.ParseCertificate(roots.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	principal, err := identity.PrincipalFromCertificate(leaf)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: listener.Addr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: listener.Addr().String(),
		Credentials: roots, Transport: TransportAuto, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- server.ServeListener(ctx, listener) }()

	sessionID := [16]byte{1}
	clientInner, clientInnerPeer := net.Pipe()
	defer clientInnerPeer.Close()
	serverInner, serverInnerPeer := net.Pipe()
	defer serverInnerPeer.Close()
	clientFlow := newMultipathFlow(ctx, clientInner, sessionID, 7, 1024, 0, 0, nil, client.metrics, logger)
	serverMPFlow := newMultipathFlow(ctx, serverInner, sessionID, 7, 1024, 0, 0, nil, server.metrics, logger)
	clientQUIC, serverQUIC := net.Pipe()
	if err := clientFlow.addLane(&mpLane{id: 0, kind: TransportQUIC, fc: newFrameConn(clientQUIC), control: true}); err != nil {
		t.Fatal(err)
	}
	if err := serverMPFlow.addLane(&mpLane{id: 0, kind: TransportQUIC, fc: newFrameConn(serverQUIC), control: true}); err != nil {
		t.Fatal(err)
	}
	serverSession := newServerFlow(serverMPFlow, principal, TransportQUIC, 1)
	if !server.registerSession(sessionID, serverSession) {
		t.Fatal("server session registration failed")
	}
	defer server.unregisterSession(sessionID, serverSession)

	standby, err := client.dialTCPStandby(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !client.publishTCPStandby(standby) {
		t.Fatal("standby publish failed")
	}
	if err := client.openStandbyRecoveryLane(clientFlow, sessionID, 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Reading OPEN_OK releases the client while the server goroutine may still
	// be activating its staged lane. Wait for that post-ack commit rather than
	// racing an immediate snapshot against it.
	deadline := time.Now().Add(time.Second)
	serverLanes := serverMPFlow.healthyLanes()
	for (len(serverLanes) != 1 || serverLanes[0].kind != TransportTCP) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		serverLanes = serverMPFlow.healthyLanes()
	}
	clientLanes := clientFlow.healthyLanes()
	if len(clientLanes) != 1 || len(serverLanes) != 1 {
		t.Fatalf("client/server lanes = %d/%d, want 1/1", len(clientLanes), len(serverLanes))
	}
	if clientLanes[0].kind != TransportTCP || serverLanes[0].kind != TransportTCP {
		t.Fatalf("handoff retained a non-TCP lane")
	}

	clientFlow.closeAll()
	serverMPFlow.closeAll()
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("server shutdown: %v", err)
	}
}
