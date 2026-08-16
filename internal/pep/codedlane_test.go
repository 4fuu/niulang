package pep

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
	"github.com/icourses-dev/wanopt/internal/pathsim"
	"github.com/icourses-dev/wanopt/internal/protocol"
)

// codedPair brings up a server and a client, optionally with a lossy emulated
// path between them, and returns the client's SOCKS listener address.
func codedPair(t *testing.T, pooled bool, path *pathsim.Config) (socks string, destination net.Listener) {
	return codedPairWith(t, pooled, path, echoDestination)
}

// codedPairWith lets a test choose what the destination does with the bytes.
func codedPairWith(t *testing.T, pooled bool, path *pathsim.Config, serve func(net.Listener)) (socks string, destination net.Listener) {
	t.Helper()
	// Every endpoint in this process is loopback, so every test shares one
	// path model and would otherwise size its code from what the test before
	// it measured on a different channel.
	pathmodel.Reset()
	t.Cleanup(pathmodel.Reset)
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destinationListener.Close() })
	go serve(destinationListener)

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
		Metrics: metrics.New(),
		// The first connection to an erasing path spends about five seconds
		// on the QUIC handshake alone, so a bound of five is a coin flip.
		HandshakeTimeout: 30 * time.Second,
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
		EnableQUICPool: pooled,
		InitialLanes:   1, MaxLanes: 1, Logger: logger, Metrics: metrics.New(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = server.ServePacketConn(ctx, packetConn) }()
	go func() { _ = client.ServeListener(ctx, clientListener) }()
	lastClient = client
	return clientListener.Addr().String(), destinationListener
}

// lastClient is the client the most recent harness built, for tests that need
// to call into it rather than through its SOCKS port.
var lastClient *Client

// clientServerAcross brings up a client and server across a path and returns
// the client, for tests that drive it directly rather than through SOCKS.
func clientServerAcross(t *testing.T, path *pathsim.Config) (*Client, net.Listener) {
	t.Helper()
	socks, destination := codedPairWith(t, true, path, echoDestination)
	_ = socks
	return lastClient, destination
}

// dialWithRetries opens a flow, allowing for a path that sometimes loses the
// attempt outright.
func dialWithRetries(t *testing.T, socks string, destination net.Listener, attempts int) net.Conn {
	t.Helper()
	for attempt := 1; ; attempt++ {
		conn, err := trySocksDial(socks, destination, 90*time.Second)
		if err == nil {
			return conn
		}
		if attempt >= attempts {
			t.Fatalf("no flow after %d attempts: %v", attempts, err)
		}
		t.Logf("flow attempt %d failed on a 42%% erasure channel: %v", attempt, err)
	}
}

