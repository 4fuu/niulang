package stripe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// drainLanes runs n lane workers, each taking chunks and completing them after
// the delay its rate implies. It returns the bytes each lane carried, which is
// the share the pacing produced.
func drainLanes(t *testing.T, s *Scheduler, rates []time.Duration) (map[uint64]uint64, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var mu sync.Mutex
	carried := make(map[uint64]uint64)
	received := make(map[uint64][]byte)

	var wg sync.WaitGroup
	for i := range rates {
		wg.Add(1)
		go func(laneID uint64, perByte time.Duration) {
			defer wg.Done()
			for {
				chunk, err := s.Next(ctx, laneID, 0)
				if err != nil || chunk == nil {
					return
				}
				if perByte > 0 {
					time.Sleep(perByte * time.Duration(len(chunk.Data)))
				}
				mu.Lock()
				carried[laneID] += uint64(len(chunk.Data))
				if len(chunk.Data) > 0 {
					received[chunk.Offset] = append([]byte(nil), chunk.Data...)
				}
				mu.Unlock()
				s.Complete(laneID, chunk)
			}
		}(uint64(i), rates[i])
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	var out []byte
	for offset := uint64(0); ; {
		data, ok := received[offset]
		if !ok {
			break
		}
		out = append(out, data...)
		offset += uint64(len(data))
	}
	return carried, out
}

func TestSchedulerDeliversEveryByteInOrder(t *testing.T) {
	payload := make([]byte, 512*1024)
	rand.New(rand.NewSource(1)).Read(payload)
	s := New(bytes.NewReader(payload), Config{ChunkSize: 8 * 1024, LaneWindow: 2})
	defer s.Close()

	_, got := drainLanes(t, s, []time.Duration{0, 0, 0})
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(payload))
	}
}

// The property the whole design exists for: a lane's share is whatever it can
// carry. Nothing measures the lanes or divides the work between them -- a fast
// lane simply comes back for more sooner.
func TestFastLaneCarriesMoreWithoutBeingMeasured(t *testing.T) {
	payload := make([]byte, 256*1024)
	s := New(bytes.NewReader(payload), Config{ChunkSize: 4 * 1024, LaneWindow: 1})
	defer s.Close()

	// Lane 0 is eight times slower per byte than lanes 1 and 2.
	carried, got := drainLanes(t, s, []time.Duration{
		800 * time.Nanosecond, 100 * time.Nanosecond, 100 * time.Nanosecond,
	})
	if len(got) != len(payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(payload))
	}
	slow, fast := carried[0], carried[1]+carried[2]
	if slow == 0 {
		t.Fatal("slow lane carried nothing; it should still contribute")
	}
	if fast <= slow*3 {
		t.Fatalf("fast lanes carried %d, slow lane %d: pacing did not follow capacity", fast, slow)
	}
}

// A lane that stops entirely must not be able to hold up the transfer. Under
// the design this replaces, the bytes committed to that lane's queue were
// already sequenced and the receiver could not deliver past them.
func TestStalledLaneDoesNotHoldUpTheTransfer(t *testing.T) {
	payload := make([]byte, 128*1024)
	rand.New(rand.NewSource(2)).Read(payload)
	s := New(bytes.NewReader(payload), Config{
		ChunkSize: 4 * 1024, LaneWindow: 2, RetransmitAfter: 50 * time.Millisecond,
	})
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A supervisor re-offers chunks the stalled lane is sitting on.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s.ReissueExpired()
			}
		}
	}()
	defer close(done)

	var mu sync.Mutex
	received := make(map[uint64][]byte)

	// Lane 0 takes a chunk and never completes it. Its goroutine never
	// returns, which is the point: only the healthy lane's progress is waited
	// on, because a real stalled lane does not politely exit either.
	go func() {
		if _, err := s.Next(ctx, 0, 0); err != nil {
			return
		}
		<-ctx.Done()
	}()

	// Lane 1 works normally and must be able to finish the whole transfer.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			chunk, err := s.Next(ctx, 1, 0)
			if err != nil || chunk == nil {
				return
			}
			mu.Lock()
			if len(chunk.Data) > 0 {
				received[chunk.Offset] = append([]byte(nil), chunk.Data...)
			}
			mu.Unlock()
			s.Complete(1, chunk)
		}
	}()

	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("a single stalled lane blocked the transfer")
	}

	mu.Lock()
	defer mu.Unlock()
	var out []byte
	for offset := uint64(0); ; {
		data, ok := received[offset]
		if !ok {
			break
		}
		out = append(out, data...)
		offset += uint64(len(data))
	}
	if !bytes.Equal(out, payload) {
		t.Fatalf("healthy lane delivered %d of %d bytes", len(out), len(payload))
	}
}

