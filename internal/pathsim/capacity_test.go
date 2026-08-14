package pathsim

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// TestForwardingCapacity measures what the emulator itself can forward, which
// bounds every transport number taken through it. An emulated path that
// saturates below the configured rate silently becomes the bottleneck, and a
// transport measured against it is measured against the instrument.
func TestForwardingCapacity(t *testing.T) {
	for _, rtt := range []time.Duration{0, 20 * time.Millisecond, 200 * time.Millisecond} {
		rtt := rtt
		t.Run(fmt.Sprint(rtt), func(t *testing.T) {
			sink, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close()
			relay, err := New("127.0.0.1:0", sink.LocalAddr().String(), Config{
				OneWayDelay: rtt / 2,
			})
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

			// Drain the sink so it never applies backpressure.
			go func() {
				buf := make([]byte, 2048)
				for {
					if _, _, err := sink.ReadFrom(buf); err != nil {
						return
					}
				}
			}()

			payload := make([]byte, 1200)
			const duration = 3 * time.Second
			deadline := time.Now().Add(duration)
			sent := 0
			for time.Now().Before(deadline) {
				for i := 0; i < 64; i++ {
					if _, err := client.WriteTo(payload, target); err == nil {
						sent++
					}
				}
			}
			// Let the delay queue drain what it holds.
			time.Sleep(rtt + 500*time.Millisecond)
			up, _ := relay.Stats()
			mbits := float64(up.BytesOut) * 8 / duration.Seconds() / 1e6
			t.Logf("rtt=%v offered=%d packets forwarded=%d capacity=%.0f Mbit/s",
				rtt, sent, up.PacketsOut, mbits)
			if mbits < 100 {
				t.Fatalf("emulator forwards only %.0f Mbit/s at %v; every transport number through it is instrument-limited", mbits, rtt)
			}
		})
	}
}
