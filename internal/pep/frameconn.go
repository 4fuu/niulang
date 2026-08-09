package pep

import (
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
}

func newFrameConn(conn io.ReadWriteCloser, maxPayload uint32) *frameConn {
	if maxPayload == 0 || maxPayload > protocol.DefaultMaxPayload {
		maxPayload = protocol.DefaultMaxPayload
	}
	return &frameConn{conn: conn, maxPayload: maxPayload}
}

func (c *frameConn) Read() (protocol.Frame, error) {
	return protocol.ReadFrame(c.conn, c.maxPayload)
}

func (c *frameConn) Write(f protocol.Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return protocol.WriteFrame(c.conn, f)
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
	return protocol.WriteFrame(c.conn, f)
}

func (c *frameConn) Close() error { return c.conn.Close() }
