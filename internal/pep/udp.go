package pep

// This file implements the SOCKS5 UDP-associate data plane. UDP packets are
// carried as individual authenticated wanopt TypePacket frames, so packet
// boundaries survive the reliable fallback transport. QUIC stream mode is
// intentionally used for now: it works over the same auto-race/TCP rescue
// path as CONNECT. Native QUIC DATAGRAM transport can be added behind this
// interface later without changing the SOCKS contract.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/session"
	"github.com/icourses-dev/wanopt/internal/socks5"
)

const (
	udpReadPoll                = time.Second
	maxUDPReconnectAttempts    = 3
	udpReconnectInitialBackoff = 200 * time.Millisecond
	udpReconnectMaximumBackoff = 2 * time.Second
)

// errUDPAssociationCloseAck is kept distinct from nil so a peer-generated
// final acknowledgement cannot be mistaken for a transport failure. A clean
// SOCKS dissociation is the only normal path that should produce it.
var errUDPAssociationCloseAck = errors.New("UDP association close acknowledged")
var errUDPControlClosed = errors.New("SOCKS UDP control connection closed")

type udpCounters struct {
	up   atomic.Uint64
	down atomic.Uint64
}

func (c *Client) handleUDPAssociate(ctx context.Context, control net.Conn) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		_ = socks5.WriteReply(control, socks5.ReplyGeneralFailure, nil)
		c.cfg.Logger.Warn("local UDP relay bind failed", "error", err)
		return
	}
	defer udpConn.Close()

	assocCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	lane, flowID, err := c.openUDPAssociation(assocCtx)
	if err != nil {
		_ = socks5.WriteReply(control, socks5.ReplyHostUnreachable, nil)
		c.cfg.Logger.Warn("remote UDP association open failed", "error", err)
		return
	}
	if err := socks5.WriteReply(control, socks5.ReplySucceeded, udpConn.LocalAddr()); err != nil {
		_ = lane.fc.Close()
		return
	}
	_ = control.SetDeadline(time.Time{})

	// A SOCKS control connection remains open for the lifetime of its UDP
	// association. Reading one byte is enough to observe a clean close while
	// keeping the control channel otherwise unused as required by RFC 1928.
	controlClosed := make(chan struct{})
	go func() {
		var b [1]byte
		_, _ = control.Read(b[:])
		close(controlClosed)
	}()

	activity := make(chan struct{}, 1)
	var peerMu sync.RWMutex
	var peer *net.UDPAddr
	var counters udpCounters

	c.metrics.FlowStarted()
	started := time.Now()
	idleTimer := time.NewTimer(c.cfg.FlowIdleTimeout)
	lifetimeTimer := time.NewTimer(c.cfg.FlowMaxLifetime)
	defer idleTimer.Stop()
	defer lifetimeTimer.Stop()
	failed := false
	gracefulClose := false
	var endErr error

