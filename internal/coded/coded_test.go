package coded

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// lossyPipe is a pair of carriers connected to each other through an erasure
// channel, so a test can put the measured path between two channels without a
// socket.
type lossyPipe struct {
	mu     sync.Mutex
	rng    *rand.Rand
	loss   float64
	burst  float64
	inBad  bool
	closed bool
	done   chan struct{}
	out    chan []byte // where this end's Send goes
	in     chan []byte // where this end's Receive reads
	delay  time.Duration
	sent   int
	lost   int
}

func newPipes(seed int64, loss, burst float64, delay time.Duration) (*lossyPipe, *lossyPipe) {
	a2b := make(chan []byte, 8192)
	b2a := make(chan []byte, 8192)
	rng := rand.New(rand.NewSource(seed))
	a := &lossyPipe{rng: rng, loss: loss, burst: burst, out: a2b, in: b2a, delay: delay, done: make(chan struct{})}
	b := &lossyPipe{rng: rand.New(rand.NewSource(seed + 1)), loss: loss, burst: burst, out: b2a, in: a2b, delay: delay, done: make(chan struct{})}
	return a, b
}

func (p *lossyPipe) drop() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent++
	if p.loss <= 0 {
		return false
	}
	var lost bool
	if p.burst <= 1 {
		lost = p.rng.Float64() < p.loss
	} else {
		recover := 1 / p.burst
		enter := recover * p.loss / (1 - p.loss)
		if p.inBad {
			if p.rng.Float64() < recover {
				p.inBad = false
			}
			lost = true
		} else if p.rng.Float64() < enter {
			p.inBad = true
			lost = true
		}
	}
	if lost {
		p.lost++
	}
	return lost
}

func (p *lossyPipe) Send(d []byte) error {
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		return errors.New("closed")
	}
	if p.drop() {
		return nil
	}
	cp := append([]byte(nil), d...)
	deliver := func() {
		defer func() { _ = recover() }() // a send on a closed channel during teardown
		select {
		case p.out <- cp:
		default: // the peer is not keeping up; this is a drop like any other
		}
	}
	if p.delay > 0 {
		time.AfterFunc(p.delay, deliver)
		return nil
	}
	deliver()
	return nil
}

