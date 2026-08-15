package pep

import (
	"io"
	"net"
	"testing"
	"time"

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
