package pep

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
)

func TestPooledTransportTimeoutClassification(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "deadline", err: context.DeadlineExceeded, want: true},
		{name: "wrapped deadline", err: errors.Join(errors.New("read failed"), context.DeadlineExceeded), want: true},
		{name: "EOF", err: io.EOF, want: false},
		{name: "application close", err: net.ErrClosed, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := pooledTransportTimedOut(test.err); got != test.want {
				t.Fatalf("pooledTransportTimedOut(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
