package pep

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/bojieli/queqiao/internal/protocol"
	"github.com/bojieli/queqiao/internal/session"
)

func randomFlowID() (uint64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("generate flow id: %w", err)
	}
	id := binary.BigEndian.Uint64(b[:])
	if id == 0 {
		id = 1
	}
	return id, nil
}

func clientAuthenticateKindResult(fc *frameConn, secret []byte, sessionID [16]byte, laneID uint64, kind session.HelloKind, now time.Time) (session.HelloOK, error) {
	hello, err := session.NewHello(secret, sessionID, laneID, kind, now)
	if err != nil {
		return session.HelloOK{}, err
	}
	if err := fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeHello, SessionID: sessionID, Class: protocol.ClassNew},
		Payload: hello.MarshalBinary(),
	}); err != nil {
		return session.HelloOK{}, fmt.Errorf("send session hello: %w", err)
	}
	f, err := fc.Read()
	if err != nil {
		return session.HelloOK{}, fmt.Errorf("read session acknowledgement: %w", err)
	}
	if f.Header.Type == protocol.TypeReset {
		return session.HelloOK{}, errors.New("server rejected session authentication")
	}
	if f.Header.Type != protocol.TypeHelloOK || f.Header.SessionID != sessionID || f.Header.FlowID != 0 {
		return session.HelloOK{}, errors.New("invalid session acknowledgement")
	}
	var ok session.HelloOK
	if err := ok.UnmarshalBinary(f.Payload); err != nil {
		return session.HelloOK{}, fmt.Errorf("decode session acknowledgement: %w", err)
	}
	return ok, nil
}

func serverAuthenticateHelloWithCapabilities(fc *frameConn, secret []byte, guard *session.ReplayGuard, now time.Time, capabilities uint64) (session.Hello, error) {
	return serverAuthenticateHelloFrame(fc, secret, guard, now, capabilities, nil)
}

func serverAuthenticateHelloFrame(fc *frameConn, secret []byte, guard *session.ReplayGuard, now time.Time, capabilities uint64, prefetched *protocol.Frame) (session.Hello, error) {
	return serverAuthenticateHelloFrameCallback(fc, secret, guard, now, capabilities, prefetched, nil)
}

// serverAuthenticateHelloFrameCallback is the handshake implementation used
// by pooled QUIC streams. The callback runs after the PSK, timestamp, and
// replay checks have succeeded but before HelloOK is written. Marking the
// connection authenticated at this point closes the small race in which the
// client receives HelloOK and opens an OPEN_FAST stream before the original
// stream has finished its handler.
func serverAuthenticateHelloFrameCallback(fc *frameConn, secret []byte, guard *session.ReplayGuard, now time.Time, capabilities uint64, prefetched *protocol.Frame, onAccepted func(session.Hello)) (session.Hello, error) {
	var f protocol.Frame
	var err error
	if prefetched != nil {
		f = *prefetched
	} else {
		f, err = fc.Read()
	}
	if err != nil {
		return session.Hello{}, fmt.Errorf("read session hello: %w", err)
	}
	if f.Header.Type != protocol.TypeHello || f.Header.FlowID != 0 {
		return session.Hello{}, errors.New("expected session hello")
	}
	var hello session.Hello
	if err := hello.UnmarshalBinary(f.Payload); err != nil {
		return session.Hello{}, fmt.Errorf("decode session hello: %w", err)
	}
	if session.IsZeroSessionID(hello.SessionID) || f.Header.SessionID != hello.SessionID {
		return session.Hello{}, errors.New("invalid session identity")
	}
	if err := hello.Verify(secret, now); err != nil {
		return session.Hello{}, err
	}
	if err := guard.Accept(hello.Nonce, now); err != nil {
		return session.Hello{}, err
	}
	if onAccepted != nil {
		onAccepted(hello)
	}
	ok, err := session.NewHelloOKWithCapabilities(now, capabilities)
	if err != nil {
		return session.Hello{}, err
	}
	if err := fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeHelloOK, SessionID: hello.SessionID, Class: protocol.ClassNew},
		Payload: ok.MarshalBinary(),
	}); err != nil {
		return session.Hello{}, fmt.Errorf("send session acknowledgement: %w", err)
	}
	return hello, nil
}
