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
	"github.com/icourses-dev/wanopt/internal/limiter"
	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/session"
)

// Must exceed the client's bounded lane-replacement wait so a final-ACK loss
// can still be repaired after QUIC dead-path detection and scheduler backoff.
// Tombstones retain only final sequence metadata and remain bounded by the
// configured session admission limit.
const completedSessionLinger = 90 * time.Second

type ServerConfig struct {
	ListenAddr                    string
	Certificate                   tls.Certificate
	Secret                        []byte
	MaxPayload                    uint32
	ChunkSize                     int
	HandshakeTimeout              time.Duration
	FlowIdleTimeout               time.Duration
	FlowMaxLifetime               time.Duration
	MaxSessions                   int
	DestinationPolicy             DestinationPolicy
	EnableTCP                     bool
	EnableQUIC                    bool
	Congestion                    CongestionControlKind
	BrutalBytesPerSec             uint64
	AdaptiveMinBytesSec           uint64
	AdaptiveMaxBytesSec           uint64
	AggregateBytesPerSec          uint64
	InteractiveReserveBytesPerSec uint64
	// ReplayMemoryBytes bounds the total memory all remote flows may hold in
	// their replay windows. Zero selects the package default.
	ReplayMemoryBytes uint64
	Metrics           *metrics.Registry
	MaxLanes          int
	Logger            *slog.Logger
	// testLaneWriteHook is intentionally unexported and nil in production. It
	// lets package integration tests reproduce loss of a specific logical
	// frame without depending on encrypted QUIC packet layout.
	testLaneWriteHook func(protocol.Frame) error
}

type Server struct {
	cfg              ServerConfig
	replay           *session.ReplayGuard
	semaphore        chan struct{}
	sessionsMu       sync.RWMutex
	sessions         map[[16]byte]*serverFlow
	maxObservedLanes atomic.Int64
	budget           *limiter.Budget
	metrics          *metrics.Registry
	// replayBudget bounds the memory all remote flows may hold in their
	// replay windows.
	replayBudget *replayBudget
	// quicCapabilities is kept on the server instance so capability negotiation
	// is consistent for every stream on a connection. It is initialized to the
	// current capability set; tests may set it to zero to model an older peer.
	quicCapabilities uint64
	quicFastStreams  atomic.Bool
}

type serverFlow struct {
	flow      *multipathFlow
	maxLanes  int
	completed atomic.Bool
	tombstone sync.Once
	mu        sync.Mutex
}

// quicAuthState records authentication of the QUIC connection itself. The
// first pooled stream still uses the normal nonce/HMAC Hello exchange. Once
// that succeeds, later streams may use TypeOpenFast; they remain isolated by
// their own random session and flow IDs. Dedicated lanes never share this
// state with another QUIC connection.
type quicAuthState struct{ authenticated atomic.Bool }

// serverLaneBudget is the total lanes this endpoint will admit for one flow.
// It must match the client's split in bulkLaneBudget: counting the reserved
// control lane against the bulk maximum makes the server reject and close
// every joined bulk lane, which the peer sees as an immediate EOF and retries,
// churning through lanes instead of transferring.
func serverLaneBudget(maxLanes int, reserveControl bool) int {
	bulk, control := bulkLaneBudget(maxLanes, reserveControl)
	return bulk + control
}

