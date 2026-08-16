// Package protocol defines the versioned, bounded queqiao frame envelope.
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	Magic0 = byte('W')
	Magic1 = byte('O')
	// Version is the framing this build speaks, and the only thing that stops
	// two builds that disagree from appearing to work.
	//
	// It covers more than this header. Version 3 is version 2's frames carried
	// over a sliding-window code rather than a block code, and the change is
	// invisible here -- the frames are identical, and only the datagrams
	// underneath them differ. A version 3 sender against a version 2 receiver
	// would therefore complete its handshake, send bulk over datagrams the peer
	// parses as shards, and have every frame silently dropped as unparseable
	// while the session re-issued them forever.
	//
	// So anything that changes what is on the wire, at any layer this protocol
	// carries, changes this. A mismatch then fails on the first frame, which is
	// a diagnosable failure rather than a stalled flow.
	Version           = byte(3)
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
	// FlagAckRanges is valid only on ACK. The payload carries byte ranges the
	// receiver already holds out of order, beyond the cumulative sequence.
	//
	// A striped flow's sender otherwise learns only the contiguous receive
	// point, which sits behind whatever the slowest lane has not delivered, so
	// its retention window has to cover the whole reorder span. It is
	// capability-gated: a peer that does not advertise support never sees it.
	FlagAckRanges uint16 = 1 << 7
	knownFlags           = FlagFin | FlagAckFinal | FlagAckUp | FlagAckDown | FlagCloseAbort | FlagReserveControl | FlagLaneJoin | FlagAckRanges
)

type Type byte

// Removed in version 2: WINDOW, PING and PONG. All three were specified and
// none was ever sent. WINDOW described a receiver-advertised byte limit, which
// QUIC's own stream and connection flow control already provides and which the
// scheduler bounds again above it; PING and PONG described a liveness probe
// that QUIC's keepalive and idle timeout already perform. A frame type that
// exists only in the document is worse than no frame type: it reads as a safety
// property the implementation does not have.
const (
	TypeHello Type = iota + 1
	TypeHelloOK
	TypeOpen
	TypeOpenOK
	TypeData
	TypeAck
	TypeClose
	TypeReset
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

func ackRangesFlagValid(t Type, flags uint16) bool {
	return flags&FlagAckRanges == 0 || t == TypeAck
}

// MaxAckRanges bounds one acknowledgement's range list, so a peer cannot make
// the receiver allocate or the sender iterate without limit.
const MaxAckRanges = 16

// AckRangeSize is the encoded width of one range: two big-endian uint64s.
const AckRangeSize = 16

// EncodeAckRanges serializes received byte ranges for an ACK payload.
func EncodeAckRanges(ranges [][2]uint64) ([]byte, error) {
	if len(ranges) > MaxAckRanges {
		return nil, fmt.Errorf("acknowledgement carries %d ranges, limit is %d", len(ranges), MaxAckRanges)
	}
	payload := make([]byte, 0, len(ranges)*AckRangeSize)
	for _, r := range ranges {
		if r[0] >= r[1] {
			return nil, errors.New("acknowledgement range is empty or inverted")
		}
		payload = binary.BigEndian.AppendUint64(payload, r[0])
		payload = binary.BigEndian.AppendUint64(payload, r[1])
	}
	return payload, nil
}

// DecodeAckRanges parses an ACK payload. Ranges must be non-empty, strictly
// increasing, and disjoint: a peer that sends overlapping or unordered ranges
// is either broken or trying to make the sender release bytes it should retain.
func DecodeAckRanges(payload []byte, cumulative uint64) ([][2]uint64, error) {
	if len(payload)%AckRangeSize != 0 {
		return nil, errors.New("acknowledgement range payload is misaligned")
	}
	count := len(payload) / AckRangeSize
	if count > MaxAckRanges {
		return nil, fmt.Errorf("acknowledgement carries %d ranges, limit is %d", count, MaxAckRanges)
	}
	ranges := make([][2]uint64, 0, count)
	previousEnd := cumulative
	for i := range count {
		start := binary.BigEndian.Uint64(payload[i*AckRangeSize:])
		end := binary.BigEndian.Uint64(payload[i*AckRangeSize+8:])
		if start >= end {
			return nil, errors.New("acknowledgement range is empty or inverted")
		}
		if start < previousEnd {
			return nil, errors.New("acknowledgement ranges overlap or are unordered")
		}
		previousEnd = end
		ranges = append(ranges, [2]uint64{start, end})
	}
	return ranges, nil
}

func (h Header) Encode(dst []byte) error {
	if len(dst) < HeaderSize {
		return io.ErrShortBuffer
	}
	if h.Version != Version || !h.Type.valid() || h.Class > ClassBulk || h.Flags&^knownFlags != 0 || !reserveControlFlagValid(h.Type, h.Flags) || !laneJoinFlagValid(h.Type, h.Flags) || !ackRangesFlagValid(h.Type, h.Flags) {
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
	if h.Flags&^knownFlags != 0 || !reserveControlFlagValid(h.Type, h.Flags) || !laneJoinFlagValid(h.Type, h.Flags) || !ackRangesFlagValid(h.Type, h.Flags) {
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

// ParseFrame decodes one frame that already sits whole in memory.
//
// A datagram substrate delivers a frame complete or not at all, so there is
// nothing to read incrementally and no reader to hold. Requiring one would
// mean wrapping every arrival in a bytes.Reader for the sake of an interface
// it does not need.
//
// It is an error for the slice to hold anything after the frame: the caller
// framed it, so a trailing byte means the framing disagrees with the parse.
func ParseFrame(b []byte, maxPayload uint32) (Frame, error) {
	if len(b) < HeaderSize {
		return Frame{}, io.ErrUnexpectedEOF
	}
	h, err := DecodeHeader(b[:HeaderSize], maxPayload)
	if err != nil {
		return Frame{}, err
	}
	rest := b[HeaderSize:]
	if uint32(len(rest)) != h.PayloadLen {
		return Frame{}, fmt.Errorf("frame payload is %d bytes, header declares %d", len(rest), h.PayloadLen)
	}
	payload := make([]byte, len(rest))
	copy(payload, rest)
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
