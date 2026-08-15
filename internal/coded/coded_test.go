package coded

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/icourses-dev/wanopt/internal/pathmodel"
)

// lossyPipe is a pair of carriers connected through an erasure channel, so a
// test can put the measured path between two coded paths without a socket.
type lossyPipe struct {
	mu     sync.Mutex
	rng    *rand.Rand
	loss   float64
	closed bool
	done   chan struct{}
	out    chan []byte
	in     chan []byte
	sent   int
	lost   int
}

func newPipes(seed int64, loss float64) (*lossyPipe, *lossyPipe) {
	a2b, b2a := make(chan []byte, 8192), make(chan []byte, 8192)
	return &lossyPipe{
			rng: rand.New(rand.NewSource(seed)), loss: loss,
			out: a2b, in: b2a, done: make(chan struct{}),
		}, &lossyPipe{
			rng: rand.New(rand.NewSource(seed + 1)), loss: loss,
			out: b2a, in: a2b, done: make(chan struct{}),
		}
}

func (p *lossyPipe) Send(d []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("closed")
	}
	p.sent++
	drop := p.loss > 0 && p.rng.Float64() < p.loss
	if drop {
		p.lost++
	}
	p.mu.Unlock()
	if drop {
		return nil
	}
	cp := append([]byte(nil), d...)
	select {
	case p.out <- cp:
	default: // the peer is not keeping up, which is a drop like any other
	}
	return nil
}

func (p *lossyPipe) Receive() ([]byte, error) {
	select {
	case d := <-p.in:
		return d, nil
	case <-p.done:
		return nil, io.EOF
	}
}

func (p *lossyPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.closed = true
		close(p.done)
	}
	return nil
}

func (p *lossyPipe) stats() (sent, lost int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sent, p.lost
}

// measuredPath is a model that already knows what the live channel does, which
// is what a real one gets from the congestion controller on the same
// connection.
func measuredPath(floor float64) *pathmodel.PathModel {
	m := pathmodel.NewPathModel()
	m.Report(1, floor, 5000, 2e6)
	return m
}

func paths(t *testing.T, seed int64, loss, floor float64) (*Path, *Path, *lossyPipe) {
	t.Helper()
	pa, pb := newPipes(seed, loss)
	cfg := Config{ShardBytes: 1100, RoundTrip: 60 * time.Millisecond, Path: measuredPath(floor)}
	a, b := New(pa, cfg), New(pb, cfg)
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b, pa
}

// The whole job: frames crossing the channel the path actually is, with the
// code repairing what it erases.
func TestFramesCrossTheMeasuredChannel(t *testing.T) {
	a, b, wire := paths(t, 1, 0.42, 0.42)

	const frames = 400
	sent := make([][]byte, frames)
	rng := rand.New(rand.NewSource(2))
	go func() {
		for i := range sent {
			sent[i] = make([]byte, 200+rng.Intn(3000))
			rng.Read(sent[i])
			if err := a.Send(sent[i]); err != nil {
				return
			}
		}
	}()

	// One receiver, not one per frame: Receive blocks until something arrives,
	// so a goroutine per attempt leaks one for every frame the channel ate.
	arrived := make(chan []byte, frames)
	go func() {
		defer close(arrived)
		for {
			frame, err := b.Receive()
			if err != nil {
				return
			}
			arrived <- frame
		}
	}()

	got := make([][]byte, 0, frames)
	// The path is unreliable by design, so waiting for every frame would be
	// waiting for something that was never promised. Wait until they stop
	// coming instead.
	quiet := time.NewTimer(3 * time.Second)
	defer quiet.Stop()
collect:
	for len(got) < frames {
		select {
		case frame, ok := <-arrived:
			if !ok {
				break collect
			}
			got = append(got, frame)
			quiet.Reset(3 * time.Second)
		case <-quiet.C:
			break collect
		}
	}

	// Frames may be lost; what must never happen is a frame arriving corrupt,
	// because the session above trusts what it is handed.
	byLength := map[string]bool{}
	for _, frame := range sent {
		byLength[string(frame)] = true
	}
	for i, frame := range got {
		if !byLength[string(frame)] {
			t.Fatalf("frame %d arrived corrupt (%d bytes)", i, len(frame))
		}
	}
	stats := a.Stats()
	wireSent, wireLost := wire.stats()
	t.Logf("%d of %d frames arrived; wire sent %d dropped %d (%.0f%%); plan (%d,%d) rate %.3f",
		len(got), frames, wireSent, wireLost, 100*float64(wireLost)/float64(wireSent),
		stats.Plan.K, stats.Plan.N, stats.Plan.Rate)
	if len(got) < frames*95/100 {
		t.Fatalf("only %d of %d frames survived a 42%% erasure channel", len(got), frames)
	}
}

// A path clean enough not to warrant parity must not be given any, and must
// say so, because that is what tells the layer above to use its stream instead.
func TestACleanPathDoesNotCode(t *testing.T) {
	a, _, _ := paths(t, 3, 0, 0)
	if a.Coding() {
		t.Fatalf("a lossless path reports it is coding: %+v", a.Stats().Plan)
	}
}

// And a path that erases must say the opposite, or the layer above will keep
// bulk on a stream that head-of-line blocks at every gap.
func TestAnErasingPathCodes(t *testing.T) {
	a, _, _ := paths(t, 4, 0.42, 0.42)
	if !a.Coding() {
		t.Fatalf("a 42%% erasure channel reports it is not coding: %+v", a.Stats().Plan)
	}
}

// Frames are packed together when several are waiting, because a block of one
// frame wastes most of what the code could have done. Nothing is timed: the
// signal is that the queue drained.
func TestWaitingFramesShareABlock(t *testing.T) {
	a, b, wire := paths(t, 5, 0, 0.42)
	const frames = 64
	payload := bytes.Repeat([]byte("x"), 500)
	for i := 0; i < frames; i++ {
		if err := a.Send(payload); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < frames; i++ {
		if _, err := b.Receive(); err != nil {
			t.Fatal(err)
		}
	}
	sent, _ := wire.stats()
	t.Logf("%d frames of %d bytes cost %d datagrams", frames, len(payload), sent)
	// One datagram per frame would mean nothing was ever packed. The code adds
	// parity, so the bound is generous; what it rules out is a block per frame
	// with no packing at all.
	if sent >= frames*3 {
		t.Fatalf("%d datagrams for %d frames: frames are not sharing blocks", sent, frames)
	}
}

// A frame no block can hold has to be refused rather than truncated, so the
// layer above can put it on the stream instead.
func TestAnOversizedFrameIsRefused(t *testing.T) {
	a, _, _ := paths(t, 6, 0, 0.42)
	huge := make([]byte, 8<<20)
	if err := a.Send(huge); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("sending an 8 MiB frame returned %v, want ErrFrameTooLarge", err)
	}
}

// A dead carrier must surface rather than hang.
func TestAClosedCarrierEndsThePath(t *testing.T) {
	a, _, _ := paths(t, 7, 0, 0)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = a.Receive()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("receive on a closed path hung")
	}
	if err := a.Send([]byte("x")); err == nil {
		t.Fatal("send on a closed path succeeded")
	}
}
