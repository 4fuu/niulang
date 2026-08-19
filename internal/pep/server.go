package pep

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/bojieli/queqiao/internal/classifier"
	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/limiter"
	"github.com/bojieli/queqiao/internal/metrics"
	"github.com/bojieli/queqiao/internal/protocol"
	"github.com/bojieli/queqiao/internal/session"
)

// Must exceed the client's bounded lane-replacement wait so a final-ACK loss
// can still be repaired after QUIC dead-path detection and scheduler backoff.
// Tombstones retain only final sequence metadata and remain bounded by the
// configured session admission limit.
const completedSessionLinger = 90 * time.Second

type ServerConfig struct {
	ListenAddr        string
	Credentials       identity.ServerCredentials
	Enrollment        *identity.EnrollmentService
	MaxPayload        uint32
	ChunkSize         int
	HandshakeTimeout  time.Duration
	FlowIdleTimeout   time.Duration
	FlowMaxLifetime   time.Duration
	MaxSessions       int
	DestinationPolicy DestinationPolicy
	EnableTCP         bool
	EnableQUIC        bool
	// TCPFallbackLanes is the admission ceiling for one negotiated TCP-only
	// flow. The client chooses the active target; keeping the server ceiling at
	// 16 lets operators compare 8 and 16 without changing the gateway.
	TCPFallbackLanes int
	// TCPCongestion selects the Linux kernel congestion controller inherited by
	// accepted fallback sockets. "system" leaves the host default untouched.
	TCPCongestion                 string
	Congestion                    CongestionControlKind
	BrutalBytesPerSec             uint64
	AdaptiveMinBytesSec           uint64
	AdaptiveMaxBytesSec           uint64
	AggregateBytesPerSec          uint64
	InteractiveReserveBytesPerSec uint64
	// StreamReceiveWindow and ConnectionReceiveWindow override the QUIC
	// receive windows. Zero selects the defaults, which match TUIC.
	StreamReceiveWindow     uint64
	ConnectionReceiveWindow uint64
	Metrics                 *metrics.Registry
	Logger                  *slog.Logger
	// UDPOnStream keeps SOCKS UDP packets on the lane's control stream even
	// where the QUIC connection negotiated datagrams. See the client's field:
	// it is a measurement control, and both endpoints must agree for the
	// comparison to mean anything.
	UDPOnStream bool
	// testLaneWriteHook is intentionally unexported and nil in production. It
	// lets package integration tests reproduce loss of a specific logical
	// frame without depending on encrypted QUIC packet layout.
	testLaneWriteHook func(protocol.Frame) error
}

type Server struct {
	cfg              ServerConfig
	semaphore        chan struct{}
	connections      chan struct{}
	enrollments      chan struct{}
	sessionsMu       sync.RWMutex
	sessions         map[[16]byte]*serverFlow
	accountMu        sync.Mutex
	accountSessions  map[string]int
	maxObservedLanes atomic.Int64
	budget           *limiter.Budget
	metrics          *metrics.Registry
	// udpRelays holds the relay sockets of UDP associations whose lane died,
	// so the replacement association keeps the source address the destination
	// has been talking to.
	udpRelays *udpRelayStore
}

type serverFlow struct {
	flow        *multipathFlow
	principal   identity.Principal
	maxLanes    int
	tcpMaxLanes int
	tcpMode     bool
	completed   atomic.Bool
	tombstone   sync.Once
	mu          sync.Mutex
}

// quicAuthState carries the immutable device principal established by the
// mutual TLS handshake. flows counts sessions multiplexed on the connection.
type quicAuthState struct {
	principal identity.Principal
	flows     atomic.Int64
}

// shared reports whether more than one flow is using this connection.
func (a *quicAuthState) shared() bool {
	if a == nil {
		return false
	}
	return a.flows.Load() > 1
}

// serverLaneBudget is the total lanes this endpoint will admit for one flow.
// It must match the client's split in bulkLaneBudget: counting the reserved
// control lane against the bulk maximum makes the server reject and close
// every joined bulk lane, which the peer sees as an immediate EOF and retries,
// churning through lanes instead of transferring.
func serverLaneBudget(reserveControl bool) int {
	bulk, control := bulkLaneBudget(reserveControl)
	return bulk + control
}

