package multipath

import (
	"bytes"
	"errors"
	"testing"
)

func TestReassemblerReordersAndDrains(t *testing.T) {
	r := NewReassembler(Config{MaxBufferedBytes: 64, MaxBufferedFrames: 8})
	if out, closed, err := r.Insert(Segment{Sequence: 5, Payload: []byte(" world")}); err != nil || out != nil || closed {
		t.Fatalf("out-of-order insert: out=%q closed=%v err=%v", out, closed, err)
	}
	out, closed, err := r.Insert(Segment{Sequence: 0, Payload: []byte("hello")})
	if err != nil || closed || !bytes.Equal(out, []byte("hello world")) {
		t.Fatalf("drain: out=%q closed=%v err=%v", out, closed, err)
	}
	if _, closed, err := r.Insert(Segment{Sequence: 11, Final: true}); err != nil || !closed {
		t.Fatalf("fin: closed=%v err=%v", closed, err)
	}
	if !r.Closed() || r.NextSequence() != 11 {
		t.Fatalf("state: closed=%v next=%d", r.Closed(), r.NextSequence())
	}
}

func TestReassemblerBoundsOutOfOrderData(t *testing.T) {
	r := NewReassembler(Config{MaxBufferedBytes: 4, MaxBufferedFrames: 1})
	if _, _, err := r.Insert(Segment{Sequence: 10, Payload: []byte("12345")}); !errors.Is(err, ErrWindowExceeded) {
		t.Fatalf("expected byte window error, got %v", err)
	}
	if _, _, err := r.Insert(Segment{Sequence: 2, Payload: []byte("12")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Insert(Segment{Sequence: 4, Payload: []byte("34")}); !errors.Is(err, ErrWindowExceeded) {
		t.Fatalf("expected frame window error, got %v", err)
	}
}

func TestReassemblerRejectsOverlap(t *testing.T) {
	r := NewReassembler(DefaultConfig())
	if _, _, err := r.Insert(Segment{Sequence: 4, Payload: []byte("abcd")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Insert(Segment{Sequence: 6, Payload: []byte("xy")}); err == nil {
		t.Fatal("expected overlap rejection")
	}
}