func (p *lossyPipe) Receive() ([]byte, error) {
	select {
	case d, ok := <-p.in:
		if !ok {
			return nil, io.EOF
		}
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

// transfer sends payload from a to b and returns what arrived.
func transfer(t *testing.T, a, b *Channel, payload []byte, timeout time.Duration) []byte {
	t.Helper()
	done := make(chan []byte, 1)
	go func() {
		got := make([]byte, 0, len(payload))
		buf := make([]byte, 32*1024)
		for len(got) < len(payload) {
			n, err := b.Read(buf)
			if n > 0 {
				got = append(got, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		done <- got
	}()
	go func() {
		if _, err := a.Write(payload); err != nil {
			t.Errorf("write: %v", err)
		}
		if err := a.Flush(); err != nil {
			t.Errorf("flush: %v", err)
		}
	}()
	select {
	case got := <-done:
		return got
	case <-time.After(timeout):
		t.Fatalf("transfer did not finish in %v", timeout)
		return nil
	}
}

func randomPayload(seed int64, n int) []byte {
	p := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(p)
	return p
}

// The point of the whole layer: a byte stream that arrives intact across the
// channel the path actually is.
func TestAStreamCrossesTheMeasuredChannelIntact(t *testing.T) {
	pa, pb := newPipes(1, 0.42, 1, 0)
	a := NewChannel(pa, Config{ShardBytes: 1100, RoundTrip: 60 * time.Millisecond})
	b := NewChannel(pb, Config{ShardBytes: 1100, RoundTrip: 60 * time.Millisecond})
	defer a.Close()
	defer b.Close()

	payload := randomPayload(7, 512*1024)
	got := transfer(t, a, b, payload, 60*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("stream corrupted: got %d bytes of %d", len(got), len(payload))
	}
	sent, lost := pa.stats()
	// The measurement belongs to the receiving end: loss is only visible where
	// packets fail to arrive. The sending end knows it only from the reports.
	snap, _ := b.Snapshot()
	_, plan := a.Snapshot()
	t.Logf("carrier sent %d datagrams, dropped %d (%.1f%%); estimator loss=%.3f floor=%.3f burst=%.2f memoryless=%v",
		sent, lost, 100*float64(lost)/float64(sent), snap.Loss, snap.Floor, snap.BurstFactor, snap.Memoryless)
	t.Logf("plan (%d,%d) rate=%.3f overhead=%.2fx: %s", plan.K, plan.N, plan.Rate, plan.Overhead(), plan.Why)
	t.Logf("wire cost %.2f datagrams per data shard", float64(sent)*1100/float64(len(payload)))
}

// Correlated loss is the other regime and must not corrupt or stall the
// stream, even though the code is less efficient against it.
func TestAStreamCrossesABurstyChannelIntact(t *testing.T) {
	pa, pb := newPipes(2, 0.42, 6, 0)
	a := NewChannel(pa, Config{ShardBytes: 1100, RoundTrip: 60 * time.Millisecond})
	b := NewChannel(pb, Config{ShardBytes: 1100, RoundTrip: 60 * time.Millisecond})
	defer a.Close()
	defer b.Close()

	payload := randomPayload(8, 256*1024)
	got := transfer(t, a, b, payload, 60*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("stream corrupted on a bursty channel: got %d of %d bytes", len(got), len(payload))
	}
	snap, _ := b.Snapshot()
	t.Logf("estimator loss=%.3f burst factor=%.2f memoryless=%v", snap.Loss, snap.BurstFactor, snap.Memoryless)
	if snap.Memoryless {
		t.Errorf("a channel with mean burst 6 was measured as memoryless: %+v", snap)
	}
}

// A path that loses almost nothing must not be made to carry parity. This is
// the check that the adaptivity goes both ways: the same code that spends 2x
// on the live path has to spend almost nothing on a clean one.
func TestACleanPathPaysAlmostNoOverhead(t *testing.T) {
	pa, pb := newPipes(3, 0, 1, 0)
	a := NewChannel(pa, Config{ShardBytes: 1100, RoundTrip: 20 * time.Millisecond})
	b := NewChannel(pb, Config{ShardBytes: 1100, RoundTrip: 20 * time.Millisecond})
	defer a.Close()
	defer b.Close()

	payload := randomPayload(9, 512*1024)
	got := transfer(t, a, b, payload, 30*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("stream corrupted on a clean path: got %d of %d bytes", len(got), len(payload))
	}
	sent, _ := pa.stats()
	overhead := float64(sent) * 1100 / float64(len(payload))
	t.Logf("clean path wire cost %.3f datagrams per data shard", overhead)
	if overhead > 1.2 {
		t.Fatalf("a lossless path cost %.2fx the wire; parity is being sent for nothing", overhead)
	}
}

// Every byte has to arrive even when the code cannot repair the block, which
// is what the retransmission path is for. Half the datagrams gone is well past
// what any rate this controller will choose can carry.
func TestRetransmissionCoversWhatTheCodeCannot(t *testing.T) {
	pa, pb := newPipes(4, 0.65, 1, 0)
	cfg := Config{ShardBytes: 1100, RoundTrip: 40 * time.Millisecond, ReportInterval: 15 * time.Millisecond}
	a := NewChannel(pa, cfg)
	b := NewChannel(pb, cfg)
	defer a.Close()
	defer b.Close()

	payload := randomPayload(10, 128*1024)
	got := transfer(t, a, b, payload, 90*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("stream did not survive 65%% loss: got %d of %d bytes", len(got), len(payload))
	}
}

// Short writes are the interactive case, and they must not each cost a full
// block's worth of datagrams.
func TestShortWritesDoNotCostAWholeBlock(t *testing.T) {
	pa, pb := newPipes(5, 0.42, 1, 0)
	cfg := Config{ShardBytes: 1100, RoundTrip: 40 * time.Millisecond, ReportInterval: 15 * time.Millisecond}
	a := NewChannel(pa, cfg)
	b := NewChannel(pb, cfg)
	defer a.Close()
	defer b.Close()

	const messages, size = 40, 64
	payload := randomPayload(11, messages*size)
	done := make(chan []byte, 1)
	go func() {
		got := make([]byte, 0, len(payload))
		buf := make([]byte, 4096)
		for len(got) < len(payload) {
			n, err := b.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		done <- got
	}()
	for i := 0; i < messages; i++ {
		if _, err := a.Write(payload[i*size : (i+1)*size]); err != nil {
			t.Fatal(err)
		}
		if err := a.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-done:
		if !bytes.Equal(got, payload) {
			t.Fatalf("short writes corrupted: %d of %d bytes", len(got), len(payload))
		}
	case <-time.After(60 * time.Second):
		t.Fatal("short writes did not arrive")
	}
	sent, _ := pa.stats()
	t.Logf("%d datagrams for %d messages of %d bytes: %.1f per message", sent, messages, size,
		float64(sent)/messages)
	// Eight copies of a one-shard block is what 42% loss costs to reach a
	// residual of a thousandth. Anything near a full block per message would
	// mean the plan's length was used instead of the data's.
	if float64(sent)/messages > 24 {
		t.Fatalf("%.1f datagrams per %d-byte message: short blocks are being sized "+
			"by the plan rather than by their own length", float64(sent)/messages, size)
	}
}

// Flow control has to exist and has to release. A sender that never blocked
// would hold the whole stream for repair; one that never woke would deadlock.
func TestWriteBlocksAndResumes(t *testing.T) {
	pa, pb := newPipes(6, 0.42, 1, 0)
	cfg := Config{
		ShardBytes: 1100, RoundTrip: 40 * time.Millisecond,
		ReportInterval: 10 * time.Millisecond, MaxOutstandingBlocks: 4,
	}
	a := NewChannel(pa, cfg)
	b := NewChannel(pb, cfg)
	defer a.Close()
	defer b.Close()

	payload := randomPayload(12, 1<<20)
	got := transfer(t, a, b, payload, 90*time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("bounded sender corrupted the stream: %d of %d bytes", len(got), len(payload))
	}
}

// A dead carrier must surface as an error on both sides rather than a hang.
func TestAClosedCarrierEndsTheChannel(t *testing.T) {
	pa, pb := newPipes(7, 0, 1, 0)
	a := NewChannel(pa, Config{ShardBytes: 1100, RoundTrip: 20 * time.Millisecond})
	b := NewChannel(pb, Config{ShardBytes: 1100, RoundTrip: 20 * time.Millisecond})
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("write to a closed channel succeeded")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = b.Read(make([]byte, 16))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read on a closed channel hung")
	}
}