laneLoop:
	for {
		// A lane has its own cancellation context. The association context and
		// the local UDP socket survive a transport rescue, preserving the SOCKS
		// port and the pinned application peer.
		laneCtx, laneCancel := context.WithCancel(assocCtx)
		resultCh := make(chan error, 2)
		go func(activeLane *authenticatedLane, activeFlowID uint64) {
			resultCh <- c.runClientUDPUplink(laneCtx, udpConn, activeLane.fc, activeLane.sessionID, activeFlowID, &peerMu, &peer, activity, &counters)
		}(lane, flowID)
		go func(activeLane *authenticatedLane, activeFlowID uint64) {
			resultCh <- runClientUDPDownlink(laneCtx, udpConn, activeLane.fc, activeLane.sessionID, activeFlowID, &peerMu, &peer, activity, &counters)
		}(lane, flowID)

		for {
			select {
			case endErr = <-resultCh:
				if errors.Is(endErr, errUDPAssociationCloseAck) {
					// A peer is not allowed to close an association silently, but a
					// final ACK is a valid terminal event if the SOCKS control socket
					// disappears at the same time.
					gracefulClose = true
					laneCancel()
					_ = lane.fc.Close()
					goto done
				}
				if assocCtx.Err() != nil {
					laneCancel()
					_ = lane.fc.Close()
					goto done
				}
				// Stop both workers before opening another authenticated
				// association. This prevents the old uplink worker from consuming a
				// packet from the preserved local UDP socket after the replacement
				// becomes active.
				stopUDPAssociationLane(laneCancel, lane, resultCh, 1)
				c.metrics.LaneFailure()
				newLane, newFlowID, reconnectErr := c.rescueUDPAssociation(assocCtx, controlClosed)
				if errors.Is(reconnectErr, errUDPControlClosed) {
					gracefulClose = true
					endErr = nil
					goto done
				}
				if reconnectErr != nil {
					failed = true
					endErr = reconnectErr
					goto done
				}
				lane = newLane
				flowID = newFlowID
				c.metrics.LaneReplacement()
				continue laneLoop
			case <-controlClosed:
				gracefulClose = true
				closeErr := c.closeUDPAssociation(lane, flowID, resultCh)
				laneCancel()
				_ = lane.fc.Close()
				if closeErr != nil {
					failed = true
					endErr = closeErr
				}
				goto done
			case <-assocCtx.Done():
				laneCancel()
				_ = lane.fc.Close()
				if !errors.Is(assocCtx.Err(), context.Canceled) {
					failed = true
					endErr = assocCtx.Err()
				}
				goto done
			case <-activity:
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(c.cfg.FlowIdleTimeout)
			case <-idleTimer.C:
				c.metrics.FlowTimeout()
				failed = true
				endErr = errors.New("UDP association idle timeout")
				laneCancel()
				_ = lane.fc.Close()
				goto done
			case <-lifetimeTimer.C:
				c.metrics.FlowTimeout()
				failed = true
				endErr = errors.New("UDP association lifetime exceeded")
				laneCancel()
				_ = lane.fc.Close()
				goto done
			}
		}
	}
done:
	if endErr != nil && failed {
		c.cfg.Logger.Debug("UDP association ended", "error", endErr, "age", time.Since(started))
	} else if gracefulClose {
		c.cfg.Logger.Debug("UDP association closed", "age", time.Since(started))
	}
	c.metrics.FlowFinished(counters.up.Load(), counters.down.Load(), failed)
	// Closing both descriptors releases goroutines blocked in Read/Write. The
	// control watcher is released by handleLocal's deferred control close.
	_ = lane.fc.Close()
	_ = udpConn.Close()
}

// rescueUDPAssociation opens a fresh authenticated association while keeping
// the local SOCKS UDP socket alive. A new server-side UDP relay is intentional
// for this first rescue implementation: it bounds protocol state and works
// over both transports without a resumable UDP-session wire extension. At
// most three attempts are made, with bounded exponential backoff. AUTO mode's
// health machine is updated before each attempt, so a repeatedly dead QUIC
// path causes the next attempt to select TLS/TCP rather than spinning on UDP.
func (c *Client) rescueUDPAssociation(ctx context.Context, controlClosed <-chan struct{}) (*authenticatedLane, uint64, error) {
	var lastErr error
	for attempt := 0; attempt < maxUDPReconnectAttempts; attempt++ {
		if err := waitForUDPReconnect(ctx, controlClosed, udpReconnectBackoff(attempt)); err != nil {
			return nil, 0, err
		}
		c.metrics.UDPAssociationReconnect()
		c.udpHealth.failure(time.Now())
		attemptCtx, attemptCancel := context.WithTimeout(ctx, c.cfg.DialTimeout+c.cfg.HandshakeTimeout)
		lane, flowID, err := c.openUDPAssociation(attemptCtx)
		attemptCancel()
		if err == nil {
			return lane, flowID, nil
		}
		lastErr = err
		c.metrics.UDPAssociationRescueFailure()
		c.cfg.Logger.Warn("UDP association rescue unavailable", "attempt", attempt+1, "error", err)
	}
	if lastErr == nil {
		lastErr = errors.New("UDP association rescue exhausted")
	}
	return nil, 0, fmt.Errorf("UDP association rescue exhausted after %d attempts: %w", maxUDPReconnectAttempts, lastErr)
}

func udpReconnectBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := udpReconnectInitialBackoff
	for i := 0; i < attempt && delay < udpReconnectMaximumBackoff; i++ {
		delay *= 2
	}
	if delay > udpReconnectMaximumBackoff {
		return udpReconnectMaximumBackoff
	}
	return delay
}

func waitForUDPReconnect(ctx context.Context, controlClosed <-chan struct{}, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-controlClosed:
		return errUDPControlClosed
	case <-timer.C:
		return nil
	}
}

