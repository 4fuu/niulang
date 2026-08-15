// Package coded carries a reliable, ordered byte stream over an unreliable
// datagram service, repairing loss with an erasure code first and
// retransmission only for what the code could not repair.
//
// This is the shape the measured path calls for. About 42% of packets are
// dropped independently of the sending rate, and a reliable stream built on
// retransmission alone spends 1.75 transmissions per packet and leaves 18% of
// them waiting three round trips or more. Coding spends the same bandwidth --
// on a memoryless erasure channel nothing beats (1-p) times the line rate --
// but spends it without the round trips.
//
// Retransmission does not disappear; it stops being the primary mechanism.
// Driving a code's residual failure rate to zero costs parity geometrically,
// so the rate controller aims at a residual near a thousandth and this layer
// retransmits that thousandth. What it retransmits is not the missing shards,
// which it cannot identify, but any shards the receiver does not hold: with an
// MDS code any k of the n reconstruct, so a repair is a matter of quantity and
// never of identity.
//
// A retransmission crosses the same erasure channel as everything else, so
// sending exactly the deficit would repair only 58% of the blocks that needed
// it. The deficit is divided by the arrival rate before it is sent.
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
)

// SizedCarrier is a Carrier that knows how large a datagram it will accept. A
// carrier that does not implement it is taken at the Config's word.
type SizedCarrier interface {
	Carrier
	MaxDatagramBytes() int
}

// ErrDatagramTooLarge is a carrier refusing a datagram for its size. It is not
// fatal: the limit moves with the path, and a datagram refused for being over
// it is loss like any other, which this layer repairs. A carrier reports it so
// the count is visible rather than silent.
var ErrDatagramTooLarge = errors.New("coded: datagram too large for the carrier")

// Carrier is the unreliable datagram service underneath. A QUIC connection
// with datagrams enabled is one; so is a UDP socket.
//
// Send may drop. Receive blocks until a datagram arrives or the carrier fails,
// and returns a slice the channel takes ownership of.
//
// Close must make a blocked Receive return. A carrier that does not will hang
// the channel's Close, because the channel waits for its receive loop to stop
// before reporting that it has.
type Carrier interface {
	Send([]byte) error
	Receive() ([]byte, error)
	Close() error
}

const (
	// datagram types.
	typeShard  = 0
	typeReport = 1

	// shardHeader is type, transmission sequence, block, shard index, k, n and
	// the block's data length. The sequence is what lets the receiver measure
	// the channel: loss is only visible as a gap in the order things were
	// sent, and the block and shard indices are not that order.
	shardHeader = 1 + 4 + 4 + 1 + 1 + 1 + 4
)

// Config describes the channel. The zero value is not usable; NewChannel fills
// in defaults for everything except the carrier.
type Config struct {
	// ShardBytes is the payload one shard carries. It must leave room for
	// shardHeader inside the carrier's datagram limit.
	ShardBytes int
	// Class selects the latency-against-efficiency trade the code makes.
	Class fec.Class
	// RoundTrip seeds the block length and the report interval before the
	// path has been measured.
	RoundTrip time.Duration
	// TargetResidual is the block failure rate the code aims at, which is what
	// retransmission is then left to cover.
	TargetResidual float64
	// MaxOutstandingBlocks bounds what the sender retains for repair, and so
	// bounds the memory both ends hold. A Write blocks once it is reached,
	// which is this layer's flow control.
	MaxOutstandingBlocks int
	// ReportInterval is how often the receiver tells the sender what it holds.
	ReportInterval time.Duration
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
	if c.MaxOutstandingBlocks <= 0 {
		// Small enough that a sender cannot commit a large transfer to the
		// wire before the first report tells it what the path is doing. Every
		// block sealed before that report is necessarily uncoded, because
		// nothing has measured the loss yet, and those blocks cost a
		// retransmission each.
		c.MaxOutstandingBlocks = 16
	}
	if c.ReportInterval <= 0 {
		// Half a round trip: often enough that a repair is under way before a
		// retransmission timer would have fired, rarely enough that reports
		// are a negligible share of the reverse path.
		c.ReportInterval = c.RoundTrip / 2
		if c.ReportInterval < 10*time.Millisecond {
			c.ReportInterval = 10 * time.Millisecond
		}
	}
	return c
}

