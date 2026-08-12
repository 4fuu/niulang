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

func TestReassemblerFinMayArriveBeforeMissingData(t *testing.T) {
	r := NewReassembler(Config{MaxBufferedBytes: 64, MaxBufferedFrames: 8})
	if _, closed, err := r.Insert(Segment{Sequence: 5, Payload: []byte(" world")}); err != nil || closed {
		t.Fatalf("data insert: closed=%v err=%v", closed, err)
	}
	if _, closed, err := r.Insert(Segment{Sequence: 11, Final: true}); err != nil || closed {
		t.Fatalf("buffered FIN insert: closed=%v err=%v", closed, err)
	}
	out, closed, err := r.Insert(Segment{Sequence: 0, Payload: []byte("hello")})
	if err != nil || !closed || !bytes.Equal(out, []byte("hello world")) {
		t.Fatalf("FIN drain: out=%q closed=%v err=%v", out, closed, err)
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

func TestReceivedRangesMergesAndOrders(t *testing.T) {
	r := NewReassembler(DefaultConfig())
	// Deliberately out of order, with two adjacent segments that must merge.
	for _, segment := range []Segment{
		{Sequence: 300, Payload: []byte("ccc")},
		{Sequence: 100, Payload: []byte("aa")},
		{Sequence: 102, Payload: []byte("bb")},
	} {
		if _, _, err := r.Insert(segment); err != nil {
			t.Fatalf("insert at %d: %v", segment.Sequence, err)
		}
	}
	got := r.ReceivedRanges(8)
	want := [][2]uint64{{100, 104}, {300, 303}}
	if len(got) != len(want) {
		t.Fatalf("ranges = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ranges = %v, want %v", got, want)
		}
	}
}

// A truncated report must describe the bytes closest to the contiguous point,
// which are the ones the sender is most likely to still be holding.
func TestReceivedRangesTruncatesFromTheLowest(t *testing.T) {
	r := NewReassembler(DefaultConfig())
	for i := range uint64(5) {
		if _, _, err := r.Insert(Segment{Sequence: 100 + i*10, Payload: []byte("xx")}); err != nil {
			t.Fatal(err)
		}
	}
	got := r.ReceivedRanges(2)
	if len(got) != 2 || got[0][0] != 100 || got[1][0] != 110 {
		t.Fatalf("ranges = %v, want the two lowest", got)
	}
}

func TestReceivedRangesIgnoresContiguousAndEmpty(t *testing.T) {
	r := NewReassembler(DefaultConfig())
	if got := r.ReceivedRanges(4); got != nil {
		t.Fatalf("ranges = %v on an empty reassembler, want none", got)
	}
	// A segment delivered contiguously is not buffered, so it is not a range.
	if _, _, err := r.Insert(Segment{Sequence: 0, Payload: []byte("abc")}); err != nil {
		t.Fatal(err)
	}
	if got := r.ReceivedRanges(4); got != nil {
		t.Fatalf("ranges = %v after contiguous delivery, want none", got)
	}
	if got := r.ReceivedRanges(0); got != nil {
		t.Fatalf("ranges = %v with a zero cap, want none", got)
	}
}