func (s *serverFlow) addLane(lane *mpLane) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tcpMode && lane.kind != TransportTCP {
		return errors.New("flow has switched to TCP fallback")
	}
	if lane.kind == TransportTCP && !s.tcpMode {
		// The first authenticated TCP rescue is a transport handoff, not another
		// path in a mixed bundle. Retiring QUIC immediately re-offers its chunks
		// to the reliable scheduler before admitting the TCP lane.
		s.flow.retireLanesExcept(TransportTCP)
		s.tcpMode = true
		s.maxLanes = s.tcpMaxLanes
	}
	if s.flow.laneCount() >= s.maxLanes {
		// The peer can detect a dead QUIC socket before this endpoint does
		// (for example, when the return path is black-holed). Retire the
		// oldest lane with the same role as its authenticated replacement.
		// A bulk replacement must never evict the control lane, and a control
		// generation replacement must not evict healthy bulk capacity.
		if !s.flow.retireOldestLane(lane.control) || s.flow.laneCount() >= s.maxLanes {
			return errors.New("flow lane limit reached")
		}
	}
	if err := s.flow.addLane(lane); err != nil {
		return err
	}
	if s.tcpMode && s.maxLanes > 1 {
		s.flow.tcpStriping.Store(true)
	}
	return nil
}

func newServerFlow(flow *multipathFlow, principal identity.Principal, initialKind TransportKind, tcpMaxLanes int) *serverFlow {
	serverSession := &serverFlow{
		flow: flow, principal: principal, maxLanes: serverLaneBudget(flow.reserveControlLane),
		tcpMaxLanes: tcpMaxLanes,
	}
	if initialKind == TransportTCP {
		serverSession.tcpMode = true
		serverSession.maxLanes = tcpMaxLanes
		flow.tcpStriping.Store(tcpMaxLanes > 1)
	}
	return serverSession
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("server listen address is required")
	}
	if err := cfg.Credentials.Validate(); err != nil {
		return nil, fmt.Errorf("server identity: %w", err)
	}
	if cfg.MaxPayload == 0 || cfg.MaxPayload > protocol.DefaultMaxPayload {
		cfg.MaxPayload = 256 * 1024
	}
	if cfg.ChunkSize <= 0 || cfg.ChunkSize > int(cfg.MaxPayload) {
		cfg.ChunkSize = defaultChunkSize
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.FlowIdleTimeout <= 0 {
		cfg.FlowIdleTimeout = defaultFlowIdleTimeout
	}
	if cfg.FlowMaxLifetime <= 0 {
		cfg.FlowMaxLifetime = defaultFlowMaxLifetime
	}
	if cfg.FlowIdleTimeout > cfg.FlowMaxLifetime {
		return nil, errors.New("flow idle timeout cannot exceed maximum lifetime")
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 4096
	}
	if cfg.MaxSessions > maxConfiguredSessions {
		return nil, fmt.Errorf("maximum sessions must not exceed %d", maxConfiguredSessions)
	}
	if cfg.TCPFallbackLanes == 0 {
		cfg.TCPFallbackLanes = maxTCPFallbackLanes
	}
	if cfg.TCPFallbackLanes < 1 || cfg.TCPFallbackLanes > maxTCPFallbackLanes {
		return nil, fmt.Errorf("TCP fallback lanes must be between 1 and %d", maxTCPFallbackLanes)
	}
	var err error
	cfg.TCPCongestion, err = normalizeTCPCongestion(cfg.TCPCongestion)
	if err != nil {
		return nil, err
	}
	if cfg.Congestion == "" {
		cfg.Congestion = defaultCongestion()
	}
	if cfg.Congestion != CongestionReno && cfg.Congestion != CongestionBBR && cfg.Congestion != CongestionBBRTUIC && cfg.Congestion != CongestionErasure && cfg.Congestion != CongestionAdaptive && cfg.Congestion != CongestionBrutal {
		return nil, fmt.Errorf("unsupported QUIC congestion controller %q", cfg.Congestion)
	}
	if cfg.Congestion == CongestionBrutal && cfg.BrutalBytesPerSec == 0 {
		return nil, errors.New("brutal congestion requires a positive per-lane byte rate")
	}
	if cfg.AdaptiveMinBytesSec == 0 {
		cfg.AdaptiveMinBytesSec = defaultAdaptiveMinBytesPerSec
	}
	if cfg.AdaptiveMaxBytesSec == 0 {
		cfg.AdaptiveMaxBytesSec = defaultAdaptiveMaxBytesPerSec
	}
	if cfg.AdaptiveMaxBytesSec < cfg.AdaptiveMinBytesSec {
		return nil, errors.New("adaptive maximum byte rate cannot be below its minimum")
	}
	if cfg.AggregateBytesPerSec == 0 && cfg.InteractiveReserveBytesPerSec != 0 {
		return nil, errors.New("interactive reserve requires an aggregate byte budget")
	}
	if cfg.InteractiveReserveBytesPerSec > cfg.AggregateBytesPerSec {
		return nil, errors.New("interactive reserve cannot exceed aggregate byte budget")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	if !cfg.EnableTCP && !cfg.EnableQUIC {
		cfg.EnableTCP = true
	}
	server := &Server{
		cfg:             cfg,
		semaphore:       make(chan struct{}, cfg.MaxSessions),
		connections:     make(chan struct{}, cfg.MaxSessions),
		enrollments:     make(chan struct{}, min(cfg.MaxSessions, 64)),
		sessions:        make(map[[16]byte]*serverFlow),
		accountSessions: make(map[string]int),
		budget:          limiter.New(limiter.Config{TotalBytesPerSec: cfg.AggregateBytesPerSec, ReserveBytesPerSec: cfg.InteractiveReserveBytesPerSec}),
		metrics:         cfg.Metrics,
		udpRelays:       newUDPRelayStore(),
	}
	return server, nil
}

// Metrics exposes aggregate counters for an optional operator endpoint.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

func (s *Server) Serve(ctx context.Context) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.watchAuthorizationStore(serveCtx)
	errCh := make(chan error, 2)
	count := 0
	if s.cfg.EnableTCP {
		count++
		go func() { errCh <- s.serveTCP(serveCtx) }()
	}
	if s.cfg.EnableQUIC {
		count++
		go func() { errCh <- s.serveQUIC(serveCtx) }()
	}
	var firstErr error
	for range count {
		err := <-errCh
		if err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

func (s *Server) watchAuthorizationStore(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			changed, err := s.cfg.Credentials.Store.Refresh()
			if err != nil {
				s.cfg.Logger.Error("authorization refresh failed; retaining last known-good state", "error", err)
			} else if changed {
				s.cfg.Logger.Info("authorization state reloaded")
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) serveTCP(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	listener, err := lc.Listen(ctx, "tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on remote TLS/TCP address: %w", err)
	}
	if err := setTCPListenerCongestion(listener, s.cfg.TCPCongestion); err != nil {
		_ = listener.Close()
		return fmt.Errorf("configure remote TLS/TCP congestion control: %w", err)
	}
	return s.ServeListener(ctx, listener)
}

// ServeListener runs the authenticated server on an already-bound listener.
// This also supports socket activation and deterministic integration tests.
func (s *Server) ServeListener(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	tlsConfig, err := identity.ServerTLSConfig(s.cfg.Credentials, defaultALPN, s.cfg.Enrollment != nil)
	if err != nil {
		return fmt.Errorf("configure server TLS identity: %w", err)
	}
	var wg sync.WaitGroup
	s.cfg.Logger.Info("remote TLS/TCP listener ready", "address", listener.Addr().String())
	for {
		raw, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accept remote lane: %w", acceptErr)
		}
		if err := setTCPConnCongestion(raw, s.cfg.TCPCongestion); err != nil {
			_ = raw.Close()
			return fmt.Errorf("configure accepted TLS/TCP congestion control: %w", err)
		}
		select {
		case s.semaphore <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-s.semaphore }()
				s.handleTCP(ctx, tls.Server(raw, tlsConfig))
			}()
		default:
			_ = raw.Close()
			s.cfg.Logger.Warn("remote session limit reached")
		}
	}
}

