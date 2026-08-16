package pep

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bojieli/queqiao/internal/protocol"
)

const defaultChunkSize = 32 * 1024

type FlowStats struct {
	Started   time.Time
	Ended     time.Time
	BytesSent uint64
	BytesRead uint64
	// LaneBytes records payload bytes carried by each outer lane. It is
	// intentionally optional so one-lane callers can ignore it, while
	// benchmarks and operators can verify actual striping rather than merely
	// counting successful lane handshakes.
	LaneBytes map[uint64]LaneStats
}

type LaneStats struct {
	Kind     TransportKind
	Sent     uint64
	Received uint64
}

type closeWriter interface {
	CloseWrite() error
}

type flowRunner struct {
	fc           *frameConn
	inner        net.Conn
	sessionID    [16]byte
	flowID       uint64
	chunkSize    int
	class        atomic.Uint32
	sent         atomic.Uint64
	received     atomic.Uint64
	sendAckFlag  uint16
	recvAckFlag  uint16
	sendSequence atomic.Uint64
	finalAck     chan struct{}
	sendDone     chan struct{}
}

func newFlowRunner(fc *frameConn, inner net.Conn, sessionID [16]byte, flowID uint64, chunkSize int, sendAckFlag, recvAckFlag uint16) *flowRunner {
	if chunkSize <= 0 || chunkSize > int(fc.maxPayload) {
		chunkSize = defaultChunkSize
	}
	r := &flowRunner{
		fc: fc, inner: inner, sessionID: sessionID, flowID: flowID, chunkSize: chunkSize,
		sendAckFlag: sendAckFlag, recvAckFlag: recvAckFlag,
		finalAck: make(chan struct{}, 1), sendDone: make(chan struct{}),
	}
	r.class.Store(uint32(protocol.ClassNew))
	return r
}

func (r *flowRunner) run(ctx context.Context) (FlowStats, error) {
	stats := FlowStats{Started: time.Now()}
	type result struct {
		direction string
		err       error
	}
	results := make(chan result, 2)
	go func() { results <- result{direction: "send", err: r.sendInner(ctx)} }()
	go func() { results <- result{direction: "receive", err: r.receiveInner(ctx)} }()

	completed := 0
	for completed < 2 {
		select {
		case result := <-results:
			completed++
			if result.err != nil {
				_ = r.fc.Close()
				_ = r.inner.Close()
				stats.Ended = time.Now()
				stats.BytesSent = r.sent.Load()
				stats.BytesRead = r.received.Load()
				return stats, fmt.Errorf("%s direction: %w", result.direction, result.err)
			}
		case <-ctx.Done():
			_ = r.fc.Close()
			_ = r.inner.Close()
			stats.Ended = time.Now()
			stats.BytesSent = r.sent.Load()
			stats.BytesRead = r.received.Load()
			return stats, ctx.Err()
		}
	}
	_ = r.fc.Close()
	_ = r.inner.Close()
	stats.Ended = time.Now()
	stats.BytesSent = r.sent.Load()
	stats.BytesRead = r.received.Load()
	return stats, nil
}

func (r *flowRunner) sendInner(ctx context.Context) (err error) {
	defer close(r.sendDone)
	buf := make([]byte, r.chunkSize)
	var sequence uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := r.inner.Read(buf)
		if n > 0 {
			if ^uint64(0)-sequence < uint64(n) {
				return errors.New("flow send sequence overflow")
			}
			payload := append([]byte(nil), buf[:n]...)
			if writeErr := r.fc.Write(protocol.Frame{
				Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeData, SessionID: r.sessionID, FlowID: r.flowID, Sequence: sequence, Class: protocol.Class(r.class.Load())},
				Payload: payload,
			}); writeErr != nil {
				return writeErr
			}
			sequence += uint64(n)
			r.sent.Add(uint64(n))
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				r.sendSequence.Store(sequence)
				if writeErr := r.fc.Write(protocol.Frame{Header: protocol.Header{
					Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
					SessionID: r.sessionID, FlowID: r.flowID, Sequence: sequence, Class: protocol.Class(r.class.Load()),
				}}); writeErr != nil {
					return writeErr
				}
				select {
				case <-r.finalAck:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return err
		}
	}
}

func (r *flowRunner) receiveInner(ctx context.Context) error {
	var expected uint64
	remoteFin := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := r.fc.Read()
		if err != nil {
			return err
		}
		if f.Header.SessionID != r.sessionID || f.Header.FlowID != r.flowID {
			return errors.New("frame belongs to another session or flow")
		}
		switch f.Header.Type {
		case protocol.TypeData:
			if remoteFin {
				return errors.New("data received after flow FIN")
			}
			if f.Header.Sequence != expected {
				return fmt.Errorf("unexpected data sequence %d, want %d", f.Header.Sequence, expected)
			}
			if len(f.Payload) == 0 {
				return errors.New("empty data frame")
			}
			if ^uint64(0)-expected < uint64(len(f.Payload)) {
				return errors.New("flow receive sequence overflow")
			}
			if err := writeFull(r.inner, f.Payload); err != nil {
				return err
			}
			expected += uint64(len(f.Payload))
			r.received.Add(uint64(len(f.Payload)))
		case protocol.TypeClose:
			if f.Header.Flags&protocol.FlagFin == 0 || len(f.Payload) != 0 || f.Header.Sequence != expected {
				return errors.New("invalid flow close frame")
			}
			if cw, ok := r.inner.(closeWriter); ok {
				if err := cw.CloseWrite(); err != nil && !expectedHalfCloseError(err) {
					return err
				}
			}
			if err := r.fc.Write(protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeAck,
				Flags:     protocol.FlagAckFinal | r.recvAckFlag,
				SessionID: r.sessionID, FlowID: r.flowID, Sequence: expected,
				Class: protocol.Class(r.class.Load()),
			}}); err != nil {
				return err
			}
			remoteFin = true
			select {
			case <-r.sendDone:
				return nil
			default:
			}
		case protocol.TypeAck:
			if f.Header.Flags&protocol.FlagAckFinal != 0 && f.Header.Flags&r.sendAckFlag != 0 && f.Header.Sequence == r.sendSequence.Load() {
				select {
				case r.finalAck <- struct{}{}:
				default:
				}
				if remoteFin {
					return nil
				}
			}
		case protocol.TypeReset:
			if len(f.Payload) > 1 {
				return fmt.Errorf("peer reset flow: %s", string(f.Payload[1:]))
			}
			return errors.New("peer reset flow")
		default:
			return fmt.Errorf("unexpected flow frame type %d", f.Header.Type)
		}
	}
}

// expectedHalfCloseError handles the normal race where an HTTP client closes
// its local socket immediately after consuming the complete response, before
// the proxy's best-effort CloseWrite reaches the socket. These errors do not
// imply missing payload or a failed transport; all read/write errors remain
// fatal.
func expectedHalfCloseError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.EPIPE)
}

// expectedDestinationCloseError covers the peer-side destination reset that
// can follow a client's full SOCKS close immediately after its response. The
// server has already observed the client's FIN in this case, so no logical
// application bytes remain to deliver.
func expectedDestinationCloseError(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.ENOTCONN)
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