var (
	ErrClosed = errors.New("coded: channel closed")
	// ErrStalled means the receiver stopped reporting progress for long enough
	// that repair cannot proceed. It is a dead path, not a slow one.
	ErrStalled = errors.New("coded: peer stopped acknowledging")
)

type sentBlock struct {
	shards  [][]byte
	k, n    int
	dataLen int
	sentAt  time.Time
	// repairs counts how many extra shards have been sent for this block. It
	// is a statistic, not a budget: a block that gives up is a stream that
	// stalls forever, because delivery is in order and nothing behind it can
	// pass. What bounds the effort is the backoff on deadline, which limits
	// the rate of retries rather than their number.
	repairs int
	// deadline is when this block is resent without having been asked for, and
	// retries is how many times that has happened. This is what covers the
	// case the report cannot: if every datagram of a block is lost, the peer
	// does not know the block exists, so it cannot ask for it and the sender
	// would wait for a report that will never mention it.
	deadline time.Time
	retries  int
}

type recvBlock struct {
	shards  [][]byte
	present []bool
	held    int
	k, n    int
	dataLen int
	firstAt time.Time
	data    []byte
	done    bool
}

// Channel is a reliable ordered byte stream over a datagram carrier. It is
// full duplex: Write sends, Read receives, and repair control travels in both
// directions on the same carrier.
type Channel struct {
	cfg     Config
	carrier Carrier

	mu     sync.Mutex
	cond   *sync.Cond
	closed bool
	err    error

	// Sending.
	nextBlock uint64
	nextSeq   uint32
	partial   []byte
	sent      map[uint64]*sentBlock
	acked     uint64 // every block below this is complete at the peer

	// Receiving.
	blocks       map[uint64]*recvBlock
	deliverNext  uint64
	highestBlock uint64
	seenAny      bool
	ready        []byte

	// Measurement. The estimator measures the inbound direction, because loss
	// is only visible where packets fail to arrive. The code protecting the
	// outbound direction therefore cannot be sized from it: the peer measures
	// that one, and reports it back. Sizing a code from the wrong direction's
	// loss is silently wrong on any asymmetric path, and on a symmetric one it
	// is wrong until the reverse direction happens to carry traffic.
	estimator *lossmodel.Estimator
	plan      fec.Plan
	rate      float64 // bytes per second, as last observed
	// peerFloor and peerBurst are what the peer last measured on the direction
	// this end sends into. Before any report they are zero, which reads as an
	// unmeasured path and produces no parity -- the first block is uncoded and
	// repaired by retransmission, and every block after it is coded.
	peerFloor float64
	peerBurst float64

	// outbox is the one place datagrams leave from. A carrier's Send may
	// block -- quic-go's datagram queue blocks once it holds its maximum --
	// and the receive path must never be the thing that blocks on it: repairs
	// are sent in response to a report, so a blocking Send inside the receive
	// loop stops the loop that would have read the acknowledgements that
	// unblock it. Over an in-memory carrier that never blocks this is
	// invisible; over QUIC it is a deadlock, and it stalled a 1 MiB transfer
	// indefinitely.
	outbox   chan []byte
	dropped  atomic.Uint64
	oversize atomic.Uint64

	wg   sync.WaitGroup
	stop chan struct{}
}

// NewChannel starts a coded channel over the carrier. Close stops it.
func NewChannel(carrier Carrier, cfg Config) *Channel {
	cfg = cfg.withDefaults()
	c := &Channel{
		cfg:       cfg,
		carrier:   carrier,
		sent:      make(map[uint64]*sentBlock),
		blocks:    make(map[uint64]*recvBlock),
		estimator: lossmodel.New(lossmodel.Config{}),
		stop:      make(chan struct{}),
		// Deep enough to hold a whole block plus the repairs a report can ask
		// for, so neither has to be dropped merely for arriving together.
		outbox: make(chan []byte, 2*MaxBlockShards),
	}
	c.cond = sync.NewCond(&c.mu)
	c.wg.Add(3)
	go c.receiveLoop()
	go c.reportLoop()
	go c.sendLoop()
	return c
}

