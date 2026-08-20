package pep

import (
	"errors"
	"io"
	"net"
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

// expectedHalfCloseError handles the normal race where an application closes
// its local socket immediately after consuming the complete response, before
// the proxy's read or best-effort CloseWrite observes an orderly EOF. Windows
// reports a full local close as WSAECONNRESET or WSAECONNABORTED; that is an
// application cancellation, not evidence that the outer tunnel failed. The
// codes themselves are per-platform -- the Windows ones are not the syscall
// constants of the same name -- so they live in sockerr_windows.go and
// sockerr_other.go.
func expectedHalfCloseError(err error) bool {
	return errors.Is(err, net.ErrClosed) || halfCloseErrno(err)
}

// expectedDestinationCloseError covers the peer-side destination reset that
// can follow a client's full SOCKS close immediately after its response. The
// server has already observed the client's FIN in this case, so no logical
// application bytes remain to deliver.
func expectedDestinationCloseError(err error) bool {
	return errors.Is(err, net.ErrClosed) || destinationCloseErrno(err)
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
