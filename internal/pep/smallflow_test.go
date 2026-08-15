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

func median(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

// A small request and its small reply are the case an erasure channel treats
// worst. Three packets at 42% loss means three quarters of exchanges lose one,
// and with nothing behind it to trigger a fast retransmit the recovery is a
// probe timeout -- a round trip, then another if the probe is lost too.
//
// Coding repairs that without a round trip, but only if the path is known to
// erase before the exchange starts, because a block sealed knowing nothing
// carries no parity. Both halves run on one connection, so the only thing that
// differs between them is what is known.
func TestSmallExchangesAreRepairedOnceThePathIsKnown(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	const oneWay = 150 * time.Millisecond
	loopback := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	key := pathKey(loopback, loopback)
	// Every pair in this process reaches loopback by loopback, so they share
	// one path key. Start from nothing so the first half really is blind.
	pathmodel.Forget(key)
	t.Cleanup(func() { pathmodel.Forget(key) })

	path := pathsim.Config{
		OneWayDelay: oneWay, RateBytesPerSec: uint64(25e6 / 8),
		PolicerRefillPeriod: 8 * time.Millisecond, LossRate: 0.42, Seed: 41,
	}
	socks, destination := codedPairWith(t, true, &path, requestResponse(2700))
	// A connection to a path that erases 42% of packets is sometimes simply
	// lost, which is the path and not a defect. This measures what exchanges
	// cost once a flow exists, so it is worth another attempt to get one.
	conn := dialWithRetries(t, socks, destination, 3)
	defer conn.Close()

	exchange := func(n int) []time.Duration {
		var samples []time.Duration
		request, reply := make([]byte, 16), make([]byte, 2700)
		for i := 0; i < n; i++ {
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

	blind := median(exchange(10))
	// What the endpoint pair is already known to erase. A long-lived proxy
	// learns this from its own traffic or from the prewarm; only the floor is
	// seeded, because a delivered rate would also claim a share of the
	// bottleneck and that is a different experiment.
	pathmodel.Shared(key).Report(99, 0.42, 5000, 0)
	knowing := median(exchange(10))

	t.Logf("median exchange: %v when the path is unknown, %v once it is known (round trip %v)",
		blind.Round(time.Millisecond), knowing.Round(time.Millisecond), 2*oneWay)
	if knowing >= blind {
		t.Errorf("knowing the path gave %v against %v when blind; a small exchange "+
			"should be repaired by the code rather than by a probe timeout",
			knowing.Round(time.Millisecond), blind.Round(time.Millisecond))
	}
}