// MaxBlockShards is the largest number of datagrams one block can become.
const MaxBlockShards = 256

// send hands a datagram to the writer. Application data waits for room,
// because that is the backpressure the producer should feel. Control and
// repair traffic never waits: a dropped repair is loss, which this protocol
// already repairs, while a blocked control path is a stall it does not.
func (c *Channel) send(d []byte, wait bool) {
	if wait {
		select {
		case c.outbox <- d:
		case <-c.stop:
		}
		return
	}
	select {
	case c.outbox <- d:
	default:
		c.dropped.Add(1)
	}
}

func (c *Channel) sendLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.stop:
			return
		case d := <-c.outbox:
			err := c.carrier.Send(d)
			if errors.Is(err, ErrDatagramTooLarge) {
				// The path's estimate moved under us. Losing this shard is
				// cheaper than losing the connection, and the next block is
				// sized from the carrier's revised limit.
				c.oversize.Add(1)
				continue
			}
			if err != nil {
				c.failWith(err)
				return
			}
		}
	}
}

// Write appends to the stream. It blocks while the sender is holding as many
// blocks as it is allowed to, which is this layer's flow control: the peer's
// reports are what release it.
func (c *Channel) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		c.mu.Lock()
		if err := c.fatal(); err != nil {
			c.mu.Unlock()
			return written, err
		}
		blockData := c.blockDataBytes()
		room := blockData - len(c.partial)
		take := min(room, len(p))
		c.partial = append(c.partial, p[:take]...)
		p = p[take:]
		written += take
		full := len(c.partial) >= blockData
		c.mu.Unlock()
		if full {
			if err := c.Flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

// Flush seals whatever is buffered into a block and sends it. A stream that
// stops mid-block would otherwise leave those bytes undelivered until the next
// write, which for an interactive flow is indistinguishable from a stall.
func (c *Channel) Flush() error {
	c.mu.Lock()
	if err := c.fatal(); err != nil {
		c.mu.Unlock()
		return err
	}
	if len(c.partial) == 0 {
		c.mu.Unlock()
		return nil
	}
	// Wait for room. The bound is on retained blocks, because that is what
	// both ends have to hold for a repair to be possible at all.
	for len(c.sent) >= c.cfg.MaxOutstandingBlocks {
		c.cond.Wait()
		if err := c.fatal(); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	data := c.partial
	c.partial = nil
	_, datagrams := c.sealLocked(data)
	c.mu.Unlock()

	for _, d := range datagrams {
		c.send(d, true)
	}
	return nil
}

// sealLocked encodes one block and builds its datagrams. It must be called
// with the lock held; the datagrams are sent outside it so a slow carrier does
// not block the receive path.
func (c *Channel) sealLocked(data []byte) (uint64, [][]byte) {
	plan := c.planLocked()

	// k is however many shards this block's bytes actually fill, which for a
	// flushed short write is far fewer than the plan's. Sizing a short block
	// by the plan's rate would send a long block's worth of datagrams for a
	// few bytes and still under-protect them.
	//
	// It is the shard size that fixes k, never the plan's. Clamping k to the
	// plan instead makes the shards grow when the plan shrinks between the
	// write that filled the buffer and the flush that seals it, and a shard
	// one byte over the carrier's datagram limit is not a slow path but a
	// dead one.
	shardBytes := c.shardBytes()
	k := (len(data) + shardBytes - 1) / shardBytes
	if k < 1 {
		k = 1
	}
	if perShard := (len(data) + k - 1) / k; perShard < shardBytes {
		shardBytes = perShard
	}
	if shardBytes < 1 {
		shardBytes = 1
	}
	_ = plan

	n := k
	if want, ok := fec.ShardsFor(k, c.outboundLocked(), c.paramsLocked()); ok {
		n = want
	}

	shards := make([][]byte, n)
	for i := range shards {
		shards[i] = make([]byte, shardBytes)
	}
	for i := 0; i < k; i++ {
		lo := i * shardBytes
		if lo >= len(data) {
			break
		}
		copy(shards[i], data[lo:min(lo+shardBytes, len(data))])
	}
	if n > k {
		codec, err := fec.New(k, n)
		if err != nil {
			n, shards = k, shards[:k]
		} else if err := codec.Encode(shards); err != nil {
			n, shards = k, shards[:k]
		}
	}

	id := c.nextBlock
	c.nextBlock++
	now := time.Now()
	c.sent[id] = &sentBlock{
		shards: shards, k: k, n: n, dataLen: len(data),
		sentAt: now, deadline: now.Add(c.retransmitAfter()),
	}

	datagrams := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		datagrams = append(datagrams, c.encodeShardLocked(id, i, k, n, len(data), shards[i]))
	}
	return id, datagrams
}

func (c *Channel) encodeShardLocked(block uint64, index, k, n, dataLen int, payload []byte) []byte {
	d := make([]byte, shardHeader+len(payload))
	d[0] = typeShard
	binary.BigEndian.PutUint32(d[1:], c.nextSeq)
	c.nextSeq++
	binary.BigEndian.PutUint32(d[5:], uint32(block))
	d[9] = byte(index)
	d[10] = byte(k - 1)
	d[11] = byte(n - 1)
	binary.BigEndian.PutUint32(d[12:], uint32(dataLen))
	copy(d[shardHeader:], payload)
	return d
}

// blockDataBytes is how many stream bytes make one block under the current
// plan.
func (c *Channel) blockDataBytes() int {
	plan := c.planLocked()
	k := plan.K
	if !plan.Code || k <= 0 {
		k = 8
	}
	return k * c.shardBytes()
}

// shardBytes is the configured shard size, reduced to whatever the carrier
// will actually accept.
//
// The limit is not a constant. QUIC's estimate of what fits in a packet moves
// with the path, and a datagram over it is refused rather than fragmented, so
// the size has to be asked for rather than assumed.
func (c *Channel) shardBytes() int {
	size := c.cfg.ShardBytes
	if sized, ok := c.carrier.(SizedCarrier); ok {
		if limit := sized.MaxDatagramBytes() - shardHeader; limit > 0 && limit < size {
			size = limit
		}
	}
	if size < 1 {
		size = 1
	}
	return size
}

// planLocked re-sizes the code from what the estimator currently believes. It
// is cheap, and re-deciding per block is what makes the code adaptive: a path
// whose loss doubles gets a stronger code within one block rather than one
// connection.
func (c *Channel) planLocked() fec.Plan {
	c.plan = fec.Choose(c.outboundLocked(), c.paramsLocked())
	return c.plan
}

// outboundLocked is what the peer last measured about the direction this end
// sends into, in the shape the rate controller reads.
func (c *Channel) outboundLocked() lossmodel.Snapshot {
	burst := c.peerBurst
	if burst < 1 {
		burst = 1
	}
	return lossmodel.Snapshot{
		Loss: c.peerFloor, Floor: c.peerFloor, Recent: c.peerFloor,
		BurstFactor: burst, ArrivalAfterLoss: 1 - c.peerFloor,
	}
}

// paramsLocked describes this flow to the rate controller.
func (c *Channel) paramsLocked() fec.Params {
	rate := c.rate
	if rate <= 0 {
		// Before anything has been measured, assume the block length the round
		// trip allows at a modest rate rather than a long one, so a first
		// block cannot be a large latency commitment.
		rate = float64(c.cfg.ShardBytes) * 64 / c.cfg.RoundTrip.Seconds()
	}
	return fec.Params{
		Class:           c.cfg.Class,
		ShardBytes:      c.cfg.ShardBytes,
		RateBytesPerSec: rate,
		RoundTrip:       c.cfg.RoundTrip,
		InterleaveDepth: 1,
		TargetResidual:  c.cfg.TargetResidual,
	}
}

// retransmitAfter is how long a block waits, unmentioned by any report,
// before the sender resends it unasked. It is longer than a round trip so a
// report in flight is never raced, and it is the only timer in this layer:
// everything else is driven by what the peer says it holds.
func (c *Channel) retransmitAfter() time.Duration {
	return c.cfg.RoundTrip*3/2 + c.cfg.ReportInterval
}

// Read returns stream bytes in order.
func (c *Channel) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.ready) == 0 {
		if c.closed {
			if c.err != nil {
				return 0, c.err
			}
			return 0, io.EOF
		}
		c.cond.Wait()
	}
	n := copy(p, c.ready)
	c.ready = c.ready[n:]
	return n, nil
}