// stopUDPAssociationLane waits for both lane workers after cancellation. The
// uplink uses a one-second read poll, so this bound prevents an old worker from
// consuming a datagram after a replacement lane has been installed while also
// keeping shutdown independent of a broken transport.
func stopUDPAssociationLane(cancel context.CancelFunc, lane *authenticatedLane, results <-chan error, completed int) {
	cancel()
	if lane != nil && lane.fc != nil {
		_ = lane.fc.Close()
	}
	deadline := time.NewTimer(udpReadPoll + 500*time.Millisecond)
	defer deadline.Stop()
	for completed < 2 {
		select {
		case <-results:
			completed++
		case <-deadline.C:
			return
		}
	}
}

// closeUDPAssociation performs the bounded half-close handshake. Other
// frames may be in flight when the SOCKS control connection closes, so a
// non-ACK worker error is recorded but does not prevent waiting for the final
// acknowledgement from the downlink worker.
func (c *Client) closeUDPAssociation(lane *authenticatedLane, flowID uint64, results <-chan error) error {
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lane.fc.WriteContext(closeCtx, protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
		SessionID: lane.sessionID, FlowID: flowID,
		Class: protocol.ClassInteractive,
	}}); err != nil {
		return err
	}
	ackDeadline := time.NewTimer(2 * time.Second)
	defer ackDeadline.Stop()
	var lastErr error
	for received := 0; received < 2; received++ {
		select {
		case err := <-results:
			if errors.Is(err, errUDPAssociationCloseAck) {
				return nil
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				lastErr = err
			}
		case <-ackDeadline.C:
			if lastErr != nil {
				return lastErr
			}
			return errors.New("UDP association close acknowledgement timeout")
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return errors.New("UDP association close acknowledgement missing")
}

func (c *Client) openUDPAssociation(ctx context.Context) (*authenticatedLane, uint64, error) {
	lane, err := c.chooseAuthenticatedLane(ctx)
	if err != nil {
		return nil, 0, err
	}
	flowID, err := randomFlowID()
	if err != nil {
		_ = lane.fc.Close()
		return nil, 0, err
	}
	_ = lane.outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	openType := protocol.TypeOpen
	if lane.fastOpen {
		openType = protocol.TypeOpenFast
	}
	if err := lane.fc.Write(protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: openType, SessionID: lane.sessionID,
		FlowID: flowID, Class: protocol.ClassInteractive,
	}, Payload: session.UDPAssociationMarker}); err != nil {
		_ = lane.fc.Close()
		return nil, 0, fmt.Errorf("send UDP association open: %w", err)
	}
	response, err := lane.fc.Read()
	if err != nil {
		_ = lane.fc.Close()
		return nil, 0, fmt.Errorf("read UDP association acknowledgement: %w", err)
	}
	if response.Header.SessionID != lane.sessionID || response.Header.FlowID != flowID {
		_ = lane.fc.Close()
		return nil, 0, errors.New("UDP association acknowledgement identity mismatch")
	}
	if response.Header.Type == protocol.TypeReset {
		_ = lane.fc.Close()
		return nil, 0, errors.New("remote UDP association rejected")
	}
	if response.Header.Type != protocol.TypeOpenOK || len(response.Payload) != 0 {
		_ = lane.fc.Close()
		return nil, 0, errors.New("invalid UDP association acknowledgement")
	}
	_ = lane.outer.SetDeadline(time.Time{})
	return lane, flowID, nil
}

func (c *Client) runClientUDPUplink(ctx context.Context, udpConn *net.UDPConn, fc *frameConn, sessionID [16]byte, flowID uint64, peerMu *sync.RWMutex, peer **net.UDPAddr, activity chan<- struct{}, counters *udpCounters) error {
	buf := make([]byte, 65535)
	var sequence uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_ = udpConn.SetReadDeadline(time.Now().Add(udpReadPoll))
		n, addr, err := udpConn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if n == 0 {
			// Zero-length UDP datagrams are valid and are encoded with an empty
			// application payload. Keep the source association nevertheless.
		}
		peerMu.Lock()
		if *peer == nil {
			*peer = cloneUDPAddr(addr)
		} else if !udpAddrEqual(*peer, addr) {
			peerMu.Unlock()
			continue
		}
		peerMu.Unlock()
		datagram, err := socks5.ReadUDPDatagram(buf[:n])
		if err != nil {
			// Bad client datagrams are isolated to that packet; terminating the
			// authenticated association would let a malformed application packet
			// cause avoidable failover and collateral latency spikes.
			continue
		}
		payload, err := session.EncodeUDPPacket(datagram.Destination, datagram.Payload)
		if err != nil {
			continue
		}
		if err := fc.WriteContext(ctx, protocol.Frame{Header: protocol.Header{
			Version: protocol.Version, Type: protocol.TypePacket, SessionID: sessionID,
			FlowID: flowID, Sequence: sequence, Class: protocol.ClassInteractive,
		}, Payload: payload}); err != nil {
			return err
		}
		// packet count/byte counters are deliberately payload-only.
		sequence++
		counters.up.Add(uint64(len(datagram.Payload)))
		notifyActivity(activity)
	}
}

