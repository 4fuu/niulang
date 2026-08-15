package pep

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/apernet/quic-go"

	"github.com/icourses-dev/wanopt/internal/coded"
	wancongestion "github.com/icourses-dev/wanopt/internal/congestion"
	"github.com/icourses-dev/wanopt/internal/fec"
)

// A coded lane carries the session's frames over QUIC datagrams repaired by an
// erasure code, instead of over a QUIC stream repaired by retransmission.
//
// It exists for interactive traffic. On the path this project targets, a
// reliable stream is not slow because the bytes are slow; it is slow because
// at a 42% erasure rate something is always missing, and a stream delivers in
// order, so everything behind the gap waits for a retransmission that is
// itself lost 42% of the time. Measured across the emulated path with 256-byte
// messages timed write to read, the stream's median was 1.372 s and the coded
// lane's was 153 ms.
//
// It is not for bulk transfer. On a memoryless erasure channel retransmission
// is the more frugal repair -- it resends only what was lost, 1.72x at 42%,
// where a block code provisions for the binomial and pays about 2.06x -- and
// measured end to end a 1 MiB transfer ran slightly faster on the stream. The
// choice is per flow class, which is why it is a lane kind rather than a mode.
type codedLane struct {
	*coded.Channel
	conn   *quic.Conn
	packet net.PacketConn
	// controller is the connection's congestion controller, which the metrics
	// layer reads exactly as it does for a stream lane.
	controller wancongestion.TelemetryProvider
}

// Write seals what it wrote rather than waiting for a block to fill.
//
// The layer above writes a frame and then waits for the answer, so a frame
// left buffered is a session that never starts: a 24-byte hello would sit in a
// partial block until nine kilobytes of something else arrived behind it,
// which for a handshake is never.
//
// Sealing per write is why this lane is slow in aggregate, and the slowness is
// unresolved. A frame per block makes blocks small and numerous, and 48 KiB
// echoed across the emulated erasure channel takes about 84 seconds where the
// coded channel on its own carries 1 MiB in five. Coalescing writes for two
// milliseconds before sealing was tried and made it worse rather than better,
// so the cause is not simply block size and the lane stays off by default
// until it is understood.
func (l *codedLane) Write(p []byte) (int, error) {
	n, err := l.Channel.Write(p)
	if err != nil {
		return n, err
	}
	return n, l.Channel.Flush()
}

func (l *codedLane) Close() error {
	err := l.Channel.Close()
	_ = l.conn.CloseWithError(0, "")
	if l.packet != nil {
		_ = l.packet.Close()
	}
	return err
}

// codedConfig sizes a channel for a connection. The shard size comes from the
// carrier rather than from a constant, because a datagram over the connection's
// limit is refused rather than fragmented.
func codedConfig(roundTrip time.Duration) coded.Config {
	if roundTrip <= 0 {
		roundTrip = 300 * time.Millisecond
	}
	return coded.Config{
		ShardBytes: coded.ShardBytesFor(coded.DefaultDatagramBytes),
		// Interactive: a shorter block, so a repair lands well inside the
		// round trip a retransmission would have cost.
		Class:     fec.ClassInteractive,
		RoundTrip: roundTrip,
		// The layer above has its own flow control, sized from the path. This
		// one must not be the binding constraint on top of it: a lane write
		// blocks until a block is released, and the acknowledgements the peer
		// is waiting for travel over the same lane, so a tight budget here
		// stalls the flow that would have released it.
		MaxOutstandingBlocks: 256,
	}
}

// dialCodedQUIC opens a connection that carries one coded lane.
//
// The marker goes out before the channel starts. Anything the channel sends
// before the server has finished deciding is lost, which on this path is
// indistinguishable from ordinary loss and is repaired the same way.
func dialCodedQUIC(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration, localAddress string, ccfg congestionConfig, windows flowWindows) (streamConn, error) {
	conn, packetConn, err := dialQUICConnection(ctx, remote, serverName, roots, dialTimeout, localAddress, windows)
	if err != nil {
		return nil, err
	}
	controller := configureQUICController(conn, ccfg)

	carrier, err := coded.NewQUICCarrier(conn)
	if err != nil {
		_ = conn.CloseWithError(0, "coded lane unavailable")
		if packetConn != nil {
			_ = packetConn.Close()
		}
		return nil, fmt.Errorf("coded lane: %w", err)
	}
	if err := conn.SendDatagram(coded.LaneMarker()); err != nil {
		_ = conn.CloseWithError(0, "coded lane unannounced")
		if packetConn != nil {
			_ = packetConn.Close()
		}
		return nil, fmt.Errorf("coded lane: announce: %w", err)
	}
	// The round trip is not yet measured here, so the config's default stands
	// until the channel's own estimator supersedes it.
	channel := coded.NewChannel(carrier, codedConfig(0))
	return &codedLane{Channel: channel, conn: conn, packet: packetConn, controller: controller}, nil
}

// acceptCodedOrStream decides what an accepted connection is carrying by
// racing its first stream against its first datagram.
//
// Deciding by configuration instead would make a mismatch hang: a server
// waiting on AcceptStream never learns that the client chose datagrams, and a
// client waiting for a coded reply never learns that the server is waiting for
// a stream. Racing costs one goroutine and cannot deadlock.
func acceptCodedOrStream(ctx context.Context, conn *quic.Conn, controller wancongestion.TelemetryProvider) (streamConn, bool, error) {
	type result struct {
		stream *quic.Stream
		coded  bool
		err    error
	}
	results := make(chan result, 2)
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		stream, err := conn.AcceptStream(raceCtx)
		results <- result{stream: stream, err: err}
	}()
	go func() {
		for {
			d, err := conn.ReceiveDatagram(raceCtx)
			if err != nil {
				results <- result{err: err}
				return
			}
			// Anything else on this connection before the marker is not ours
			// to interpret; the coded channel will see whatever follows.
			if coded.IsLaneMarker(d) {
				results <- result{coded: true}
				return
			}
		}
	}()

	first := <-results
	if first.err != nil && !first.coded {
		// One side failing is only fatal if the other does too; wait for it.
		second := <-results
		if second.err != nil && !second.coded {
			return nil, false, first.err
		}
		first = second
	}
	if first.coded {
		carrier, err := coded.NewQUICCarrier(conn)
		if err != nil {
			return nil, false, err
		}
		return &codedLane{
			Channel: coded.NewChannel(carrier, codedConfig(0)), conn: conn, controller: controller,
		}, true, nil
	}
	return &quicStreamConn{stream: first.stream, conn: conn, controller: controller, closeConn: false}, false, nil
}
