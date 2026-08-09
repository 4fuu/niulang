package pep

import (
	"io"
	"sync"

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

func (c *frameConn) Close() error { return c.conn.Close() }
