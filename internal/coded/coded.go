// Package coded carries frames over an unreliable datagram service, repairing
// the path's erasures with a code so that most of them cost nothing.
//
// It does not make delivery reliable, and that is the point. The path this
// project targets erases about 42% of packets independently of the sending
// rate, and a code sized for that delivers all but about a thousandth of its
// blocks without a round trip. The remaining thousandth is a job the layer
// above already does: the session sequences by byte offset, acknowledges with
// ranges, retains what is unacknowledged and re-issues it.
//
// A reliability layer here would be a second and worse copy of that. The first
// version of this package was exactly that -- its own block acknowledgements,
// its own retransmission timer, its own flow control, its own in-order
// delivery -- and it carried 1.2 Mbit/s where the path carried 14.5, because
// its feedback was a timer where QUIC's is an arrival and its delivery was
// in-order where the session above already tolerates gaps.
//
// So here a block either repairs or it does not. If it does not, its frames
// are lost, which is a thing the session above is built to survive.
package coded

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/icourses-dev/wanopt/internal/fec"
	"github.com/icourses-dev/wanopt/internal/lossmodel"
	"github.com/icourses-dev/wanopt/internal/pathmodel"
)

// Carrier is the unreliable datagram service underneath. A QUIC connection
// with datagrams enabled is one; so is a UDP socket.
//
// Close must make a blocked Receive return, because the path waits for its
// receive loop to stop before reporting that it has.
type Carrier interface {
	Send([]byte) error
	Receive() ([]byte, error)
	Close() error
}

// SizedCarrier is a Carrier that knows how large a datagram it will accept. A
// carrier that does not implement it is taken at the Config's word.
type SizedCarrier interface {
	Carrier
	MaxDatagramBytes() int
}

var (
	// ErrDatagramTooLarge is a carrier refusing a datagram for its size. It is
	// not fatal: the limit moves with the path, and a datagram refused for
	// being over it is loss like any other.
	ErrDatagramTooLarge = errors.New("coded: datagram too large for the carrier")
	// ErrClosed is returned once the path has stopped.
	ErrClosed = errors.New("coded: path closed")
	// ErrFrameTooLarge means a frame cannot fit even a maximal block.
	ErrFrameTooLarge = errors.New("coded: frame exceeds one block")
)

const (
	// shardHeader is the transmission sequence, the block, the shard's index
	// within it, k, n, and the block's payload length.
	//
	// The transmission sequence is what makes the channel measurable: loss is
	// only visible as a gap in the order things were sent, and neither the
	// block nor the shard index is that order.
	shardHeader = 4 + 4 + 1 + 1 + 1 + 4
	// frameHeader prefixes each frame inside a block's payload.
	frameHeader = 4
	// maxInboundBlocks bounds what a receiver holds for blocks still arriving.
	// It only has to cover the blocks genuinely in flight; past that, a block
	// is older than any shard still coming for it.
	maxInboundBlocks = 64
)

// Config describes the path. Every field has a usable default.
type Config struct {
	// ShardBytes is the payload one shard carries. It is reduced to whatever
	// the carrier will actually accept.
	ShardBytes int
	// Class selects the latency-against-efficiency trade the code makes.
	Class fec.Class
	// RoundTrip bounds the block length: a block that takes longer to send
	// than a retransmission takes to arrive has given up what coding was for.
	RoundTrip time.Duration
	// TargetResidual is the share of blocks allowed to arrive unrepairable.
	// It should not be zero: driving it down costs parity geometrically, and
	// the residual is what the session above is good at.
	TargetResidual float64
	// Path is what the endpoint pair has been measured to do, shared with
	// everything else sending to it -- above all the congestion controller,
	// whose own acknowledgements reveal the erasure rate of exactly this
	// direction. Without it a path starts out believing it is clean and sends
	// its first blocks unprotected.
	Path *pathmodel.PathModel
	// Pending bounds the frames queued for sending before Send blocks.
	Pending int
}

func (c Config) withDefaults() Config {
	if c.ShardBytes <= 0 {
		c.ShardBytes = 1100
	}
	if c.RoundTrip <= 0 {
		c.RoundTrip = 300 * time.Millisecond
	}
	if c.TargetResidual <= 0 {
		c.TargetResidual = 1e-3
	}
	if c.Pending <= 0 {
		c.Pending = 256
	}
	return c
}

