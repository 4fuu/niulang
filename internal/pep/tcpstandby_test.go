package pep

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/identity"
	"github.com/4fuu/niulang/internal/protocol"
)

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
	clientLanes, serverLanes := clientFlow.healthyLanes(), serverMPFlow.healthyLanes()
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