func (s *Server) handleTCP(ctx context.Context, conn *tls.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(handshakeBound(conn, s.cfg.HandshakeTimeout)))
	if err := conn.HandshakeContext(ctx); err != nil {
		s.cfg.Logger.Debug("remote TLS handshake failed", "error", err)
		return
	}
	state := conn.ConnectionState()
	if state.NegotiatedProtocol == identity.EnrollmentALPN {
		if s.cfg.Enrollment != nil && s.admitEnrollment() {
			defer s.releaseEnrollment()
			_ = s.cfg.Enrollment.Serve(conn)
		}
		return
	}
	if state.NegotiatedProtocol == identity.RenewalALPN {
		if s.cfg.Enrollment != nil && s.admitEnrollment() {
			defer s.releaseEnrollment()
			if principal, err := identity.PrincipalFromTLS(state); err == nil {
				_ = s.cfg.Enrollment.Renew(conn, principal)
			}
		}
		return
	}
	if state.NegotiatedProtocol != defaultALPN {
		return
	}
	principal, err := identity.PrincipalFromTLS(state)
	if err != nil {
		return
	}
	s.handleSession(ctx, conn, principal, nil)
}

func (s *Server) serveQUIC(ctx context.Context) error {
	packetConn, err := net.ListenPacket("udp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on remote QUIC address: %w", err)
	}
	return s.ServePacketConn(ctx, packetConn)
}

