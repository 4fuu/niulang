package pep

import (
	"bufio"
	"context"
	"io"
	"sync"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
)

type frameConn struct {
	conn       io.ReadWriteCloser
	maxPayload uint32
	writeMu    sync.Mutex
	// writeBuf is reused under writeMu so that every frame reaches the
	// transport as one write without allocating per frame.
	writeBuf []byte
	// reader coalesces the header and payload reads. A QUIC stream read is not
	// buffered, so reading a 46-byte header directly from it costs one full
	// stream-lock acquisition per frame in addition to the payload reads.
	reader *bufio.Reader
}

// frameReadBuffer is sized above one default chunk so a data frame's header
// and payload are normally satisfied from one underlying stream read.
const frameReadBuffer = 64 * 1024

func newFrameConn(conn io.ReadWriteCloser, maxPayload uint32) *frameConn {
	if maxPayload == 0 || maxPayload > protocol.DefaultMaxPayload {
		maxPayload = protocol.DefaultMaxPayload
	}
	return &frameConn{conn: conn, maxPayload: maxPayload, reader: bufio.NewReaderSize(conn, frameReadBuffer)}
}

func (c *frameConn) Read() (protocol.Frame, error) {
	return protocol.ReadFrame(c.reader, c.maxPayload)
}

func (c *frameConn) Write(f protocol.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeLocked(f)
}

func (c *frameConn) writeLocked(f protocol.Frame) error {
	buf, err := protocol.AppendFrame(c.writeBuf[:0], f)
	if err != nil {
		return err
	}
	// Retain the grown buffer for the next frame, but do not let one oversized
	// control payload pin a large allocation for the life of the lane.
	if cap(buf) <= int(c.maxPayload)+protocol.HeaderSize {
		c.writeBuf = buf
	}
	return writeFull(c.conn, buf)
}

const frameWriteTimeout = 15 * time.Second

// WriteContext bounds a transport write even when the peer stops consuming
// stream data.  A QUIC stream and TLS connection both expose a write-specific
// deadline; transports without that optional method retain their normal
// behavior and are still interruptible by Close from the flow coordinator.
func (c *frameConn) WriteContext(ctx context.Context, f protocol.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadlineConn, ok := c.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		deadline := time.Now().Add(frameWriteTimeout)
		if ctxDeadline, has := ctx.Deadline(); has && ctxDeadline.Before(deadline) {
			deadline = ctxDeadline
		}
		if err := deadlineConn.SetWriteDeadline(deadline); err != nil {
			return err
		}
		defer deadlineConn.SetWriteDeadline(time.Time{})
	}
	return c.writeLocked(f)
}

func (c *frameConn) Close() error { return c.conn.Close() }
