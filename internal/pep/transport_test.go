package pep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/apernet/quic-go"
)

type closeTrackingQUICStream struct {
	cancelReads int
	closes      int
}

func (*closeTrackingQUICStream) Read([]byte) (int, error)          { return 0, io.EOF }
func (*closeTrackingQUICStream) Write(p []byte) (int, error)       { return len(p), nil }
func (*closeTrackingQUICStream) SetDeadline(time.Time) error       { return nil }
func (*closeTrackingQUICStream) SetWriteDeadline(time.Time) error  { return nil }
func (s *closeTrackingQUICStream) CancelRead(quic.StreamErrorCode) { s.cancelReads++ }
func (s *closeTrackingQUICStream) Close() error                    { s.closes++; return nil }

func TestQUICLaneCloseReleasesBothStreamDirections(t *testing.T) {
	stream := &closeTrackingQUICStream{}
	lane := &quicStreamConn{stream: stream}
	if err := lane.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lane.Close(); err != nil {
		t.Fatal(err)
	}
	if stream.cancelReads != 1 || stream.closes != 1 {
		t.Fatalf("close called CancelRead %d times and Close %d times, want one each", stream.cancelReads, stream.closes)
	}
}

func TestQUICPathEvidenceExcludesPeerAndLifecycleFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want quicPathEvidence
	}{
		{name: "success", want: quicPathAvailable},
		{name: "destination response", err: errDestinationUnavailable, want: quicPathAvailable},
		{name: "protocol response", err: peerResponse(errors.New("rejected")), want: quicPathAvailable},
		{name: "caller cancellation", err: context.Canceled, want: quicPathNeutral},
		{name: "peer application close", err: &quic.ApplicationError{Remote: true, ErrorMessage: "server shutdown"}, want: quicPathNeutral},
		{name: "peer transport close", err: &quic.TransportError{Remote: true, ErrorCode: quic.InternalError}, want: quicPathNeutral},
		{name: "stateless reset", err: &quic.StatelessResetError{}, want: quicPathNeutral},
		{name: "stream cancellation", err: &quic.StreamError{Remote: true}, want: quicPathNeutral},
		{name: "plain EOF", err: io.EOF, want: quicPathNeutral},
		{name: "handshake timeout", err: &quic.HandshakeTimeoutError{}, want: quicPathUnavailable},
		{name: "idle timeout", err: fmt.Errorf("wrapped: %w", &quic.IdleTimeoutError{}), want: quicPathUnavailable},
		{name: "attempt deadline", err: context.DeadlineExceeded, want: quicPathUnavailable},
		{name: "local no viable path", err: &quic.TransportError{ErrorCode: quic.NoViablePathError}, want: quicPathUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyQUICPathEvidence(test.err); got != test.want {
				t.Fatalf("evidence = %d, want %d", got, test.want)
			}
		})
	}
}

func TestNegativeQUICEvidenceRequiresReachableTCPControl(t *testing.T) {
	tests := []struct {
		name      string
		quic, tcp quicPathEvidence
		want      quicPathEvidence
	}{
		{name: "timed out while TCP worked", quic: quicPathUnavailable, tcp: quicPathAvailable, want: quicPathUnavailable},
		{name: "pending is not negative evidence", tcp: quicPathAvailable, want: quicPathNeutral},
		{name: "peer closed while TCP worked", quic: quicPathNeutral, tcp: quicPathAvailable, want: quicPathNeutral},
		{name: "both transports failed", quic: quicPathUnavailable, tcp: quicPathNeutral, want: quicPathNeutral},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := differentialQUICPathEvidence(test.quic, test.tcp); got != test.want {
				t.Fatalf("evidence = %d, want %d", got, test.want)
			}
		})
	}
}

func TestValidateLocalAddressSpec(t *testing.T) {
	for _, spec := range []string{"", "auto", "if:en0", "192.0.2.10", "2001:db8::10"} {
		if err := validateLocalAddressSpec(spec); err != nil {
			t.Errorf("validateLocalAddressSpec(%q): %v", spec, err)
		}
	}
	for _, spec := range []string{"not-an-address", "if:", "if:   "} {
		if err := validateLocalAddressSpec(spec); err == nil {
			t.Errorf("validateLocalAddressSpec(%q) unexpectedly succeeded", spec)
		}
	}
}

func TestResolveLocalAddressLiteral(t *testing.T) {
	got, err := resolveLocalAddress("192.0.2.10")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "192.0.2.10" {
		t.Fatalf("resolved literal = %s", got)
	}
}

func TestResolveLocalAddressAutoOrInterfaceReportsOperationalState(t *testing.T) {
	// The CI host may have no physical IPv4 interface, or may expose more than
	// one. In either case the important contract is a bounded, actionable error;
	// when auto succeeds it must return an IPv4 address that can be bound.
	got, err := resolveLocalAddress("auto")
	if err != nil {
		if !strings.Contains(err.Error(), "IPv4") && !strings.Contains(err.Error(), "physical") {
			t.Fatalf("unexpected auto-resolution error: %v", err)
		}
		return
	}
	if !got.Is4() {
		t.Fatalf("auto selected non-IPv4 address %s", got)
	}
}
