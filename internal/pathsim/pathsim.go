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
	// LossRate is the independent per-packet drop probability applied in each
	// direction, in [0,1).
	LossRate float64
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
	if cfg.LossRate > 0 && d.rng.Float64() < cfg.LossRate {
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
	return start.Add(cfg.OneWayDelay), true
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

	wg   sync.WaitGroup
	done chan struct{}
}

// New starts a relay on listen (use "127.0.0.1:0" for an ephemeral port) that
// forwards to target.
func New(listen, target string, cfg Config) (*Relay, error) {
	if cfg.LossRate < 0 || cfg.LossRate >= 1 {
		return nil, fmt.Errorf("loss rate %v must be in [0,1)", cfg.LossRate)
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
		flowCfg.LossRate = 0
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
	wait := time.Until(deliver)
	if wait <= 0 {
		d.packetsOut.Add(1)
		d.bytesOut.Add(uint64(len(payload)))
		send(payload)
		return
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
			d.packetsOut.Add(1)
			d.bytesOut.Add(uint64(len(payload)))
			send(payload)
		case <-r.done:
		}
	}()
}
