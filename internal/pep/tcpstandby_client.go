package pep

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/4fuu/niulang/internal/protocol"
	"github.com/4fuu/niulang/internal/session"
)

const (
	standbyHeartbeatInterval = time.Second
	standbyHealthyFor        = 4 * time.Second
	standbyRetryMax          = 30 * time.Second
)

type tcpStandby struct {
	mu         sync.Mutex
	outer      streamConn
	fc         *frameConn
	generation [16]byte
	sequence   uint64
	lastAck    time.Time
	failed     bool
	claimed    bool
	claimedCh  chan struct{}
	finishedCh chan struct{}
	finishOnce sync.Once
}

func standbyProbe(generation [16]byte, sequence uint64) protocol.Frame {
	return protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeProbe,
		SessionID: generation, Sequence: sequence, Class: protocol.ClassNew,
	}, Payload: []byte{byte(standbyRecoveryHandoff)}}
}

func (c *Client) dialTCPStandby(ctx context.Context) (*tcpStandby, error) {
	generation, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	outer, err := dialTCPALPN(ctx, c.cfg.RemoteAddr, c.currentCredentials(), c.cfg.DialTimeout, c.cfg.LocalAddress, c.cfg.SocketControl, protocol.StandbyALPN)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*tcpStandby, error) {
		_ = outer.Close()
		return nil, err
	}
	fc := newFrameConnLimited(outer, c.memoryLimits.frameReadBuffer, c.memoryLimits.eventQueue)
	standby := &tcpStandby{
		outer: outer, fc: fc, generation: generation,
		sequence: 1, claimedCh: make(chan struct{}), finishedCh: make(chan struct{}),
	}
	registration := standbyProbe(generation, standby.sequence)
	_ = outer.SetDeadline(time.Now().Add(handshakeBound(outer, c.cfg.HandshakeTimeout)))
	if err := fc.Write(registration); err != nil {
		return fail(fmt.Errorf("register TCP standby: %w", err))
	}
	response, err := fc.Read()
	if err != nil {
		return fail(fmt.Errorf("read TCP standby registration: %w", err))
	}
	if !validStandbyProbe(response, generation, 0) || response.Header.Sequence != registration.Header.Sequence {
		return fail(errors.New("gateway returned an invalid TCP standby acknowledgement"))
	}
	_ = outer.SetDeadline(time.Time{})
	standby.lastAck = time.Now()
	return standby, nil
}

func (s *tcpStandby) heartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed {
		return errStandbyClaimed
	}
	if s.failed {
		return errors.New("TCP standby is unavailable")
	}
	s.sequence++
	probe := standbyProbe(s.generation, s.sequence)
	_ = s.outer.SetDeadline(time.Now().Add(standbyHealthyFor))
	if err := s.fc.Write(probe); err != nil {
		s.failed = true
		return err
	}
	response, err := s.fc.Read()
	if err != nil {
		s.failed = true
		return err
	}
	if !validStandbyProbe(response, s.generation, s.sequence-1) || response.Header.Sequence != s.sequence {
		s.failed = true
		return errors.New("invalid TCP standby heartbeat acknowledgement")
	}
	_ = s.outer.SetDeadline(time.Time{})
	s.lastAck = time.Now()
	return nil
}

func (s *tcpStandby) claim(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || s.failed || s.lastAck.IsZero() || now.Sub(s.lastAck) > standbyHealthyFor {
		return false
	}
	s.claimed = true
	close(s.claimedCh)
	_ = s.outer.SetDeadline(time.Time{})
	return true
}

func (s *tcpStandby) finishClaim() { s.finishOnce.Do(func() { close(s.finishedCh) }) }

func (s *tcpStandby) close() {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
	_ = s.fc.Close()
}

func (c *Client) publishTCPStandby(standby *tcpStandby) bool {
	c.standbyMu.Lock()
	defer c.standbyMu.Unlock()
	if c.tcpStandby != nil {
		return false
	}
	c.tcpStandby = standby
	c.metrics.TCPStandbyRegistration()
	c.metrics.TCPStandbyReady()
	return true
}

func (c *Client) removeTCPStandby(standby *tcpStandby) {
	c.standbyMu.Lock()
	if c.tcpStandby == standby {
		c.tcpStandby = nil
		c.metrics.TCPStandbyClosed()
	}
	c.standbyMu.Unlock()
}

func (c *Client) invalidateTCPStandby() {
	c.standbyMu.Lock()
	standby := c.tcpStandby
	c.tcpStandby = nil
	c.standbyMu.Unlock()
	if standby != nil {
		c.metrics.TCPStandbyClosed()
		standby.close()
	}
}

func (c *Client) standbyReady(now time.Time) bool {
	c.standbyMu.Lock()
	standby := c.tcpStandby
	c.standbyMu.Unlock()
	if standby == nil {
		return false
	}
	standby.mu.Lock()
	defer standby.mu.Unlock()
	return !standby.claimed && !standby.failed && !standby.lastAck.IsZero() && now.Sub(standby.lastAck) <= standbyHealthyFor
}

func (c *Client) claimTCPStandby(now time.Time) *tcpStandby {
	c.standbyMu.Lock()
	standby := c.tcpStandby
	if standby != nil {
		c.tcpStandby = nil
		c.metrics.TCPStandbyClosed()
	}
	c.standbyMu.Unlock()
	if standby == nil {
		return nil
	}
	if standby.claim(now) {
		c.metrics.TCPStandbyClaim()
		return standby
	}
	c.metrics.TCPStandbyFailure()
	standby.close()
	return nil
}

func (c *Client) maintainTCPStandby(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		standby, err := c.dialTCPStandby(ctx)
		if err != nil {
			c.metrics.TCPStandbyFailure()
			c.cfg.Logger.Debug("TCP standby unavailable", "error", err, "retry_in", backoff)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			backoff *= 2
			if backoff > standbyRetryMax {
				backoff = standbyRetryMax
			}
			continue
		}
		if !c.publishTCPStandby(standby) {
			standby.close()
			return
		}
		backoff = time.Second
		c.cfg.Logger.Debug("TCP standby ready")
		ticker := time.NewTicker(standbyHeartbeatInterval)
		claimed := false
	readyLoop:
		for {
			select {
			case <-ctx.Done():
				break readyLoop
			case <-standby.claimedCh:
				claimed = true
				break readyLoop
			case <-ticker.C:
				if err := standby.heartbeat(); err != nil {
					if !errors.Is(err, errStandbyClaimed) {
						c.metrics.TCPStandbyFailure()
					}
					break readyLoop
				}
			}
		}
		ticker.Stop()
		c.removeTCPStandby(standby)
		if claimed {
			select {
			case <-standby.finishedCh:
			case <-ctx.Done():
				standby.close()
				return
			}
		} else {
			standby.close()
		}
	}
}

func (c *Client) activateTCPStandby(standby *tcpStandby, sessionID [16]byte, flowID, laneID uint64) (*mpLane, error) {
	defer standby.finishClaim()
	lane, err := c.completeLaneJoin(&authenticatedLane{
		fc: standby.fc, outer: standby.outer, sessionID: sessionID,
		kind: TransportTCP, laneID: laneID,
		tcpStriping: c.cfg.TCPFallbackLanes > 1,
	}, flowID, 0)
	if err != nil {
		return nil, err
	}
	return lane, nil
}
