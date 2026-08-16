// Package session contains the authenticated session handshake and bounded
// flow metadata used by the client and server data planes.
package session

import (
	"bytes"
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
	// HelloOK remains byte-for-byte compatible with the original version-1
	// acknowledgement. Capable peers reserve its opaque nonce rather than
	// lengthening the payload, so old clients and servers continue to interoperate.
	helloOKSize         = 8 + 16
	helloOKExtendedSize = helloOKSize + 8 // interim capability draft, receive-only
	maxClockSkew        = 5 * time.Minute
	// CapabilityFastStreams allows a client to skip a repeated Hello exchange
	// on a QUIC connection after the first stream has authenticated it. The
	// capability is scoped to that TLS connection and never authorizes a
	// standalone lane.
	CapabilityFastStreams uint64 = 1 << 0
	// CapabilityReserveControl allows OPEN / OPEN_FAST to request that lane 0
	// remain reserved for interactive/control traffic after bulk promotion.
	// The capability is negotiated so a new client never sends the flag to an
	// older production peer that would reject unknown frame flags.
	CapabilityReserveControl uint64 = 1 << 1
	// CapabilityFastLaneJoin allows a separately authenticated QUIC pool to
	// attach a stream to an existing logical flow without repeating a full
	// QUIC handshake for every bulk lane.
	CapabilityFastLaneJoin uint64 = 1 << 2
	// CapabilityAckRanges allows a receiver to report byte ranges it already
	// holds out of order alongside the cumulative acknowledgement. It is
	// advertised by the side that would *consume* those reports, because a
	// peer that cannot parse them must never be sent them.
	CapabilityAckRanges uint64 = 1 << 3
	// CapabilityUDPResume allows a client to ask that a UDP association's
	// remote relay socket be retained across a lane failure and reclaimed by
	// the replacement association, so the destination keeps seeing one source
	// address. A client must not ask unless the server advertised this: an
	// older server reads the resume marker as neither an association nor a
	// destination and refuses the flow.
	CapabilityUDPResume uint64 = 1 << 4
)

var helloOKCapabilityMarker = [8]byte{'W', 'O', 'C', 'A', 'P', '0', '0', '1'}

type HelloKind byte

const (
	HelloNew HelloKind = iota
	// HelloJoin retains wire value 1 for compatibility with deployed peers.
	HelloJoin
	// HelloPool authenticates a QUIC connection-level pool without creating a
	// destination flow. It is accepted only on QUIC connections.
	HelloPool
)

// Hello is authenticated with the configured pre-shared secret. TLS protects
// it in transit; the MAC additionally prevents accidental acceptance by a
// listener that shares a certificate but not the queqiao credential.
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
	if h.Kind != HelloNew && h.Kind != HelloJoin && h.Kind != HelloPool {
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
	mac.Write([]byte("queqiao/hello/v1\x00"))
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

// HelloOK confirms that the server accepted the session. Capability-free
// acknowledgements retain the original random nonce. A capable server places
// an eight-byte marker and eight capability bytes in that otherwise opaque
// field, which old peers safely continue to ignore.
type HelloOK struct {
	Timestamp    int64
	Nonce        [16]byte
	Capabilities uint64
}

func NewHelloOK(now time.Time) (HelloOK, error) {
	return NewHelloOKWithCapabilities(now, 0)
}

func NewHelloOKWithCapabilities(now time.Time, capabilities uint64) (HelloOK, error) {
	var ok HelloOK
	ok.Timestamp = now.Unix()
	ok.Capabilities = capabilities
	if _, err := rand.Read(ok.Nonce[:]); err != nil {
		return HelloOK{}, fmt.Errorf("generate hello acknowledgement nonce: %w", err)
	}
	if capabilities != 0 {
		copy(ok.Nonce[:8], helloOKCapabilityMarker[:])
		binary.BigEndian.PutUint64(ok.Nonce[8:16], capabilities)
	}
	return ok, nil
}

func (h HelloOK) MarshalBinary() []byte {
	b := make([]byte, helloOKSize)
	binary.BigEndian.PutUint64(b[0:8], uint64(h.Timestamp))
	nonce := h.Nonce
	if h.Capabilities != 0 {
		copy(nonce[:8], helloOKCapabilityMarker[:])
		binary.BigEndian.PutUint64(nonce[8:16], h.Capabilities)
	}
	copy(b[8:24], nonce[:])
	return b
}

func (h *HelloOK) UnmarshalBinary(b []byte) error {
	if len(b) != helloOKSize && len(b) != helloOKExtendedSize {
		return fmt.Errorf("invalid hello acknowledgement length %d", len(b))
	}
	h.Timestamp = int64(binary.BigEndian.Uint64(b[0:8]))
	copy(h.Nonce[:], b[8:24])
	h.Capabilities = 0
	if len(b) == helloOKExtendedSize {
		// Accept the short-lived 32-byte capability draft emitted by the
		// development service before the wire-compatible marker format landed.
		h.Capabilities = binary.BigEndian.Uint64(b[24:32])
		return nil
	}
	if bytes.Equal(h.Nonce[:8], helloOKCapabilityMarker[:]) {
		h.Capabilities = binary.BigEndian.Uint64(h.Nonce[8:16])
	}
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