// ServePacketConn runs the QUIC listener on an already-bound UDP socket.
func (s *Server) ServePacketConn(ctx context.Context, packetConn net.PacketConn) error {
	tlsConfig, err := identity.ServerTLSConfig(s.cfg.Credentials, defaultALPN, s.cfg.Enrollment != nil)
	if err != nil {
		_ = packetConn.Close()
		return fmt.Errorf("configure server TLS identity: %w", err)
	}
	listener, err := quic.Listen(packetConn, tlsConfig, quicServerConfig(
		flowWindows{stream: s.cfg.StreamReceiveWindow, connection: s.cfg.ConnectionReceiveWindow}))
	if err != nil {
		_ = packetConn.Close()
		return fmt.Errorf("create QUIC listener: %w", err)
	}
	defer listener.Close()
	defer packetConn.Close()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	s.cfg.Logger.Info("remote QUIC listener ready", "address", listener.Addr().String())
	var wg sync.WaitGroup
	for {
		conn, acceptErr := listener.Accept(ctx)
		if acceptErr != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil
			}
			return fmt.Errorf("accept QUIC lane: %w", acceptErr)
		}
		if !s.admitConnection() {
			_ = conn.CloseWithError(0x100, "server connection limit reached")
			s.cfg.Logger.Warn("remote QUIC connection limit reached")
			continue
		}
		// Session admission is performed per QUIC stream in handleQUIC. Holding
		// one slot from that session semaphore for the lifetime of a multiplexed
		// connection would incorrectly reduce MaxSessions and prevent the
		// connection from carrying the configured number of independent flows.
		// The separate connection semaphore above bounds idle/untrusted peers.
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer s.releaseConnection()
			s.handleQUIC(ctx, conn)
		}()
	}
}

