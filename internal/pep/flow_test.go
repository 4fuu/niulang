package pep

import (
	"errors"
	"net"
	"syscall"
	"testing"
)

func TestExpectedHalfCloseError(t *testing.T) {
	for name, err := range map[string]error{
		"closed":           net.ErrClosed,
		"notconn":          syscall.ENOTCONN,
		"brokenpipe":       syscall.EPIPE,
		"connection reset": syscall.ECONNRESET,
		"connection abort": syscall.ECONNABORTED,
	} {
		t.Run(name, func(t *testing.T) {
			if !expectedHalfCloseError(err) {
				t.Fatalf("expected %v to be treated as a benign half-close", err)
			}
		})
	}
	if expectedHalfCloseError(errors.New("unrelated application error")) {
		t.Fatal("unrelated error was treated as a benign half-close")
	}
}