// A lane that dies must cost only what it was holding, and that must be
// recoverable on any other lane.
func TestRetiredLaneReleasesItsChunks(t *testing.T) {
	payload := make([]byte, 64*1024)
	rand.New(rand.NewSource(3)).Read(payload)
	s := New(bytes.NewReader(payload), Config{ChunkSize: 4 * 1024, LaneWindow: 3})
	defer s.Close()

	ctx := context.Background()
	// Lane 0 takes its window and dies.
	var held []*Chunk
	for i := 0; i < 3; i++ {
		chunk, err := s.Next(ctx, 0, 0)
		if err != nil || chunk == nil {
			t.Fatalf("lane 0 next: %v", err)
		}
		held = append(held, chunk)
	}
	s.RetireLane(0)

	// Every chunk it held must be available to lane 1.
	seen := make(map[uint64]bool)
	for range held {
		chunk, err := s.Next(ctx, 1, 0)
		if err != nil || chunk == nil {
			t.Fatalf("lane 1 next: %v", err)
		}
		seen[chunk.Offset] = true
		s.Complete(1, chunk)
	}
	for _, chunk := range held {
		if !seen[chunk.Offset] {
			t.Fatalf("chunk at offset %d was lost with the lane", chunk.Offset)
		}
	}
}

// A chunk must never be re-issued on the lane already carrying it: that is the
// one lane whose failure the copy would not survive.
func TestReissueAvoidsTheLaneAlreadyCarryingIt(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	payload := make([]byte, 8*1024)
	s := New(bytes.NewReader(payload), Config{
		ChunkSize: 4 * 1024, LaneWindow: 4, RetransmitAfter: time.Second, Now: clock,
	})
	defer s.Close()

	ctx := context.Background()
	first, err := s.Next(ctx, 0, 0)
	if err != nil || first == nil {
		t.Fatalf("next: %v", err)
	}
	now = now.Add(2 * time.Second)
	if n := s.ReissueExpired(); n != 1 {
		t.Fatalf("re-offered %d chunks, want 1", n)
	}
	// Lane 0 must not be handed the same chunk back.
	next, err := s.Next(ctx, 0, 0)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if next != nil && next.Offset == first.Offset {
		t.Fatal("re-issued a chunk on the lane already carrying it")
	}
	// Lane 1 must be.
	other, err := s.Next(ctx, 1, 0)
	if err != nil || other == nil {
		t.Fatalf("lane 1 next: %v", err)
	}
	if other.Offset != first.Offset {
		t.Fatalf("lane 1 got offset %d, want the expired chunk at %d", other.Offset, first.Offset)
	}
	// And either copy completing must release both lanes' hold on that chunk.
	// Lane 0 legitimately still carries the chunk it was handed instead, so
	// what matters is that its charge for *this* chunk was released.
	s.mu.Lock()
	before := s.laneLoad[0]
	s.mu.Unlock()
	s.Complete(1, other)
	s.mu.Lock()
	after := s.laneLoad[0]
	s.mu.Unlock()
	if after != before-1 {
		t.Fatalf("lane 0 charge went %d -> %d; completing elsewhere must release its attempt", before, after)
	}
}