func (s *Server) admitConnection() bool {
	select {
	case s.connections <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseConnection() { <-s.connections }

func (s *Server) admitEnrollment() bool {
	select {
	case s.enrollments <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseEnrollment() { <-s.enrollments }

func (s *Server) handleQUIC(ctx context.Context, conn *quic.Conn) {
	var wg sync.WaitGroup
	// Close the shared connection before waiting for stream handlers. This
	// ordering is important during shutdown: a handler blocked in Read must be
	// released before Wait can complete.
	defer wg.Wait()
	defer conn.CloseWithError(0, "queqiao session complete")
	controller := configureQUICController(conn, congestionConfig{
		kind: s.cfg.Congestion, brutalBytesPerSecond: s.cfg.BrutalBytesPerSec,
		adaptiveMinBytesPerSec: s.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: s.cfg.AdaptiveMaxBytesSec,
	})
	state := conn.ConnectionState().TLS
	if state.NegotiatedProtocol == identity.EnrollmentALPN {
		if s.cfg.Enrollment == nil || !s.admitEnrollment() {
			return
		}
		defer s.releaseEnrollment()
		stream, err := acceptQUICStream(ctx, conn, controller)
		if err == nil {
			_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
			_ = s.cfg.Enrollment.Serve(stream)
			_ = stream.Close()
		}
		return
	}
	if state.NegotiatedProtocol == identity.RenewalALPN {
		if s.cfg.Enrollment == nil || !s.admitEnrollment() {
			return
		}
		defer s.releaseEnrollment()
		principal, err := identity.PrincipalFromTLS(state)
		if err != nil {
			return
		}
		stream, err := acceptQUICStream(ctx, conn, controller)
		if err == nil {
			_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
			_ = s.cfg.Enrollment.Renew(stream, principal)
			_ = stream.Close()
		}
		return
	}
	if state.NegotiatedProtocol != defaultALPN {
		return
	}
	principal, err := identity.PrincipalFromTLS(state)
	if err != nil {
		return
	}
	auth := &quicAuthState{principal: principal}
	// Mutual TLS authenticates the whole QUIC connection before any stream is
	// accepted. Every stream therefore begins directly with OPEN or JOIN.
	dispatch := func(lane streamConn) bool {
		select {
		case s.semaphore <- struct{}{}:
			wg.Add(1)
			go func(lane streamConn) {
				defer wg.Done()
				defer func() { <-s.semaphore }()
				s.handleSession(ctx, lane, principal, auth)
			}(lane)
			return true
		default:
			_ = lane.Close()
			s.cfg.Logger.Warn("remote session limit reached")
			return false
		}
	}
	for {
		// Waiting for another stream is not a handshake operation. Applying the
		// per-stream authentication timeout here used to close the entire QUIC
		// connection after ten seconds without a *new* stream, even while an
		// existing long download was actively transferring. Each accepted stream
		// still gets the bounded authentication deadline in handleSession; the
		// outer connection is bounded by QUIC's idle timeout and server shutdown.
		stream, err := acceptQUICStream(ctx, conn, controller)
		if err != nil {
			if ctx.Err() == nil {
				s.cfg.Logger.Debug("accept QUIC stream failed", "error", err)
			}
			return
		}
		dispatch(stream)
	}
}

func (s *Server) handleSession(ctx context.Context, conn streamConn, principal identity.Principal, auth *quicAuthState) {
	defer conn.Close()
	if auth != nil {
		auth.flows.Add(1)
		defer auth.flows.Add(-1)
	}
	sessionStarted := time.Now()
	_ = conn.SetDeadline(time.Now().Add(handshakeBound(conn, s.cfg.HandshakeTimeout)))
	fc := newFrameConn(conn, s.cfg.MaxPayload)
	fc.setPacketsOnStream(s.cfg.UDPOnStream)
	open, err := fc.Read()
	if err != nil {
		s.cfg.Logger.Debug("read authenticated stream open", "error", err)
		return
	}
	// A QUIC connection may outlive a revocation. Re-authorize every new
	// stream so a disabled device cannot open flows, probes, or replacement
	// lanes merely because its original TLS handshake predates the change.
	if _, err := s.cfg.Credentials.Store.Authorize(principal, time.Now()); err != nil {
		return
	}
	if open.Header.Type == protocol.TypeProbe {
		s.handlePathProbe(fc, open)
		return
	}
	if open.Header.Type == protocol.TypeJoin {
		if session.IsZeroSessionID(open.Header.SessionID) || open.Header.FlowID == 0 || open.Header.Sequence != 0 || open.Header.Flags&^protocol.FlagReserveControl != 0 || len(open.Payload) != 8 {
			_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: open.Header.SessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid lane join")})
			return
		}
		laneID := binary.BigEndian.Uint64(open.Payload)
		if laneID == 0 {
			_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: open.Header.SessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid lane join")})
			return
		}
		s.handleLaneJoinOpen(ctx, conn, fc, principal, open.Header.SessionID, laneID, open)
		return
	}
	sessionID := open.Header.SessionID
	laneID := uint64(0)
	if open.Header.Type != protocol.TypeOpen || session.IsZeroSessionID(sessionID) || open.Header.FlowID == 0 || open.Header.Sequence != 0 {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetProtocol, "invalid flow open")})
		return
	}
	if err := s.acquireAccountSession(principal); err != nil {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetFlowLimit, "account session unavailable")})
		return
	}
	defer s.releaseAccountSession(principal.AccountID)
	if session.IsUDPAssociation(open.Payload) {
		s.handleUDPAssociation(ctx, conn, fc, principal, sessionID, open.Header.FlowID, nil, false)
		return
	}
	if token, resumable := session.DecodeUDPResumeOpen(open.Payload); resumable {
		s.handleUDPAssociation(ctx, conn, fc, principal, sessionID, open.Header.FlowID, token, true)
		return
	}
	destination, err := session.DecodeDestination(open.Payload)
	if err != nil {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetDestination, "destination unavailable")})
		return
	}
	destinationDialStarted := time.Now()
	destinationConn, err := s.cfg.DestinationPolicy.DialContext(ctx, destination)
	if err != nil {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetDestination, "destination unavailable")})
		s.cfg.Logger.Debug("destination dial failed", "error", err)
		return
	}
	s.cfg.Logger.Debug("remote flow opened", "transport", transportKindForConn(conn), "account", principal.AccountID, "device", principal.DeviceID, "open_duration", destinationDialStarted.Sub(sessionStarted), "destination_dial_duration", time.Since(destinationDialStarted), "total_duration", time.Since(sessionStarted))
	defer destinationConn.Close()
	flow := newMultipathFlow(ctx, destinationConn, sessionID, open.Header.FlowID, s.cfg.ChunkSize, protocol.FlagAckDown, protocol.FlagAckUp, s.budget, s.metrics, s.cfg.Logger)
	// Wire version 1 requires range acknowledgements on both endpoints.
	flow.ackRanges.Store(true)
	flow.idleTimeout = s.cfg.FlowIdleTimeout
	flow.maxLifetime = s.cfg.FlowMaxLifetime
	flow.reserveControlLane = open.Header.Flags&protocol.FlagReserveControl != 0
	flow.controlLaneShared = auth.shared
	initialKind := transportKindForConn(conn)
	serverSession := newServerFlow(flow, principal, initialKind, s.cfg.TCPFallbackLanes)
	if err := serverSession.addLane(&mpLane{
		id: laneID, kind: initialKind, fc: fc, writeHook: s.cfg.testLaneWriteHook,
		control: flow.reserveControlLane && initialKind == TransportQUIC,
	}); err != nil {
		flow.closeAll()
		return
	}
	s.observeLanes(serverSession.flow.laneCount())
	if !s.registerSession(sessionID, serverSession) {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetFlowLimit, "session already exists")})
		flow.closeAll()
		return
	}
	registered := true
	go s.watchFlowCompletion(ctx, sessionID, serverSession)
	go s.watchAuthorization(ctx, serverSession)
	defer func() {
		if registered {
			s.cfg.Logger.Debug("session released with its flow", "lanes", serverSession.flow.laneCount())
			s.unregisterSession(sessionID, serverSession)
		}
	}()
	if err := fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeOpenOK, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}}); err != nil {
		flow.closeAll()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	s.metrics.FlowStarted()
	stats, err := flow.run(ctx)
	// A peer may close the transport immediately after receiving the final
	// bytes, racing the server's final-ACK bookkeeping. If both directions
	// have observed FIN sequences, the logical flow is complete even when the
	// runner reports the late socket EOF as an error; retain the same bounded
	// tombstone so a replacement lane can replay the final ACK. Do not retain
	// one-sided or context-canceled flows.
	flowComplete := err == nil || (ctx.Err() == nil && serverSession.flow.finSent.Load() && serverSession.flow.remoteFinSeen.Load())
	if !flowComplete && ctx.Err() == nil && serverSession.flow.remoteFinSeen.Load() && expectedDestinationCloseError(err) {
		flowComplete = true
	}
	s.metrics.FlowFinished(stats.BytesRead, stats.BytesSent, !flowComplete && err != nil && !errors.Is(err, context.Canceled))
	if flowComplete {
		// Keep a bounded tombstone long enough for a client that lost the
		// final cumulative ACK to authenticate a replacement lane and finish
		// its local close handshake. No destination connection or payload is
		// retained; only the flow's final sequence metadata remains.
		s.retainCompletedSession(sessionID, serverSession)
		registered = false
	}
	codedFrames, streamFrames := flow.dataSubstrates()
	substrate, hasCoded := flow.codedSubstrate()
	logFields := []any{"session_id", fmt.Sprintf("%x", sessionID), "flow_id", flow.flowID,
		"transport", transportKindForConn(conn), "bytes_from_client", stats.BytesRead, "bytes_to_client", stats.BytesSent,
		"duration", stats.Ended.Sub(stats.Started), "lane_bytes", stats.LaneBytes,
		// Where the payload went. The server is the sender for a download, so
		// this is the split that decides what a download costs.
		"data_coded", codedFrames, "data_stream", streamFrames,
		"coded_substrate", codedSubstrateFields(substrate, hasCoded),
		"class", classifier.Class(flow.class.Load())}
	logFields = append(logFields, codedSubstrateLogFields(substrate, hasCoded)...)
	if !flowComplete && err != nil && !errors.Is(err, context.Canceled) {
		s.cfg.Logger.Warn("remote flow ended with error", append(logFields, "error", err)...)
		return
	}
	s.cfg.Logger.Info("remote flow complete", logFields...)
}

