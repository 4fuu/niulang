package pep

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sort"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/classifier"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
	"github.com/icourses-dev/wanopt/internal/pathsim"
	"github.com/icourses-dev/wanopt/internal/protocol"
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
	pathmodel.Shared(key).Report(99, 0.42, 5000, 0, 0)
	knowing := median(exchange(10))

	t.Logf("median exchange: %v when the path is unknown, %v once it is known (round trip %v)",
		blind.Round(time.Millisecond), knowing.Round(time.Millisecond), 2*oneWay)
	if knowing >= blind {
		t.Errorf("knowing the path gave %v against %v when blind; a small exchange "+
			"should be repaired by the code rather than by a probe timeout",
			knowing.Round(time.Millisecond), blind.Round(time.Millisecond))
	}
}

// A refused destination and a lost attempt are different answers, and only one
// of them is worth asking again.
//
// The peer answering "I could not reach that" has told the application
// something true; asking again only delays it. A path that lost the asking has
// told it nothing, and reporting that as an unreachable destination is a lie
// about the destination -- the application's own retry costs a fresh TCP
// connection and a fresh SOCKS negotiation for something this layer could have
// tried again itself.
func TestOnlyALostAttemptIsRetried(t *testing.T) {
	client := &Client{cfg: ClientConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}

	attempts := 0
	client.openFlowForTest = func() (*openedFlow, error) {
		attempts++
		return nil, errDestinationUnavailable
	}
	if _, err := client.openFlowWithRetries(context.Background(), "example.test:80"); err == nil {
		t.Fatal("a refused destination was reported as success")
	}
	if attempts != 1 {
		t.Fatalf("a refused destination was asked %d times, want 1", attempts)
	}

	attempts = 0
	client.openFlowForTest = func() (*openedFlow, error) {
		attempts++
		return nil, errors.New("quic lane: context deadline exceeded")
	}
	if _, err := client.openFlowWithRetries(context.Background(), "example.test:80"); err == nil {
		t.Fatal("a lost attempt was reported as success")
	}
	if attempts != flowOpenAttempts {
		t.Fatalf("a lost attempt was asked %d times, want %d", attempts, flowOpenAttempts)
	}

	// And a path that loses the first attempt but not the second must not cost
	// the application anything at all.
	attempts = 0
	client.openFlowForTest = func() (*openedFlow, error) {
		if attempts++; attempts == 1 {
			return nil, errors.New("quic lane: context deadline exceeded")
		}
		return &openedFlow{}, nil
	}
	if _, err := client.openFlowWithRetries(context.Background(), "example.test:80"); err != nil {
		t.Fatalf("a path that lost one attempt failed the flow: %v", err)
	}
}

// A flow becomes what it is as it ages, not only when it reads.
//
// This is the considered answer rather than the immediate one: a flow that has
// already moved more than a small exchange stops coding at once, without
// waiting for the class. Both matter, because they arrive at different times.
//
// The classifier was driven only by reads from the inner connection, and a
// server reading a ten megabyte file from a local destination does every one
// of those inside a second -- before the bulk test's minimum age -- and then
// never reads again. Nothing re-examined the flow, so it stayed ClassNew for
// its whole life and was coded from first byte to last.
func TestAFlowIsReclassifiedAsItAges(t *testing.T) {
	inner, peer := net.Pipe()
	defer inner.Close()
	defer peer.Close()
	f := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, 64*1024,
		protocol.FlagAckUp, protocol.FlagAckDown, nil, nil, nil)

	// Ten megabytes read before the bulk test's minimum age has elapsed, which
	// is what a fast local destination gives a server.
	f.observe(84, false)
	f.bytesDown.Add(84)
	for sent := 0; sent < 10_000_000; sent += 64 * 1024 {
		f.observe(64*1024, true)
		f.bytesUp.Add(64 * 1024)
	}
	if got := classifier.Class(f.class.Load()); got != classifier.ClassNew {
		t.Fatalf("class %v before the minimum age, want new", got)
	}

	// The reads are over. Only age separates this flow from being bulk, and
	// re-examining it is the only thing that can notice.
	f.started = f.started.Add(-5 * time.Second)
	f.lastClassified.Store(0)
	f.refreshClass()
	if got := classifier.Class(f.class.Load()); got != classifier.ClassBulk {
		t.Fatalf("class %v after ageing, want bulk; the flow was never "+
			"re-examined once it stopped reading", got)
	}
}

// And a flow that has already moved bulk quantities stops coding immediately,
// without waiting a second for the class to settle. That second is where a
// download produces most of its frames.
func TestAFlowThatHasMovedBulkQuantitiesStopsCodingAtOnce(t *testing.T) {
	inner, peer := net.Pipe()
	defer inner.Close()
	defer peer.Close()
	f := newMultipathFlow(context.Background(), inner, [16]byte{1}, 1, 64*1024,
		protocol.FlagAckUp, protocol.FlagAckDown, nil, nil, nil)

	if !f.prefersCodingOverRetransmission() {
		t.Fatal("a flow that has carried nothing should still prefer coding")
	}
	f.bytesUp.Store(codedFlowBytes + 1)
	if f.prefersCodingOverRetransmission() {
		t.Fatalf("a flow that has moved %d bytes still prefers coding, and will "+
			"go on doing so for the second its class takes to settle", f.bytesUp.Load())
	}
}
