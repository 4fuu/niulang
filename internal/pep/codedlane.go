package pep

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/apernet/quic-go"

	"github.com/icourses-dev/wanopt/internal/coded"
	wancongestion "github.com/icourses-dev/wanopt/internal/congestion"
	"github.com/icourses-dev/wanopt/internal/fec"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
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

	// pending carries frames to the sealer. Writing to a queue rather than
	// straight through is what lets a block be sealed on the right signal.
	pending chan []byte
	done    chan struct{}
	closing sync.Once
	sealed  sync.WaitGroup
}

// Write queues a frame. The block it lands in is sealed by sealLoop, on the
// only signal that needs no tuning: whether anything else is waiting.
//
// Neither of the obvious policies works. Sealing per write puts one frame in
// each block, and blocks that small are too numerous for a receiver's report
// to name, so most fall through to the retransmission timer -- 48 KiB across
// the emulated erasure channel took 84 seconds that way. Waiting for a block
// to fill is worse: the layer above writes a frame and then waits for the
// answer, so a buffered frame is a session that never starts. Waiting a fixed
// two milliseconds is worse still, because on a request-response protocol
// every delay lands on the critical path of the next request and they compound.
//
// Draining the queue is the signal that fits, and it is not a policy at all.
// Under load there is always another frame waiting, so blocks fill and the
// code is efficient. When the producer stops, the block seals at once and the
// latency is the path's. Nothing has to be chosen, and nothing has to be
// re-chosen when the path or the traffic changes.
func (l *codedLane) Write(p []byte) (int, error) {
	frame := make([]byte, len(p))
	copy(frame, p)
	select {
	case l.pending <- frame:
		return len(p), nil
	case <-l.done:
		if err := l.Channel.Err(); err != nil {
			return 0, err
		}
		return 0, net.ErrClosed
	}
}

// sealLoop packs every frame that is already waiting into one block, then
// seals it because nothing more is waiting.
func (l *codedLane) sealLoop() {
	defer l.sealed.Done()
	for {
		var frame []byte
		select {
		case <-l.done:
			return
		case frame = <-l.pending:
		}
		if _, err := l.Channel.Write(frame); err != nil {
			return
		}
		// Everything already queued belongs in this block. Taking it costs
		// nothing and is what makes a busy lane efficient.
		for draining := true; draining; {
			select {
			case next := <-l.pending:
				if _, err := l.Channel.Write(next); err != nil {
					return
				}
			default:
				draining = false
			}
		}
		if err := l.Channel.Flush(); err != nil {
			return
		}
	}
}

func (l *codedLane) Close() error {
	l.closing.Do(func() { close(l.done) })
	l.sealed.Wait()
	err := l.Channel.Close()
	_ = l.conn.CloseWithError(0, "")
	if l.packet != nil {
		_ = l.packet.Close()
	}
	return err
}

// newCodedLane starts a lane and its sealer.
func newCodedLane(channel *coded.Channel, conn *quic.Conn, packet net.PacketConn, controller wancongestion.TelemetryProvider) *codedLane {
	lane := &codedLane{
		Channel: channel, conn: conn, packet: packet, controller: controller,
		// Deep enough that a burst of frames queues rather than blocking the
		// producer, shallow enough that the producer feels backpressure well
		// before the channel's own retention bound is reached.
		pending: make(chan []byte, 256),
		done:    make(chan struct{}),
	}
	lane.sealed.Add(1)
	go lane.sealLoop()
	return lane
}

// codedConfig sizes a channel for a connection. The shard size comes from the
// carrier rather than from a constant, because a datagram over the connection's
// limit is refused rather than fragmented.
func codedConfig(roundTrip time.Duration, path *pathmodel.PathModel) coded.Config {
	if roundTrip <= 0 {
		roundTrip = 300 * time.Millisecond
	}
	return coded.Config{
		ShardBytes: coded.ShardBytesFor(coded.DefaultDatagramBytes),
		// Interactive: a shorter block, so a repair lands well inside the
		// round trip a retransmission would have cost.
		Class:     fec.ClassInteractive,
		RoundTrip: roundTrip,
		// The path has already been measured by whatever else sends here, so
		// the first block is coded for what is known rather than for nothing.
		Path: path,
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
	channel := coded.NewChannel(carrier, codedConfig(0, pathmodel.Shared(peerKey(conn))))
	return newCodedLane(channel, conn, packetConn, controller), nil
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
		return newCodedLane(coded.NewChannel(carrier, codedConfig(0, pathmodel.Shared(peerKey(conn)))), conn, nil, controller), true, nil
	}
	return &quicStreamConn{stream: first.stream, conn: conn, controller: controller, closeConn: false}, false, nil
}
