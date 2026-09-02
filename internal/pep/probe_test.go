package pep

import (
	"context"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/identity"
	"github.com/4fuu/niulang/internal/metrics"
)

func TestClientProbeAuthenticatesProviderWithoutOpeningFlow(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serverMetrics := metrics.New()
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
		Logger: logger, Metrics: serverMetrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.ServePacketConn(ctx, packetConn) }()

	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: packetConn.LocalAddr().String(),
		Credentials: clientCredentials, EnableQUICPool: false, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	probeContext, probeCancel := context.WithTimeout(ctx, 5*time.Second)
	result, err := client.Probe(probeContext)
	probeCancel()
	if err != nil {
		t.Fatal(err)
	}
	if result.Transport != TransportQUIC {
		t.Fatalf("probe transport %q, want %q", result.Transport, TransportQUIC)
	}
	if result.Latency <= 0 || result.Latency > 5*time.Second {
		t.Fatalf("invalid probe latency %v", result.Latency)
	}
	if snapshot := serverMetrics.Snapshot(); snapshot.FlowsStarted != 0 {
		t.Fatalf("probe opened a destination flow: %+v", snapshot)
	}

	cancel()
	if err := <-serverResult; err != nil {
		t.Fatalf("server shutdown: %v", err)
	}
}

func TestClientProbeRejectsRevokedDevice(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	leaf, err := x509.ParseCertificate(clientCredentials.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	principal, err := identity.PrincipalFromCertificate(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if err := serverCredentials.Store.RevokeDevice(principal.DeviceID, time.Now()); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
		Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.ServePacketConn(ctx, listener) }()

	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: listener.LocalAddr().String(),
		Credentials: clientCredentials, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	probeContext, probeCancel := context.WithTimeout(ctx, 2*time.Second)
	_, probeErr := client.Probe(probeContext)
	probeCancel()
	if probeErr == nil {
		t.Fatal("revoked device unexpectedly authenticated")
	}

	cancel()
	if err := <-serverResult; err != nil {
		t.Fatalf("server shutdown: %v", err)
	}
}
