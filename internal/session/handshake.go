// Package session contains the authenticated session handshake and bounded
// flow metadata used by the client and server data planes.
package session

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	HandshakeVersion byte = 1
	// helloPayloadSize is deliberately fixed. A fixed authenticated envelope
	// avoids parser ambiguity before a session has been established.
	helloPayloadSize = 8 + 16 + 16 + 8 + 1 + 32
	helloOKSize      = 8 + 16 + 8
	maxClockSkew     = 5 * time.Minute
)

type HelloKind byte

const (
	HelloNew HelloKind = iota
	HelloJoin
)

// Hello is authenticated with the configured pre-shared secret. TLS protects
// it in transit; the MAC additionally prevents accidental acceptance by a
// listener that shares a certificate but not the wanopt credential.
type Hello struct {
	Timestamp int64
	Nonce     [16]byte
	SessionID [16]byte
	LaneID    uint64
	Kind      HelloKind
	MAC       [32]byte
}

func NewHello(secret []byte, sessionID [16]byte, laneID uint64, kind HelloKind, now time.Time) (Hello, error) {
	if len(secret) < 16 {
		return Hello{}, errors.New("session secret must contain at least 16 bytes")
	}
	var h Hello
	if _, err := rand.Read(h.Nonce[:]); err != nil {
		return Hello{}, fmt.Errorf("generate hello nonce: %w", err)
	}
	h.Timestamp = now.Unix()
	h.SessionID = sessionID
	h.LaneID = laneID
	h.Kind = kind
	h.MAC = macHello(secret, h)
	return h, nil
}

func (h Hello) MarshalBinary() []byte {
	b := make([]byte, helloPayloadSize)
	binary.BigEndian.PutUint64(b[0:8], uint64(h.Timestamp))
	copy(b[8:24], h.Nonce[:])
	copy(b[24:40], h.SessionID[:])
	binary.BigEndian.PutUint64(b[40:48], h.LaneID)
	b[48] = byte(h.Kind)
	copy(b[49:81], h.MAC[:])
	return b
}

func (h *Hello) UnmarshalBinary(b []byte) error {
	if len(b) != helloPayloadSize {
		return fmt.Errorf("invalid hello length %d", len(b))
	}
	h.Timestamp = int64(binary.BigEndian.Uint64(b[0:8]))
	copy(h.Nonce[:], b[8:24])
	copy(h.SessionID[:], b[24:40])
	h.LaneID = binary.BigEndian.Uint64(b[40:48])
	h.Kind = HelloKind(b[48])
	copy(h.MAC[:], b[49:81])
	if h.Kind != HelloNew && h.Kind != HelloJoin {
		return errors.New("unsupported hello kind")
	}
	return nil
}

func (h Hello) Verify(secret []byte, now time.Time) error {
	if len(secret) < 16 {
		return errors.New("session secret must contain at least 16 bytes")
	}
	if delta := now.Unix() - h.Timestamp; delta > int64(maxClockSkew/time.Second) || delta < -int64(maxClockSkew/time.Second) {
		return errors.New("hello timestamp outside allowed clock skew")
	}
	want := macHello(secret, h)
	if !hmac.Equal(want[:], h.MAC[:]) {
		return errors.New("invalid session authentication")
	}
	return nil
}

func macHello(secret []byte, h Hello) [32]byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("wanopt/hello/v1\x00"))
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(h.Timestamp))
	mac.Write(b[:])
	mac.Write(h.Nonce[:])
	mac.Write(h.SessionID[:])
	binary.BigEndian.PutUint64(b[:], h.LaneID)
	mac.Write(b[:])
	mac.Write([]byte{byte(h.Kind)})
	var out [32]byte
	copy(out[:], mac.Sum(nil))
	return out
}

// HelloOK confirms that the server accepted the session and supplies a
// random value which is useful for diagnostics and future key derivation.
type HelloOK struct {
	Timestamp    int64
	Nonce        [16]byte
	Capabilities uint64
}

func NewHelloOK(now time.Time) (HelloOK, error) {
	var ok HelloOK
	ok.Timestamp = now.Unix()
	if _, err := rand.Read(ok.Nonce[:]); err != nil {
		return HelloOK{}, fmt.Errorf("generate hello acknowledgement nonce: %w", err)
	}
	return ok, nil
}

func (h HelloOK) MarshalBinary() []byte {
	b := make([]byte, helloOKSize)
	binary.BigEndian.PutUint64(b[0:8], uint64(h.Timestamp))
	copy(b[8:24], h.Nonce[:])
	binary.BigEndian.PutUint64(b[24:32], h.Capabilities)
	return b
}

func (h *HelloOK) UnmarshalBinary(b []byte) error {
	if len(b) != helloOKSize {
		return fmt.Errorf("invalid hello acknowledgement length %d", len(b))
	}
	h.Timestamp = int64(binary.BigEndian.Uint64(b[0:8]))
	copy(h.Nonce[:], b[8:24])
	h.Capabilities = binary.BigEndian.Uint64(b[24:32])
	return nil
}

func NewSessionID() ([16]byte, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("generate session id: %w", err)
	}
	return id, nil
}

func IsZeroSessionID(id [16]byte) bool {
	var zero [16]byte
	return id == zero
}

// ResetCode values are intentionally coarse. They are suitable for metrics
// and client behavior without leaking destination reachability details.
type ResetCode byte

const (
	ResetProtocol ResetCode = iota + 1
	ResetAuthentication
	ResetDestination
	ResetFlowLimit
	ResetTransport
)

func ResetPayload(code ResetCode, message string) []byte {
	if len(message) > 256 {
		message = message[:256]
	}
	b := make([]byte, 1+len(message))
	b[0] = byte(code)
	copy(b[1:], message)
	return b
}
