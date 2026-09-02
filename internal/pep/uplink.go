package pep

import (
	"context"
	"net"
	"time"

	"github.com/4fuu/niulang/internal/netbind"
)

// The path a client is on is not a property of the server. Moving from Wi-Fi
// to a cellular link, or bringing up a VPN, changes the erasure rate, the
// bottleneck and the minimum round trip all at once, and everything this
// transport sizes is sized from those three.
//
// Nothing in QUIC tells the application this happened. The connection either
// keeps working from a new address or dies, and in both cases the measurements
// behind it belong to a path that no longer exists. Carrying them forward is
// worse than having none: an erasure floor from a cellular link tells a
// Wi-Fi connection to spend two thirds of its bandwidth on parity it does not
// need, and a Wi-Fi floor tells a cellular one to send none of the parity it
// does.
//
// So the client watches which local address the kernel would use to reach the
// server, and treats a change as what it is: a different path, whose model
// starts empty and is filled by a connection opened for that purpose rather
// than by whichever flow happens to be first.
const (
	// uplinkPollInterval is how often the local address is checked. It costs a
	// socket that is opened, asked which route it was given, and closed
	// without sending anything, so this can be frequent enough to catch a
	// change before the user notices it.
	uplinkPollInterval = 2 * time.Second
)

// currentUplink is the local address an outer connection will use to reach
// the server.
//
// When an address or interface was configured, every QUIC dial is
// explicitly bound to it. Asking the unbound routing table in that case can
// produce a different answer (notably while a VPN owns the default route),
// causing the watcher to repeatedly discard a healthy pool. Re-resolving the
// configured spec preserves DHCP and interface-change detection while asking
// the same question as the real dial path.
//
// Without an explicit binding, dialling a UDP socket sends no packets: it
// binds the socket and asks the routing table which source address this
// destination gets.
func (c *Client) currentUplink() string {
	address, _ := c.currentUplinkState()
	return address
}

// currentUplinkState distinguishes an observed loss of a configured dynamic
// binding from an inconclusive route probe. That distinction matters when two
// networks assign the same private address: the address before and after the
// interruption is equal, but every connection and path measurement still
// belongs to the network which disappeared.
func (c *Client) currentUplinkState() (address string, unavailable bool) {
	if c.cfg.LocalAddress != "" {
		ip, err := resolveLocalAddress(c.cfg.LocalAddress)
		if err != nil {
			// A literal address is immutable, while if:NAME and auto are resolved
			// from live interface state. Failure of the latter is positive
			// evidence that the bound uplink is currently unavailable.
			return "", netbind.IsDynamic(c.cfg.LocalAddress)
		}
		return ip.String(), false
	}
	conn, err := (&net.Dialer{Control: c.cfg.SocketControl}).Dial("udp", c.cfg.RemoteAddr)
	if err != nil {
		// An unbound probe can fail for reasons unrelated to the local link
		// (resolution, endpoint syntax, or policy), so it remains inconclusive.
		return "", false
	}
	defer conn.Close()
	return addressHost(conn.LocalAddr()), false
}

type uplinkWatchState struct {
	known       string
	interrupted bool
}

// observe reports whether connections belonging to the previous path must be
// discarded. A definite unavailable interval is itself a path boundary, even
// if the address on the other side is textually identical. Inconclusive empty
// observations stay harmless and do not churn a healthy pool.
func (s *uplinkWatchState) observe(current string, unavailable bool) (from string, changed bool) {
	if current == "" {
		if unavailable {
			s.interrupted = true
		}
		return s.known, false
	}
	from = s.known
	changed = s.interrupted || current != s.known
	s.known = current
	s.interrupted = false
	return from, changed
}

// watchUplink notices the machine changing how it reaches the server.
func (c *Client) watchUplink(ctx context.Context, known string) {
	state := uplinkWatchState{known: known}
	ticker := time.NewTicker(uplinkPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, unavailable := c.currentUplinkState()
		from, changed := state.observe(current, unavailable)
		if !changed {
			continue
		}
		c.cfg.Logger.Info("uplink changed", "from", from, "to", current)
		c.onUplinkChanged(ctx)
	}
}

// onUplinkChanged abandons what belonged to the old path and starts the next
// connection before anything needs it.
func (c *Client) onUplinkChanged(ctx context.Context) {
	// The pooled connection is bound to the address that is gone. Even where
	// it survives by migrating, its congestion state, its erasure floor and
	// its bottleneck all describe the old path.
	c.closeQUICPool()
	c.prewarmPath(ctx)
}

// prewarmPath opens the shared connection without delaying listener readiness
// or sending traffic that competes with the first real flow.
//
// acquireControlQUICGeneration is singleflight, so a first flow arriving while
// this runs joins the same dial rather than starting or waiting behind a second
// handshake.
func (c *Client) prewarmPath(ctx context.Context) {
	if !c.cfg.EnableQUICPool {
		return
	}
	warm, cancel := context.WithTimeout(ctx, prewarmTimeout)
	defer cancel()
	_, err := c.acquireControlQUICGeneration(warm, congestionConfig{
		kind: c.cfg.Congestion, brutalBytesPerSecond: c.cfg.BrutalBytesPerSec,
		adaptiveMinBytesPerSec: c.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: c.cfg.AdaptiveMaxBytesSec,
		wireCaps: c.wireCaps,
	})
	if err != nil {
		c.cfg.Logger.Debug("path prewarm failed", "error", err)
	}
}

// prewarmTimeout bounds a background connection attempt.
const prewarmTimeout = 20 * time.Second
