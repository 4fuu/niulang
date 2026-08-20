package udperr

import (
	"errors"
	"net"
	"os"
	"testing"
	"time"
)

// The distinction this package exists to draw: a datagram's error leaves the
// socket usable, a closed socket does not, and anything unrecognised is
// treated as fatal so a permanent error cannot become a busy loop.
//
// The per-datagram codes themselves live in the build-tagged sample files,
// because a code is only per-datagram on the platform that reports it. On
// Windows syscall.ECONNREFUSED is a synthetic APPLICATION_ERROR value rather
// than WSAECONNREFUSED, and syscall.Errno.Is does not bridge the two, so a
// table of Unix errnos asserts nothing about what a Winsock socket returns.
func TestOneDatagramsErrorIsNotTheSocketsError(t *testing.T) {
	for _, test := range []struct {
		name      string
		err       error
		transient bool
		fatal     bool
	}{
		{"nothing", nil, false, false},
		{"closed socket", net.ErrClosed, false, true},
		{"wrapped closed socket", &net.OpError{Op: "read", Err: net.ErrClosed}, false, true},
		{"deadline", os.ErrDeadlineExceeded, false, false},
		{"unrecognised", errors.New("never seen before"), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Transient(test.err); got != test.transient {
				t.Errorf("Transient(%v) = %v, want %v", test.err, got, test.transient)
			}
			if got := Fatal(test.err); got != test.fatal {
				t.Errorf("Fatal(%v) = %v, want %v", test.err, got, test.fatal)
			}
		})
	}
}

// Each code this platform reports for a single datagram must be skippable,
// bare and wrapped the way the net package hands it back.
func TestThisPlatformsPerDatagramCodesAreSkippable(t *testing.T) {
	if len(perDatagramSamples) == 0 {
		t.Fatal("no per-datagram codes named for this platform")
	}
	for _, sample := range perDatagramSamples {
		t.Run(sample.name, func(t *testing.T) {
			for label, err := range map[string]error{
				"bare":    sample.err,
				"wrapped": &net.OpError{Op: "read", Err: sample.err},
			} {
				if !Transient(err) {
					t.Errorf("Transient(%s %v) = false, want true", label, err)
				}
				if Fatal(err) {
					t.Errorf("Fatal(%s %v) = true, want false", label, err)
				}
			}
		})
	}
}

// A closed socket must stay fatal even though Winsock reports the close with a
// code this package otherwise skips, so the two checks cannot be reordered.
func TestAClosedSocketIsNeverTransient(t *testing.T) {
	closed := &net.OpError{Op: "read", Err: net.ErrClosed}
	if Transient(closed) {
		t.Fatal("a closed socket must not be reported as a per-datagram error")
	}
	if !Fatal(closed) {
		t.Fatal("a closed socket must be fatal")
	}
}

// The sample lists above are hand-written, so they can drift from what the
// host actually reports -- which is the mistake that first shipped here. This
// draws a real ICMP port-unreachable and requires the classifier to recognise
// whatever code arrives, so a listed-codes-versus-real-codes mismatch fails
// here instead of silently killing a read loop.
//
// A connected socket is used because that is the narrowest case any supported
// platform reports: Linux and Windows both surface it, and Windows surfaces it
// on unconnected sockets too. macOS reports nothing at all, so it skips.
func TestTheCodeTheHostActuallyReportsIsRecognised(t *testing.T) {
	closed, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("no loopback UDP: %v", err)
	}
	vanished := closed.LocalAddr().(*net.UDPAddr)
	if err := closed.Close(); err != nil {
		t.Fatalf("close the port before probing it: %v", err)
	}

	conn, err := net.DialUDP("udp", nil, vanished)
	if err != nil {
		t.Skipf("no connected UDP socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// The first send draws the ICMP reply; a later read reports it. Sending
	// more than once because which read carries it is not guaranteed.
	buf := make([]byte, 128)
	for attempt := 0; attempt < 8; attempt++ {
		if _, err := conn.Write([]byte("probe")); err != nil {
			if Transient(err) {
				// The write itself reported it, which is also correct.
				t.Logf("the send reported the unreachable peer: %#v", err)
				return
			}
			t.Skipf("cannot send to a closed port: %v", err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatalf("set a read deadline: %v", err)
		}
		_, _, err := conn.ReadFromUDP(buf)
		if err == nil {
			t.Fatal("a closed port answered")
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			continue
		}
		if !Transient(err) {
			t.Fatalf("the host reported %#v for an unreachable peer and this package "+
				"calls it fatal; the per-datagram sample list is missing that code", err)
		}
		if Fatal(err) {
			t.Fatalf("Fatal(%#v) = true for a live socket", err)
		}
		t.Logf("read %d reported the unreachable peer: %#v", attempt+1, err)
		return
	}
	t.Skip("this host does not report an unreachable peer to the sender")
}