// Path carries frames over a datagram carrier, coded against erasure.
type Path struct {
	cfg     Config
	carrier Carrier

	pending  chan []byte
	received chan []byte

	mu     sync.Mutex
	blocks map[uint32]*inbound
	order  []uint32

	nextSeq   atomic.Uint32
	nextBlock atomic.Uint32

	estimator *lossmodel.Estimator
	// plan is cached because choosing one walks a binomial search, and the
	// question is asked per frame while the answer changes on the timescale
	// the path does.
	plan         atomic.Pointer[fec.Plan]
	plannedAt    atomic.Int64
	sent         atomic.Uint64
	repaired     atomic.Uint64
	unrepairable atomic.Uint64
	oversize     atomic.Uint64

	closeOnce sync.Once
	done      chan struct{}
	wg        sync.WaitGroup
	err       atomic.Pointer[error]
}

type inbound struct {
	shards  [][]byte
	present []bool
	held    int
	k, n    int
	length  int
	done    bool
}

// New starts a coded path over the carrier. Close stops it.
func New(carrier Carrier, cfg Config) *Path {
	cfg = cfg.withDefaults()
	p := &Path{
		cfg: cfg, carrier: carrier,
		pending:   make(chan []byte, cfg.Pending),
		received:  make(chan []byte, cfg.Pending),
		blocks:    make(map[uint32]*inbound),
		estimator: lossmodel.New(lossmodel.Config{ReorderTolerance: 32}),
		done:      make(chan struct{}),
	}
	p.wg.Add(2)
	go p.sendLoop()
	go p.receiveLoop()
	return p
}

// Send queues a frame. Delivery is not guaranteed: the code repairs what the
// path erases, and what it cannot repair is the caller's to notice.
func (p *Path) Send(frame []byte) error {
	if len(frame)+frameHeader > p.maxBlockPayload() {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(frame))
	}
	// Closed is checked before the queue, not alongside it: a select chooses
	// at random among ready cases, so offering both would accept sends on a
	// closed path whenever the queue happened to have room.
	select {
	case <-p.done:
		return p.failure()
	default:
	}
	queued := make([]byte, len(frame))
	copy(queued, frame)
	select {
	case p.pending <- queued:
		return nil
	case <-p.done:
		return p.failure()
	}
}

// Receive returns the next frame to arrive, repaired if it had to be.
func (p *Path) Receive() ([]byte, error) {
	select {
	case frame := <-p.received:
		return frame, nil
	case <-p.done:
		// Whatever was already repaired is still worth delivering.
		select {
		case frame := <-p.received:
			return frame, nil
		default:
		}
		return nil, p.failure()
	}
}

// sendLoop packs every frame that is already waiting into one block, then
// sends it because nothing more is waiting.
//
// Draining is the seal signal, and it is not a policy. Under load there is
// always another frame, so blocks fill and the code is efficient; when the
// producer stops the block goes at once, so latency is the path's. Neither
// size nor time is chosen, so neither has to be re-chosen when the path or the
// traffic changes -- and a fixed delay is worse than either, because on a
// request-response protocol every delay lands on the critical path of the next
// request and they compound.
func (p *Path) sendLoop() {
	defer p.wg.Done()
	for {
		var first []byte
		select {
		case <-p.done:
			return
		case first = <-p.pending:
		}
		block := appendFrame(make([]byte, 0, p.maxBlockPayload()), first)

		for packing := true; packing; {
			select {
			case next := <-p.pending:
				if len(block)+frameHeader+len(next) > p.maxBlockPayload() {
					if err := p.transmit(block); err != nil {
						return
					}
					block = appendFrame(block[:0], next)
					continue
				}
				block = appendFrame(block, next)
			default:
				packing = false
			}
		}
		if err := p.transmit(block); err != nil {
			return
		}
	}
}

