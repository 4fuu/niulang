package pep

import (
	"io"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/pathmodel"
	"github.com/icourses-dev/wanopt/internal/pathsim"
)

// requestResponse serves a destination that answers each small request with a
// small response, which is what an interactive flow actually looks like.
func requestResponse(size int) func(net.Listener) {
	return func(listener net.Listener) {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 16)
				reply := make([]byte, size)
				for {
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					if _, err := c.Write(reply); err != nil {
						return
					}
				}
			}(conn)
		}
	}
}

// A small request and its small reply are the case the erasure channel treats
// worst. Three packets at 38% loss means three quarters of exchanges lose one,
// and with nothing behind it to trigger a fast retransmit the recovery is a
// probe timeout -- a round trip, then another if the probe is lost too.
//
// Coding repairs it without a round trip, but only if the path is known to
// erase before the exchange starts. This measures both, so the difference is
// the seeding rather than an opinion about it.
func TestSmallExchangeLatencyDependsOnKnowingThePath(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	const oneWay = 150 * time.Millisecond
	// A path is an uplink and a peer, and in this harness both are loopback.
	loopback := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	path := pathsim.Config{
		OneWayDelay: oneWay, RateBytesPerSec: uint64(25e6 / 8),
		PolicerRefillPeriod: 8 * time.Millisecond, LossRate: 0.42, Seed: 41,
	}

	exchange := func(t *testing.T, seeded bool) []time.Duration {
		t.Helper()
		socks, destination := codedPairWith(t, true, &path, requestResponse(2700))
		if seeded {
			// What the endpoint pair is already known to do. A long-lived
			// proxy learns this from its own traffic; a fresh one does not,
			// and that is the whole difference being measured.
			pathmodel.Shared(pathKey(loopback, loopback)).Report(99, 0.42, 5000, 1.2e6)
		}
		conn := socksDial(t, socks, destination, 60*time.Second)
		defer conn.Close()

		var samples []time.Duration
		request, reply := make([]byte, 16), make([]byte, 2700)
		for i := 0; i < 12; i++ {
			start := time.Now()
			if _, err := conn.Write(request); err != nil {
				t.Fatal(err)
			}
			if _, err := io.ReadFull(conn, reply); err != nil {
				t.Fatalf("exchange %d: %v", i, err)
			}
			samples = append(samples, time.Since(start))
		}
		return samples
	}

	report := func(name string, samples []time.Duration) time.Duration {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		median := samples[len(samples)/2]
		t.Logf("%s: median %v  p90 %v  max %v (round trip %v)",
			name, median.Round(time.Millisecond),
			samples[int(0.9*float64(len(samples)-1))].Round(time.Millisecond),
			samples[len(samples)-1].Round(time.Millisecond), 2*oneWay)
		return median
	}

	blind := report("path unknown", exchange(t, false))
	pathmodel.Shared(pathKey(loopback, loopback)).Report(99, 0.42, 5000, 1.2e6)
	knowing := report("path known", exchange(t, true))

	if knowing >= blind {
		t.Errorf("knowing the path gave median %v against %v when blind; "+
			"coding a small exchange should repair it without a round trip",
			knowing.Round(time.Millisecond), blind.Round(time.Millisecond))
	}
}
