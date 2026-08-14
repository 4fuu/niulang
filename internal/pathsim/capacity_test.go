package pathsim

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// blast offers packets as fast as it can for a fixed period and reports what
// the relay delivered, in Mbit/s.
func blast(t *testing.T, cfg Config, duration time.Duration) float64 {
	t.Helper()
	sink, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	relay, err := New("127.0.0.1:0", sink.LocalAddr().String(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	target, err := net.ResolveUDPAddr("udp", relay.LocalAddr())
	if err != nil {
		t.Fatal(err)
	}
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, _, err := sink.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	payload := make([]byte, 1200)
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		for i := 0; i < 32; i++ {
			_, _ = client.WriteTo(payload, target)
		}
	}
	time.Sleep(2*cfg.OneWayDelay + 500*time.Millisecond)
	up, _ := relay.Stats()
	return float64(up.BytesOut) * 8 / duration.Seconds() / 1e6
}

// TestForwardingCapacity measures what the emulator itself can forward with no
// rate limiter, which bounds every transport number taken through it.
func TestForwardingCapacity(t *testing.T) {
	for _, rtt := range []time.Duration{0, 20 * time.Millisecond, 200 * time.Millisecond} {
		got := blast(t, Config{OneWayDelay: rtt / 2}, 3*time.Second)
		t.Logf("rtt=%v unlimited capacity=%.0f Mbit/s", rtt, got)
		if got < 100 {
			t.Fatalf("emulator forwards only %.0f Mbit/s at %v", got, rtt)
		}
	}
}

// TestRateLimiterDeliversItsConfiguredRate is the calibration that matters for
// any utilisation claim. A transport is said to under-use a link by comparing
// its goodput with the configured rate; that comparison is only meaningful if
// the emulator's own rate limiter delivers the rate it was asked for. It runs
// at every point of the bandwidth-delay grid the benchmarks use, because the
// limiter's queue is sized from the product and is exactly where a fault would
// hide.
func TestRateLimiterDeliversItsConfiguredRate(t *testing.T) {
	for _, rtt := range []time.Duration{50, 200, 400} {
		for _, mbits := range []float64{50, 200, 400} {
			rtt, mbits := rtt*time.Millisecond, mbits
			name := fmt.Sprintf("rtt%v_rate%.0f", rtt, mbits)
			t.Run(name, func(t *testing.T) {
				cfg := Config{
					OneWayDelay:     rtt / 2,
					RateBytesPerSec: uint64(mbits * 1e6 / 8),
				}
				got := blast(t, cfg, 4*time.Second)
				ratio := got / mbits
				t.Logf("rtt=%v rate=%.0f delivered=%.1f Mbit/s ratio=%.2f", rtt, mbits, got, ratio)
				if ratio < 0.9 {
					t.Fatalf("rate limiter delivered %.2f of its configured rate at rtt=%v rate=%.0f; "+
						"utilisation measured against this path is measuring the emulator", ratio, rtt, mbits)
				}
			})
		}
	}
}