func runClientUDPDownlink(ctx context.Context, udpConn *net.UDPConn, fc *frameConn, sessionID [16]byte, flowID uint64, peerMu *sync.RWMutex, peer **net.UDPAddr, activity chan<- struct{}, counters *udpCounters) error {
	var expectedSequence uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := fc.Read()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if frame.Header.SessionID != sessionID || frame.Header.FlowID != flowID {
			return errors.New("invalid UDP association frame")
		}
		if frame.Header.Type == protocol.TypeAck && frame.Header.Flags == protocol.FlagAckFinal && frame.Header.Sequence == 0 && len(frame.Payload) == 0 {
			return errUDPAssociationCloseAck
		}
		if frame.Header.Type != protocol.TypePacket || frame.Header.Flags != 0 {
			return errors.New("invalid UDP association frame")
		}
		if frame.Header.Sequence != expectedSequence {
			return fmt.Errorf("unexpected UDP packet sequence %d, want %d", frame.Header.Sequence, expectedSequence)
		}
		expectedSequence++
		destination, payload, err := session.DecodeUDPPacket(frame.Payload)
		if err != nil {
			return err
		}
		// The destination is returned by the server as a numeric source address.
		// It is encoded back into the SOCKS response below.
		var packet bytes.Buffer
		if err := socks5.WriteUDPDatagram(&packet, destination, payload); err != nil {
			return err
		}
		peerMu.RLock()
		addr := cloneUDPAddr(*peer)
		peerMu.RUnlock()
		if addr == nil {
			continue
		}
		_ = udpConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := udpConn.WriteToUDP(packet.Bytes(), addr); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		notifyActivity(activity)
		counters.down.Add(uint64(len(payload)))
	}
}

