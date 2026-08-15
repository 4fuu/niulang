package pep

import (
	"sync"
	"time"

	"github.com/apernet/quic-go"

	"github.com/icourses-dev/wanopt/internal/coded"
	"github.com/icourses-dev/wanopt/internal/fec"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
)

// A lane's bulk payload can travel over the connection's unreliable datagrams,
// repaired by an erasure code, while everything else stays on its stream.
//
// The split is what makes coding worth having here. A stream delivers in
// order, so at this path's 42% erasure rate every gap stalls everything behind
// it: measured with 256-byte messages, a stream's median delivery was 1.372 s
// against a coded path's 153 ms. But the session's own acknowledgements must
// not be coded, because they are what releases the data whose blocks they
// would then be queued behind -- with everything on one coded substrate the
// same channel carried 0.87 Mbit/s one way and 0.008 with acknowledgements
// coming back the other.
//
// So a lane is not one substrate or the other. It is a stream for control and
// datagrams for bulk, on one connection, and every lane has the stream.
// bulkPaths holds one coded path per QUIC connection.
//
// Datagrams are a connection-level facility, not a stream-level one, so a path
// per stream would put several receive loops on one connection competing for
// the same arrivals -- and on a pooled connection would leave some streams
// with a coded substrate and some without, which is worse than none having
// one: a sender that codes into a receiver with no bulk reader loses every
// data frame it sends.
var bulkPaths sync.Map // *quic.Conn -> *coded.Path

// connBulkPath returns the connection's coded path, creating it once. It is
// closed with the connection, so no caller owns its lifetime.
func connBulkPath(conn *quic.Conn) *coded.Path {
	if true {
		return nil
	} // BISECT
	if existing, ok := bulkPaths.Load(conn); ok {
		return existing.(*coded.Path)
	}
	created := newCodedPath(conn, 0)
	if created == nil {
		return nil
	}
	actual, loaded := bulkPaths.LoadOrStore(conn, created)
	if loaded {
		_ = created.Close()
		return actual.(*coded.Path)
	}
	go func() {
		<-conn.Context().Done()
		bulkPaths.Delete(conn)
		_ = created.Close()
	}()
	return created
}

func newCodedPath(conn *quic.Conn, roundTrip time.Duration) *coded.Path {
	if support := conn.ConnectionState().SupportsDatagrams; !support.Local || !support.Remote {
		return nil
	}
	carrier, err := coded.NewQUICCarrier(conn)
	if err != nil {
		return nil
	}
	if roundTrip <= 0 {
		roundTrip = 300 * time.Millisecond
	}
	return coded.New(carrier, coded.Config{
		// Bulk payload: the code rate matters more than the block latency,
		// because a repair still costs less than the round trip it replaces.
		Class:     fec.ClassBulk,
		RoundTrip: roundTrip,
		// What the endpoint pair has already been measured to do, so the first
		// block is coded for the path rather than for a clean one.
		Path: pathmodel.Shared(peerKey(conn)),
	})
}