func (c *Channel) receiveLoop() {
	defer c.wg.Done()
	for {
		d, err := c.carrier.Receive()
		if err != nil {
			c.failWith(err)
			return
		}
		if len(d) == 0 {
			continue
		}
		switch d[0] {
		case typeShard:
			c.onShard(d)
		case typeReport:
			c.onReport(d)
		}
	}
}

func (c *Channel) onShard(d []byte) {
	if len(d) < shardHeader {
		return
	}
	seq := binary.BigEndian.Uint32(d[1:])
	block := uint64(binary.BigEndian.Uint32(d[5:]))
	index := int(d[9])
	k := int(d[10]) + 1
	n := int(d[11]) + 1
	dataLen := int(binary.BigEndian.Uint32(d[12:]))
	payload := d[shardHeader:]
	if index >= n || k > n || len(payload) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	// The estimator measures the channel, so every arrival counts -- including
	// a duplicate or a shard for a block already delivered, because the gaps
	// between transmission sequence numbers are what loss looks like.
	c.estimator.Observe(uint64(seq))

	if block < c.deliverNext {
		return // already delivered
	}
	if !c.seenAny || block > c.highestBlock {
		c.highestBlock = block
		c.seenAny = true
	}
	b := c.blocks[block]
	if b == nil {
		if len(c.blocks) >= c.cfg.MaxOutstandingBlocks*2 {
			return // the peer is ahead of what this end agreed to hold
		}
		b = &recvBlock{
			shards: make([][]byte, n), present: make([]bool, n),
			k: k, n: n, dataLen: dataLen, firstAt: time.Now(),
		}
		c.blocks[block] = b
	}
	if b.done || index >= len(b.present) || b.present[index] {
		return
	}
	b.shards[index] = append([]byte(nil), payload...)
	b.present[index] = true
	b.held++
	if b.held < b.k {
		return
	}
	c.repairLocked(b)
	c.deliverLocked()
}