const (
	maxPathProbeFrames = 128
	maxPathProbeBytes  = 128 * 1024
)

// handlePathProbe accepts only a small, destination-free sequence. Transport
// acknowledgements are the response; no application frame is reflected, so
// the probe cannot be used as an amplifier or destination oracle.
func (s *Server) handlePathProbe(fc *frameConn, first protocol.Frame) {
	frames, bytes := 0, 0
	frame := first
	sessionID := first.Header.SessionID
	for {
		if frame.Header.Type != protocol.TypeProbe || session.IsZeroSessionID(frame.Header.SessionID) ||
			frame.Header.SessionID != sessionID ||
			frame.Header.FlowID != 0 || frame.Header.Sequence != uint64(frames) || frame.Header.Flags != 0 ||
			frame.Header.Class != protocol.ClassNew || len(frame.Payload) == 0 || len(frame.Payload) > 1200 ||
			bytes+len(frame.Payload) > maxPathProbeBytes {
			return
		}
		frames++
		bytes += len(frame.Payload)
		if frames >= maxPathProbeFrames || bytes >= maxPathProbeBytes {
			return
		}
		next, err := fc.Read()
		if err != nil {
			return
		}
		frame = next
	}
}

// watchFlowCompletion closes a correctness gap between the application FIN
// exchange and the flow runner's final goroutine return. Both direction FIN
// sequences prove that no additional payload can be delivered, so retaining
// a tombstone at that point lets a replacement lane replay the final ACK even
// if the peer has already closed its socket and the runner is waiting for a
// late duplicate ACK.
func (s *Server) watchFlowCompletion(ctx context.Context, sessionID [16]byte, serverSession *serverFlow) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if ctx.Err() == nil && serverSession.flow.finSent.Load() && serverSession.flow.remoteFinSeen.Load() {
				s.retainCompletedSession(sessionID, serverSession)
				// Both FINs are an application-level proof that no additional
				// payload can be delivered in either direction. Tear down the
				// physical lanes now; otherwise sendInner can wait forever for a
				// final ACK lost in the last-lane close race and receiveInner can
				// wait forever for another frame. The tombstone above preserves
				// final-ACK metadata for bounded replay.
				serverSession.flow.closeAll()
				return
			}
		case <-serverSession.flow.doneChan():
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) retainCompletedSession(sessionID [16]byte, serverSession *serverFlow) {
	serverSession.tombstone.Do(func() {
		serverSession.completed.Store(true)
		time.AfterFunc(completedSessionLinger, func() {
			s.cfg.Logger.Debug("session tombstone expired")
			s.unregisterSession(sessionID, serverSession)
		})
	})
}