// Memory must stay bounded when lanes are slower than the source.
func TestOutstandingIsBounded(t *testing.T) {
	payload := make([]byte, 1024*1024)
	s := New(bytes.NewReader(payload), Config{
		ChunkSize: 4 * 1024, LaneWindow: 2, MaxOutstanding: 8,
	})
	defer s.Close()

	ctx := context.Background()
	var held []*Chunk
	for i := 0; i < 2; i++ {
		chunk, err := s.Next(ctx, 0, 0)
		if err != nil || chunk == nil {
			t.Fatalf("next: %v", err)
		}
		held = append(held, chunk)
	}
	// Give the producer time to read ahead as far as it will.
	time.Sleep(100 * time.Millisecond)
	s.mu.Lock()
	buffered := len(s.pending) + len(s.live)
	s.mu.Unlock()
	if buffered > 8 {
		t.Fatalf("buffered %d chunks, want at most MaxOutstanding=8", buffered)
	}
	for _, c := range held {
		s.Complete(0, c)
	}
}

// The end of the stream must be ordered with the data, not race ahead of it.
func TestFinalChunkIsTheLastByteRange(t *testing.T) {
	payload := make([]byte, 10*1024)
	s := New(bytes.NewReader(payload), Config{ChunkSize: 4 * 1024, LaneWindow: 8})
	defer s.Close()

	ctx := context.Background()
	var last *Chunk
	var end uint64
	for {
		chunk, err := s.Next(ctx, 0, 0)
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if chunk == nil {
			break
		}
		if chunk.End() > end {
			end = chunk.End()
		}
		if chunk.Final {
			last = chunk
		}
		s.Complete(0, chunk)
	}
	if last == nil {
		t.Fatal("stream never produced a final chunk")
	}
	if last.End() != end {
		t.Fatalf("final chunk ends at %d but the stream ends at %d", last.End(), end)
	}
	if end != uint64(len(payload)) {
		t.Fatalf("stream ended at %d, want %d", end, len(payload))
	}
}

func TestSourceErrorIsReported(t *testing.T) {
	want := errors.New("source broke")
	s := New(&failingReader{err: want}, Config{ChunkSize: 1024, LaneWindow: 1})
	defer s.Close()

	_, err := s.Next(context.Background(), 0, 0)
	if !errors.Is(err, want) {
		t.Fatalf("next error = %v, want %v", err, want)
	}
}

type failingReader struct {
	err  error
	once bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.once {
		return 0, r.err
	}
	r.once = true
	return 0, r.err
}

func TestCloseReleasesWaiters(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	s := New(pr, Config{ChunkSize: 1024, LaneWindow: 1})

	errs := make(chan error, 1)
	go func() {
		_, err := s.Next(context.Background(), 0, 0)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	s.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("next after close = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release a waiting lane")
	}
}

func TestNextRespectsContext(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	s := New(pr, Config{ChunkSize: 1024, LaneWindow: 1})
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := s.Next(ctx, 0, 0)
		errs <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("next after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next ignored context cancellation")
	}
}

// The lane declares how much it can hold, so a lane whose capacity collapses
// stops being handed work without the scheduler measuring it.
func TestLaneWindowInBytesBoundsCommitment(t *testing.T) {
	payload := make([]byte, 256*1024)
	s := New(bytes.NewReader(payload), Config{ChunkSize: 4 * 1024, LaneWindow: 64})
	defer s.Close()

	ctx := context.Background()
	// A 16 KiB window admits four 4 KiB chunks and no more.
	var held []*Chunk
	for i := 0; i < 4; i++ {
		chunk, err := s.Next(ctx, 0, 16*1024)
		if err != nil || chunk == nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		held = append(held, chunk)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Next(ctx, 0, 16*1024)
	}()
	select {
	case <-done:
		t.Fatal("lane was handed a fifth chunk beyond its declared window")
	case <-time.After(100 * time.Millisecond):
	}
	// Completing one frees exactly one chunk's worth of room.
	s.Complete(0, held[0])
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("completing a chunk did not free window space")
	}
}

// A window smaller than one chunk must not deadlock the lane.
func TestTinyWindowStillAdmitsOneChunk(t *testing.T) {
	payload := make([]byte, 64*1024)
	s := New(bytes.NewReader(payload), Config{ChunkSize: 8 * 1024, LaneWindow: 4})
	defer s.Close()

	chunk, err := s.Next(context.Background(), 0, 128)
	if err != nil || chunk == nil {
		t.Fatalf("a window below one chunk stalled the lane: %v", err)
	}
}
