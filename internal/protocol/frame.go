// Package protocol defines the versioned, bounded wanopt frame envelope.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Magic0            = byte('W')
	Magic1            = byte('O')
	Version           = byte(1)
	HeaderSize        = 46
	DefaultMaxPayload = 1 << 20
	// FlagFin marks that the sender has reached EOF for the direction carried
	// by the frame. A FIN is carried on a zero-length DATA frame so that it is
	// ordered with respect to preceding bytes.
	FlagFin uint16 = 1 << 0
	// FlagAckFinal marks an acknowledgement as final for the flow.
	FlagAckFinal uint16 = 1 << 1
	// FlagAckUp and FlagAckDown identify which byte direction an ACK covers.
	// They are needed because one framed lane carries both directions.
	FlagAckUp   uint16 = 1 << 2
	FlagAckDown uint16 = 1 << 3
	// FlagCloseAbort marks a FIN as a full application-side close rather than
	// a half-close. It lets the peer release a keep-alive destination when the
	// local socket was already closed after consuming its response.
	FlagCloseAbort uint16 = 1 << 4
	// FlagReserveControl is valid only on OPEN / OPEN_FAST. It asks a capable
	// peer to keep lane 0 as a control/rescue lane once an independent bulk
	// lane is attached. Older peers never see this flag because it is gated by
	// the negotiated control-lane capability.
	FlagReserveControl uint16 = 1 << 5
	// FlagLaneJoin is valid only on OPEN_JOIN_FAST. The lane identifier is
	// carried in the bounded payload after the authenticated session/flow ID.
	FlagLaneJoin uint16 = 1 << 6
	knownFlags          = FlagFin | FlagAckFinal | FlagAckUp | FlagAckDown | FlagCloseAbort | FlagReserveControl | FlagLaneJoin
)

type Type byte

const (
	TypeHello Type = iota + 1
	TypeHelloOK
	TypeOpen
	TypeOpenOK
	TypeData
	TypeAck
	TypeWindow
	TypeClose
	TypeReset
	TypePing
	TypePong
	// TypePacket carries one bounded SOCKS UDP datagram. It is intentionally
	// distinct from TypeData: packet payloads preserve datagram boundaries and
	// are not inserted into the byte-stream reassembler.
	TypePacket
	// TypeOpenFast opens a new logical flow on a QUIC connection whose first
	// stream has already completed the PSK handshake. It is accepted only by
	// the connection-level authenticated stream pool; independent lanes and
	// TLS/TCP continue to use TypeHello followed by TypeOpen.
	TypeOpenFast
	// TypeOpenJoinFast attaches a stream on an authenticated secondary QUIC
	// pool to an existing logical flow. Its payload is exactly one big-endian
	// uint64 lane ID.
	TypeOpenJoinFast
)

func (t Type) valid() bool { return t >= TypeHello && t <= TypeOpenJoinFast }

type Class byte

const (
	ClassNew Class = iota
	ClassInteractive
	ClassBulk
)

type Header struct {
	Version    byte
	Type       Type
	Flags      uint16
	SessionID  [16]byte
	FlowID     uint64
	Sequence   uint64
	PayloadLen uint32
	Class      Class
}

type Frame struct {
	Header  Header
	Payload []byte
}

func reserveControlFlagValid(t Type, flags uint16) bool {
	return flags&FlagReserveControl == 0 || t == TypeOpen || t == TypeOpenFast
}

func laneJoinFlagValid(t Type, flags uint16) bool {
	return flags&FlagLaneJoin == 0 || t == TypeOpenJoinFast
}