// repairLocked reconstructs a block once k shards are in hand.
func (c *Channel) repairLocked(b *recvBlock) {
	if b.done {
		return
	}
	if b.n > b.k {
		codec, err := fec.New(b.k, b.n)
		if err != nil {
			return
		}
		if err := codec.Reconstruct(b.shards, b.present); err != nil {
			return
		}
	}
	data := make([]byte, 0, b.dataLen)
	for i := 0; i < b.k && len(data) < b.dataLen; i++ {
		take := min(len(b.shards[i]), b.dataLen-len(data))
		data = append(data, b.shards[i][:take]...)
	}
	if len(data) != b.dataLen {
		return
	}
	b.data = data
	b.done = true
	// The shards have served their purpose and are the bulk of the memory.
	b.shards = nil
	b.present = nil
}

// deliverLocked moves every consecutive completed block into the read buffer.
func (c *Channel) deliverLocked() {
	moved := false
	for {
		b := c.blocks[c.deliverNext]
		if b == nil || !b.done {
			break
		}
		c.ready = append(c.ready, b.data...)
		delete(c.blocks, c.deliverNext)
		c.deliverNext++
		moved = true
	}
	if moved {
		c.cond.Broadcast()
	}
}

// report describes what the receiver holds: every block below cumulative is
// complete, highest is the largest block it has seen any shard of, and each
// entry is a block it has begun but cannot finish.
//
// A block the receiver has heard nothing of is not in the report and cannot
// be: it does not know the block exists. The sender infers those from the gap
// between cumulative and highest, which is why highest is carried at all.
func (c *Channel) buildReportLocked() []byte {
	const maxEntries = 16
	d := make([]byte, 0, 64)
	d = append(d, typeReport)
	// What this end measures about its inbound direction is what the peer
	// needs to size the code it sends. Per-mille and hundredths are finer than
	// the estimate's own accuracy and cost two bytes each.
	snapshot := c.estimator.Snapshot()
	d = binary.BigEndian.AppendUint16(d, uint16(min(1000, int(snapshot.Floor*1000+0.5))))
	d = binary.BigEndian.AppendUint16(d, uint16(min(65535, int(snapshot.BurstFactor*100+0.5))))
	d = binary.BigEndian.AppendUint32(d, uint32(c.deliverNext))
	highest := c.deliverNext
	if c.seenAny && c.highestBlock+1 > highest {
		highest = c.highestBlock + 1
	}
	d = binary.BigEndian.AppendUint32(d, uint32(highest))
	countAt := len(d)
	d = append(d, 0)
	entries := 0
	for id := c.deliverNext; id < highest && entries < maxEntries; id++ {
		b := c.blocks[id]
		if b == nil || b.done {
			continue
		}
		d = binary.BigEndian.AppendUint32(d, uint32(id))
		d = append(d, byte(b.n-1))
		bitmap := make([]byte, (b.n+7)/8)
		for i, ok := range b.present {
			if ok {
				bitmap[i/8] |= 1 << (i % 8)
			}
		}
		d = append(d, bitmap...)
		entries++
	}
	d[countAt] = byte(entries)
	return d
}

