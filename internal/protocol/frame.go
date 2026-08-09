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
)

func (t Type) valid() bool { return t >= TypeHello && t <= TypePong }

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

func (h Header) Encode(dst []byte) error {
	if len(dst) < HeaderSize {
		return io.ErrShortBuffer
	}
	if h.Version != Version || !h.Type.valid() || h.Class > ClassBulk {
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
	if h.Version != Version || !h.Type.valid() || h.Class > ClassBulk {
		return Header{}, errors.New("unsupported frame header")
	}
	if maxPayload == 0 || maxPayload > DefaultMaxPayload {
		maxPayload = DefaultMaxPayload
	}
	if h.PayloadLen > maxPayload {
		return Header{}, fmt.Errorf("payload length %d exceeds limit %d", h.PayloadLen, maxPayload)
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

func WriteFrame(w io.Writer, f Frame) error {
	if uint64(len(f.Payload)) > DefaultMaxPayload {
		return errors.New("payload exceeds default limit")
	}
	f.Header.PayloadLen = uint32(len(f.Payload))
	var raw [HeaderSize]byte
	if err := f.Header.Encode(raw[:]); err != nil {
		return err
	}
	if _, err := w.Write(raw[:]); err != nil {
		return err
	}
	_, err := w.Write(f.Payload)
	return err
}