// socksDial opens a SOCKS5 connection through the proxy to the destination.
func socksDial(t *testing.T, socks string, destination net.Listener, deadline time.Duration) net.Conn {
	t.Helper()
	conn, err := trySocksDial(socks, destination, deadline)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// trySocksDial is socksDial for callers that want to handle a failure.
//
// The deadline bounds the SOCKS negotiation and is then restarted, so what the
// caller gets is its own budget rather than whatever is left of one. Setting it
// once at dial made every caller share a single wall clock with the flow's
// setup, and on a 42% erasure channel setup has no small worst case: a QUIC
// handshake alone can cost tens of seconds there. Two tests failed that way
// before it was fixed here -- one in the blind half of a measurement, one
// under -race while the rest of the tree saturated the machine -- and both
// read as transport failures rather than as the harness running out of time.
func trySocksDial(socks string, destination net.Listener, deadline time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", socks, 5*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		conn.Close()
		return nil, err
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil {
		conn.Close()
		return nil, err
	}
	host, portText, _ := net.SplitHostPort(destination.Addr().String())
	ip := net.ParseIP(host).To4()
	port, _ := strconv.Atoi(portText)
	request := append([]byte{5, 1, 0, 1}, ip...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		conn.Close()
		return nil, err
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		conn.Close()
		return nil, err
	}
	if reply[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("SOCKS connect failed: %v", reply)
	}
	_ = conn.SetDeadline(time.Now().Add(deadline))
	return conn, nil
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
	// This used to fail three times in four, and what fixed it was removing a
	// round trip rather than finding a lost frame. Every flow waited for its
	// open to be acknowledged, and on a channel that erases 42% of packets an
	// exchange that need not happen is an exchange that can fail. With flows
	// answered as soon as their open is queued it passes nine times in ten.
	//
	// Things ruled out along the way, recorded so they are not re-checked: not
	// the split and not the code (it failed 0 of 4 with no coded substrate and
	// 1 of 4 with one), not the congestion controller (it failed on the stock
	// one too), and not QUIC losing a lone small write -- an isolated 61-byte
	// write on an idle stream across this same channel arrived in 555 ms, five
	// times of five, and at the moment of failure the connection had sent
	// 14 KB, received 8 KB, lost one packet and had a 307 ms smoothed round
	// trip.
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

// A flow must cross a clean path and an erasure channel alike, with no
// configuration distinguishing them. On the clean one the coded path declines
// to code and bulk stays on the stream; on the erasing one it codes and bulk
// moves to datagrams. Nothing above knows which happened.
func TestOneBuildServesACleanPathAndAnErasureChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	clean := pathsim.Config{OneWayDelay: 5 * time.Millisecond, Seed: 21}
	erasing := pathsim.Config{
		OneWayDelay: 150 * time.Millisecond, RateBytesPerSec: uint64(25e6 / 8),
		PolicerRefillPeriod: 8 * time.Millisecond, LossRate: 0.42, Seed: 17,
	}
	for _, test := range []struct {
		name string
		path pathsim.Config
	}{{"clean path", clean}, {"erasure channel", erasing}} {
		t.Run(test.name, func(t *testing.T) {
			socks, destination := codedPair(t, false, &test.path)
			conn := socksDial(t, socks, destination, 120*time.Second)
			defer conn.Close()

			payload := make([]byte, 48*1024)
			rand.New(rand.NewSource(4)).Read(payload)
			start := time.Now()
			go func() { _, _ = conn.Write(payload) }()

			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatalf("read back across %s: %v", test.name, err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("%s corrupted the flow", test.name)
			}
			t.Logf("%s: %d bytes echoed in %v", test.name, len(payload),
				time.Since(start).Round(time.Millisecond))
		})
	}
}

// The default controller decides whether the target path is reachable at all,
// so it is not a matter of taste.
//
// Measured live on the China-US link, one lane, three interleaved rounds:
// reno 0.11, 0.10, 0.08 Mbit/s against erasure's 10.31, 11.63, 10.90 -- a
// factor of a hundred, and erasure was the only one that never collapsed. On a
// clean path the erasure controller reduces to BBR, because the floor it
// measures is zero and the correction it applies is one, so defaulting to it
// costs nothing where it is not needed.
func TestTheDefaultControllerIsTheOneThatReachesThePath(t *testing.T) {
	if got := defaultCongestion(); got != CongestionErasure {
		t.Fatalf("default congestion controller is %q, want %q", got, CongestionErasure)
	}
}

// A datagram that arrives before its flow does must wait for it, not be
// thrown away.
//
// Flows open without waiting for the peer to acknowledge them, which is what
// makes opening one cost nothing. It also means the first data frame can
// overtake the open that names it: the frame arrives, nobody has claimed that
// flow yet, and dropping it there is not an unreliable substrate doing its job
// but a race lost by a few hundred microseconds. The session recovers, on a
// timer -- measured, that was every short flow costing 1.055 s on a 300 ms
// path, and on the live link a quarter of them costing 1.05 to 1.79 s.
func TestADatagramWaitsForTheFlowThatOwnsIt(t *testing.T) {
	demux := &bulkDemux{flows: make(map[uint64]*subscription)}
	const flowID = 42
	early := protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeData, FlowID: flowID, Sequence: 0,
	}, Payload: []byte("arrived before the flow existed")}

	demux.mu.Lock()
	demux.holdLocked(early)
	demux.mu.Unlock()

	frames := demux.subscribe(flowID)
	select {
	case got := <-frames:
		if string(got.Payload) != string(early.Payload) {
			t.Fatalf("held frame came back as %q", got.Payload)
		}
	default:
		t.Fatal("a frame that arrived before its flow was dropped, so the flow " +
			"waits for the session to re-issue what had already arrived")
	}

	// What is held is bounded, or a peer naming flows that never arrive would
	// cost memory without limit.
	demux.mu.Lock()
	for i := 0; i < maxHeldFrames*2; i++ {
		demux.holdLocked(protocol.Frame{Header: protocol.Header{
			Version: protocol.Version, Type: protocol.TypeData, FlowID: uint64(1000 + i),
		}, Payload: []byte("x")})
	}
	held := 0
	for _, frames := range demux.held {
		held += len(frames)
	}
	demux.mu.Unlock()
	if held > maxHeldFrames {
		t.Fatalf("held %d frames for flows that do not exist, want at most %d", held, maxHeldFrames)
	}
}
