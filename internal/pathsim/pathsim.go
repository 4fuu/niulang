// Package pathsim provides a deterministic UDP path emulator. It exists so
// transport changes can be compared against a fixed reference under a
// reproducible delay/loss/bandwidth regime instead of a live long-haul link
// whose loss rate moves by tens of percent between trials.
//
// The emulator is a UDP relay: clients send to Relay.LocalAddr and packets are
// forwarded to the configured target. Each direction independently applies
// tail-drop queueing at the configured bottleneck rate, random loss from a
// seeded generator, and a fixed propagation delay.
package pathsim

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes one emulated path. Zero values disable the corresponding
// impairment, so a Config{} relay is a plain forwarder.
type Config struct {
	// OneWayDelay is added in each direction, so the emulated RTT is twice
	// this value.
	OneWayDelay time.Duration
	// DelayJitter is the maximum extra delay added to a packet, drawn
	// uniformly from [0, DelayJitter). Because the draw is per packet, this
	// also produces reordering, which is what a real path does and which
	// changes when a QUIC sender declares a packet lost.
	DelayJitter time.Duration
	// LossRate is the overall per-packet drop probability applied in each
	// direction, in [0,1).
	LossRate float64
	// UpstreamLossRate overrides LossRate for the client-to-server direction.
	// Zero means "use LossRate", so a symmetric path needs only LossRate.
	//
	// Asymmetric loss is worth modelling because a transport can depend on the
	// reverse direction in ways that are invisible when both directions are
	// impaired equally: anything the receiver has to send back - protocol
	// acknowledgements, window updates - competes for a congestion window that
	// heavy reverse loss collapses, and the forward flow stalls even though its
	// own direction is healthy.
	UpstreamLossRate float64
	// LossBurstPackets is the mean length, in packets, of a loss burst. One
	// (or zero) gives independent Bernoulli loss. Larger values switch to a
	// Gilbert model: the path alternates between a lossless good state and a
	// bad state that drops everything, with LossRate as the long-run fraction
	// of packets in the bad state.
	//
	// Long-haul loss is correlated, and correlated loss is a different regime
	// for a transport than the same average rate spread evenly: a burst can
	// take out a whole flight, including the retransmissions of the previous
	// one. Independent loss alone will not reproduce it.
	LossBurstPackets float64
	// RateBytesPerSec is the bottleneck serialization rate in each direction.
	// Zero means unlimited.
	RateBytesPerSec uint64
	// PerFlowRateBytesPerSec additionally polices each client source address
	// independently, modelling a path that shapes per 4-tuple rather than only
	// in aggregate. This is the regime in which multiple transport lanes can
	// raise a single application flow's goodput; with only an aggregate limit
	// they cannot, and should not be expected to. Zero disables it.
	PerFlowRateBytesPerSec uint64
	// QueueBytes bounds the bottleneck buffer. Packets that would exceed it
	// are tail-dropped. Zero selects one bandwidth-delay product, with a small
	// floor, which is the usual "reasonably provisioned router" assumption.
	QueueBytes int
	// Seed makes the loss pattern reproducible across runs.
	Seed int64
	// MTU bounds a single datagram. Zero selects 1500.
	MTU int
}

func (c Config) withDefaults() Config {
	if c.MTU <= 0 {
		c.MTU = 1500
	}
	if c.QueueBytes <= 0 {
		if c.RateBytesPerSec > 0 {
			bdp := float64(c.RateBytesPerSec) * (2 * c.OneWayDelay).Seconds()
			c.QueueBytes = int(bdp)
		}
		if c.QueueBytes < 64*1024 {
			c.QueueBytes = 64 * 1024
		}
	}
	return c
}

// Stats reports what the emulator did, so a benchmark can distinguish "the
// transport was slow" from "the emulated path dropped the traffic".
type Stats struct {
	PacketsIn      uint64
	PacketsOut     uint64
	PacketsLost    uint64
	PacketsDropped uint64 // tail drop at the bottleneck queue
	BytesIn        uint64
	BytesOut       uint64
}

type direction struct {
	mu       sync.Mutex
	nextFree time.Time
	rng      *rand.Rand
	// inBurst is the Gilbert model's bad state. It is meaningful only when
	// the configuration asks for correlated loss.
	inBurst bool
	// lossRate is this direction's drop probability, which may differ from the
	// other direction's.
	lossRate float64

	packetsIn      atomic.Uint64
	packetsOut     atomic.Uint64
	packetsLost    atomic.Uint64
	packetsDropped atomic.Uint64
	bytesIn        atomic.Uint64
	bytesOut       atomic.Uint64
}

func (d *direction) stats() Stats {
	return Stats{
		PacketsIn: d.packetsIn.Load(), PacketsOut: d.packetsOut.Load(),
		PacketsLost: d.packetsLost.Load(), PacketsDropped: d.packetsDropped.Load(),
		BytesIn: d.bytesIn.Load(), BytesOut: d.bytesOut.Load(),
	}
}

