package udperr

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

// The distinction this package exists to draw: a datagram's error leaves the
// socket usable, a closed socket does not, and anything unrecognised is
// treated as fatal so a permanent error cannot become a busy loop.
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
		{"unreachable peer", syscall.ECONNREFUSED, true, false},
		{"wrapped unreachable peer", &net.OpError{Op: "read", Err: syscall.ECONNREFUSED}, true, false},
		{"reset", syscall.ECONNRESET, true, false},
		{"oversized datagram", syscall.EMSGSIZE, true, false},
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