func appendFrame(block, frame []byte) []byte {
	at := len(block)
	block = append(block, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(block[at:], uint32(len(frame)))
	return append(block, frame...)
}

// transmit codes one block and puts its shards on the wire.
func (p *Path) transmit(payload []byte) error {
	shardBytes := p.shardBytes()
	k := (len(payload) + shardBytes - 1) / shardBytes
	if k < 1 {
		k = 1
	}
	if perShard := (len(payload) + k - 1) / k; perShard < shardBytes {
		shardBytes = perShard
	}

	n := k
	if want, ok := fec.ShardsFor(k, p.channel(), p.params()); ok {
		n = want
	}
	shards := make([][]byte, n)
	for i := range shards {
		shards[i] = make([]byte, shardBytes)
	}
	for i := 0; i < k; i++ {
		lo := i * shardBytes
		if lo >= len(payload) {
			break
		}
		copy(shards[i], payload[lo:min(lo+shardBytes, len(payload))])
	}
	if n > k {
		codec, err := fec.New(k, n)
		if err != nil {
			n, shards = k, shards[:k]
		} else if err := codec.Encode(shards); err != nil {
			n, shards = k, shards[:k]
		}
	}

	block := p.nextBlock.Add(1) - 1
	for i := 0; i < n; i++ {
		d := make([]byte, shardHeader+len(shards[i]))
		binary.BigEndian.PutUint32(d, p.nextSeq.Add(1)-1)
		binary.BigEndian.PutUint32(d[4:], block)
		d[8] = byte(i)
		d[9] = byte(k - 1)
		d[10] = byte(n - 1)
		binary.BigEndian.PutUint32(d[11:], uint32(len(payload)))
		copy(d[shardHeader:], shards[i])

		err := p.carrier.Send(d)
		switch {
		case err == nil:
			p.sent.Add(1)
		case errors.Is(err, ErrDatagramTooLarge):
			// The path's estimate moved under us. Losing this shard is cheaper
			// than losing the connection, and the next block is sized from the
			// carrier's revised limit.
			p.oversize.Add(1)
		default:
			p.fail(err)
			return err
		}
	}
	return nil
}

func (p *Path) receiveLoop() {
	defer p.wg.Done()
	for {
		d, err := p.carrier.Receive()
		if err != nil {
			p.fail(err)
			return
		}
		p.onShard(d)
	}
}

func (p *Path) onShard(d []byte) {
	if len(d) <= shardHeader {
		return
	}
	seq := binary.BigEndian.Uint32(d)
	block := binary.BigEndian.Uint32(d[4:])
	index := int(d[8])
	k := int(d[9]) + 1
	n := int(d[10]) + 1
	length := int(binary.BigEndian.Uint32(d[11:]))
	shard := d[shardHeader:]
	if index >= n || k > n || length <= 0 {
		return
	}

	p.mu.Lock()
	// Every arrival measures the channel, including one for a block already
	// delivered: the gaps between transmission sequences are what loss is.
	p.estimator.Observe(uint64(seq))

	b := p.blocks[block]
	if b == nil {
		b = &inbound{
			shards: make([][]byte, n), present: make([]bool, n),
			k: k, n: n, length: length,
		}
		p.blocks[block] = b
		p.order = append(p.order, block)
		p.evictLocked()
	}
	if b.done || index >= len(b.present) || b.present[index] {
		p.mu.Unlock()
		return
	}
	b.shards[index] = append([]byte(nil), shard...)
	b.present[index] = true
	b.held++
	if b.held < b.k {
		p.mu.Unlock()
		return
	}
	frames := p.repairLocked(b)
	p.mu.Unlock()

	for _, frame := range frames {
		select {
		case p.received <- frame:
		case <-p.done:
			return
		}
	}
}

// repairLocked reconstructs a block and splits it into the frames it carried.
func (p *Path) repairLocked(b *inbound) [][]byte {
	if b.done {
		return nil
	}
	if b.n > b.k {
		codec, err := fec.New(b.k, b.n)
		if err != nil {
			return nil
		}
		if err := codec.Reconstruct(b.shards, b.present); err != nil {
			return nil
		}
	}
	payload := make([]byte, 0, b.length)
	for i := 0; i < b.k && len(payload) < b.length; i++ {
		take := min(len(b.shards[i]), b.length-len(payload))
		payload = append(payload, b.shards[i][:take]...)
	}
	if len(payload) != b.length {
		return nil
	}
	b.done, b.shards, b.present = true, nil, nil
	p.repaired.Add(1)

	var frames [][]byte
	for len(payload) >= frameHeader {
		size := int(binary.BigEndian.Uint32(payload))
		payload = payload[frameHeader:]
		if size > len(payload) {
			break
		}
		frames = append(frames, append([]byte(nil), payload[:size]...))
		payload = payload[size:]
	}
	return frames
}

// evictLocked drops the oldest blocks once too many are held. Nothing
// retransmits, so a block that has not completed by now will not.
func (p *Path) evictLocked() {
	for len(p.order) > maxInboundBlocks {
		oldest := p.order[0]
		p.order = p.order[1:]
		if b, ok := p.blocks[oldest]; ok {
			if !b.done {
				p.unrepairable.Add(1)
			}
			delete(p.blocks, oldest)
		}
	}
}

// shardBytes is the configured shard size, reduced to what the carrier
// accepts. The limit is not a constant: QUIC's estimate of what fits in a
// packet moves with the path, and a datagram over it is refused rather than
// fragmented.
func (p *Path) shardBytes() int {
	size := p.cfg.ShardBytes
	if sized, ok := p.carrier.(SizedCarrier); ok {
		if limit := sized.MaxDatagramBytes() - shardHeader; limit > 0 && limit < size {
			size = limit
		}
	}
	if size < 1 {
		size = 1
	}
	return size
}

// maxBlockPayload is the most one block may carry.
func (p *Path) maxBlockPayload() int {
	plan := p.currentPlan()
	shards := plan.K
	if !plan.Code || shards <= 0 {
		shards = 8
	}
	return shards * p.shardBytes()
}

// channel is what is known about the direction this path sends into.
//
// The peer is not asked. The congestion controller on this connection already
// measures it -- the erasure rate of the direction it sends into is exactly
// what its own acknowledgements reveal -- so the shared model has the answer
// before the first block is sealed, which is when it is needed. Asking the
// peer instead means the first blocks are sealed knowing nothing, and a block
// sealed knowing nothing carries no parity.
func (p *Path) channel() lossmodel.Snapshot {
	floor := 0.0
	if p.cfg.Path != nil {
		floor, _ = p.cfg.Path.Current()
	}
	return lossmodel.Snapshot{
		Loss: floor, Floor: floor, Recent: floor,
		BurstFactor: 1, ArrivalAfterLoss: 1 - floor,
	}
}

func (p *Path) params() fec.Params {
	return fec.Params{
		Class:           p.cfg.Class,
		ShardBytes:      p.shardBytes(),
		RateBytesPerSec: p.rate(),
		RoundTrip:       p.cfg.RoundTrip,
		InterleaveDepth: 1,
		TargetResidual:  p.cfg.TargetResidual,
	}
}

// rate is the sending rate the block length is sized against.
func (p *Path) rate() float64 {
	if p.cfg.Path != nil {
		if _, share := p.cfg.Path.Current(); share > 0 {
			return share
		}
	}
	return float64(p.shardBytes()) * 64 / p.cfg.RoundTrip.Seconds()
}

// planTTL is how long a chosen code stands before it is chosen again. The
// path's erasure rate moves on the scale of seconds, and the estimate behind
// it is filtered over thousands of packets, so a fifth of a second is far
// finer than the input can justify.
const planTTL = 200 * time.Millisecond

func (p *Path) currentPlan() fec.Plan {
	now := time.Now().UnixNano()
	if at := p.plannedAt.Load(); now-at < int64(planTTL) {
		if stored := p.plan.Load(); stored != nil {
			return *stored
		}
	}
	plan := fec.Choose(p.channel(), p.params())
	p.plan.Store(&plan)
	p.plannedAt.Store(now)
	return plan
}

// Coding reports whether this path is currently worth sending bulk payload
// over.
//
// It is false on a path clean enough that parity costs more than it saves, and
// then bulk belongs on the stream instead: datagrams have no reliability of
// their own, so an uncoded lost frame waits for the session's re-issue where a
// stream would have retransmitted it in a round trip. Asking this rather than
// configuring it is what lets one build serve both a clean path and a 42%
// erasure channel without being told which it is on.
func (p *Path) Coding() bool { return p.currentPlan().Code }

// Stats reports what the path has done and what it believes.
type Stats struct {
	Snapshot     lossmodel.Snapshot
	Plan         fec.Plan
	Sent         uint64
	Repaired     uint64
	Unrepairable uint64
	Oversize     uint64
}

// Stats reports what this path has done and what it believes.
func (p *Path) Stats() Stats {
	p.mu.Lock()
	snapshot := p.estimator.Snapshot()
	p.mu.Unlock()
	plan := fec.Plan{}
	if stored := p.plan.Load(); stored != nil {
		plan = *stored
	}
	return Stats{
		Snapshot: snapshot, Plan: plan,
		Sent: p.sent.Load(), Repaired: p.repaired.Load(),
		Unrepairable: p.unrepairable.Load(), Oversize: p.oversize.Load(),
	}
}

func (p *Path) fail(err error) {
	if err != nil && !errors.Is(err, io.EOF) {
		wrapped := fmt.Errorf("coded: carrier: %w", err)
		p.err.CompareAndSwap(nil, &wrapped)
	}
	p.closeOnce.Do(func() { close(p.done) })
}

func (p *Path) failure() error {
	if stored := p.err.Load(); stored != nil {
		return *stored
	}
	return ErrClosed
}

// Close stops the path and its carrier.
func (p *Path) Close() error {
	p.closeOnce.Do(func() { close(p.done) })
	err := p.carrier.Close()
	p.wg.Wait()
	return err
}
