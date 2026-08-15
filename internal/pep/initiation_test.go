package pep

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/pathmodel"
	"github.com/icourses-dev/wanopt/internal/pathsim"
)

// Flow initiation is what an application feels. The first connection to a
// server may cost what it must -- a QUIC handshake and one authentication
// exchange -- but every flow after that is a fresh application connection on a
// pool that is already up, and should cost as close to nothing as the protocol
// allows.
//
// This measures it the way a user would: the wall-clock time from dialing the
// local SOCKS port to having the reply in hand, across an emulated 300 ms
// path, for the first flow and then for later ones.
func TestFlowInitiationCostsNoRoundTripsWhenTheConnectionIsWarm(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	const oneWay = 150 * time.Millisecond
	path := pathsim.Config{OneWayDelay: oneWay, Seed: 31}
	socks, destination := codedPairWith(t, true, &path, echoDestination)

	measure := func() time.Duration {
		start := time.Now()
		conn := socksDial(t, socks, destination, 30*time.Second)
		elapsed := time.Since(start)
		// Prove the flow actually carries data, not just that a reply arrived.
		if _, err := conn.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, 1)
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("flow did not carry data: %v", err)
		}
		_ = conn.Close()
		return elapsed
	}

	// Establishment is the first flow, which authenticates the connection, and
	// the second, which proves the fast open once so no later flow has to.
	// Both are allowed to cost a round trip. Everything after is the case an
	// application actually repeats.
	establishing := []time.Duration{measure(), measure()}
	var warm []time.Duration
	for i := 0; i < 3; i++ {
		warm = append(warm, measure())
	}
	roundTrip := 2 * oneWay
	t.Logf("establishing %v %v; warm flows %v %v %v (round trip %v)",
		establishing[0].Round(time.Millisecond), establishing[1].Round(time.Millisecond),
		warm[0].Round(time.Millisecond), warm[1].Round(time.Millisecond),
		warm[2].Round(time.Millisecond), roundTrip)

	for i, elapsed := range warm {
		if elapsed > roundTrip/4 {
			t.Errorf("warm flow %d took %v against a %v round trip: a flow on a "+
				"connection that is already established should not wait for the far end",
				i, elapsed.Round(time.Millisecond), roundTrip)
		}
	}
	// And establishment must stay bounded rather than creeping: two round
	// trips for the first flow, one for the second.
	if establishing[0] > 3*roundTrip {
		t.Errorf("first flow took %v, more than three round trips", establishing[0].Round(time.Millisecond))
	}
	_ = net.Dialer{}
}

// A path is an uplink and a peer, not a peer. The same server reached over
// Wi-Fi and over a cellular link erases differently, is bottlenecked
// differently and has a different minimum round trip; carrying one's
// measurements into the other is worse than having none, because everything
// downstream is sized from a confident wrong answer.
func TestAPathIsAnUplinkAndAPeer(t *testing.T) {
	wifi := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 20), Port: 51000}
	cellular := &net.UDPAddr{IP: net.IPv4(10, 55, 3, 7), Port: 51000}
	server := &net.UDPAddr{IP: net.IPv4(23, 135, 236, 244), Port: 12443}

	overWiFi := pathKey(wifi, server)
	overCellular := pathKey(cellular, server)
	if overWiFi == overCellular {
		t.Fatalf("two uplinks to one server share a path key: %q", overWiFi)
	}
	// The same uplink and peer must key the same, whatever port it used, or a
	// second lane would be treated as a different path and learn it again.
	second := &net.UDPAddr{IP: wifi.IP, Port: 51001}
	if again := pathKey(second, server); again != overWiFi {
		t.Fatalf("a second lane on one uplink keyed %q against %q", again, overWiFi)
	}
}

// The uplink is discovered by asking the routing table which source address
// this destination gets, which sends nothing and so can be asked often.
func TestTheUplinkIsWhicheverAddressReachesTheServer(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client := &Client{cfg: ClientConfig{RemoteAddr: listener.LocalAddr().String()}}

	uplink := client.currentUplink()
	if uplink != "127.0.0.1" {
		t.Fatalf("uplink to a loopback server = %q, want 127.0.0.1", uplink)
	}
	// Asking twice must give the same answer, or every poll would look like a
	// network change and tear down the pool.
	if again := client.currentUplink(); again != uplink {
		t.Fatalf("uplink changed between two questions: %q then %q", uplink, again)
	}
	// A destination that cannot be reached at all has no uplink, and must not
	// be reported as one: an empty answer is ignored rather than acted on, so
	// a transient resolver failure cannot look like a network change and tear
	// the pool down. The address is malformed rather than merely unresolvable,
	// because a resolver that answers everything -- this machine's does, with
	// a fake address in 198.18.0.0/15 -- would otherwise give it an uplink.
	unreachable := &Client{cfg: ClientConfig{RemoteAddr: "127.0.0.1:not-a-port"}}
	if got := unreachable.currentUplink(); got != "" {
		t.Fatalf("unreachable server reported uplink %q", got)
	}
}

// The prewarm exists to measure, so it has to leave a measurement behind.
//
// A path that erases is only coded around once something has noticed it does,
// and the first flow on a fresh uplink notices nothing: a handshake is about
// ten packets, and an erasure rate estimated from ten packets is a guess wider
// than the parity it would choose. The prewarm sends enough to answer the
// question before a flow has to ask it.
func TestThePrewarmLeavesTheUplinkMeasured(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	path := pathsim.Config{
		OneWayDelay: 150 * time.Millisecond, RateBytesPerSec: uint64(25e6 / 8),
		PolicerRefillPeriod: 8 * time.Millisecond, LossRate: 0.42, Seed: 53,
	}
	loopback := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	key := pathKey(loopback, loopback)

	// Start from nothing known about this uplink.
	before, _ := pathmodel.Shared(key).Current()

	client, _ := clientServerAcross(t, &path)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client.prewarmPath(ctx)

	after, _ := pathmodel.Shared(key).Current()
	t.Logf("erasure floor known for this uplink: %.3f before the prewarm, %.3f after", before, after)
	if after <= 0 {
		t.Fatal("the prewarm left the uplink unmeasured, so the first flow on it " +
			"will be carried uncoded across a channel that erases 42% of packets")
	}
	// And what it measured has to resemble the path, or it is worse than
	// nothing: everything downstream is sized from this number.
	if after < 0.2 || after > 0.7 {
		t.Fatalf("measured floor %.3f on a 42%% erasure channel", after)
	}
}
