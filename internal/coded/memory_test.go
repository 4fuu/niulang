package coded

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type idleBenchmarkCarrier struct {
	closed chan struct{}
	once   sync.Once
}

func newIdleBenchmarkCarrier() *idleBenchmarkCarrier {
	return &idleBenchmarkCarrier{closed: make(chan struct{})}
}

func (c *idleBenchmarkCarrier) Send([]byte) error { return nil }

func (c *idleBenchmarkCarrier) Receive() ([]byte, error) {
	<-c.closed
	return nil, io.EOF
}

func (c *idleBenchmarkCarrier) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func BenchmarkIdlePathConstruction(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		path := New(newIdleBenchmarkCarrier(), Config{Pending: 1})
		if err := path.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

type observedReceiveCarrier struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newObservedReceiveCarrier() *observedReceiveCarrier {
	return &observedReceiveCarrier{started: make(chan struct{}), closed: make(chan struct{})}
}

func (c *observedReceiveCarrier) Send([]byte) error { return nil }

func (c *observedReceiveCarrier) Receive() ([]byte, error) {
	c.once.Do(func() { close(c.started) })
	<-c.closed
	return nil, io.EOF
}

func (c *observedReceiveCarrier) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

func TestIdlePathLeavesFECStateUnallocated(t *testing.T) {
	carrier := newObservedReceiveCarrier()
	path := New(carrier, Config{Pending: 1})
	select {
	case <-carrier.started:
	case <-time.After(time.Second):
		t.Fatal("receive loop did not enter Carrier.Receive")
	}

	if stats := path.Stats(); stats.Recovered != 0 || stats.Lost != 0 || stats.Window != 0 || stats.Plan != (Stats{}.Plan) {
		t.Fatalf("idle stats = %+v, want empty decoder semantics", stats)
	}
	_ = path.Coding()
	if path.encoder != nil || path.decoder != nil {
		t.Fatal("idle Stats or Coding allocated FEC state")
	}
	if err := path.Close(); err != nil {
		t.Fatal(err)
	}
	if path.encoder != nil || path.decoder != nil {
		t.Fatal("Close allocated FEC state")
	}
}

func sourceDatagram(seq, esi uint32, payload []byte) []byte {
	d := make([]byte, sourceHeader+symbolHeader+frameHeader+len(payload))
	binary.BigEndian.PutUint32(d, seq)
	d[4] = kindSource
	binary.BigEndian.PutUint32(d[5:], esi)
	vector := d[sourceHeader:]
	binary.BigEndian.PutUint16(vector, uint16(frameHeader+len(payload)))
	binary.BigEndian.PutUint16(vector[4:], 1)
	binary.BigEndian.PutUint32(vector[symbolHeader:], uint32(len(payload)))
	copy(vector[symbolHeader+frameHeader:], payload)
	return d
}

func repairDatagram(seq, rid, first uint32, count int) []byte {
	d := make([]byte, repairHeader)
	binary.BigEndian.PutUint32(d, seq)
	d[4] = kindRepair
	binary.BigEndian.PutUint32(d[5:], rid)
	binary.BigEndian.PutUint32(d[9:], first)
	binary.BigEndian.PutUint16(d[13:], uint16(count))
	return d
}

func TestReceiverAllocatesDecoderOnceForValidCodingTraffic(t *testing.T) {
	path := receiveOnly()
	if frames := path.onDatagram([]byte{0, 0, 0, 0, 9, 0, 0, 0, 0}); len(frames) != 0 {
		t.Fatalf("unknown datagram delivered %d frames", len(frames))
	}
	path.onDatagram(repairDatagram(1, 0, 0, 0))
	malformedSource := make([]byte, sourceHeader)
	malformedSource[4] = kindSource
	path.onDatagram(malformedSource)
	if path.decoder != nil {
		t.Fatal("unknown or malformed traffic allocated a decoder")
	}

	frames := path.onDatagram(sourceDatagram(2, 0, []byte("first")))
	if len(frames) != 1 || string(frames[0]) != "first" {
		t.Fatalf("first source delivered %q", frames)
	}
	decoder := path.decoder
	if decoder == nil {
		t.Fatal("first valid source did not allocate a decoder")
	}
	path.onDatagram(sourceDatagram(3, 1, []byte("second")))
	if path.decoder != decoder {
		t.Fatal("second source replaced the decoder")
	}

	repairOnly := receiveOnly()
	repairOnly.onDatagram(repairDatagram(1, 7, 0, 1))
	if repairOnly.decoder == nil {
		t.Fatal("first valid repair did not allocate a decoder")
	}
}

func TestFirstReceivedRepairAllocatesDecoderAndRecoversSource(t *testing.T) {
	pa, pb := newPipes(41, 0)
	dropped := false
	pa.drop = func(d []byte) bool {
		if !dropped && d[4] == kindSource {
			dropped = true
			return true
		}
		return false
	}
	cfg := Config{SymbolBytes: 1100, Path: measuredPath(0.42)}
	sender, receiver := New(pa, cfg), New(pb, cfg)
	defer sender.Close()
	defer receiver.Close()

	want := []byte("recovered from the first repair")
	if err := sender.Send(want); err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 1)
	go func() {
		frame, _ := receiver.Receive()
		received <- frame
	}()
	select {
	case got := <-received:
		if string(got) != string(want) {
			t.Fatalf("recovered frame = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("first received repair did not recover the erased source")
	}
	if !dropped || receiver.decoder == nil || receiver.Stats().Recovered == 0 {
		t.Fatalf("recovery did not exercise lazy decoder: dropped=%v stats=%+v", dropped, receiver.Stats())
	}
}

type observedSendCarrier struct {
	*observedReceiveCarrier
	sent chan byte
	err  error
}

func newObservedSendCarrier(err error) *observedSendCarrier {
	return &observedSendCarrier{observedReceiveCarrier: newObservedReceiveCarrier(), sent: make(chan byte, 16), err: err}
}

func (c *observedSendCarrier) Send(d []byte) error {
	c.sent <- d[4]
	return c.err
}

func TestSenderAllocatesEncoderOnceOnFirstSource(t *testing.T) {
	carrier := newObservedSendCarrier(nil)
	path := New(carrier, Config{Pending: 2})
	defer path.Close()
	if path.encoder != nil {
		t.Fatal("constructor allocated an encoder")
	}

	var encoder any
	for i, payload := range [][]byte{[]byte("first"), []byte("second")} {
		done := make(chan struct{})
		if err := path.SendTracked(payload, func() { close(done) }); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("sender did not hand source to carrier")
		}
		if kind := <-carrier.sent; kind != kindSource {
			t.Fatalf("first transmitted datagram kind = %d, want source", kind)
		}
		if path.encoder == nil {
			t.Fatal("source send did not allocate an encoder")
		}
		if i == 0 {
			encoder = path.encoder
		} else if path.encoder != encoder {
			t.Fatal("second source replaced the encoder")
		}
	}
}

func TestCarrierReceiveFailureDoesNotAllocateFECState(t *testing.T) {
	want := errors.New("receive failed")
	path := New(&receiveFailureCarrier{err: want}, Config{Pending: 1})
	select {
	case <-path.done:
	case <-time.After(time.Second):
		t.Fatal("carrier receive failure did not stop path")
	}
	if path.encoder != nil || path.decoder != nil {
		t.Fatal("carrier failure allocated FEC state")
	}
	if err := path.Close(); err != nil {
		t.Fatal(err)
	}
}

type receiveFailureCarrier struct{ err error }

func (c *receiveFailureCarrier) Send([]byte) error        { return c.err }
func (c *receiveFailureCarrier) Receive() ([]byte, error) { return nil, c.err }
func (c *receiveFailureCarrier) Close() error             { return nil }

func TestStatsConcurrentWithFirstReceive(t *testing.T) {
	path := receiveOnly()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := uint32(0); i < 1000; i++ {
			path.onDatagram(sourceDatagram(i, i, []byte("frame")))
		}
	}()
	for {
		select {
		case <-done:
			return
		default:
			_ = path.Stats()
		}
	}
}

func BenchmarkDecoderLazyAllocation(b *testing.B) {
	template := sourceDatagram(0, 0, []byte("frame"))
	b.Run("first_source", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			path := receiveOnly()
			path.onDatagram(append([]byte(nil), template...))
		}
	})
	b.Run("subsequent_sources", func(b *testing.B) {
		path := receiveOnly()
		path.onDatagram(append([]byte(nil), template...))
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			d := append([]byte(nil), template...)
			binary.BigEndian.PutUint32(d, uint32(i+1))
			binary.BigEndian.PutUint32(d[5:], uint32(i+1))
			path.onDatagram(d)
		}
	})
}

func BenchmarkEncoderLazyAllocation(b *testing.B) {
	newSender := func() *Path {
		return &Path{cfg: Config{}.withDefaults(), carrier: newIdleBenchmarkCarrier()}
	}
	payload := []byte("frame")
	b.Run("first_source", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := newSender().emit(payload, 0, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("subsequent_sources", func(b *testing.B) {
		path := newSender()
		if err := path.emit(payload, 0, 1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if err := path.emit(payload, 0, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}
