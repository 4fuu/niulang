package pep

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/socks5"
)

func testCertificate(t *testing.T) (tlsCertificate tls.Certificate, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wanopt.test"},
		DNSNames:     []string{"wanopt.test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	tlsCertificate, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("append test certificate")
	}
	return tlsCertificate, roots
}

func TestTLSOneLaneSOCKSEndToEnd(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go func() {
		for {
			conn, acceptErr := destinationListener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: serverListener.Addr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServeListener(ctx, serverListener) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn, err := net.DialTimeout("tcp", clientListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatal(err)
	}
	if method != [2]byte{5, 0} {
		t.Fatalf("method response %v", method)
	}
	host, portText, _ := net.SplitHostPort(destinationListener.Addr().String())
	ip := net.ParseIP(host).To4()
	port, _ := net.LookupPort("tcp", portText)
	request := []byte{5, 1, 0, 1}
	request = append(request, ip...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0 {
		t.Fatalf("SOCKS connect failed: %v", reply)
	}

	payload := bytes.Repeat([]byte("wanopt-one-lane-"), 8192)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func TestUDPAssociateSOCKSEndToEnd(t *testing.T) {
	runUDPAssociateSOCKSEndToEnd(t, TransportTCP)
}

func TestUDPAssociateQUICSOCKSEndToEnd(t *testing.T) {
	runUDPAssociateSOCKSEndToEnd(t, TransportQUIC)
}

func runUDPAssociateSOCKSEndToEnd(t *testing.T, transport TransportKind) {
	destination, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, readErr := destination.ReadFromUDP(buf)
			if readErr != nil {
				return
			}
			_, _ = destination.WriteToUDP(buf[:n], addr)
		}
	}()

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var serverListener net.Listener
	var serverPacketConn net.PacketConn
	if transport == TransportQUIC {
		serverPacketConn, err = net.ListenPacket("udp", "127.0.0.1:0")
	} else {
		serverListener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		t.Fatal(err)
	}
	serverAddr := serverListenerAddr(serverListener, serverPacketConn)
	server, err := NewServer(ServerConfig{
		ListenAddr: serverAddr, Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: transport != TransportQUIC, EnableQUIC: transport != TransportTCP, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: serverAddr, ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: transport, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	if transport == TransportQUIC {
		go func() { errorsCh <- server.ServePacketConn(ctx, serverPacketConn) }()
	} else {
		go func() { errorsCh <- server.ServeListener(ctx, serverListener) }()
	}
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	control, err := net.DialTimeout("tcp", clientListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := control.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(control, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("method response %v err=%v", method, err)
	}
	if _, err := control.Write([]byte{5, socks5.CommandUDPAssociate, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(control, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != socks5.ReplySucceeded {
		t.Fatalf("UDP associate failed: %v", reply)
	}
	bound := net.JoinHostPort(net.IP(reply[4:8]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(reply[8:10]))))
	udpClient, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer udpClient.Close()
	host, portText, _ := net.SplitHostPort(destination.LocalAddr().String())
	port, _ := strconv.Atoi(portText)
	request := []byte{0, 0, 0, 1}
	request = append(request, net.ParseIP(host).To4()...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	request = append(request, []byte("udp-echo")...)
	if _, err := udpClient.WriteToUDP(request, mustUDPAddr(t, bound)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	_ = udpClient.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := udpClient.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	got, err := socks5.ReadUDPDatagram(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != "udp-echo" {
		t.Fatalf("UDP payload %q", got.Payload)
	}
	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func serverListenerAddr(listener net.Listener, packetConn net.PacketConn) string {
	if listener != nil {
		return listener.Addr().String()
	}
	return packetConn.LocalAddr().String()
}

func mustUDPAddr(t *testing.T, address string) *net.UDPAddr {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestQUICOneLaneSOCKSEndToEnd(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go echoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableQUIC: true, Logger: logger,
		Metrics: metrics.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: packetConn.LocalAddr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportQUIC, EnableQUICPool: true, InitialLanes: 2, Logger: logger,
		Metrics: metrics.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServePacketConn(ctx, packetConn) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn, err := net.DialTimeout("tcp", clientListener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatal(err)
	}
	if method != [2]byte{5, 0} {
		t.Fatalf("method response %v", method)
	}
	host, portText, _ := net.SplitHostPort(destinationListener.Addr().String())
	ip := net.ParseIP(host).To4()
	port, _ := strconv.Atoi(portText)
	request := []byte{5, 1, 0, 1}
	request = append(request, ip...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0 {
		t.Fatalf("SOCKS connect failed: %v", reply)
	}
	payload := bytes.Repeat([]byte("wanopt-quic-"), 8192)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if server.MaxObservedLanes() < 2 {
		t.Fatalf("expected a joined QUIC lane, observed %d", server.MaxObservedLanes())
	}
	// A second logical flow must reuse the pooled QUIC connection without
	// disturbing the first flow's session or destination stream.
	conn2 := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
	defer conn2.Close()
	payload2 := bytes.Repeat([]byte("wanopt-pooled-flow-"), 1024)
	if _, err := conn2.Write(payload2); err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn2.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	got2, err := io.ReadAll(conn2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, payload2) {
		t.Fatalf("pooled flow echo mismatch: got %d bytes, want %d", len(got2), len(payload2))
	}
	// A completed logical flow must release its worker goroutines and active
	// gauge promptly even when the final ACK races with physical lane close.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if server.Metrics().Snapshot().ActiveFlows == 0 && client.Metrics().Snapshot().ActiveFlows == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.Metrics().Snapshot(); got.ActiveFlows != 0 || got.FlowsCompleted != 2 {
		t.Fatalf("server flow lifecycle leaked: active=%d started=%d completed=%d failed=%d", got.ActiveFlows, got.FlowsStarted, got.FlowsCompleted, got.FlowsFailed)
	}
	if got := client.Metrics().Snapshot(); got.ActiveFlows != 0 || got.FlowsCompleted != 2 {
		t.Fatalf("client flow lifecycle leaked: active=%d started=%d completed=%d failed=%d", got.ActiveFlows, got.FlowsStarted, got.FlowsCompleted, got.FlowsFailed)
	}
	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func TestQUICFlowSurvivesOneLaneFailure(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go slowEchoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableQUIC: true,
		MaxLanes: 2, ChunkSize: 4 * 1024, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: packetConn.LocalAddr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportQUIC,
		InitialLanes: 2, MaxLanes: 2, ChunkSize: 4 * 1024, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServePacketConn(ctx, packetConn) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	payload := bytes.Repeat([]byte("wanopt-lane-recovery-"), 128*1024)
	writeErr := make(chan error, 1)
	go func() {
		_, err := conn.Write(payload)
		if tcp, ok := conn.(*net.TCPConn); ok && err == nil {
			err = tcp.CloseWrite()
		}
		writeErr <- err
	}()

	deadline := time.Now().Add(5 * time.Second)
	var failedLane *mpLane
	for time.Now().Before(deadline) && failedLane == nil {
		server.sessionsMu.RLock()
		for _, sessionFlow := range server.sessions {
			lanes := sessionFlow.flow.healthyLanes()
			if len(lanes) >= 2 {
				failedLane = lanes[0]
				break
			}
		}
		server.sessionsMu.RUnlock()
		if failedLane == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if failedLane == nil {
		t.Fatal("two-lane flow was not established")
	}
	if err := failedLane.fc.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("recovered echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func TestAutoTransportFallsBackToTCPWhenUDPUnavailable(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go echoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: serverListener.Addr().String(), Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, EnableQUIC: false, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: serverListener.Addr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportAuto, FallbackDelay: 10 * time.Millisecond, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServeListener(ctx, serverListener) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
	defer conn.Close()
	payload := []byte("auto-fallback")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("fallback echo mismatch: got %q, want %q", got, payload)
	}

	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func TestAutoFlowInstallsTCPRescueAfterAllQUICLanesFail(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go slowEchoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp", serverListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: serverListener.Addr().String(), Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, EnableQUIC: true,
		MaxLanes: 2, ChunkSize: 4 * 1024, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: serverListener.Addr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportAuto, FallbackDelay: 300 * time.Millisecond,
		MaxLanes: 2, ChunkSize: 4 * 1024, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 3)
	go func() { errorsCh <- server.ServeListener(ctx, serverListener) }()
	go func() { errorsCh <- server.ServePacketConn(ctx, packetConn) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	payload := bytes.Repeat([]byte("wanopt-auto-rescue-"), 32*1024)
	writeErr := make(chan error, 1)
	go func() {
		_, writeErrValue := conn.Write(payload)
		if tcp, ok := conn.(*net.TCPConn); ok && writeErrValue == nil {
			writeErrValue = tcp.CloseWrite()
		}
		writeErr <- writeErrValue
	}()

	deadline := time.Now().Add(5 * time.Second)
	closedQUIC := false
	rescuedTCP := false
	for time.Now().Before(deadline) {
		server.sessionsMu.RLock()
		var sessionFlow *serverFlow
		for _, candidate := range server.sessions {
			sessionFlow = candidate
			break
		}
		var lane *mpLane
		if sessionFlow != nil {
			lanes := sessionFlow.flow.healthyLanes()
			if len(lanes) > 0 {
				lane = lanes[0]
			}
		}
		server.sessionsMu.RUnlock()
		if lane != nil && lane.kind == TransportQUIC && !closedQUIC {
			_ = lane.fc.Close()
			closedQUIC = true
		}
		if closedQUIC && lane != nil && lane.kind == TransportTCP {
			rescuedTCP = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !closedQUIC {
		t.Fatal("initial QUIC lane was not established")
	}
	if !rescuedTCP {
		t.Fatal("TCP rescue lane was not established")
	}

	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("TCP rescue echo mismatch: got %d bytes, want %d", len(got), len(payload))
	}

	cancel()
	for i := 0; i < 3; i++ {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func TestCompletedFlowTombstoneReplaysFinalAck(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go echoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: serverListener.Addr().String(), Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, EnableQUIC: false,
		Logger: logger, Metrics: metrics.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: serverListener.Addr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportTCP, Logger: logger, Metrics: metrics.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServeListener(ctx, serverListener) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
	payload := []byte("tombstone")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	got, err := io.ReadAll(conn)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo mismatch: got %q, want %q", got, payload)
	}

	var sessionID [16]byte
	var flowID uint64
	var finalSequence uint64
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		server.sessionsMu.RLock()
		for id, sessionFlow := range server.sessions {
			if sessionFlow.completed.Load() {
				sessionID = id
				flowID = sessionFlow.flow.flowID
				finalSequence = sessionFlow.flow.remoteFinSequence.Load()
			}
		}
		server.sessionsMu.RUnlock()
		if flowID != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if flowID == 0 {
		t.Fatal("completed flow tombstone was not retained")
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && server.Metrics().Snapshot().ActiveFlows != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := server.Metrics().Snapshot(); got.ActiveFlows != 0 || got.FlowsCompleted != 1 {
		t.Fatalf("server completion watcher did not release flow: active=%d completed=%d failed=%d", got.ActiveFlows, got.FlowsCompleted, got.FlowsFailed)
	}

	joinCtx, joinCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer joinCancel()
	lane, err := client.openJoinLane(joinCtx, TransportTCP, sessionID, flowID, 99)
	if err != nil {
		t.Fatal(err)
	}
	defer lane.fc.Close()
	ack, err := lane.fc.Read()
	if err != nil {
		t.Fatal(err)
	}
	if ack.Header.Type != protocol.TypeAck || ack.Header.Flags&protocol.FlagAckFinal == 0 || ack.Header.Sequence != finalSequence {
		t.Fatalf("unexpected tombstone ACK: type=%d flags=%x sequence=%d want=%d", ack.Header.Type, ack.Header.Flags, ack.Header.Sequence, finalSequence)
	}
	fin, err := lane.fc.Read()
	if err != nil {
		t.Fatal(err)
	}
	if fin.Header.Type != protocol.TypeClose || fin.Header.Flags&protocol.FlagFin == 0 {
		t.Fatalf("unexpected tombstone FIN: type=%d flags=%x", fin.Header.Type, fin.Header.Flags)
	}

	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func TestFullApplicationCloseAbortsKeepAliveDestination(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	response := bytes.Repeat([]byte("fixed-response-"), 4096)
	go holdResponseDestination(destinationListener, response)

	certificate, roots := testCertificate(t)
	secret := []byte("integration-test-secret-value-32bytes")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverMetrics := metrics.New()
	server, err := NewServer(ServerConfig{
		ListenAddr: serverListener.Addr().String(), Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableTCP: true, EnableQUIC: false,
		Logger: logger, Metrics: serverMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientMetrics := metrics.New()
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: serverListener.Addr().String(), ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportTCP, Logger: logger, Metrics: clientMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServeListener(ctx, serverListener) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	conn := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
	if _, err := conn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(response))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("response mismatch: got %d bytes, want %d", len(got), len(response))
	}
	// Deliberately close the application socket fully. There is no TCP
	// CloseWrite here; the tunnel must carry the explicit full-close marker.
	_ = conn.Close()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if serverMetrics.Snapshot().ActiveFlows == 0 && clientMetrics.Snapshot().ActiveFlows == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := serverMetrics.Snapshot(); got.ActiveFlows != 0 || got.FlowsCompleted != 1 || got.FlowsFailed != 0 {
		t.Fatalf("server did not close keep-alive flow cleanly: active=%d completed=%d failed=%d", got.ActiveFlows, got.FlowsCompleted, got.FlowsFailed)
	}
	if got := clientMetrics.Snapshot(); got.ActiveFlows != 0 || got.FlowsCompleted != 1 || got.FlowsFailed != 0 {
		t.Fatalf("client did not close full application flow cleanly: active=%d completed=%d failed=%d", got.ActiveFlows, got.FlowsCompleted, got.FlowsFailed)
	}

	cancel()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("service shutdown: %v", err)
		}
	}
}

func dialTestSOCKS(t *testing.T, proxyAddr, destinationAddr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatal(err)
	}
	if method != [2]byte{5, 0} {
		t.Fatalf("method response %v", method)
	}
	host, portText, err := net.SplitHostPort(destinationAddr)
	if err != nil {
		t.Fatal(err)
	}
	ip := net.ParseIP(host).To4()
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	request := append([]byte{5, 1, 0, 1}, ip...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[1] != 0 {
		t.Fatalf("SOCKS connect failed: %v", reply)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn
}

func slowEchoDestination(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			buf := make([]byte, 4*1024)
			for {
				n, readErr := conn.Read(buf)
				if n > 0 {
					time.Sleep(time.Millisecond)
					if err := writeFull(conn, buf[:n]); err != nil {
						return
					}
				}
				if readErr != nil {
					return
				}
			}
		}()
	}
}

func echoDestination(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}()
	}
}

func holdResponseDestination(listener net.Listener, response []byte) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			if err := writeFull(conn, response); err != nil {
				return
			}
			// Keep the destination socket alive until the proxy receives the
			// application's full-close marker and closes this connection.
			_, _ = conn.Read(make([]byte, 1))
		}()
	}
}
