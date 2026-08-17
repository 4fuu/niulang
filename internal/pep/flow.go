package pep

import (
	"errors"
	"io"
	"net"
	"syscall"
	"time"
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