// handleLaneJoinOpen runs only after mutual TLS and exact JOIN validation. It
// additionally binds the new lane to the principal that created the session;
// session and flow IDs are routing identifiers, never bearer credentials.
func (s *Server) handleLaneJoinOpen(ctx context.Context, conn streamConn, fc *frameConn, principal identity.Principal, sessionID [16]byte, laneID uint64, open protocol.Frame) {
	if session.IsZeroSessionID(sessionID) || laneID == 0 || open.Header.FlowID == 0 {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid lane join identity")})
		return
	}
	serverSession := s.lookupSession(sessionID)
	if serverSession == nil || serverSession.flow.flowID != open.Header.FlowID || !samePrincipal(serverSession.principal, principal) {
		// A rescue arriving after the session is gone is the failure mode that
		// matters most on a lossy path, and it used to be silent on this side.
		s.cfg.Logger.Debug("lane join refused: unknown session", "lane", laneID, "known", serverSession != nil)
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "unknown session")})
		return
	}
	kind := transportKindForConn(conn)
	controlReplacement := open.Header.Flags&protocol.FlagReserveControl != 0
	if controlReplacement && (!serverSession.flow.reserveControlLane || kind != TransportQUIC) {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid control lane replacement")})
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if serverSession.completed.Load() {
		s.cfg.Logger.Debug("lane join reached a completed session", "lane", laneID)
		if err := fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeOpenOK, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}}); err != nil {
			return
		}
		// The completed server flow has already acknowledged the peer's FIN;
		// repeat that ACK on this authenticated replacement lane.
		_ = fc.Write(protocol.Frame{Header: protocol.Header{
			Version: protocol.Version, Type: protocol.TypeAck, Flags: protocol.FlagAckFinal | serverSession.flow.recvAckFlag,
			SessionID: sessionID, FlowID: open.Header.FlowID, Sequence: serverSession.flow.remoteFinSequence.Load(),
			Class: protocol.ClassBulk,
		}})
		// The final ACK above acknowledges the peer's FIN.  If the server's
		// own FIN was lost while the last physical lane was closing, replay it
		// as well; otherwise the client can receive the complete application
		// body yet remain stuck waiting for the remote half-close until its
		// replacement timeout.  Keep the ACK first for compatibility with
		// clients that begin consuming the tombstone immediately after
		// OpenOK, then let the normal flow reader process this FIN.
		if serverSession.flow.finSent.Load() {
			flags := uint16(protocol.FlagFin)
			if serverSession.flow.localAbortSent.Load() {
				flags |= protocol.FlagCloseAbort
			}
			_ = fc.Write(protocol.Frame{Header: protocol.Header{
				Version: protocol.Version, Type: protocol.TypeClose, Flags: flags,
				SessionID: sessionID, FlowID: open.Header.FlowID, Sequence: serverSession.flow.finSequence.Load(),
				Class: protocol.ClassBulk,
			}})
		}
		return
	}
	replacement := &mpLane{
		id: laneID, kind: kind, fc: fc, writeHook: s.cfg.testLaneWriteHook,
		control: controlReplacement, staged: true,
	}
	if err := serverSession.addLane(replacement); err != nil {
		s.cfg.Logger.Debug("lane join refused: lane unavailable", "lane", laneID, "error", err)
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetFlowLimit, "lane unavailable")})
		return
	}
	if err := fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeOpenOK, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}}); err != nil {
		return
	}
	if err := serverSession.flow.activateLane(replacement); err != nil {
		return
	}
	s.cfg.Logger.Debug("lane joined", "lane", laneID, "transport", kind, "control", controlReplacement, "lanes", serverSession.flow.laneCount())
	s.observeLanes(serverSession.flow.laneCount())
	// A replacement can arrive after the destination has already reached EOF
	// but before the original lane carried the logical FIN. Replay any known
	// close state on this active lane immediately. Without this, the first
	// rescue only lets the peer's FIN reach the server; the server then marks a
	// tombstone and the client needs a second rescue merely to learn the FIN it
	// had already received as application bytes. FIN/ACK frames are
	// idempotent at the reassembler and cumulative-ACK state, so replaying them
	// is safe even when the original frame was merely delayed.
	if serverSession.flow.remoteFinSeen.Load() {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{
			Version: protocol.Version, Type: protocol.TypeAck,
			Flags:     protocol.FlagAckFinal | serverSession.flow.recvAckFlag,
			SessionID: sessionID, FlowID: open.Header.FlowID,
			Sequence: serverSession.flow.remoteFinSequence.Load(), Class: protocol.ClassBulk,
		}})
	}
	if serverSession.flow.finSent.Load() {
		flags := uint16(protocol.FlagFin)
		if serverSession.flow.localAbortSent.Load() {
			flags |= protocol.FlagCloseAbort
		}
		_ = fc.Write(protocol.Frame{Header: protocol.Header{
			Version: protocol.Version, Type: protocol.TypeClose, Flags: flags,
			SessionID: sessionID, FlowID: open.Header.FlowID,
			Sequence: serverSession.flow.finSequence.Load(), Class: protocol.ClassBulk,
		}})
	}
	select {
	case <-serverSession.flow.doneChan():
	case <-ctx.Done():
	}
}

