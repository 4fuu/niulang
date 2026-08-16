package coded

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/pathmodel"
)

// lossyPipe is a pair of carriers connected through an erasure channel, so a
// test can put the measured path between two coded paths without a socket.
type lossyPipe struct {
	mu   sync.Mutex
	rng  *rand.Rand
	loss float64
	// drop erases particular datagrams rather than random ones, so a test can
	// ask what happens to a specific symbol.
	drop   func([]byte) bool
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
	if p.drop != nil && p.drop(d) {
		drop = true
	}
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
	m.Report(1, floor, 5000, 2e6, 0)
	return m
}

func paths(t *testing.T, seed int64, loss, floor float64) (*Path, *Path, *lossyPipe) {
	t.Helper()
	pa, pb := newPipes(seed, loss)
	cfg := Config{SymbolBytes: 1100, RoundTrip: 60 * time.Millisecond, Path: measuredPath(floor)}
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

// Frames are packed together when several are waiting, because a symbol
// carrying one small frame wastes most of the datagram it cost. Nothing is
// timed: the signal is that the queue drained.
func TestWaitingFramesShareASymbol(t *testing.T) {
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
	// parity, so the bound is generous; what it rules out is a symbol per frame
	// with no packing at all.
	if sent >= frames*3 {
		t.Fatalf("%d datagrams for %d frames: frames are not sharing symbols", sent, frames)
	}
}

// A frame larger than a symbol occupies symbols of its own, and arrives whole.
func TestALargeFrameIsCarriedInFragments(t *testing.T) {
	a, b, wire := paths(t, 20, 0, 0.42)
	frame := make([]byte, 32*1024)
	rand.New(rand.NewSource(21)).Read(frame)
	if err := a.Send(frame); err != nil {
		t.Fatal(err)
	}
	got, err := b.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("a %d-byte frame came back as %d bytes", len(frame), len(got))
	}
	sent, _ := wire.stats()
	t.Logf("a %d-byte frame cost %d datagrams", len(frame), sent)
	if sent < 20 {
		t.Fatalf("%d datagrams for a %d-byte frame: it was not fragmented", sent, len(frame))
	}
}

// A symbol lost for good costs the frames it carried and nothing behind them.
//
// This is the whole reason for using datagrams. A stream would have held every
// later frame until the lost one was retransmitted -- a round trip on a path
// whose round trip is the thing being avoided -- and so would a receiver that
// reassembled the symbols into one stream before reading frames out of it.
// Here the frames that were not in the lost symbol do not depend on it.
func TestALostSymbolDoesNotBlockWhatFollows(t *testing.T) {
	pa, pb := newPipes(22, 0)
	// Every repair is erased as well, so the lost symbol is lost for good and
	// what arrives is only what did not depend on it.
	sources := 0
	pa.drop = func(d []byte) bool {
		if d[4] == kindRepair {
			return true
		}
		sources++
		return sources == 2
	}
	cfg := Config{SymbolBytes: 1100, RoundTrip: 60 * time.Millisecond, Path: measuredPath(0.42)}
	a, b := New(pa, cfg), New(pb, cfg)
	t.Cleanup(func() { a.Close(); b.Close() })

	// Each frame is most of a symbol, so each gets one of its own.
	const frames = 5
	sent := make([][]byte, frames)
	for i := range sent {
		sent[i] = bytes.Repeat([]byte{byte('a' + i)}, 1000)
		if err := a.Send(sent[i]); err != nil {
			t.Fatal(err)
		}
	}

	arrived := make(chan []byte, frames)
	go func() {
		for {
			frame, err := b.Receive()
			if err != nil {
				return
			}
			arrived <- frame
		}
	}()

	var got [][]byte
	deadline := time.After(2 * time.Second)
	for len(got) < frames-1 {
		select {
		case frame := <-arrived:
			got = append(got, frame)
		case <-deadline:
			t.Fatalf("only %d of %d frames arrived; one erasure held back the "+
				"frames that did not depend on it", len(got), frames-1)
		}
	}
	for _, frame := range got {
		if len(frame) != 1000 {
			t.Fatalf("a frame arrived %d bytes long", len(frame))
		}
	}
}

// A short exchange is the case the code is worst at and the one that matters
// most: a lone symbol has nothing to share parity with, and nothing follows it
// to trigger a retransmission either. The tail protection is what covers it,
// and this measures whether it does.
func TestAShortExchangeSurvivesTheErasureChannel(t *testing.T) {
	a, b, wire := paths(t, 23, 0.42, 0.42)
	const exchanges = 100
	payload := bytes.Repeat([]byte("q"), 40)

	arrived := make(chan []byte, exchanges)
	go func() {
		for {
			frame, err := b.Receive()
			if err != nil {
				return
			}
			arrived <- frame
		}
	}()

	got := 0
	for i := 0; i < exchanges; i++ {
		if err := a.Send(payload); err != nil {
			t.Fatal(err)
		}
		select {
		case <-arrived:
			got++
		case <-time.After(100 * time.Millisecond):
		}
	}
	sent, lost := wire.stats()
	t.Logf("%d of %d short exchanges arrived; wire sent %d dropped %d (%.0f%%)",
		got, exchanges, sent, lost, 100*float64(lost)/float64(sent))
	if got < exchanges*95/100 {
		t.Fatalf("only %d of %d short exchanges survived: the tail of a burst is "+
			"not being protected", got, exchanges)
	}
}

// A frame no symbol can hold has to be refused rather than truncated, so the
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

// A path must be able to size its code from the direction it can measure.
//
// The erasure rate of the direction a sender sends into is learned from its
// own acknowledgements, and only a sender with something to send has any. A
// client that asks small questions and receives large answers never measures
// its own direction -- so with nothing else to go on it would carry its
// requests uncoded across a channel it has every reason to know erases.
//
// What it receives, it can always measure. Borrowing that is an assumption,
// and a weaker one than assuming the path is clean.
func TestAPathSizesItsCodeFromWhatItCanMeasure(t *testing.T) {
	// A model that knows nothing, which is what a lane carrying almost no
	// traffic leaves behind.
	pa, pb := newPipes(30, 0)
	cfg := Config{SymbolBytes: 1100, RoundTrip: 60 * time.Millisecond, Path: pathmodel.NewPathModel()}
	quiet, loud := New(pa, cfg), New(pb, cfg)
	t.Cleanup(func() { quiet.Close(); loud.Close() })

	if quiet.Coding() {
		t.Fatal("a path that has measured nothing at all decided to code")
	}

	// The other end sends, and the erasing channel is visible in the gaps of
	// the sequence it stamped, however little this end has sent.
	payload := bytes.Repeat([]byte("x"), 500)
	pb.mu.Lock()
	pb.loss = 0.42
	pb.mu.Unlock()
	for i := 0; i < 400; i++ {
		if err := loud.Send(payload); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("the receiving end never measured the channel: %+v", quiet.Stats().Snapshot)
		default:
		}
		if quiet.Stats().Snapshot.Decided >= reverseFloorSamples {
			break
		}
		if _, err := quiet.Receive(); err != nil {
			t.Fatal(err)
		}
	}

	// A path re-chooses its code on its own cadence, so the answer that
	// matters is the one after it has had the chance to.
	time.Sleep(codingTTL + 20*time.Millisecond)
	if !quiet.Coding() {
		t.Errorf("a path receiving across a 42%% erasure channel will not code what "+
			"it sends: %+v", quiet.Stats())
	}
}