func (s *serverFlow) addLane(lane *mpLane) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flow.laneCount() >= s.maxLanes {
		// The peer can detect a dead QUIC socket before this endpoint does
		// (for example, when the return path is black-holed). Retire one
		// oldest active lane to admit the authenticated replacement while
		// preserving the configured active-lane and memory bound.
		if !s.flow.retireOldestLane() || s.flow.laneCount() >= s.maxLanes {
			return errors.New("flow lane limit reached")
		}
	}
	if err := s.flow.addLane(lane); err != nil {
		return err
	}
	return nil
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.ListenAddr == "" {
		return nil, errors.New("server listen address is required")
	}
	if len(cfg.Certificate.Certificate) == 0 || cfg.Certificate.PrivateKey == nil {
		return nil, errors.New("server TLS certificate and key are required")
	}
	if len(cfg.Secret) < 16 {
		return nil, errors.New("server secret must contain at least 16 bytes")
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
	if cfg.MaxLanes <= 0 {
		cfg.MaxLanes = 8
	}
	if cfg.MaxLanes > 8 {
		return nil, errors.New("maximum lane count must not exceed 8")
	}
	if cfg.Congestion == "" {
		cfg.Congestion = CongestionReno
	}
	if cfg.Congestion != CongestionReno && cfg.Congestion != CongestionBBR && cfg.Congestion != CongestionBBRTUIC && cfg.Congestion != CongestionAdaptive && cfg.Congestion != CongestionBrutal {
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
		cfg:              cfg,
		replay:           session.NewReplayGuard(10*time.Minute, cfg.MaxSessions*4),
		semaphore:        make(chan struct{}, cfg.MaxSessions),
		sessions:         make(map[[16]byte]*serverFlow),
		budget:           limiter.New(limiter.Config{TotalBytesPerSec: cfg.AggregateBytesPerSec, ReserveBytesPerSec: cfg.InteractiveReserveBytesPerSec}),
		metrics:          cfg.Metrics,
		replayBudget:     newReplayBudget(int64(cfg.ReplayMemoryBytes)),
		quicCapabilities: session.CapabilityFastStreams | session.CapabilityReserveControl | session.CapabilityFastLaneJoin,
	}
	server.quicFastStreams.Store(true)
	return server, nil
}

// Metrics exposes aggregate counters for an optional operator endpoint.
func (s *Server) Metrics() *metrics.Registry { return s.metrics }

func (s *Server) Serve(ctx context.Context) error {
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
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

func (s *Server) serveTCP(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	listener, err := lc.Listen(ctx, "tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on remote TLS/TCP address: %w", err)
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

	tlsConfig := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.cfg.Certificate},
		NextProtos:   []string{defaultALPN},
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
	_ = conn.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	if err := conn.HandshakeContext(ctx); err != nil {
		s.cfg.Logger.Debug("remote TLS handshake failed", "error", err)
		return
	}
	if conn.ConnectionState().NegotiatedProtocol != defaultALPN {
		return
	}
	s.handleSession(ctx, conn, nil)
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
	listener, err := quic.Listen(packetConn, quicServerTLSConfig(s.cfg.Certificate), quicServerConfig())
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
		// Session admission is performed per QUIC stream in handleQUIC. Holding
		// one global semaphore slot for the lifetime of a multiplexed
		// connection would incorrectly reduce MaxSessions and prevent the
		// connection from carrying the configured number of independent flows.
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleQUIC(ctx, conn)
		}()
	}
}

func (s *Server) handleQUIC(ctx context.Context, conn *quic.Conn) {
	var wg sync.WaitGroup
	// Close the shared connection before waiting for stream handlers. This
	// ordering is important during shutdown: a handler blocked in Read must be
	// released before Wait can complete.
	defer wg.Wait()
	defer conn.CloseWithError(0, "wanopt session complete")
	controller := configureQUICController(conn, congestionConfig{
		kind: s.cfg.Congestion, brutalBytesPerSecond: s.cfg.BrutalBytesPerSec,
		adaptiveMinBytesPerSec: s.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: s.cfg.AdaptiveMaxBytesSec,
	})
	if conn.ConnectionState().TLS.NegotiatedProtocol != defaultALPN {
		return
	}
	auth := &quicAuthState{}
	// A single QUIC connection is a bounded stream pool. Its first stream
	// authenticates the connection; later capable streams use OPEN_FAST. Every
	// stream still owns an independent random logical-flow identity, while QUIC
	// supplies one shared congestion controller and packet-loss state. This is
	// the same multiplexing property that makes TUIC effective for short flows,
	// without sharing application/session framing state.
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
		select {
		case s.semaphore <- struct{}{}:
			wg.Add(1)
			go func(stream streamConn) {
				defer wg.Done()
				defer func() { <-s.semaphore }()
				s.handleSession(ctx, stream, auth)
			}(stream)
		default:
			_ = stream.Close()
			s.cfg.Logger.Warn("remote session limit reached")
		}
	}
}