func (s *Server) handleUDPAssociation(ctx context.Context, conn streamConn, fc *frameConn, sessionID [16]byte, flowID uint64) {
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: flowID, Class: protocol.ClassInteractive}, Payload: session.ResetPayload(session.ResetTransport, "UDP relay unavailable")})
		return
	}
	defer udpConn.Close()
	if err := fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeOpenOK, SessionID: sessionID, FlowID: flowID, Class: protocol.ClassInteractive}}); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	assocCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	frames := make(chan protocol.Frame, 32)
	frameErr := make(chan error, 1)
	go func() {
		for {
			frame, readErr := fc.Read()
			if readErr != nil {
				frameErr <- readErr
				return
			}
			select {
			case frames <- frame:
			case <-assocCtx.Done():
				return
			}
			// A graceful UDP dissociation is terminal. Stop reading immediately
			// after queueing CLOSE so a following transport EOF cannot race the
			// CLOSE event and turn a clean association into a false failure.
			if frame.Header.Type == protocol.TypeClose {
				return
			}
		}
	}()
	packets := make(chan udpDatagram, 32)
	packetErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 65535)
		for {
			_ = udpConn.SetReadDeadline(time.Now().Add(udpReadPoll))
			n, addr, readErr := udpConn.ReadFromUDP(buf)
			if readErr != nil {
				if ne, ok := readErr.(net.Error); ok && ne.Timeout() {
					if assocCtx.Err() != nil {
						packetErr <- assocCtx.Err()
						return
					}
					continue
				}
				packetErr <- readErr
				return
			}
			destination, err := session.DestinationFromUDPAddr(addr)
			if err != nil {
				continue
			}
			select {
			case packets <- udpDatagram{destination: destination, payload: append([]byte(nil), buf[:n]...)}:
			case <-assocCtx.Done():
				return
			}
		}
	}()

	var counters udpCounters
	var expectedSequence uint64
	var replySequence uint64
	s.metrics.FlowStarted()
	idleTimer := time.NewTimer(s.cfg.FlowIdleTimeout)
	lifetimeTimer := time.NewTimer(s.cfg.FlowMaxLifetime)
	defer idleTimer.Stop()
	defer lifetimeTimer.Stop()
	failed := false
	var endErr error
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(s.cfg.FlowIdleTimeout)
	}
	for {
		select {
		case frame := <-frames:
			if frame.Header.SessionID != sessionID || frame.Header.FlowID != flowID {
				endErr = errors.New("invalid UDP association frame")
				failed = true
				goto done
			}
			if frame.Header.Type == protocol.TypeClose {
				if frame.Header.Flags != protocol.FlagFin || len(frame.Payload) != 0 || frame.Header.Sequence != 0 {
					endErr = errors.New("invalid UDP association close")
					failed = true
				}
				if !failed {
					_ = fc.WriteContext(assocCtx, protocol.Frame{Header: protocol.Header{
						Version: protocol.Version, Type: protocol.TypeAck, Flags: protocol.FlagAckFinal,
						SessionID: sessionID, FlowID: flowID, Sequence: 0, Class: protocol.ClassInteractive,
					}})
				}
				goto done
			}
			if frame.Header.Type != protocol.TypePacket || frame.Header.Flags != 0 {
				endErr = errors.New("invalid UDP association frame")
				failed = true
				goto done
			}
			destination, payload, decodeErr := session.DecodeUDPPacket(frame.Payload)
			if decodeErr != nil {
				endErr = decodeErr
				failed = true
				goto done
			}
			if frame.Header.Sequence != expectedSequence {
				endErr = fmt.Errorf("unexpected UDP packet sequence %d, want %d", frame.Header.Sequence, expectedSequence)
				failed = true
				goto done
			}
			expectedSequence++
			resolveCtx, resolveCancel := context.WithTimeout(assocCtx, 10*time.Second)
			addresses, resolveErr := s.cfg.DestinationPolicy.ResolveUDPAddr(resolveCtx, destination)
			resolveCancel()
			if resolveErr != nil {
				continue
			}
			var writeErr error
			for _, address := range addresses {
				_ = udpConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, writeErr = udpConn.WriteToUDP(payload, address); writeErr == nil {
					break
				}
			}
			if writeErr != nil {
				continue
			}
			counters.up.Add(uint64(len(payload)))
			resetIdle()
		case packet := <-packets:
			payload, encodeErr := session.EncodeUDPPacket(packet.destination, packet.payload)
			if encodeErr != nil {
				continue
			}
			if writeErr := fc.WriteContext(assocCtx, protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypePacket, SessionID: sessionID,
				FlowID: flowID, Sequence: replySequence, Class: protocol.ClassInteractive,
			}, Payload: payload}); writeErr != nil {
				endErr = writeErr
				failed = true
				goto done
			}
			replySequence++
			counters.down.Add(uint64(len(packet.payload)))
			resetIdle()
		case err := <-frameErr:
			endErr = err
			failed = true
			s.cfg.Logger.Debug("UDP relay frame reader ended", "error", err)
			goto done
		case err := <-packetErr:
			endErr = err
			failed = true
			s.cfg.Logger.Debug("UDP relay packet reader ended", "error", err)
			goto done
		case <-idleTimer.C:
			s.metrics.FlowTimeout()
			endErr = errors.New("UDP association idle timeout")
			failed = true
			goto done
		case <-lifetimeTimer.C:
			s.metrics.FlowTimeout()
			endErr = errors.New("UDP association lifetime exceeded")
			failed = true
			goto done
		case <-assocCtx.Done():
			goto done
		}
	}
done:
	if endErr != nil && failed {
		s.cfg.Logger.Debug("UDP association ended", "error", endErr)
	}
	s.metrics.FlowFinished(counters.up.Load(), counters.down.Load(), failed)
	_ = conn.Close()
}

type udpDatagram struct {
	destination string
	payload     []byte
}

func notifyActivity(ch chan<- struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}
	return &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}
}

func udpAddrEqual(a, b *net.UDPAddr) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Port == b.Port && a.Zone == b.Zone && a.IP.Equal(b.IP)
}
