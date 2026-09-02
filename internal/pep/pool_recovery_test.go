package pep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A pooled connection is a failure domain, but it must not be a recovery
// stampede. Retiring one generation with several live logical flows should
// create exactly one new UDP socket, rejoin every flow as a stream on it, and
// preserve every destination connection.
func TestPooledFlowsRecoverThroughOneReplacementGeneration(t *testing.T) {
	destinationListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer destinationListener.Close()
	go echoDestination(destinationListener)

	certificate, roots := testCertificate(t)
	logWriter := io.Writer(io.Discard)
	if testing.Verbose() {
		logWriter = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelDebug}))
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	server, err := NewServer(ServerConfig{
		ListenAddr: packetConn.LocalAddr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}

	clientListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer clientListener.Close()
	var udpSockets atomic.Int64
	client, err := NewClient(ClientConfig{
		ListenAddr: clientListener.Addr().String(), RemoteAddr: packetConn.LocalAddr().String(),
		LocalAddress: "127.0.0.1", Credentials: roots,
		EnableQUICPool: true, HandshakeTimeout: 3 * time.Second,
		Logger: logger,
		SocketControl: func(network, _ string, _ syscall.RawConn) error {
			if strings.HasPrefix(network, "udp") {
				udpSockets.Add(1)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.ServePacketConn(ctx, packetConn) }()
	go func() { errorsCh <- client.ServeListener(ctx, clientListener) }()

	const flowCount = 8
	flows := make([]net.Conn, 0, flowCount)
	defer func() {
		for _, flow := range flows {
			_ = flow.Close()
		}
	}()
	for i := range flowCount {
		flow := dialTestSOCKS(t, clientListener.Addr().String(), destinationListener.Addr().String())
		_ = flow.SetDeadline(time.Now().Add(15 * time.Second))
		assertEchoRoundTrip(t, flow, fmt.Sprintf("before-reset-%02d", i))
		flows = append(flows, flow)
	}
	baselineSockets := udpSockets.Load()
	if baselineSockets != 1 {
		t.Fatalf("initial pooled flows opened %d UDP sockets, want one", baselineSockets)
	}

	client.closeControlQUICPool("injected generation failure")
	type result struct {
		index int
		err   error
	}
	results := make(chan result, flowCount)
	var workers sync.WaitGroup
	for i, flow := range flows {
		workers.Add(1)
		go func() {
			defer workers.Done()
			payload := []byte(fmt.Sprintf("after-reset-%02d", i))
			if _, err := flow.Write(payload); err != nil {
				results <- result{index: i, err: fmt.Errorf("write: %w", err)}
				return
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(flow, got); err != nil {
				results <- result{index: i, err: fmt.Errorf("read: %w", err)}
				return
			}
			if string(got) != string(payload) {
				results <- result{index: i, err: fmt.Errorf("echo %q, want %q", got, payload)}
				return
			}
			results <- result{index: i}
		}()
	}
	workers.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Errorf("flow %d did not survive generation replacement: %v", result.index, result.err)
		}
	}
	if t.Failed() {
		return
	}
	if got := udpSockets.Load(); got != baselineSockets+1 {
		t.Fatalf("generation recovery opened %d new UDP sockets, want one", got-baselineSockets)
	}
	snapshot := client.Metrics().Snapshot()
	if snapshot.LaneReplacements != flowCount {
		t.Fatalf("lane replacements = %d, want %d", snapshot.LaneReplacements, flowCount)
	}
	for _, flow := range flows {
		_ = flow.Close()
	}
	cancel()
	for range 2 {
		select {
		case err := <-errorsCh:
			if err != nil {
				t.Fatalf("service shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("service shutdown timeout")
		}
	}
}

func TestControlPoolDialOwnershipAndGenerationGuards(t *testing.T) {
	for _, test := range []struct {
		name        string
		resetPool   bool
		wantSockets int64
	}{
		{name: "caller cancellation does not cancel shared dial", wantSockets: 1},
		{name: "path reset supersedes in-flight dial", resetPool: true, wantSockets: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate, roots := testCertificate(t)
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer packetConn.Close()
			server, err := NewServer(ServerConfig{
				ListenAddr: packetConn.LocalAddr().String(), Credentials: certificate,
				DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: logger,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			serverErr := make(chan error, 1)
			go func() { serverErr <- server.ServePacketConn(ctx, packetConn) }()

			entered, release := make(chan struct{}), make(chan struct{})
			var gate sync.Once
			var sockets atomic.Int64
			client, err := NewClient(ClientConfig{
				ListenAddr: "127.0.0.1:0", RemoteAddr: packetConn.LocalAddr().String(),
				LocalAddress: "127.0.0.1", Credentials: roots,
				EnableQUICPool: true,
				DialTimeout:    2 * time.Second, HandshakeTimeout: 2 * time.Second,
				Logger: logger,
				SocketControl: func(_, _ string, _ syscall.RawConn) error {
					sockets.Add(1)
					gate.Do(func() {
						close(entered)
						<-release
					})
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			type laneResult struct {
				lane streamConn
				err  error
			}
			firstCtx, cancelFirst := context.WithCancel(ctx)
			defer cancelFirst()
			first := make(chan laneResult, 1)
			go func() {
				lane, dialErr := client.dialPooledQUICLane(firstCtx, congestionConfig{kind: defaultCongestion()})
				first <- laneResult{lane: lane, err: dialErr}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first pool dial did not reach socket creation")
			}

			if test.resetPool {
				client.closeControlQUICPool("injected path epoch change")
			} else {
				cancelFirst()
			}
			second := make(chan laneResult, 1)
			go func() {
				lane, dialErr := client.dialPooledQUICLane(ctx, congestionConfig{kind: defaultCongestion()})
				second <- laneResult{lane: lane, err: dialErr}
			}()
			if test.resetPool {
				deadline := time.Now().Add(time.Second)
				for sockets.Load() < 2 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			close(release)

			secondResult := <-second
			if secondResult.err != nil {
				t.Fatalf("second waiter did not acquire shared generation: %v", secondResult.err)
			}
			_ = secondResult.lane.Close()
			firstResult := <-first
			if test.resetPool {
				if firstResult.err != nil {
					t.Fatalf("superseded waiter did not retry new generation: %v", firstResult.err)
				}
				_ = firstResult.lane.Close()
			} else if !errors.Is(firstResult.err, context.Canceled) {
				t.Fatalf("cancelled waiter error = %v, want context cancellation", firstResult.err)
			}
			if got := sockets.Load(); got != test.wantSockets {
				t.Fatalf("UDP sockets = %d, want %d", got, test.wantSockets)
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
		})
	}
}

func TestTimedOutControlStreamRetiresItsQUICGeneration(t *testing.T) {
	certificate, roots := testCertificate(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	packetConn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packetConn.Close()
	server, err := NewServer(ServerConfig{
		ListenAddr: packetConn.LocalAddr().String(), Credentials: certificate,
		DestinationPolicy: DestinationPolicy{AllowPrivate: true}, Logger: logger,
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

	first, err := client.dialPooledQUICLane(ctx, congestionConfig{kind: defaultCongestion()})
	if err != nil {
		t.Fatal(err)
	}
	firstPooled, ok := first.(*controlPoolStreamConn)
	if !ok {
		t.Fatalf("pooled lane type = %T", first)
	}
	firstGeneration := firstPooled.generation
	firstPooled.transportFailed(io.EOF)
	client.quicMu.Lock()
	afterEOF := client.quicGeneration
	client.quicMu.Unlock()
	if afterEOF != firstGeneration {
		t.Fatal("ordinary stream EOF retired a healthy pooled generation")
	}

	firstPooled.transportFailed(context.DeadlineExceeded)
	client.quicMu.Lock()
	afterTimeout := client.quicGeneration
	client.quicMu.Unlock()
	if afterTimeout != nil {
		t.Fatal("timed-out stream left its pooled generation reusable")
	}
	_ = first.Close()

	second, err := client.dialPooledQUICLane(ctx, congestionConfig{kind: defaultCongestion()})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondPooled, ok := second.(*controlPoolStreamConn)
	if !ok {
		t.Fatalf("replacement pooled lane type = %T", second)
	}
	if secondPooled.generation == firstGeneration {
		t.Fatal("next lane reused the timed-out QUIC generation")
	}
	if got := sockets.Load(); got != 2 {
		t.Fatalf("UDP sockets = %d, want one replacement generation", got)
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

func TestCancelledWaiterDoesNotStartAControlPoolDial(t *testing.T) {
	_, roots := testCertificate(t)
	var sockets atomic.Int64
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:9",
		LocalAddress: "127.0.0.1", Credentials: roots,
		EnableQUICPool: true,
		SocketControl: func(_, _ string, _ syscall.RawConn) error {
			sockets.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.dialPooledQUICLane(ctx, congestionConfig{kind: defaultCongestion()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled pool dial = %v, want context cancellation", err)
	}
	if got := sockets.Load(); got != 0 {
		t.Fatalf("cancelled waiter created %d UDP sockets, want none", got)
	}
}

func assertEchoRoundTrip(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("write echo payload: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo payload: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("echo = %q, want %q", got, payload)
	}
}