func (c *Channel) onReport(d []byte) {
	const reportHeader = 1 + 2 + 2 + 4 + 4 + 1
	if len(d) < reportHeader {
		return
	}
	peerFloor := float64(binary.BigEndian.Uint16(d[1:])) / 1000
	peerBurst := float64(binary.BigEndian.Uint16(d[3:])) / 100
	cumulative := uint64(binary.BigEndian.Uint32(d[5:]))
	highest := uint64(binary.BigEndian.Uint32(d[9:]))
	entries := int(d[13])
	rest := d[reportHeader:]

	type deficit struct {
		block  uint64
		unheld []int
		need   int
	}
	var repairs []deficit

	c.mu.Lock()
	c.peerFloor, c.peerBurst = peerFloor, peerBurst
	// Everything below cumulative is complete at the peer and can be released.
	for id := c.acked; id < cumulative; id++ {
		delete(c.sent, id)
	}
	if cumulative > c.acked {
		c.acked = cumulative
		c.cond.Broadcast()
	}

	reported := make(map[uint64]bool, entries)
	for i := 0; i < entries; i++ {
		if len(rest) < 5 {
			break
		}
		id := uint64(binary.BigEndian.Uint32(rest))
		n := int(rest[4]) + 1
		bitmapLen := (n + 7) / 8
		if len(rest) < 5+bitmapLen {
			break
		}
		bitmap := rest[5 : 5+bitmapLen]
		rest = rest[5+bitmapLen:]
		reported[id] = true

		b := c.sent[id]
		if b == nil {
			continue
		}
		held, unheld := 0, make([]int, 0, b.n)
		for j := 0; j < b.n; j++ {
			if j < n && bitmap[j/8]&(1<<(j%8)) != 0 {
				held++
			} else {
				unheld = append(unheld, j)
			}
		}
		if need := b.k - held; need > 0 {
			repairs = append(repairs, deficit{block: id, unheld: unheld, need: need})
		}
	}
	// Blocks the peer has seen nothing of: it cannot report them, so they are
	// the ones between what it has finished and what it has glimpsed.
	for id := cumulative; id < highest; id++ {
		if reported[id] {
			continue
		}
		b := c.sent[id]
		if b == nil {
			continue
		}
		all := make([]int, b.n)
		for j := range all {
			all[j] = j
		}
		repairs = append(repairs, deficit{block: id, unheld: all, need: b.k})
	}

	// A retransmission crosses the same channel, so sending exactly the
	// deficit repairs only the arriving fraction of the blocks that needed it.
	// The channel it crosses is the outbound one, which the peer measured and
	// just reported -- not this end's inbound estimator, which measures the
	// reverse direction and on a one-way transfer measures nothing at all.
	arrival := 1 - c.peerFloor
	if arrival < 0.1 {
		arrival = 0.1
	}
	var datagrams [][]byte
	for _, r := range repairs {
		b := c.sent[r.block]
		if b == nil {
			continue
		}
		send := min(int(float64(r.need)/arrival)+1, len(r.unheld))
		for _, index := range r.unheld[:send] {
			datagrams = append(datagrams,
				c.encodeShardLocked(r.block, index, b.k, b.n, b.dataLen, b.shards[index]))
		}
		b.repairs += send
		b.deadline = time.Now().Add(c.retransmitAfter())
	}
	c.mu.Unlock()

	for _, d := range datagrams {
		c.send(d, false)
	}
}

