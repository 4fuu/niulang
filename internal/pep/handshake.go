package pep

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/session"
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

func clientAuthenticate(fc *frameConn, secret []byte, sessionID [16]byte, now time.Time) error {
	return clientAuthenticateKind(fc, secret, sessionID, 0, session.HelloNew, now)
}

func clientAuthenticateKind(fc *frameConn, secret []byte, sessionID [16]byte, laneID uint64, kind session.HelloKind, now time.Time) error {
	hello, err := session.NewHello(secret, sessionID, laneID, kind, now)
	if err != nil {
		return err
	}
	if err := fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeHello, SessionID: sessionID, Class: protocol.ClassNew},
		Payload: hello.MarshalBinary(),
	}); err != nil {
		return fmt.Errorf("send session hello: %w", err)
	}
	f, err := fc.Read()
	if err != nil {
		return fmt.Errorf("read session acknowledgement: %w", err)
	}
	if f.Header.Type == protocol.TypeReset {
		return errors.New("server rejected session authentication")
	}
	if f.Header.Type != protocol.TypeHelloOK || f.Header.SessionID != sessionID || f.Header.FlowID != 0 {
		return errors.New("invalid session acknowledgement")
	}
	var ok session.HelloOK
	if err := ok.UnmarshalBinary(f.Payload); err != nil {
		return fmt.Errorf("decode session acknowledgement: %w", err)
	}
	return nil
}

func serverAuthenticateHello(fc *frameConn, secret []byte, guard *session.ReplayGuard, now time.Time) (session.Hello, error) {
	f, err := fc.Read()
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
	ok, err := session.NewHelloOK(now)
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

func serverAuthenticate(fc *frameConn, secret []byte, guard *session.ReplayGuard, now time.Time) ([16]byte, error) {
	hello, err := serverAuthenticateHello(fc, secret, guard, now)
	if err != nil {
		return [16]byte{}, err
	}
	if hello.Kind != session.HelloNew || hello.LaneID != 0 {
		return [16]byte{}, errors.New("expected initial session hello")
	}
	return hello.SessionID, nil
}
