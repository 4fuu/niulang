package protocol

import (
	"bytes"
	"testing"
)

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
