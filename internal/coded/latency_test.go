package coded

import (
	"context"
	"encoding/binary"
	"io"
	"sort"
	"testing"
	"time"

	wancongestion "github.com/icourses-dev/wanopt/internal/congestion"
)

// percentile returns the value at q of a sorted-in-place sample.
func percentile(samples []time.Duration, q float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	i := int(q * float64(len(samples)-1))
	return samples[i]
}

// Coding buys latency, not bandwidth. On a memoryless erasure channel
// retransmission is the more frugal of the two -- it resends only what was
// actually lost, 1/(1-p) = 1.72x at this path's 42%, where a block code has to
// provision for the binomial and pays about 2.06x. What retransmission cannot
// do is deliver on time: a lost packet costs a round trip, a packet lost twice
// costs two, and at 42% loss 18% of packets need three transmissions or more.
//
// This is the measurement that decides whether the whole design is worth its
// overhead, so it is made the way a user would feel it: small messages sent at
// a steady rate, timed from write to read, over the same path and the same
// congestion controller.
func TestInteractiveLatencyAgainstAReliableStream(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	const (
		messages    = 150
		messageSize = 256
		interval    = 20 * time.Millisecond
	)

	// A message carries its index so the receiver can time it without
	// assuming anything arrived in order or at all.
	stamp := func(buf []byte, i int) {
		binary.BigEndian.PutUint32(buf, uint32(i))
	}

	measure := func(t *testing.T, write func([]byte) error, read func([]byte) error) []time.Duration {
		sent := make([]time.Time, messages)
		got := make([]time.Duration, 0, messages)
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, messageSize)
			for i := 0; i < messages; i++ {
				if err := read(buf); err != nil {
					return
				}
				index := int(binary.BigEndian.Uint32(buf))
				if index >= 0 && index < messages && !sent[index].IsZero() {
					got = append(got, time.Since(sent[index]))
				}
			}
		}()
		buf := make([]byte, messageSize)
		for i := 0; i < messages; i++ {
			stamp(buf, i)
			sent[i] = time.Now()
			if err := write(buf); err != nil {
				t.Fatalf("write %d: %v", i, err)
			}
			time.Sleep(interval)
		}
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Log("not every message arrived within the timeout")
		}
		return got
	}

	var codedLatency, streamLatency []time.Duration

	t.Run("coded datagrams", func(t *testing.T) {
		client, server := quicPair(t, liveChannel())
		sendCarrier, err := NewQUICCarrier(client)
		if err != nil {
			t.Fatal(err)
		}
		recvCarrier, err := NewQUICCarrier(server)
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{
			ShardBytes:     ShardBytesFor(DefaultDatagramBytes),
			Class:          1, // fec.ClassInteractive
			RoundTrip:      300 * time.Millisecond,
			ReportInterval: 60 * time.Millisecond,
		}
		a, b := NewChannel(sendCarrier, cfg), NewChannel(recvCarrier, cfg)
		defer a.Close()
		defer b.Close()

		codedLatency = measure(t,
			func(p []byte) error {
				if _, err := a.Write(p); err != nil {
					return err
				}
				return a.Flush()
			},
			func(p []byte) error { _, err := io.ReadFull(readerOf(b), p); return err })
		t.Logf("coded: %d of %d arrived; median %v  p90 %v  p99 %v  max %v",
			len(codedLatency), messages,
			percentile(codedLatency, 0.5).Round(time.Millisecond),
			percentile(codedLatency, 0.9).Round(time.Millisecond),
			percentile(codedLatency, 0.99).Round(time.Millisecond),
			percentile(codedLatency, 1).Round(time.Millisecond))
	})

	t.Run("reliable stream", func(t *testing.T) {
		client, server := quicPair(t, liveChannel())
		streamCh := make(chan *quicStream, 1)
		go func() {
			s, err := server.AcceptStream(context.Background())
			if err != nil {
				return
			}
			streamCh <- &quicStream{s}
		}()
		out, err := client.OpenStreamSync(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The stream is not created until something is written to it.
		if _, err := out.Write(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		var in *quicStream
		select {
		case in = <-streamCh:
		case <-time.After(30 * time.Second):
			t.Fatal("stream not accepted")
		}
		if _, err := io.ReadFull(in, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}

		streamLatency = measure(t,
			func(p []byte) error { _, err := out.Write(p); return err },
			func(p []byte) error { _, err := io.ReadFull(in, p); return err })
		t.Logf("stream: %d of %d arrived; median %v  p90 %v  p99 %v  max %v",
			len(streamLatency), messages,
			percentile(streamLatency, 0.5).Round(time.Millisecond),
			percentile(streamLatency, 0.9).Round(time.Millisecond),
			percentile(streamLatency, 0.99).Round(time.Millisecond),
			percentile(streamLatency, 1).Round(time.Millisecond))
	})

	if len(codedLatency) == 0 || len(streamLatency) == 0 {
		t.Skip("one side delivered nothing; nothing to compare")
	}
	codedTail := percentile(codedLatency, 0.99)
	streamTail := percentile(streamLatency, 0.99)
	t.Logf("tail latency: coded %v against stream %v", codedTail.Round(time.Millisecond), streamTail.Round(time.Millisecond))
	if codedTail > streamTail {
		t.Errorf("coded tail latency %v is worse than the stream's %v; coding's whole "+
			"purpose on this path is to repair without a round trip", codedTail, streamTail)
	}
}

// quicStream adapts a QUIC stream to io.Reader for the comparison.
type quicStream struct {
	s interface{ Read([]byte) (int, error) }
}

func (q *quicStream) Read(p []byte) (int, error) { return q.s.Read(p) }

// readerOf adapts a Channel to io.Reader.
func readerOf(c *Channel) io.Reader { return channelReader{c} }

type channelReader struct{ c *Channel }

func (r channelReader) Read(p []byte) (int, error) { return r.c.Read(p) }

var _ = wancongestion.NewErasureSender
