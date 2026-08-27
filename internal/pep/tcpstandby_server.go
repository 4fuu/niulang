package pep

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/4fuu/niulang/internal/identity"
	"github.com/4fuu/niulang/internal/protocol"
	"github.com/4fuu/niulang/internal/session"
)

const (
	standbyHeartbeatTimeout = 8 * time.Second
	standbyMaxLifetime      = 5 * time.Minute
)

type standbyRecoveryMode byte

const (
	standbyRecoveryHandoff standbyRecoveryMode = 1
)

type standbyPrincipalKey struct {
	provider string
	account  string
	device   string
}

type serverStandby struct {
	conn       streamConn
	generation [16]byte
}

func principalStandbyKey(principal identity.Principal) standbyPrincipalKey {
	return standbyPrincipalKey{
		provider: principal.ProviderID,
		account:  principal.AccountID,
		device:   principal.DeviceID,
	}
}

// registerStandby admits at most one idle TCP readiness connection per device.
// First-wins admission prevents two processes using the same device profile
// from continuously replacing one another. An uplink or credential change
// closes the old socket before the manager registers its replacement.
func (s *Server) registerStandby(principal identity.Principal, standby *serverStandby) (func(), bool) {
	key := principalStandbyKey(principal)
	s.standbyMu.Lock()
	if s.standbys[key] != nil || len(s.standbys) >= s.standbyLimit {
		s.standbyMu.Unlock()
		return nil, false
	}
	s.standbys[key] = standby
	s.standbyMu.Unlock()
	s.metrics.TCPStandbyRegistration()
	s.metrics.TCPStandbyReady()
	return func() {
		s.standbyMu.Lock()
		removed := false
		if s.standbys[key] == standby {
			delete(s.standbys, key)
			removed = true
		}
		s.standbyMu.Unlock()
		if removed {
			s.metrics.TCPStandbyClosed()
		}
	}, true
}

func validStandbyProbe(frame protocol.Frame, generation [16]byte, after uint64) bool {
	return frame.Header.Type == protocol.TypeProbe &&
		frame.Header.Flags == 0 && frame.Header.SessionID == generation &&
		frame.Header.FlowID == 0 && frame.Header.Sequence > after &&
		frame.Header.Class == protocol.ClassNew && len(frame.Payload) == 1 &&
		standbyRecoveryMode(frame.Payload[0]) == standbyRecoveryHandoff
}

func standbyReset(fc *frameConn, frame protocol.Frame, message string) {
	_ = fc.Write(protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeReset,
		SessionID: frame.Header.SessionID, FlowID: frame.Header.FlowID,
		Class: protocol.ClassNew,
	}, Payload: session.ResetPayload(session.ResetProtocol, message)})
}

// handleTCPStandby owns the auxiliary niulang-standby/2 state machine. The
// registration and every heartbeat are equal-size authenticated echoes. Only
// a final JOIN transfers the already-authenticated socket into a live flow.
// Registration alone cannot name a destination, consume a flow slot, or
// retire an existing QUIC lane.
func (s *Server) handleTCPStandby(ctx context.Context, conn streamConn, principal identity.Principal) {
	fc := newFrameConn(conn)
	_ = conn.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	registration, err := fc.Read()
	if err != nil || len(registration.Payload) != 1 {
		return
	}
	mode := standbyRecoveryMode(registration.Payload[0])
	if mode != standbyRecoveryHandoff ||
		session.IsZeroSessionID(registration.Header.SessionID) ||
		!validStandbyProbe(registration, registration.Header.SessionID, 0) {
		standbyReset(fc, registration, "invalid standby registration")
		return
	}
	standby := &serverStandby{conn: conn, generation: registration.Header.SessionID}
	release, admitted := s.registerStandby(principal, standby)
	if !admitted {
		s.metrics.TCPStandbyFailure()
		standbyReset(fc, registration, "standby capacity reached")
		return
	}
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			release()
		}
	}
	defer releaseOnce()
	if err := fc.Write(registration); err != nil {
		return
	}
	lastSequence := registration.Header.Sequence
	expires := time.Now().Add(standbyMaxLifetime)
	for {
		deadline := time.Now().Add(standbyHeartbeatTimeout)
		if expires.Before(deadline) {
			deadline = expires
		}
		_ = conn.SetDeadline(deadline)
		frame, err := fc.Read()
		if err != nil || !time.Now().Before(expires) {
			return
		}
		if _, err := s.cfg.Credentials.Store.Authorize(principal, time.Now()); err != nil {
			return
		}
		if frame.Header.Type == protocol.TypeProbe {
			if !validStandbyProbe(frame, registration.Header.SessionID, lastSequence) {
				standbyReset(fc, frame, "invalid standby heartbeat")
				return
			}
			lastSequence = frame.Header.Sequence
			if err := fc.Write(frame); err != nil {
				return
			}
			continue
		}
		if frame.Header.Type == protocol.TypeOpen {
			if session.IsZeroSessionID(frame.Header.SessionID) || frame.Header.FlowID == 0 ||
				frame.Header.Sequence != 0 || frame.Header.Flags != 0 || frame.Header.Class != protocol.ClassInteractive {
				standbyReset(fc, frame, "invalid standby UDP activation")
				return
			}
			token, resumable := session.DecodeUDPResumeOpen(frame.Payload)
			if !resumable {
				// A standby can resume a destination-free UDP relay, but it
				// cannot be used as an idle authenticated general-purpose OPEN.
				standbyReset(fc, frame, "invalid standby UDP activation")
				return
			}
			select {
			case s.semaphore <- struct{}{}:
				defer func() { <-s.semaphore }()
			default:
				standbyReset(fc, frame, "active session capacity reached")
				return
			}
			releaseOnce()
			if refusal := s.admitAccountFlow(principal); refusal != nil {
				s.refuseAccountFlow(fc, frame.Header.SessionID, frame.Header.FlowID, principal, refusal)
				return
			}
			defer s.releaseAccountFlow(principal)
			fc.setPacketsOnStream(s.cfg.UDPOnStream)
			setWireBulk(conn, true)
			_ = conn.SetDeadline(time.Now().Add(handshakeBound(conn, s.cfg.HandshakeTimeout)))
			s.handleUDPAssociation(ctx, conn, fc, principal, frame.Header.SessionID, frame.Header.FlowID, token, true)
			return
		}
		if frame.Header.Type != protocol.TypeJoin ||
			session.IsZeroSessionID(frame.Header.SessionID) || frame.Header.FlowID == 0 ||
			frame.Header.Sequence != 0 || frame.Header.Flags&^protocol.FlagReserveControl != 0 ||
			len(frame.Payload) != 8 {
			standbyReset(fc, frame, "invalid standby activation")
			return
		}
		laneID := binary.BigEndian.Uint64(frame.Payload)
		if laneID == 0 {
			standbyReset(fc, frame, "invalid standby activation")
			return
		}
		select {
		case s.semaphore <- struct{}{}:
			defer func() { <-s.semaphore }()
		default:
			standbyReset(fc, frame, "active session capacity reached")
			return
		}
		releaseOnce()
		_ = conn.SetDeadline(time.Now().Add(handshakeBound(conn, s.cfg.HandshakeTimeout)))
		s.handleLaneJoinOpen(ctx, conn, fc, principal, frame.Header.SessionID, laneID, frame)
		return
	}
}

var errStandbyClaimed = errors.New("TCP standby claimed")
