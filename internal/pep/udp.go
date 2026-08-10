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

const udpReadPoll = time.Second

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
	defer lane.fc.Close()
	if err := socks5.WriteReply(control, socks5.ReplySucceeded, udpConn.LocalAddr()); err != nil {
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

	resultCh := make(chan error, 2)
	activity := make(chan struct{}, 1)
	var peerMu sync.RWMutex
	var peer *net.UDPAddr
	var counters udpCounters

	go func() {
		resultCh <- c.runClientUDPUplink(assocCtx, udpConn, lane.fc, lane.sessionID, flowID, &peerMu, &peer, activity, &counters)
	}()
	go func() {
		resultCh <- runClientUDPDownlink(assocCtx, udpConn, lane.fc, lane.sessionID, flowID, &peerMu, &peer, activity, &counters)
	}()

	c.metrics.FlowStarted()
	started := time.Now()
	idleTimer := time.NewTimer(c.cfg.FlowIdleTimeout)
	lifetimeTimer := time.NewTimer(c.cfg.FlowMaxLifetime)
	defer idleTimer.Stop()
	defer lifetimeTimer.Stop()
	failed := false
	gracefulClose := false
	var endErr error
	for {
		select {
		case endErr = <-resultCh:
			failed = endErr != nil && !errors.Is(endErr, context.Canceled)
			goto done
		case <-controlClosed:
			gracefulClose = true
			goto done
		case <-assocCtx.Done():
			if !errors.Is(assocCtx.Err(), context.Canceled) {
				failed = true
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
			goto done
		case <-lifetimeTimer.C:
			c.metrics.FlowTimeout()
			failed = true
			endErr = errors.New("UDP association lifetime exceeded")
			goto done
		}
	}
done:
	if gracefulClose {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = lane.fc.WriteContext(closeCtx, protocol.Frame{Header: protocol.Header{
			Version: protocol.Version, Type: protocol.TypeClose, Flags: protocol.FlagFin,
			SessionID: lane.sessionID, FlowID: flowID,
			Class: protocol.ClassInteractive,
		}})
		closeCancel()
	}
	if endErr != nil && failed {
		c.cfg.Logger.Debug("UDP association ended", "error", endErr, "age", time.Since(started))
	}
	c.metrics.FlowFinished(counters.up.Load(), counters.down.Load(), failed)
	// Closing both descriptors releases goroutines blocked in Read/Write. The
	// control watcher is released by handleLocal's deferred control close.
	_ = lane.fc.Close()
	_ = udpConn.Close()
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
	if err := lane.fc.Write(protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeOpen, SessionID: lane.sessionID,
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
		if frame.Header.SessionID != sessionID || frame.Header.FlowID != flowID || frame.Header.Type != protocol.TypePacket || frame.Header.Flags != 0 {
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