func samePrincipal(a, b identity.Principal) bool {
	return a.ProviderID == b.ProviderID && a.AccountID == b.AccountID && a.DeviceID == b.DeviceID
}

func (s *Server) acquireAccountSession(principal identity.Principal) error {
	authorization, err := s.cfg.Credentials.Store.Authorize(principal, time.Now())
	if err != nil {
		return err
	}
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	active := s.accountSessions[principal.AccountID]
	if authorization.Account.MaxSessions > 0 && active >= authorization.Account.MaxSessions {
		return errors.New("account session limit reached")
	}
	s.accountSessions[principal.AccountID] = active + 1
	return nil
}

func (s *Server) releaseAccountSession(accountID string) {
	s.accountMu.Lock()
	defer s.accountMu.Unlock()
	if active := s.accountSessions[accountID]; active <= 1 {
		delete(s.accountSessions, accountID)
	} else {
		s.accountSessions[accountID] = active - 1
	}
}

// watchAuthorization applies revocation and account expiry to already-open
// flows. TLS prevents new use immediately; this watcher bounds an existing
// connection's remaining lifetime without a CRL or server restart.
func (s *Server) watchAuthorization(ctx context.Context, flow *serverFlow) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := s.cfg.Credentials.Store.Authorize(flow.principal, time.Now()); err != nil {
				flow.flow.closeAll()
				return
			}
		case <-flow.flow.doneChan():
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) registerSession(id [16]byte, flow *serverFlow) bool {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	if len(s.sessions) >= s.cfg.MaxSessions {
		return false
	}
	if _, exists := s.sessions[id]; exists {
		return false
	}
	s.sessions[id] = flow
	return true
}

func (s *Server) lookupSession(id [16]byte) *serverFlow {
	s.sessionsMu.RLock()
	defer s.sessionsMu.RUnlock()
	return s.sessions[id]
}

func (s *Server) unregisterSession(id [16]byte, flow *serverFlow) {
	s.sessionsMu.Lock()
	if s.sessions[id] == flow {
		delete(s.sessions, id)
	}
	s.sessionsMu.Unlock()
}

func (s *Server) observeLanes(count int) {
	for {
		old := s.maxObservedLanes.Load()
		if int64(count) <= old || s.maxObservedLanes.CompareAndSwap(old, int64(count)) {
			return
		}
	}
}

// MaxObservedLanes reports the largest number of lanes attached to any flow
// since this server instance started. It is safe for benchmark instrumentation
// and does not expose session IDs or destination metadata.
func (s *Server) MaxObservedLanes() int { return int(s.maxObservedLanes.Load()) }

func transportKindForConn(conn streamConn) TransportKind {
	if _, ok := conn.(*quicStreamConn); ok {
		return TransportQUIC
	}
	return TransportTCP
}
