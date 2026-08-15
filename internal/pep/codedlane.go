package pep

import (
	"sync"
	"time"

	"github.com/apernet/quic-go"

	"github.com/icourses-dev/wanopt/internal/coded"
	"github.com/icourses-dev/wanopt/internal/fec"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
	"github.com/icourses-dev/wanopt/internal/protocol"
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
// bulkDemux routes a connection's coded datagrams to the flow each frame
// belongs to.
//
// The datagrams are the connection's, not a lane's: one stream of them carries
// every flow multiplexed on that connection. Reading it from each lane, as the
// first version did, has every reader competing for frames that mostly belong
// to somebody else -- a frame consumed by the wrong flow is a frame the right
// one waits for forever -- and leaves a reader blocked on a connection-scoped
// receive long after its flow has gone.
//
// So there is one reader, and it demultiplexes on the flow identity the
// protocol already puts in every frame.
type bulkDemux struct {
	path *coded.Path
	mu   sync.Mutex
	// flows is keyed by flow identity. A frame for a flow nobody has claimed
	// is dropped, which is what the session's re-issue is for.
	flows map[uint64]chan protocol.Frame
}

func newBulkDemux(path *coded.Path, maxPayload uint32) *bulkDemux {
	d := &bulkDemux{path: path, flows: make(map[uint64]chan protocol.Frame)}
	go d.run(maxPayload)
	return d
}

func (d *bulkDemux) run(maxPayload uint32) {
	for {
		payload, err := d.path.Receive()
		if err != nil {
			d.mu.Lock()
			for id, ch := range d.flows {
				close(ch)
				delete(d.flows, id)
			}
			d.mu.Unlock()
			return
		}
		frame, err := protocol.ParseFrame(payload, maxPayload)
		if err != nil {
			// A block that only half arrived truncates its last frame, which
			// is ordinary loss rather than a defect.
			continue
		}
		d.mu.Lock()
		ch := d.flows[frame.Header.FlowID]
		d.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- frame:
		default:
			// The flow is not keeping up. Dropping is what an unreliable
			// substrate does, and the session re-issues what it needs.
		}
	}
}

// subscribe claims a flow's frames until release is called.
func (d *bulkDemux) subscribe(flowID uint64) <-chan protocol.Frame {
	d.mu.Lock()
	defer d.mu.Unlock()
	if existing, ok := d.flows[flowID]; ok {
		return existing
	}
	ch := make(chan protocol.Frame, 256)
	d.flows[flowID] = ch
	return ch
}

func (d *bulkDemux) release(flowID uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ch, ok := d.flows[flowID]; ok {
		delete(d.flows, flowID)
		close(ch)
	}
}

// bulkPaths holds one coded path per QUIC connection.
//
// Datagrams are a connection-level facility, not a stream-level one, so a path
// per stream would put several receive loops on one connection competing for
// the same arrivals -- and on a pooled connection would leave some streams
// with a coded substrate and some without, which is worse than none having
// one: a sender that codes into a receiver with no bulk reader loses every
// data frame it sends.
var (
	bulkPaths  sync.Map // *quic.Conn -> *coded.Path
	bulkDemuxs sync.Map // *coded.Path -> *bulkDemux
)

// connBulkDemux returns the demultiplexer for a connection's coded path.
func connBulkDemux(path *coded.Path, maxPayload uint32) *bulkDemux {
	if path == nil {
		return nil
	}
	if existing, ok := bulkDemuxs.Load(path); ok {
		return existing.(*bulkDemux)
	}
	created := newBulkDemux(path, maxPayload)
	actual, loaded := bulkDemuxs.LoadOrStore(path, created)
	if loaded {
		return actual.(*bulkDemux)
	}
	return created
}

// connBulkPath returns the connection's coded path, creating it once. It is
// closed with the connection, so no caller owns its lifetime.
func connBulkPath(conn *quic.Conn) *coded.Path {
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
		bulkDemuxs.Delete(created)
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
