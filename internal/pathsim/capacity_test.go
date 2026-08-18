package pathsim

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

// blast keeps a relay busy, waits for its delay queue to reach steady state,
// and reports delivery during a fixed observation window. offeredMbits == 0
// offers packets as fast as the host can generate them; otherwise the sender
// is paced to that rate so a calibration does not turn into a CPU-contention
// test merely because its source can flood much faster than its target.
func blast(t *testing.T, cfg Config, duration time.Duration, offeredMbits float64) float64 {
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
	stop := make(chan struct{})
	var sender sync.WaitGroup
	sender.Add(1)
	go func() {
		defer sender.Done()
		const burstPackets = 32
		next := time.Now()
		var burstInterval time.Duration
		if offeredMbits > 0 {
			burstBits := float64(burstPackets * len(payload) * 8)
			burstInterval = time.Duration(burstBits / (offeredMbits * 1e6) * float64(time.Second))
		}
		for {
			if burstInterval > 0 {
				if wait := time.Until(next); wait > 0 {
					timer := time.NewTimer(wait)
					select {
					case <-timer.C:
					case <-stop:
						if !timer.Stop() {
							select {
							case <-timer.C:
							default:
							}
						}
						return
					}
				}
				next = next.Add(burstInterval)
			}
			for range burstPackets {
				select {
				case <-stop:
					return
				default:
					_, _ = client.WriteTo(payload, target)
				}
			}
		}
	}()
	defer func() {
		close(stop)
		sender.Wait()
	}()

	// Two propagation delays plus a scheduler margin lets both the input and
	// output sides settle before the observed interval begins.
	time.Sleep(2*cfg.OneWayDelay + 500*time.Millisecond)
	before, _ := relay.Stats()
	time.Sleep(duration)
	after, _ := relay.Stats()
	return float64(after.BytesOut-before.BytesOut) * 8 / duration.Seconds() / 1e6
}

// TestForwardingCapacity measures what the emulator itself can forward with no
// rate limiter, which bounds every transport number taken through it.
func TestForwardingCapacity(t *testing.T) {
	for _, rtt := range []time.Duration{0, 20 * time.Millisecond, 200 * time.Millisecond} {
		got := blast(t, Config{OneWayDelay: rtt / 2}, 3*time.Second, 0)
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
	rates := []float64{50, 200, 400}
	if raceDetectorEnabled {
		// Race instrumentation capped the two-core hosted runner near 180
		// Mbit/s, so asking it to generate 200 or 400 Mbit/s measures CPU
		// instrumentation rather than the rate limiter. Keep one attainable
		// cell at every RTT for race coverage; the normal deep job retains the
		// complete calibration matrix.
		rates = []float64{50}
	}
	for _, rtt := range []time.Duration{50, 200, 400} {
		rtt := rtt * time.Millisecond
		unshaped := blast(t, Config{OneWayDelay: rtt / 2}, 2*time.Second, 0)
		t.Logf("rtt=%v unshaped capacity=%.0f Mbit/s", rtt, unshaped)
		for _, mbits := range rates {
			mbits := mbits
			name := fmt.Sprintf("rtt%v_rate%.0f", rtt, mbits)
			t.Run(name, func(t *testing.T) {
				// A shaped result is meaningful only if this runner can offer
				// comfortably more than the requested rate through the same
				// delayed relay. Slow hosted runners legitimately skip only the
				// unattainable cells; they still exercise every lower cell.
				offered := mbits * 1.25
				if unshaped < offered {
					t.Skipf("host forwards %.0f Mbit/s unshaped at rtt=%v; need %.0f Mbit/s to calibrate a %.0f Mbit/s limiter",
						unshaped, rtt, offered, mbits)
				}
				cfg := Config{
					OneWayDelay:     rtt / 2,
					RateBytesPerSec: uint64(mbits * 1e6 / 8),
				}
				got := blast(t, cfg, 4*time.Second, offered)
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