func (s *Server) handleSession(ctx context.Context, conn streamConn, auth *quicAuthState) {
	defer conn.Close()
	sessionStarted := time.Now()
	authFinished := time.Time{}
	openFinished := time.Time{}
	_ = conn.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	fc := newFrameConn(conn, s.cfg.MaxPayload)
	var (
		hello      session.Hello
		sessionID  [16]byte
		laneID     uint64
		open       protocol.Frame
		fastOpen   bool
		helloReady bool
	)
	// A capable pooled connection can authenticate a new stream with one
	// OPEN_FAST frame. If the stream starts with the legacy HELLO frame, retain
	// full backward compatibility with older clients and peers.
	if auth != nil && auth.authenticated.Load() {
		first, readErr := fc.Read()
		if readErr != nil {
			return
		}
		if first.Header.Type == protocol.TypeOpenJoinFast {
			if s.quicCapabilities&session.CapabilityFastLaneJoin == 0 {
				_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: first.Header.SessionID, FlowID: first.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "fast lane join unavailable")})
				return
			}
			if session.IsZeroSessionID(first.Header.SessionID) || first.Header.FlowID == 0 || first.Header.Sequence != 0 || first.Header.Flags != protocol.FlagLaneJoin || len(first.Payload) != 8 {
				_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: first.Header.SessionID, FlowID: first.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid fast lane join")})
				return
			}
			laneID := binary.BigEndian.Uint64(first.Payload)
			if laneID == 0 {
				_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: first.Header.SessionID, FlowID: first.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid fast lane join")})
				return
			}
			s.handleLaneJoinOpen(ctx, conn, fc, first.Header.SessionID, laneID, first)
			return
		} else if first.Header.Type == protocol.TypeOpenFast {
			if !s.quicFastStreams.Load() {
				_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: first.Header.SessionID, FlowID: first.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetProtocol, "fast streams unavailable")})
				return
			}
			if session.IsZeroSessionID(first.Header.SessionID) || first.Header.FlowID == 0 || first.Header.Sequence != 0 {
				_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: first.Header.SessionID, FlowID: first.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetProtocol, "invalid fast flow open")})
				return
			}
			fastOpen = true
			sessionID = first.Header.SessionID
			open = first
			laneID = 0
			authFinished = time.Now()
			openFinished = authFinished
		} else {
			hello, readErr = serverAuthenticateHelloFrameCallback(fc, s.cfg.Secret, s.replay, time.Now(), s.quicCapabilities, &first, func(h session.Hello) {
				if (h.Kind == session.HelloNew || h.Kind == session.HelloPool) && h.LaneID == 0 {
					auth.authenticated.Store(true)
				}
			})
			if readErr != nil {
				s.cfg.Logger.Warn("session authentication failed", "error", readErr)
				return
			}
			helloReady = true
			authFinished = time.Now()
		}
	}
	if !fastOpen && !helloReady {
		var err error
		if auth != nil {
			hello, err = serverAuthenticateHelloFrameCallback(fc, s.cfg.Secret, s.replay, time.Now(), s.quicCapabilities, nil, func(h session.Hello) {
				if (h.Kind == session.HelloNew || h.Kind == session.HelloPool) && h.LaneID == 0 {
					auth.authenticated.Store(true)
				}
			})
		} else {
			hello, err = serverAuthenticateHello(fc, s.cfg.Secret, s.replay, time.Now())
		}
		if err != nil {
			s.cfg.Logger.Warn("session authentication failed", "error", err)
			return
		}
		authFinished = time.Now()
	}
	if !fastOpen {
		if hello.Kind == session.HelloPool {
			// Pool bootstrap is connection-scoped and creates no flow/session
			// entry. The authenticated QUIC connection may now carry only
			// capability-gated fast operations on later streams.
			if auth == nil || hello.LaneID != 0 || s.quicCapabilities&session.CapabilityFastLaneJoin == 0 {
				return
			}
			_ = conn.SetDeadline(time.Time{})
			return
		}
		if hello.Kind == session.HelloJoin {
			s.handleLaneJoin(ctx, conn, fc, hello)
			return
		}
		if hello.Kind != session.HelloNew || hello.LaneID != 0 {
			return
		}
		sessionID = hello.SessionID
		laneID = hello.LaneID
		if auth != nil {
			auth.authenticated.Store(true)
		}
		var err error
		open, err = fc.Read()
		if err != nil {
			return
		}
		openFinished = time.Now()
	}
	if authFinished.IsZero() {
		authFinished = time.Now()
	}
	if openFinished.IsZero() {
		openFinished = authFinished
	}
	expectedOpenType := protocol.TypeOpen
	if fastOpen {
		expectedOpenType = protocol.TypeOpenFast
	}
	if open.Header.Type != expectedOpenType || open.Header.SessionID != sessionID || open.Header.FlowID == 0 || open.Header.Sequence != 0 {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassNew}, Payload: session.ResetPayload(session.ResetProtocol, "invalid flow open")})
		return
	}
	if session.IsUDPAssociation(open.Payload) {
		s.handleUDPAssociation(ctx, conn, fc, sessionID, open.Header.FlowID)
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
	s.cfg.Logger.Debug("remote flow opened", "transport", transportKindForConn(conn), "authentication_duration", authFinished.Sub(sessionStarted), "open_duration", openFinished.Sub(authFinished), "destination_dial_duration", time.Since(destinationDialStarted), "total_duration", time.Since(sessionStarted))
	defer destinationConn.Close()
	flow := newMultipathFlow(ctx, destinationConn, sessionID, open.Header.FlowID, s.cfg.ChunkSize, protocol.FlagAckDown, protocol.FlagAckUp, s.budget, s.metrics, s.cfg.Logger)
	flow.replayBudget = s.replayBudget
	flow.idleTimeout = s.cfg.FlowIdleTimeout
	flow.maxLifetime = s.cfg.FlowMaxLifetime
	flow.reserveControlLane = open.Header.Flags&protocol.FlagReserveControl != 0
	serverSession := &serverFlow{flow: flow, maxLanes: serverLaneBudget(s.cfg.MaxLanes, flow.reserveControlLane)}
	if err := serverSession.addLane(&mpLane{id: laneID, kind: transportKindForConn(conn), fc: fc, writeHook: s.cfg.testLaneWriteHook}); err != nil {
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
	defer func() {
		if registered {
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
	if err != nil && !errors.Is(err, context.Canceled) {
		s.cfg.Logger.Debug("remote flow ended with error", "error", err, "bytes_from_client", stats.BytesRead, "bytes_to_client", stats.BytesSent, "lane_bytes", stats.LaneBytes)
		return
	}
	s.cfg.Logger.Info("remote flow complete", "bytes_from_client", stats.BytesRead, "bytes_to_client", stats.BytesSent, "duration", stats.Ended.Sub(stats.Started), "lane_bytes", stats.LaneBytes, "send_stalls", stats.SendStalls, "send_stalled", stats.SendStalled)
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
		time.AfterFunc(completedSessionLinger, func() { s.unregisterSession(sessionID, serverSession) })
	})
}

func (s *Server) handleLaneJoin(ctx context.Context, conn streamConn, fc *frameConn, hello session.Hello) {
	if hello.LaneID == 0 {
		return
	}
	open, err := fc.Read()
	if err != nil {
		return
	}
	if open.Header.Type != protocol.TypeOpen || open.Header.SessionID != hello.SessionID || open.Header.FlowID == 0 || open.Header.Sequence != 0 || open.Header.Flags != 0 || len(open.Payload) != 0 {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: hello.SessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid lane join")})
		return
	}
	s.handleLaneJoinOpen(ctx, conn, fc, hello.SessionID, hello.LaneID, open)
}

// handleLaneJoinOpen is shared by the legacy HELLO_JOIN + OPEN exchange and
// the capability-gated one-frame fast join. Authentication and exact wire
// validation have completed before entry; this function owns the common
// session lookup, lane admission, tombstone replay, and lifetime handling.
func (s *Server) handleLaneJoinOpen(ctx context.Context, conn streamConn, fc *frameConn, sessionID [16]byte, laneID uint64, open protocol.Frame) {
	if session.IsZeroSessionID(sessionID) || laneID == 0 || open.Header.FlowID == 0 {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "invalid lane join identity")})
		return
	}
	serverSession := s.lookupSession(sessionID)
	if serverSession == nil || serverSession.flow.flowID != open.Header.FlowID {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetProtocol, "unknown session")})
		return
	}
	_ = conn.SetDeadline(time.Time{})
	if serverSession.completed.Load() {
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
	if err := serverSession.addLane(&mpLane{id: laneID, kind: transportKindForConn(conn), fc: fc, writeHook: s.cfg.testLaneWriteHook}); err != nil {
		_ = fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeReset, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}, Payload: session.ResetPayload(session.ResetFlowLimit, "lane unavailable")})
		return
	}
	s.observeLanes(serverSession.flow.laneCount())
	if err := fc.Write(protocol.Frame{Header: protocol.Header{Version: protocol.Version, Type: protocol.TypeOpenOK, SessionID: sessionID, FlowID: open.Header.FlowID, Class: protocol.ClassBulk}}); err != nil {
		return
	}
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