func (h Header) Encode(dst []byte) error {
	if len(dst) < HeaderSize {
		return io.ErrShortBuffer
	}
	if h.Version != Version || !h.Type.valid() || h.Class > ClassBulk || h.Flags&^knownFlags != 0 || !reserveControlFlagValid(h.Type, h.Flags) || !laneJoinFlagValid(h.Type, h.Flags) {
		return errors.New("invalid frame header")
	}
	if uint64(h.PayloadLen) > DefaultMaxPayload {
		return errors.New("payload exceeds default limit")
	}
	dst[0], dst[1], dst[2], dst[3] = Magic0, Magic1, h.Version, byte(h.Type)
	binary.BigEndian.PutUint16(dst[4:6], h.Flags)
	copy(dst[6:22], h.SessionID[:])
	binary.BigEndian.PutUint64(dst[22:30], h.FlowID)
	binary.BigEndian.PutUint64(dst[30:38], h.Sequence)
	binary.BigEndian.PutUint32(dst[38:42], h.PayloadLen)
	dst[42] = byte(h.Class)
	dst[43], dst[44], dst[45] = 0, 0, 0
	return nil
}

// Validate checks fields which are independent of the configured payload
// limit. Keeping this separate from Encode makes it possible for callers to
// validate a decoded frame before handing it to a flow state machine.
func (h Header) Validate(maxPayload uint32) error {
	if h.Version != Version || !h.Type.valid() || h.Class > ClassBulk {
		return errors.New("invalid frame header")
	}
	if h.Flags&^knownFlags != 0 || !reserveControlFlagValid(h.Type, h.Flags) || !laneJoinFlagValid(h.Type, h.Flags) {
		return errors.New("unknown frame flags")
	}
	if maxPayload == 0 || maxPayload > DefaultMaxPayload {
		maxPayload = DefaultMaxPayload
	}
	if h.PayloadLen > maxPayload {
		return fmt.Errorf("payload length %d exceeds limit %d", h.PayloadLen, maxPayload)
	}
	return nil
}

func DecodeHeader(src []byte, maxPayload uint32) (Header, error) {
	if len(src) < HeaderSize {
		return Header{}, io.ErrUnexpectedEOF
	}
	if src[0] != Magic0 || src[1] != Magic1 {
		return Header{}, errors.New("invalid frame magic")
	}
	h := Header{
		Version: src[2], Type: Type(src[3]), Flags: binary.BigEndian.Uint16(src[4:6]),
		FlowID: binary.BigEndian.Uint64(src[22:30]), Sequence: binary.BigEndian.Uint64(src[30:38]),
		PayloadLen: binary.BigEndian.Uint32(src[38:42]), Class: Class(src[42]),
	}
	copy(h.SessionID[:], src[6:22])
	if err := h.Validate(maxPayload); err != nil {
		return Header{}, fmt.Errorf("unsupported frame header: %w", err)
	}
	if src[43] != 0 || src[44] != 0 || src[45] != 0 {
		return Header{}, errors.New("non-zero reserved bits")
	}
	return h, nil
}

func ReadFrame(r io.Reader, maxPayload uint32) (Frame, error) {
	var raw [HeaderSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return Frame{}, err
	}
	h, err := DecodeHeader(raw[:], maxPayload)
	if err != nil {
		return Frame{}, err
	}
	payload := make([]byte, h.PayloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Header: h, Payload: payload}, nil
}

// AppendFrame serializes one frame into dst and returns the extended slice.
//
// The header and payload must reach the transport in a single write. When they
// were written separately, a QUIC sender that happened to be idle could
// packetize the 46-byte header into its own datagram, adding roughly one extra
// packet per data frame on the wire; on a lossy path that inflates both the
// byte count and the number of packets exposed to loss.
func AppendFrame(dst []byte, f Frame) ([]byte, error) {
	if uint64(len(f.Payload)) > DefaultMaxPayload {
		return dst, errors.New("payload exceeds default limit")
	}
	f.Header.PayloadLen = uint32(len(f.Payload))
	start := len(dst)
	dst = append(dst, make([]byte, HeaderSize)...)
	if err := f.Header.Encode(dst[start:]); err != nil {
		return dst[:start], err
	}
	return append(dst, f.Payload...), nil
}

func WriteFrame(w io.Writer, f Frame) error {
	buf, err := AppendFrame(make([]byte, 0, HeaderSize+len(f.Payload)), f)
	if err != nil {
		return err
	}
	return writeFull(w, buf)
}

// writeFull is intentionally local rather than relying on a particular
// transport. io.Writer is allowed to perform a short successful write, which
// is common for rate-limited or instrumented writers and must not corrupt the
// frame stream.
func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
