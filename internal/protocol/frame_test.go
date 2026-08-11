package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type shortWriter struct {
	max int
	b   bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.b.Write(p)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestFrameRoundTrip(t *testing.T) {
	var sid [16]byte
	for i := range sid {
		sid[i] = byte(i + 1)
	}
	want := Frame{Header: Header{Version: Version, Type: TypeData, SessionID: sid, FlowID: 7, Sequence: 4096, Class: ClassBulk}, Payload: []byte("payload")}
	want.Header.PayloadLen = uint32(len(want.Payload))
	var b bytes.Buffer
	if err := WriteFrame(&b, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFrame(&b, DefaultMaxPayload)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header != want.Header || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestWriteFrameHandlesShortWrites(t *testing.T) {
	var sid [16]byte
	f := Frame{Header: Header{Version: Version, Type: TypeData, SessionID: sid, FlowID: 7, Sequence: 3, Class: ClassBulk}, Payload: []byte("short writes are valid")}
	var w shortWriter
	w.max = 3
	if err := WriteFrame(&w, f); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(bytes.NewReader(w.b.Bytes()), DefaultMaxPayload)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got.Payload, f.Payload) || got.Header.Sequence != f.Header.Sequence {
		t.Fatalf("round trip mismatch: %#v", got)
	}
}

func TestWriteFrameRejectsZeroProgressWriter(t *testing.T) {
	var sid [16]byte
	f := Frame{Header: Header{Version: Version, Type: TypePing, SessionID: sid, Class: ClassNew}}
	if err := WriteFrame(zeroWriter{}, f); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
}

func TestDecodeRejectsOversizedPayloadBeforeAllocation(t *testing.T) {
	var raw [HeaderSize]byte
	raw[0], raw[1], raw[2], raw[3] = Magic0, Magic1, Version, byte(TypeData)
	raw[38], raw[39], raw[40], raw[41] = 0x7f, 0xff, 0xff, 0xff
	if _, err := DecodeHeader(raw[:], 1024); err == nil {
		t.Fatal("expected oversized payload error")
	}
}

func TestDecodeRejectsReservedBits(t *testing.T) {
	var raw [HeaderSize]byte
	raw[0], raw[1], raw[2], raw[3] = Magic0, Magic1, Version, byte(TypePing)
	raw[43] = 1
	if _, err := DecodeHeader(raw[:], DefaultMaxPayload); err == nil {
		t.Fatal("expected reserved-bit error")
	}
}

func TestDecodeRejectsUnknownFlags(t *testing.T) {
	var raw [HeaderSize]byte
	h := Header{Version: Version, Type: TypeData, Flags: 1 << 15, FlowID: 1, Class: ClassNew}
	if err := h.Encode(raw[:]); err == nil {
		t.Fatal("Encode unexpectedly accepted unknown flags")
	}
	// Exercise the decoder independently of Encode's validation.
	raw[0], raw[1], raw[2], raw[3] = Magic0, Magic1, Version, byte(TypeData)
	binary.BigEndian.PutUint16(raw[4:6], 1<<15)
	binary.BigEndian.PutUint64(raw[22:30], 1)
	if _, err := DecodeHeader(raw[:], DefaultMaxPayload); err == nil {
		t.Fatal("DecodeHeader accepted unknown flags")
	}
}

func TestReserveControlFlagIsScopedToOpen(t *testing.T) {
	var raw [HeaderSize]byte
	open := Header{Version: Version, Type: TypeOpenFast, Flags: FlagReserveControl, FlowID: 1, Class: ClassNew}
	if err := open.Encode(raw[:]); err != nil {
		t.Fatalf("OPEN_FAST reserve flag rejected: %v", err)
	}
	data := Header{Version: Version, Type: TypeData, Flags: FlagReserveControl, FlowID: 1, Class: ClassBulk}
	if err := data.Encode(raw[:]); err == nil {
		t.Fatal("DATA unexpectedly accepted reserve-control flag")
	}
}
