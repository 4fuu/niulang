package pathsim

import (
	"bytes"
	"math/rand"
	"net"
	"testing"
	"time"
)

// echoServer returns a UDP socket that echoes every datagram back to its
// sender, which is enough to observe the emulator's delay and loss behavior.
func echoServer(t *testing.T) net.PacketConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = conn.WriteTo(buf[:n], addr)
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestRelayAppliesRoundTripDelay(t *testing.T) {
	server := echoServer(t)
	relay, err := New("127.0.0.1:0", server.LocalAddr().String(), Config{OneWayDelay: 40 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	target, err := net.ResolveUDPAddr("udp", relay.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("round trip")
	started := time.Now()
	if _, err := client.WriteTo(payload, target); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("echoed payload = %q, want %q", buf[:n], payload)
	}
	// The emulator adds the one-way delay in each direction. Only the lower
	// bound is asserted: scheduling jitter can add to it but never remove it.
	if elapsed < 80*time.Millisecond {
		t.Fatalf("round trip took %s, want at least 80ms", elapsed)
	}
}

func TestRelayDropsAtConfiguredLossRate(t *testing.T) {
	server := echoServer(t)
	relay, err := New("127.0.0.1:0", server.LocalAddr().String(), Config{LossRate: 0.5, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()

	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	target, err := net.ResolveUDPAddr("udp", relay.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	const sent = 400
	for range sent {
		if _, err := client.WriteTo([]byte("x"), target); err != nil {
			t.Fatal(err)
		}
	}
	// Give the relay time to process the burst before sampling counters.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if up, _ := relay.Stats(); up.PacketsIn >= sent {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	up, _ := relay.Stats()
	if up.PacketsIn < sent {
		t.Fatalf("relay observed %d of %d packets", up.PacketsIn, sent)
	}
	// A seeded Bernoulli process at p=0.5 over 400 packets sits far inside
	// this band; the test is checking the loss model is applied at roughly the
	// configured rate, not the precise draw.
	if up.PacketsLost < sent*3/10 || up.PacketsLost > sent*7/10 {
		t.Fatalf("dropped %d of %d packets, want roughly half", up.PacketsLost, sent)
	}
}

func TestRelayIsReproducibleForOneSeed(t *testing.T) {
	losses := make([]uint64, 2)
	for attempt := range losses {
		server := echoServer(t)
		relay, err := New("127.0.0.1:0", server.LocalAddr().String(), Config{LossRate: 0.3, Seed: 99})
		if err != nil {
			t.Fatal(err)
		}
		client, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		target, err := net.ResolveUDPAddr("udp", relay.LocalAddr())
		if err != nil {
			t.Fatal(err)
		}
		const sent = 200
		for range sent {
			if _, err := client.WriteTo([]byte("x"), target); err != nil {
				t.Fatal(err)
			}
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if up, _ := relay.Stats(); up.PacketsIn >= sent {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		up, _ := relay.Stats()
		losses[attempt] = up.PacketsLost
		_ = client.Close()
		_ = relay.Close()
	}
	// Reproducibility is the whole reason this emulator exists: two runs of
	// the same seed over the same packet count must drop the same packets.
	if losses[0] != losses[1] {
		t.Fatalf("seeded loss was not reproducible: %d then %d", losses[0], losses[1])
	}
}

func TestRelayRejectsInvalidLossRate(t *testing.T) {
	if _, err := New("127.0.0.1:0", "127.0.0.1:1", Config{LossRate: 1}); err == nil {
		t.Fatal("loss rate of 1 was accepted")
	}
	if _, err := New("127.0.0.1:0", "127.0.0.1:1", Config{LossRate: -0.1}); err == nil {
		t.Fatal("negative loss rate was accepted")
	}
}

func TestBottleneckQueueTailDrops(t *testing.T) {
	// A slow bottleneck with a small buffer must drop rather than buffer an
	// unbounded burst; otherwise the emulator would report a queueing delay no
	// real router would provide.
	d := &direction{}
	cfg := Config{RateBytesPerSec: 1000, QueueBytes: 2000}.withDefaults()
	cfg.QueueBytes = 2000
	now := time.Now()
	accepted := 0
	for range 20 {
		if _, ok := d.schedule(now, 500, cfg); ok {
			accepted++
		}
	}
	if accepted == 20 {
		t.Fatal("bottleneck accepted an unbounded burst")
	}
	if accepted == 0 {
		t.Fatal("bottleneck dropped every packet")
	}
	if got := d.packetsDropped.Load(); got != uint64(20-accepted) {
		t.Fatalf("tail drop count = %d, want %d", got, 20-accepted)
	}
}

func TestBurstLossProducesRunsAtTheConfiguredRate(t *testing.T) {
	// Correlated loss is a different regime for a transport than the same
	// average spread evenly, so the model has to deliver both the requested
	// long-run rate and genuinely clustered drops.
	d := &direction{rng: rand.New(rand.NewSource(11))}
	cfg := Config{LossRate: 0.2, LossBurstPackets: 12}
	const trials = 200000
	dropped, runs, inRun := 0, 0, false
	for range trials {
		if d.dropLocked(cfg) {
			dropped++
			if !inRun {
				runs++
				inRun = true
			}
			continue
		}
		inRun = false
	}
	rate := float64(dropped) / trials
	if rate < 0.17 || rate > 0.23 {
		t.Fatalf("long-run drop rate %.3f, want about 0.20", rate)
	}
	meanRun := float64(dropped) / float64(runs)
	if meanRun < 9 || meanRun > 15 {
		t.Fatalf("mean burst length %.1f packets, want about 12", meanRun)
	}
}

func TestIndependentLossHasNoBurstStructure(t *testing.T) {
	d := &direction{rng: rand.New(rand.NewSource(11))}
	cfg := Config{LossRate: 0.2}
	const trials = 200000
	dropped, runs, inRun := 0, 0, false
	for range trials {
		if d.dropLocked(cfg) {
			dropped++
			if !inRun {
				runs++
				inRun = true
			}
			continue
		}
		inRun = false
	}
	// Bernoulli runs average 1/(1-p) = 1.25 packets.
	if meanRun := float64(dropped) / float64(runs); meanRun > 1.5 {
		t.Fatalf("independent loss produced %.2f-packet runs, want about 1.25", meanRun)
	}
}