// schedule returns the absolute delivery time for a packet, or ok=false when
// the packet is dropped. Serialization is modelled by a virtual transmit
// clock: a packet may only start once the previous one finished, and the
// backlog ahead of it is the queue occupancy.
func (d *direction) schedule(now time.Time, size int, cfg Config) (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lossRate > 0 && d.dropLocked(cfg) {
		d.packetsLost.Add(1)
		return time.Time{}, false
	}
	start := now
	if cfg.RateBytesPerSec > 0 {
		if d.nextFree.After(start) {
			// Queue occupancy is the backlog expressed in bytes at the
			// bottleneck rate. Tail-drop once it exceeds the buffer.
			backlog := float64(d.nextFree.Sub(now)) / float64(time.Second) * float64(cfg.RateBytesPerSec)
			if int(backlog)+size > cfg.QueueBytes {
				d.packetsDropped.Add(1)
				return time.Time{}, false
			}
			start = d.nextFree
		}
		serialize := time.Duration(float64(size) / float64(cfg.RateBytesPerSec) * float64(time.Second))
		d.nextFree = start.Add(serialize)
		start = d.nextFree
	}
	arrival := start.Add(cfg.OneWayDelay)
	if cfg.DelayJitter > 0 {
		arrival = arrival.Add(time.Duration(d.rng.Int63n(int64(cfg.DelayJitter))))
	}
	return arrival, true
}

// scheduleStream is the schedule a byte-stream relay needs: the same
// serialization and propagation model, but it never drops.
//
// Tail drop is meaningless for a stream. Discarding a chunk delivers a hole
// rather than triggering a retransmission, and treating the drop as fatal
// truncates the transfer at exactly one queue's worth of data - which is how
// this was found, with 4 MiB transfers ending at 1.25 MiB, the configured
// bandwidth-delay product. A stream relay applies backpressure instead, by
// bounding its own queue.
func (d *direction) scheduleStream(now time.Time, size int, cfg Config) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	start := now
	if cfg.RateBytesPerSec > 0 {
		if d.nextFree.After(start) {
			start = d.nextFree
		}
		serialize := time.Duration(float64(size) / float64(cfg.RateBytesPerSec) * float64(time.Second))
		d.nextFree = start.Add(serialize)
		start = d.nextFree
	}
	arrival := start.Add(cfg.OneWayDelay)
	if cfg.DelayJitter > 0 {
		arrival = arrival.Add(time.Duration(d.rng.Int63n(int64(cfg.DelayJitter))))
	}
	return arrival
}

// dropLocked decides whether this packet is lost. With no burst length
// configured it is one Bernoulli trial. Otherwise it is a two-state Gilbert
// chain whose bad state drops everything: the mean bad run is
// LossBurstPackets, and the transition into it is chosen so the long-run drop
// fraction is LossRate.
func (d *direction) dropLocked(cfg Config) bool {
	if cfg.LossBurstPackets <= 1 {
		return d.rng.Float64() < d.lossRate
	}
	recover := 1 / cfg.LossBurstPackets
	enter := recover * d.lossRate / (1 - d.lossRate)
	if d.inBurst {
		if d.rng.Float64() < recover {
			d.inBurst = false
		}
		return true
	}
	if d.rng.Float64() < enter {
		d.inBurst = true
		return true
	}
	return false
}

type peer struct {
	client net.Addr
	conn   *net.UDPConn
	last   atomic.Int64
	// up and down are this source address's own policer, used only when the
	// configuration asks for per-flow shaping. They deliberately do not apply
	// loss: loss stays a property of the shared path so that per-flow shaping
	// can be varied independently of the loss regime.
	up   direction
	down direction
}

// Relay is a running emulated path. It is safe for concurrent use and is
// stopped with Close.
type Relay struct {
	cfg    Config
	target *net.UDPAddr
	local  *net.UDPConn

	upstream   direction // client -> server
	downstream direction // server -> client

	mu     sync.Mutex
	peers  map[string]*peer
	closed bool

	// queues hold packets until their modelled arrival. One goroutine per
	// direction replaces one per in-flight packet; see deliveryQueue.
	queueMu sync.Mutex
	queues  map[*direction]*deliveryQueue

	wg   sync.WaitGroup
	done chan struct{}
}

// New starts a relay on listen (use "127.0.0.1:0" for an ephemeral port) that
// forwards to target.
func New(listen, target string, cfg Config) (*Relay, error) {
	if cfg.LossRate < 0 || cfg.LossRate >= 1 {
		return nil, fmt.Errorf("loss rate %v must be in [0,1)", cfg.LossRate)
	}
	if cfg.UpstreamLossRate < 0 || cfg.UpstreamLossRate >= 1 {
		return nil, fmt.Errorf("upstream loss rate %v must be in [0,1)", cfg.UpstreamLossRate)
	}
	cfg = cfg.withDefaults()
	targetAddr, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	listenAddr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, fmt.Errorf("resolve listen: %w", err)
	}
	local, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	r := &Relay{
		cfg: cfg, target: targetAddr, local: local,
		peers: make(map[string]*peer), done: make(chan struct{}),
	}
	// Separate generators keep each direction's loss pattern independent of
	// the other direction's packet count, which would otherwise make a result
	// depend on unrelated timing.
	r.upstream.rng = rand.New(rand.NewSource(cfg.Seed))
	r.downstream.rng = rand.New(rand.NewSource(cfg.Seed + 1))
	r.upstream.lossRate = cfg.LossRate
	if cfg.UpstreamLossRate > 0 {
		r.upstream.lossRate = cfg.UpstreamLossRate
	}
	r.downstream.lossRate = cfg.LossRate
	r.wg.Add(1)
	go r.readClient()
	return r, nil
}

