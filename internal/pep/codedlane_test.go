package pep

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/pathsim"
)

// codedPair brings up a server and a client, optionally with a lossy emulated
// path between them, and returns the client's SOCKS listener address.
func codedPair(t *testing.T, coded bool, path *pathsim.Config) (socks string, destination net.Listener) {
	t.Helper()
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destinationListener.Close() })
	go echoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	secret := []byte("coded-lane-test-secret-value-32b")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: secret,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, EnableQUIC: true, Logger: logger,
		Metrics: metrics.New(), HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	remote := packetConn.LocalAddr().String()
	if path != nil {
		relay, err := pathsim.New("127.0.0.1:0", remote, *path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = relay.Close() })
		remote = relay.LocalAddr()
	}

	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: remote, ServerName: "wanopt.test",
		Secret: secret, RootCAs: roots, Transport: TransportQUIC,
		EnableQUICPool: !coded, CodedLanes: coded,
		InitialLanes: 1, MaxLanes: 1, Logger: logger, Metrics: metrics.New(),
		Congestion: CongestionErasure,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.ServePacketConn(ctx, packetConn) }()
	go func() { _ = client.ServeListener(ctx, clientListener) }()
	return clientListener.Addr().String(), destinationListener
}

// socksDial opens a SOCKS5 connection through the proxy to the destination.
func socksDial(t *testing.T, socks string, destination net.Listener, deadline time.Duration) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", socks, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(destination.Addr().String())
	ip := net.ParseIP(host).To4()
	port, _ := strconv.Atoi(portText)
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
	return conn
}

// A coded lane has to carry the session's frames as faithfully as a stream
// does. Reliability is the layer's whole contract: it repairs with a code
// first and retransmission second, and the application above must not be able
// to tell.
func TestACodedLaneCarriesAFlowIntact(t *testing.T) {
	socks, destination := codedPair(t, true, nil)
	conn := socksDial(t, socks, destination, 30*time.Second)
	defer conn.Close()

	payload := make([]byte, 96*1024)
	rand.New(rand.NewSource(3)).Read(payload)
	go func() { _, _ = conn.Write(payload) }()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back through a coded lane: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("a coded lane corrupted the flow")
	}
}

// The same, with the path this project targets in the way. A code sized for a
// 42% erasure channel is exactly what this lane is for, so it has to hold when
// the channel is actually there.
func TestACodedLaneCarriesAFlowAcrossAnErasureChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	path := pathsim.Config{
		OneWayDelay:         150 * time.Millisecond,
		RateBytesPerSec:     uint64(25e6 / 8),
		PolicerRefillPeriod: 8 * time.Millisecond,
		LossRate:            0.42,
		Seed:                17,
	}
	socks, destination := codedPair(t, true, &path)
	conn := socksDial(t, socks, destination, 120*time.Second)
	defer conn.Close()

	payload := make([]byte, 48*1024)
	rand.New(rand.NewSource(4)).Read(payload)
	start := time.Now()
	go func() { _, _ = conn.Write(payload) }()

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read back across a 42%% erasure channel: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("a coded lane corrupted the flow across an erasure channel")
	}
	t.Logf("%d bytes echoed across a 42%% erasure channel in %v",
		len(payload), time.Since(start).Round(time.Millisecond))
}

// The server decides what a connection carries by racing its first stream
// against its first datagram, so one server must serve both kinds without
// being told which to expect. Deciding by configuration instead would make a
// mismatch hang rather than fail.
func TestOneServerServesBothLaneKinds(t *testing.T) {
	for _, coded := range []bool{false, true} {
		name := "stream lane"
		if coded {
			name = "coded lane"
		}
		t.Run(name, func(t *testing.T) {
			socks, destination := codedPair(t, coded, nil)
			conn := socksDial(t, socks, destination, 30*time.Second)
			defer conn.Close()

			want := []byte("the server was not told which kind this is")
			if _, err := conn.Write(want); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(want))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("echo through a %s: %v", name, err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("%s echoed %q", name, got)
			}
		})
	}
}
