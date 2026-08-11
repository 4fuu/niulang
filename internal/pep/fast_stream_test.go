package pep

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/session"
)

// A fast OPEN is a connection-scoped optimization, never a standalone
// authentication mechanism. An unauthenticated stream must not be able to
// open a destination merely by choosing TypeOpenFast.
func TestFastOpenRejectedOnUnauthenticatedStream(t *testing.T) {
	certificate, _ := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		server.handleSession(ctx, serverSide, &quicAuthState{})
		close(done)
	}()

	sessionID, err := session.NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	open := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeOpenFast, SessionID: sessionID, FlowID: 1,
	}, Payload: []byte("example.com:443")}
	if err := protocol.WriteFrame(clientSide, open); err != nil {
		t.Fatalf("write unauthenticated fast open: %v", err)
	}
	_ = clientSide.SetReadDeadline(time.Now().Add(time.Second))
	var raw [protocol.HeaderSize]byte
	if _, err := io.ReadFull(clientSide, raw[:]); err == nil {
		t.Fatal("unauthenticated fast open unexpectedly received a response")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unauthenticated fast-open handler did not terminate")
	}
}

// A capability-free HelloOK models an older deployed server. The client must
// keep using the per-stream Hello exchange on the same pooled QUIC connection.
func TestQUICPoolLegacyCapabilityFallsBackToHello(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
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
	// This is deliberately an internal test hook: production servers advertise
	// the current capability set, while this test emulates an older peer.
	server.quicCapabilities = 0
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

	first, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("first pooled flow: %v", err)
	}
	if !client.quicPoolAuthenticated || client.quicPoolFast || client.quicPoolControl {
		t.Fatalf("unexpected legacy pool state: authenticated=%v fast=%v control=%v", client.quicPoolAuthenticated, client.quicPoolFast, client.quicPoolControl)
	}
	second, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("legacy fallback flow: %v", err)
	}
	_ = first.outer.Close()
	_ = second.outer.Close()
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

func TestQUICPoolConcurrentFastStreams(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: packetConn.LocalAddr().String(), Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableQUIC: true, Logger: logger,
		HandshakeTimeout: time.Second, MaxSessions: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: packetConn.LocalAddr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportQUIC, EnableQUICPool: true,
		MaxLanes: 1, InitialLanes: 1, MaxSessions: 16, Logger: logger, HandshakeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ServePacketConn(ctx, packetConn) }()

	const streams = 4
	flows := make([]*openedFlow, streams)
	errs := make([]error, streams)
	var wg sync.WaitGroup
	for i := range flows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			flows[i], errs[i] = client.openFlow(ctx, destinationListener.Addr().String())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent pooled flow %d: %v", i, err)
		}
	}
	if !client.quicPoolAuthenticated || !client.quicPoolFast || !client.quicPoolControl {
		t.Fatalf("pool capabilities were not retained: authenticated=%v fast=%v control=%v", client.quicPoolAuthenticated, client.quicPoolFast, client.quicPoolControl)
	}
	for i, flow := range flows {
		if !flow.reserveControl {
			t.Fatalf("pooled flow %d did not negotiate control-lane reservation", i)
		}
	}
	for _, flow := range flows {
		_ = flow.outer.Close()
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

func TestQUICPoolReconnectResetsAuthentication(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
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

	first, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("first pooled flow: %v", err)
	}
	oldConn := client.quicConn
	if oldConn == nil || !client.quicPoolAuthenticated || !client.quicPoolFast || !client.quicPoolControl {
		t.Fatalf("initial pool was not authenticated/capable")
	}
	// Simulate a dead path after the first flow. The next stream must not reuse
	// the old connection-level authentication bit.
	_ = oldConn.CloseWithError(0, "test dead path")
	_ = first.outer.Close()
	deadline := time.Now().Add(time.Second)
	for oldConn.Context().Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if oldConn.Context().Err() == nil {
		t.Fatal("test QUIC connection did not close")
	}
	second, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("flow after pool reconnect: %v", err)
	}
	if client.quicConn == oldConn {
		t.Fatal("pool reused a dead QUIC connection")
	}
	if !client.quicPoolAuthenticated || !client.quicPoolFast || !client.quicPoolControl {
		t.Fatalf("reconnected pool did not renegotiate capabilities: authenticated=%v fast=%v control=%v", client.quicPoolAuthenticated, client.quicPoolFast, client.quicPoolControl)
	}
	_ = second.outer.Close()
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

func TestQUICPoolFastUDPAssociation(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
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

	// Warm and authenticate the pool with a normal CONNECT stream. The UDP
	// association that follows must then take the one-frame fast-open path.
	warm, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("warm pooled flow: %v", err)
	}
	udpLane, _, err := client.openUDPAssociation(ctx)
	if err != nil {
		t.Fatalf("fast UDP association: %v", err)
	}
	if !udpLane.fastOpen {
		t.Fatal("UDP association repeated the per-stream Hello handshake")
	}
	_ = warm.outer.Close()
	_ = udpLane.outer.Close()
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

func TestQUICPoolFastRejectDowngradesToLegacy(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
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

	first, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("warm pooled flow: %v", err)
	}
	if !client.quicPoolFast {
		t.Fatal("server did not advertise fast streams")
	}
	// Simulate a peer that advertised the capability during a rolling deploy
	// but cannot yet process OPEN_FAST. The client must retry once with HELLO.
	server.quicFastStreams.Store(false)
	second, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("legacy downgrade flow: %v", err)
	}
	if client.quicPoolFast {
		t.Fatal("fast capability remained enabled after protocol rejection")
	}
	_ = first.outer.Close()
	_ = second.outer.Close()
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

func TestOptimisticOpenReturnsBeforeOpenOKAndPreservesResponse(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go discardDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("fast-stream-test-secret-32-bytes")
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
		OptimisticOpen: true, MaxLanes: 1, Logger: logger, HandshakeTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.ServePacketConn(ctx, packetConn) }()

	flow, err := client.openFlow(ctx, destinationListener.Addr().String())
	if err != nil {
		t.Fatalf("optimistic open: %v", err)
	}
	if !flow.openPending {
		t.Fatal("optimistic open did not retain pending OPEN_OK state")
	}
	response, err := flow.fc.Read()
	if err != nil {
		t.Fatalf("read deferred OPEN_OK: %v", err)
	}
	if response.Header.Type != protocol.TypeOpenOK || response.Header.SessionID != flow.sessionID || response.Header.FlowID != flow.flowID || len(response.Payload) != 0 {
		t.Fatalf("deferred response = %+v, want matching OPEN_OK", response.Header)
	}
	_ = flow.outer.Close()
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

func discardDestination(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}()
	}
}