// LocalAddr is the address clients should use in place of the real server.
func (r *Relay) LocalAddr() string { return r.local.LocalAddr().String() }

// Stats returns the upstream (client to server) and downstream counters.
func (r *Relay) Stats() (up, down Stats) { return r.upstream.stats(), r.downstream.stats() }

func (r *Relay) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	close(r.done)
	peers := make([]*peer, 0, len(r.peers))
	for _, p := range r.peers {
		peers = append(peers, p)
	}
	r.peers = nil
	r.mu.Unlock()
	_ = r.local.Close()
	for _, p := range peers {
		_ = p.conn.Close()
	}
	r.queueMu.Lock()
	queues := make([]*deliveryQueue, 0, len(r.queues))
	for _, queue := range r.queues {
		queues = append(queues, queue)
	}
	r.queues = nil
	r.queueMu.Unlock()
	for _, queue := range queues {
		queue.close()
	}
	r.wg.Wait()
	return nil
}

func (r *Relay) readClient() {
	defer r.wg.Done()
	buf := make([]byte, r.cfg.MTU)
	for {
		n, addr, err := r.local.ReadFrom(buf)
		if err != nil {
			return
		}
		p, err := r.peerFor(addr)
		if err != nil {
			continue
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		r.forward(&r.upstream, &p.up, payload, func(b []byte) {
			_, _ = p.conn.Write(b)
		})
	}
}

func (r *Relay) peerFor(addr net.Addr) (*peer, error) {
	key := addr.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errors.New("relay is closed")
	}
	if existing, ok := r.peers[key]; ok {
		existing.last.Store(time.Now().UnixNano())
		return existing, nil
	}
	conn, err := net.DialUDP("udp", nil, r.target)
	if err != nil {
		return nil, err
	}
	p := &peer{client: addr, conn: conn}
	p.up.rng = rand.New(rand.NewSource(r.cfg.Seed))
	p.down.rng = rand.New(rand.NewSource(r.cfg.Seed))
	p.last.Store(time.Now().UnixNano())
	r.peers[key] = p
	r.wg.Add(1)
	go r.readServer(p)
	return p, nil
}

func (r *Relay) readServer(p *peer) {
	defer r.wg.Done()
	buf := make([]byte, r.cfg.MTU)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			return
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		client := p.client
		r.forward(&r.downstream, &p.down, payload, func(b []byte) {
			_, _ = r.local.WriteTo(b, client)
		})
	}
}

// forward applies the impairment model and hands the packet to send at its
// scheduled delivery time. One timer goroutine per in-flight packet is
// acceptable here: the emulator runs in the benchmark process, and the
// bandwidth-delay product bounds the concurrent count.
func (r *Relay) forward(d *direction, perFlow *direction, payload []byte, send func([]byte)) {
	d.packetsIn.Add(1)
	d.bytesIn.Add(uint64(len(payload)))
	now := time.Now()
	if perFlow != nil && r.cfg.PerFlowRateBytesPerSec > 0 {
		// The per-flow policer runs first and contributes only queueing delay
		// and its own tail drop; the shared path then adds loss and the
		// aggregate bottleneck.
		flowCfg := r.cfg
		flowCfg.RateBytesPerSec = r.cfg.PerFlowRateBytesPerSec
		flowCfg.OneWayDelay = 0
		released, allowed := perFlow.schedule(now, len(payload), flowCfg)
		if !allowed {
			d.packetsDropped.Add(1)
			return
		}
		if released.After(now) {
			now = released
		}
	}
	deliver, ok := d.schedule(now, len(payload), r.cfg)
	if !ok {
		return
	}
	if !deliver.After(time.Now()) {
		d.packetsOut.Add(1)
		d.bytesOut.Add(uint64(len(payload)))
		send(payload)
		return
	}
	r.queueFor(d).add(scheduled{deliver: deliver, payload: payload, send: send, direction: d})
}

// queueFor returns the delivery queue for a direction, created on first use so
// an unimpaired relay starts no extra goroutines at all.
func (r *Relay) queueFor(d *direction) *deliveryQueue {
	r.queueMu.Lock()
	defer r.queueMu.Unlock()
	if r.queues == nil {
		r.queues = make(map[*direction]*deliveryQueue, 2)
	}
	queue, ok := r.queues[d]
	if !ok {
		queue = newDeliveryQueue(r.done)
		r.queues[d] = queue
	}
	return queue
}