func (c *Channel) reportLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.ReportInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return
		}
		// The report goes out even with nothing to say. It carries this end's
		// measurement of its inbound direction, which is what the peer sizes
		// its code from, and a peer that has just started has nothing else to
		// go on.
		datagrams := [][]byte{c.buildReportLocked()}
		datagrams = append(datagrams, c.timedRepairsLocked()...)
		c.mu.Unlock()
		for _, d := range datagrams {
			c.send(d, false)
		}
	}
}

// timedRepairsLocked resends blocks no report has mentioned. A block whose
// every datagram was lost is invisible to the peer, so it cannot appear in a
// report and nothing but a timer will ever resend it. The backoff doubles so a
// path that is down does not turn into a sender that shouts.
func (c *Channel) timedRepairsLocked() [][]byte {
	now := time.Now()
	arrival := 1 - c.peerFloor
	if arrival < 0.1 {
		arrival = 0.1
	}
	var datagrams [][]byte
	for id, b := range c.sent {
		if now.Before(b.deadline) {
			continue
		}
		b.retries++
		b.deadline = now.Add(c.retransmitAfter() << min(b.retries, 4))
		send := min(int(float64(b.k)/arrival)+1, b.n)
		for i := 0; i < send; i++ {
			// Start from a different shard each retry, so a repeated loss of
			// the same indices is not repeated with them.
			index := (i + b.retries*b.k) % b.n
			datagrams = append(datagrams,
				c.encodeShardLocked(id, index, b.k, b.n, b.dataLen, b.shards[index]))
		}
		b.repairs += send
	}
	return datagrams
}

// Err reports why the channel stopped, or nil while it is running. A sender
// that has already handed off its last block learns of a carrier failure no
// other way: Write has returned, and the failure surfaces only to whoever
// reads next.
func (c *Channel) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		return nil
	}
	if c.err != nil {
		return c.err
	}
	return ErrClosed
}

// Dropped counts datagrams the writer had no room for. They are control and
// repair traffic, so they read as loss rather than as failure, but a large
// count means the carrier is slower than this channel is trying to drive it.
func (c *Channel) Dropped() uint64 { return c.dropped.Load() }

// Oversize counts datagrams the carrier refused for their size.
func (c *Channel) Oversize() uint64 { return c.oversize.Load() }

// Snapshot reports what the channel currently believes about the path, which
// is what a caller needs to decide anything above it.
func (c *Channel) Snapshot() (lossmodel.Snapshot, fec.Plan) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.estimator.Snapshot(), c.plan
}

// SetRate tells the channel the flow's current sending rate, which is what
// converts a block length into a delay. Without it the block length is guessed
// from the round trip alone.
func (c *Channel) SetRate(bytesPerSec float64) {
	c.mu.Lock()
	c.rate = bytesPerSec
	c.mu.Unlock()
}

func (c *Channel) fatal() error {
	if c.closed {
		if c.err != nil {
			return c.err
		}
		return ErrClosed
	}
	return nil
}

func (c *Channel) failWith(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		if c.err == nil && err != nil && !errors.Is(err, io.EOF) {
			c.err = fmt.Errorf("coded: carrier: %w", err)
		}
		close(c.stop)
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// Close stops the channel and its carrier.
func (c *Channel) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.wg.Wait()
		return nil
	}
	c.closed = true
	close(c.stop)
	c.cond.Broadcast()
	c.mu.Unlock()
	err := c.carrier.Close()
	c.wg.Wait()
	return err
}
