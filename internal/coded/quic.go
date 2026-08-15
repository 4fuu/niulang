package coded

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/apernet/quic-go"
)

// DefaultDatagramBytes is a datagram size that crosses a 1500-byte path with
// room for the IP, UDP and QUIC headers.
const DefaultDatagramBytes = 1200

// QUICCarrier runs a coded channel over a QUIC connection's unreliable
// datagrams (RFC 9221).
//
// Datagrams rather than streams, because the erasure code has to see the
// erasures. A QUIC stream is already reliable: it retransmits what this layer
// would rather repair, and it delivers in order, so the code would arrive
// having nothing left to do and having paid for the parity anyway. The
// congestion controller, the pacer and the loss detector on the connection all
// still apply -- what is given up is the stream's retransmission and its head
// of line blocking, which are precisely the two things a 42% erasure channel
// makes expensive.
//
// A QUIC datagram is never fragmented and is dropped rather than retransmitted
// when the connection loses it, which is exactly the service this layer wants.
type QUICCarrier struct {
	conn *quic.Conn
	ctx  context.Context
	stop context.CancelFunc
	// limit is the largest datagram payload known to be accepted. quic-go does
	// not publish its estimate, but it reports the true figure when it refuses
	// one, so the limit is learned from the first refusal rather than guessed.
	limit atomic.Int64
}

// NewQUICCarrier wraps a connection whose Config enabled datagrams. It fails
// rather than silently degrading if the peer did not negotiate them, because
// falling back to a stream would restore the retransmission this design exists
// to avoid.
func NewQUICCarrier(conn *quic.Conn) (*QUICCarrier, error) {
	// Both ends have to agree: this end must have enabled datagrams in its
	// Config, and the peer must have advertised the transport parameter.
	if support := conn.ConnectionState().SupportsDatagrams; !support.Local || !support.Remote {
		return nil, fmt.Errorf("coded: QUIC datagrams not negotiated (local %v, remote %v)",
			support.Local, support.Remote)
	}
	ctx, stop := context.WithCancel(context.Background())
	c := &QUICCarrier{conn: conn, ctx: ctx, stop: stop}
	c.limit.Store(DefaultDatagramBytes)
	return c, nil
}

// MaxDatagramBytes is the largest datagram this carrier will accept.
func (c *QUICCarrier) MaxDatagramBytes() int { return int(c.limit.Load()) }

// ShardBytesFor is the shard payload that fits a datagram of the given size,
// which is what a Config's ShardBytes should be set to. A shard that does not
// fit is not sent at all, so the size is computed rather than discovered.
func ShardBytesFor(datagramBytes int) int {
	if n := datagramBytes - shardHeader; n > 0 {
		return n
	}
	return 0
}

func (c *QUICCarrier) Send(d []byte) error {
	err := c.conn.SendDatagram(d)
	if err == nil {
		return nil
	}
	// Too large is not fatal. The connection's estimate of what fits in a
	// packet moves with the path, so a shard sized against yesterday's
	// estimate can be refused today; killing the channel over it loses the
	// session for a reason that repairs itself. The revised limit is recorded
	// so the next block is sized correctly.
	var tooLarge *quic.DatagramTooLargeError
	if errors.As(err, &tooLarge) {
		c.limit.Store(tooLarge.MaxDatagramPayloadSize)
		return fmt.Errorf("%w: %d bytes offered, %d accepted",
			ErrDatagramTooLarge, len(d), tooLarge.MaxDatagramPayloadSize)
	}
	return err
}

func (c *QUICCarrier) Receive() ([]byte, error) {
	return c.conn.ReceiveDatagram(c.ctx)
}

func (c *QUICCarrier) Close() error {
	c.stop()
	return nil
}
