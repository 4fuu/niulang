package coded

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	mathrand "math/rand"
	"testing"
	"time"

	"github.com/apernet/quic-go"
	wancongestion "github.com/icourses-dev/wanopt/internal/congestion"
	"github.com/icourses-dev/wanopt/internal/pathsim"
)

func testTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "coded.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"coded.test"},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	server := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
		NextProtos:   []string{"coded-test"},
	}
	client := &tls.Config{RootCAs: roots, ServerName: "coded.test", NextProtos: []string{"coded-test"}}
	return server, client
}

// liveChannel is the China-US path as measured on 2026-08-13: a token bucket
// at 25 Mbit/s refilled every 8 ms, with an independent 42% erasure segment
// behind it. See docs/PATH-CHARACTER-20260813.md and
// pathsim.TestTheEmulatorReproducesTheMeasuredPath.
func liveChannel() pathsim.Config {
	return pathsim.Config{
		OneWayDelay:         150 * time.Millisecond,
		RateBytesPerSec:     uint64(25e6 / 8),
		PolicerRefillPeriod: 8 * time.Millisecond,
		LossRate:            0.42,
		Seed:                23,
	}
}

// quicPair brings up a QUIC connection whose packets cross the emulated path.
func quicPair(t *testing.T, cfg pathsim.Config) (client, server *quic.Conn) {
	t.Helper()
	client, server = quicPairWithout(t, cfg)
	// The controller decides whether any of this is reachable. Across this
	// channel a loss-responsive sender gives the path away -- 0.13 Mbit/s for
	// quic-go's default, 5.56 for the BBR port this project ships -- so a
	// measurement taken over one of those measures the controller and not the
	// layer above it.
	client.SetCongestionControl(wancongestion.NewErasureSender(client.InitialPacketSize()))
	server.SetCongestionControl(wancongestion.NewErasureSender(server.InitialPacketSize()))
	return client, server
}

// quicPairWithout leaves the congestion controller to the caller.
func quicPairWithout(t *testing.T, cfg pathsim.Config) (client, server *quic.Conn) {
	t.Helper()
	serverTLS, clientTLS := testTLS(t)
	quicCfg := &quic.Config{
		EnableDatagrams:                true,
		MaxIdleTimeout:                 90 * time.Second,
		KeepAlivePeriod:                5 * time.Second,
		InitialStreamReceiveWindow:     8 << 20,
		InitialConnectionReceiveWindow: 16 << 20,
		MaxStreamReceiveWindow:         64 << 20,
		MaxConnectionReceiveWindow:     128 << 20,
	}

	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, quicCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	relay, err := pathsim.New("127.0.0.1:0", listener.Addr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })

	accepted := make(chan *quic.Conn, 1)
	go func() {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			return
		}
		accepted <- conn
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err = quic.DialAddr(ctx, relay.LocalAddr(), clientTLS, quicCfg)
	if err != nil {
		t.Fatalf("dial across the emulated path: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseWithError(0, "") })

	select {
	case server = <-accepted:
	case <-time.After(30 * time.Second):
		t.Fatal("server did not accept across the emulated path")
	}
	t.Cleanup(func() { _ = server.CloseWithError(0, "") })
	return client, server
}

// The whole design, end to end, on a real QUIC connection across the path this
// project targets: coded datagrams against the reliable stream the transport
// uses today, carrying the same bytes over the same connection.
//
// The stream is not a straw man. It is exactly what every lane in this
// repository runs on now, and it has QUIC's own loss recovery, pacing and
// congestion control behind it.
func TestCodedDatagramsAgainstAReliableStream(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	const payloadBytes = 1 << 20
	payload := make([]byte, payloadBytes)
	mathrand.New(mathrand.NewSource(31)).Read(payload)

	t.Run("coded datagrams", func(t *testing.T) {
		client, server := quicPair(t, liveChannel())
		sendCarrier, err := NewQUICCarrier(client)
		if err != nil {
			t.Fatal(err)
		}
		recvCarrier, err := NewQUICCarrier(server)
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{
			ShardBytes:           ShardBytesFor(DefaultDatagramBytes),
			RoundTrip:            300 * time.Millisecond,
			MaxOutstandingBlocks: 16,
		}
		a := NewChannel(sendCarrier, cfg)
		b := NewChannel(recvCarrier, cfg)
		defer a.Close()
		defer b.Close()

		start := time.Now()
		got := transfer(t, a, b, payload, 120*time.Second)
		elapsed := time.Since(start)
		if len(got) != len(payload) {
			t.Fatalf("received %d bytes of %d", len(got), len(payload))
		}
		for i := range got {
			if got[i] != payload[i] {
				t.Fatalf("byte %d differs", i)
			}
		}
		snap, _ := b.Snapshot()
		_, plan := a.Snapshot()
		t.Logf("coded: %.2f Mbit/s in %v; measured loss=%.3f floor=%.3f burst=%.2f memoryless=%v; plan (%d,%d) rate=%.3f",
			float64(payloadBytes)*8/elapsed.Seconds()/1e6, elapsed.Round(time.Millisecond),
			snap.Loss, snap.Floor, snap.BurstFactor, snap.Memoryless, plan.K, plan.N, plan.Rate)
	})

	t.Run("reliable stream", func(t *testing.T) {
		client, server := quicPair(t, liveChannel())
		done := make(chan int, 1)
		go func() {
			stream, err := server.AcceptStream(context.Background())
			if err != nil {
				done <- -1
				return
			}
			got, err := io.ReadAll(io.LimitReader(stream, payloadBytes))
			if err != nil && len(got) < payloadBytes {
				t.Logf("stream read: %v", err)
			}
			done <- len(got)
		}()
		stream, err := client.OpenStreamSync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		go func() {
			if _, err := stream.Write(payload); err != nil {
				t.Logf("stream write: %v", err)
			}
		}()
		select {
		case n := <-done:
			elapsed := time.Since(start)
			if n != payloadBytes {
				t.Logf("stream delivered %d of %d bytes in %v", n, payloadBytes, elapsed.Round(time.Millisecond))
				return
			}
			t.Logf("stream: %.2f Mbit/s in %v",
				float64(payloadBytes)*8/elapsed.Seconds()/1e6, elapsed.Round(time.Millisecond))
		case <-time.After(120 * time.Second):
			t.Logf("stream did not finish 1 MiB in 120 s across a 42%% erasure channel")
		}
	})
}
