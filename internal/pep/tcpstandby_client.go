package pep

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/4fuu/niulang/internal/protocol"
	"github.com/4fuu/niulang/internal/session"
)

const (
	standbyHeartbeatInterval = time.Second
	standbyHeartbeatPhaseMax = 100 * time.Millisecond
	standbyHealthyFor        = 4 * time.Second
	standbyRetryMax          = 30 * time.Second
)

type standbyHeartbeatSchedule struct {
	origin   time.Time
	interval time.Duration
	phaseMax time.Duration
	slot     uint64
	random   uint64
}

func newStandbyHeartbeatSchedule(origin time.Time, generation [16]byte) standbyHeartbeatSchedule {
	return standbyHeartbeatSchedule{
		origin:   origin,
		interval: standbyHeartbeatInterval,
		phaseMax: standbyHeartbeatPhaseMax,
		random: binary.LittleEndian.Uint64(generation[:8]) ^
			binary.LittleEndian.Uint64(generation[8:]),
	}
}

func (s *standbyHeartbeatSchedule) nextSlot() time.Time {
	s.slot++
	s.random += 0x9e3779b97f4a7c15
	random := s.random
	random = (random ^ (random >> 30)) * 0xbf58476d1ce4e5b9
	random = (random ^ (random >> 27)) * 0x94d049bb133111eb
	random ^= random >> 31
	phase := time.Duration(0)
	if s.phaseMax > 0 {
		phase = time.Duration(random % uint64(s.phaseMax+1))
	}
	return s.origin.Add(time.Duration(s.slot)*s.interval + phase)
}

func (s *standbyHeartbeatSchedule) nextAfter(now time.Time) time.Time {
	for {
		next := s.nextSlot()
		if next.After(now) {
			return next
		}
	}
}

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

func (s *tcpStandby) maintainHeartbeats(ctx context.Context, schedule standbyHeartbeatSchedule) (bool, error) {
	timer := time.NewTimer(time.Until(schedule.nextAfter(time.Now())))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, nil
		case <-s.claimedCh:
			return true, nil
		case <-timer.C:
			// Arm the next absolute slot before the heartbeat. If the exchange is
			// slow, the timer retains at most one pending event just like a ticker;
			// nextAfter then drops any additional elapsed slots.
			timer.Reset(time.Until(schedule.nextAfter(time.Now())))
			if err := s.heartbeat(); err != nil {
				if errors.Is(err, errStandbyClaimed) {
					return true, nil
				}
				return false, err
			}
		}
	}
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
		schedule := newStandbyHeartbeatSchedule(time.Now(), standby.generation)
		claimed, err := standby.maintainHeartbeats(ctx, schedule)
		if err != nil {
			c.metrics.TCPStandbyFailure()
		}
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
