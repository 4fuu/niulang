package pep

import (
	"context"
	"net"
	"time"

	"github.com/icourses-dev/wanopt/internal/session"
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

// currentUplink is the local address the kernel would use to reach the server.
//
// Dialling a UDP socket sends no packets: it binds the socket and asks the
// routing table which source address this destination gets. That is exactly
// the question being asked, and it costs nothing on the wire.
func (c *Client) currentUplink() string {
	conn, err := net.Dial("udp", c.cfg.RemoteAddr)
	if err != nil {
		return ""
	}
	defer conn.Close()
	return addressHost(conn.LocalAddr())
}

// watchUplink notices the machine changing how it reaches the server.
func (c *Client) watchUplink(ctx context.Context) {
	ticker := time.NewTicker(uplinkPollInterval)
	defer ticker.Stop()
	known := c.currentUplink()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current := c.currentUplink()
		if current == "" || current == known {
			continue
		}
		c.cfg.Logger.Info("uplink changed", "from", known, "to", current)
		known = current
		c.onUplinkChanged(ctx)
	}
}

// onUplinkChanged abandons what belonged to the old path and measures the new
// one before anything needs it.
func (c *Client) onUplinkChanged(ctx context.Context) {
	// The pooled connection is bound to the address that is gone. Even where
	// it survives by migrating, its congestion state, its erasure floor and
	// its bottleneck all describe the old path.
	c.closeQUICPool()
	c.prewarmPath(ctx)
}

// prewarmPath opens a connection for the sake of what opening one measures.
//
// The first flow on an unmeasured path is carried uncoded, because nothing yet
// knows the path erases -- and on the channel this targets that is the
// difference between a small exchange taking 618 ms and 1.9 s. Paying for the
// handshake here means the first flow the user actually asks for does not pay
// for it, in either the round trips or the ignorance.
func (c *Client) prewarmPath(ctx context.Context) {
	if c.cfg.Transport == TransportTCP {
		return
	}
	warm, cancel := context.WithTimeout(ctx, prewarmTimeout)
	defer cancel()

	sessionID, err := session.NewSessionID()
	if err != nil {
		return
	}
	lane, err := c.dialLaneMode(warm, TransportQUIC, sessionID, 0, session.HelloNew, c.cfg.EnableQUICPool, false)
	if err != nil {
		c.cfg.Logger.Debug("path prewarm failed", "error", err)
		return
	}
	// The connection stays in the pool; only this lane's framing is done with.
	// What was wanted from it is already recorded: the handshake's own
	// acknowledgements are what the congestion controller measures the path
	// from, and it publishes that to the model every flow will read.
	_ = lane.fc.Close()
}

// prewarmTimeout bounds the measurement, which is worth having and not worth
// waiting for.
const prewarmTimeout = 20 * time.Second
