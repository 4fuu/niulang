package pep

import (
	"bytes"
	"io"
	"testing"

	"github.com/4fuu/niulang/internal/protocol"
)

type frameReaderBenchmarkConn struct{}

func (frameReaderBenchmarkConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (frameReaderBenchmarkConn) Write(p []byte) (int, error) { return len(p), nil }
func (frameReaderBenchmarkConn) Close() error                { return nil }

var frameConnBenchmarkSink *frameConn

type countingFrameReader struct {
	*bytes.Reader
	reads int
}

func (r *countingFrameReader) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func (r *countingFrameReader) Write(p []byte) (int, error) { return len(p), nil }
func (r *countingFrameReader) Close() error                { return nil }

func serializedDataFrame(t *testing.T, payloadBytes int) []byte {
	t.Helper()
	raw, err := protocol.AppendFrame(nil, protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData},
		Payload: bytes.Repeat([]byte{0x5a}, payloadBytes),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDefaultFrameReaderHoldsOneDefaultDataFrame(t *testing.T) {
	if defaultFrameReadBuffer != protocol.HeaderSize+defaultChunkSize {
		t.Fatalf("default reader buffer = %d, want header + default chunk = %d", defaultFrameReadBuffer, protocol.HeaderSize+defaultChunkSize)
	}
	conn := &countingFrameReader{Reader: bytes.NewReader(serializedDataFrame(t, defaultChunkSize))}
	fc := newFrameConn(conn)
	frame, err := fc.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Payload) != defaultChunkSize {
		t.Fatalf("payload = %d bytes, want %d", len(frame.Payload), defaultChunkSize)
	}
	if conn.reads != 1 {
		t.Fatalf("default DATA frame took %d underlying reads, want 1", conn.reads)
	}
}

func TestFrameReaderAcceptsMaxPayloadAcrossReads(t *testing.T) {
	conn := &countingFrameReader{Reader: bytes.NewReader(serializedDataFrame(t, protocol.MaxPayload))}
	frame, err := newFrameConn(conn).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Payload) != protocol.MaxPayload {
		t.Fatalf("payload = %d bytes, want protocol maximum %d", len(frame.Payload), protocol.MaxPayload)
	}
	if conn.reads < 2 {
		t.Fatalf("maximum frame took %d underlying reads; test did not exercise a frame larger than the buffer", conn.reads)
	}
}

func TestFrameReaderDefaultAndConfiguredSizesAreDistinct(t *testing.T) {
	if got := newFrameConn(frameReaderBenchmarkConn{}).reader.Size(); got != defaultFrameReadBuffer {
		t.Fatalf("default reader size = %d, want %d", got, defaultFrameReadBuffer)
	}
	if got := newFrameConnSized(frameReaderBenchmarkConn{}, maxFrameReadBuffer).reader.Size(); got != maxFrameReadBuffer {
		t.Fatalf("explicit reader size = %d, want %d", got, maxFrameReadBuffer)
	}
	if got := newFrameConnSized(frameReaderBenchmarkConn{}, maxFrameReadBuffer+1).reader.Size(); got != defaultFrameReadBuffer {
		t.Fatalf("invalid reader size fell back to %d, want default %d", got, defaultFrameReadBuffer)
	}
}

func BenchmarkFrameConnReaderConstruction(b *testing.B) {
	for _, test := range []struct {
		name string
		size int
	}{
		{name: "default", size: defaultFrameReadBuffer},
		{name: "explicit_64KiB", size: maxFrameReadBuffer},
	} {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(test.size), "reader-bytes/op")
			for i := 0; i < b.N; i++ {
				frameConnBenchmarkSink = newFrameConnSized(frameReaderBenchmarkConn{}, test.size)
			}
		})
	}
}
