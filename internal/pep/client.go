package pep

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/apernet/quic-go"
	"github.com/icourses-dev/wanopt/internal/classifier"
	wancongestion "github.com/icourses-dev/wanopt/internal/congestion"
	"github.com/icourses-dev/wanopt/internal/limiter"
	"github.com/icourses-dev/wanopt/internal/metrics"
	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/scheduler"
	"github.com/icourses-dev/wanopt/internal/session"
	"github.com/icourses-dev/wanopt/internal/socks5"
)

// A peer that accepts a replacement stream and immediately closes it must not
// cause an endless replacement storm while the application waits for a final
// FIN. Recovery is deliberately finite; the logical flow then fails closed
// and the caller can retry.
const (
	maxLaneRecoveryAttempts = 8
	laneRecoveryResetAfter  = 5 * time.Minute
)

type ClientConfig struct {
	ListenAddr       string
	RemoteAddr       string
	ServerName       string
	LocalAddress     string
	Secret           []byte
	RootCAs          *x509.CertPool
	MaxPayload       uint32
	ChunkSize        int
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	FlowIdleTimeout  time.Duration
	FlowMaxLifetime  time.Duration
	MaxSessions      int
	Transport        TransportKind
	// EnableQUICPool enables a persistent multiplexed QUIC connection for
	// initial/control streams. It is opt-in because bulk performance on a
	// path-specific Reno peer can be worse than independent QUIC lanes; the
	// scheduler still opens independent lanes for measured bulk traffic.
	EnableQUICPool                bool
	Congestion                    CongestionControlKind
	BrutalBytesPerSec             uint64
	AdaptiveMinBytesSec           uint64
	AdaptiveMaxBytesSec           uint64
	AggregateBytesPerSec          uint64
	InteractiveReserveBytesPerSec uint64
	Metrics                       *metrics.Registry
	FallbackDelay                 time.Duration
	UDPFailureThreshold           int
	UDPCooldown                   time.Duration
	InitialLanes                  int
	MaxLanes                      int
	BulkStartLanes                int
	MinimumMarginalGain           float64
	Logger                        *slog.Logger
}

type Client struct {
	cfg       ClientConfig
	udpHealth *udpHealth
	budget    *limiter.Budget
	metrics   *metrics.Registry

	// One QUIC connection can carry many independent PEP streams. This is
	// intentionally a single bounded pool: it gives concurrent flows a shared
	// congestion controller (the TUIC property) while preserving the PEP
	// session/framing isolation on each stream. A dead connection is discarded
	// before the next stream is opened and is recreated on demand.
	quicMu         sync.Mutex
	quicConn       *quic.Conn
	quicPacket     net.PacketConn
	quicController wancongestion.TelemetryProvider
	// quicPoolFast is learned from the first stream's HelloOK. The session ID
	// remains per logical flow; only the TLS/QUIC connection authentication is
	// shared. A zero capability keeps compatibility with an older server.
	quicPoolFast          bool
	quicPoolAuthenticated bool
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.ListenAddr == "" || cfg.RemoteAddr == "" || cfg.ServerName == "" {
		return nil, errors.New("client listen, remote, and TLS server name are required")
	}
	if err := validateLocalAddressSpec(cfg.LocalAddress); err != nil {
		return nil, err
	}
	if len(cfg.Secret) < 16 {
		return nil, errors.New("client secret must contain at least 16 bytes")
	}
	if cfg.MaxPayload == 0 || cfg.MaxPayload > protocol.DefaultMaxPayload {
		cfg.MaxPayload = 256 * 1024
	}
	if cfg.ChunkSize <= 0 || cfg.ChunkSize > int(cfg.MaxPayload) {
		cfg.ChunkSize = defaultChunkSize
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
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
		cfg.MaxSessions = 1024
	}
	if cfg.MaxSessions > maxConfiguredSessions {
		return nil, fmt.Errorf("maximum sessions must not exceed %d", maxConfiguredSessions)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	if cfg.Transport == "" {
		cfg.Transport = TransportAuto
	}
	if cfg.Transport != TransportAuto && cfg.Transport != TransportQUIC && cfg.Transport != TransportTCP {
		return nil, fmt.Errorf("unsupported client transport %q", cfg.Transport)
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
	if cfg.FallbackDelay <= 0 {
		cfg.FallbackDelay = 300 * time.Millisecond
	}
	if cfg.InitialLanes <= 0 {
		cfg.InitialLanes = 1
	}
	if cfg.InitialLanes > 8 {
		return nil, errors.New("initial lane count must be between 1 and 8")
	}
	if cfg.MaxLanes <= 0 {
		// One lane is the unattended-safe default. Independent congestion
		// controllers can reduce goodput or amplify loss; operators may raise
		// this only after a path-specific probe campaign proves a benefit.
		cfg.MaxLanes = 1
	}
	if cfg.MaxLanes > 8 {
		return nil, errors.New("maximum lane count must not exceed 8")
	}
	if cfg.InitialLanes > cfg.MaxLanes {
		return nil, errors.New("initial lane count cannot exceed maximum lane count")
	}
	if cfg.BulkStartLanes <= 0 {
		cfg.BulkStartLanes = 1
	}
	if cfg.BulkStartLanes > cfg.MaxLanes {
		cfg.BulkStartLanes = cfg.MaxLanes
	}
	if cfg.MinimumMarginalGain <= 0 {
		cfg.MinimumMarginalGain = 0.10
	}
	return &Client{
		cfg: cfg, udpHealth: newUDPHealth(cfg.UDPFailureThreshold, cfg.UDPCooldown),
		budget: limiter.New(limiter.Config{
			TotalBytesPerSec: cfg.AggregateBytesPerSec, ReserveBytesPerSec: cfg.InteractiveReserveBytesPerSec,
		}),
		metrics: cfg.Metrics,
	}, nil
}

func (c *Client) Serve(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	listener, err := lc.Listen(ctx, "tcp", c.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on local SOCKS5 address: %w", err)
	}
	return c.ServeListener(ctx, listener)
}

// Metrics exposes aggregate counters for an optional operator endpoint.
func (c *Client) Metrics() *metrics.Registry { return c.metrics }

// ServeListener is primarily useful for tests and service managers which
// provide an already-bound socket. The listener is closed when the context is
// cancelled or the method returns.
func (c *Client) ServeListener(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	defer c.closeQUICPool()

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, c.cfg.MaxSessions)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	c.cfg.Logger.Info("local SOCKS5 listener ready", "address", listener.Addr().String(), "remote", c.cfg.RemoteAddr)
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || errors.Is(acceptErr, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			var temporary interface{ Temporary() bool }
			if errors.As(acceptErr, &temporary) && temporary.Temporary() {
				continue
			}
			return fmt.Errorf("accept local connection: %w", acceptErr)
		}
		select {
		case semaphore <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-semaphore }()
				c.handleLocal(ctx, conn)
			}()
		default:
			_ = socks5.WriteReply(conn, socks5.ReplyGeneralFailure, nil)
			_ = conn.Close()
			c.cfg.Logger.Warn("local session limit reached")
		}
	}
}

// closeQUICPool is called when the local agent stops. It is safe to call more
// than once and closes the packet socket owned by a locally-bound QUIC dial.
func (c *Client) closeQUICPool() {
	c.quicMu.Lock()
	conn, packet := c.quicConn, c.quicPacket
	c.quicConn, c.quicPacket, c.quicController = nil, nil, nil
	c.quicPoolFast, c.quicPoolAuthenticated = false, false
	c.quicMu.Unlock()
	if conn != nil {
		_ = conn.CloseWithError(0, "wanopt client stopped")
	}
	if packet != nil {
		_ = packet.Close()
	}
}

func (c *Client) handleLocal(ctx context.Context, inner net.Conn) {
	defer inner.Close()
	_ = inner.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	req, err := socks5.ReadRequest(inner)
	if err != nil {
		c.cfg.Logger.Debug("SOCKS5 negotiation failed", "error", err)
		return
	}
	if req.Command == socks5.CommandUDPAssociate {
		c.handleUDPAssociate(ctx, inner)
		return
	}
	flowOpenStarted := time.Now()
	flow, err := c.openFlow(ctx, req.Destination)
	if err != nil {
		_ = socks5.WriteReply(inner, socks5.ReplyHostUnreachable, nil)
		c.cfg.Logger.Warn("remote flow open failed", "error", err)
		return
	}
	c.cfg.Logger.Debug("local flow opened", "transport", flow.kind, "duration", time.Since(flowOpenStarted))
	flowSession := newMultipathFlow(ctx, inner, flow.sessionID, flow.flowID, c.cfg.ChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, c.budget, c.metrics, c.cfg.Logger)
	flowSession.idleTimeout = c.cfg.FlowIdleTimeout
	flowSession.maxLifetime = c.cfg.FlowMaxLifetime
	if err := flowSession.addLane(&mpLane{id: flow.laneID, kind: flow.kind, fc: flow.fc}); err != nil {
		_ = flow.fc.Close()
		flowSession.closeAll()
		return
	}
	if c.cfg.InitialLanes > 1 {
		// An explicit initial-lane setting is a benchmark/operator choice. Wait
		// for those lanes before acknowledging SOCKS CONNECT so the caller gets
		// the requested topology deterministically. The default remains one
		// pre-warmed lane and does not pay this cost.
		c.openAdditionalLanes(ctx, flowSession, flow.sessionID, flow.flowID, flow.kind)
	}
	if err := socks5.WriteReply(inner, socks5.ReplySucceeded, nil); err != nil {
		flowSession.closeAll()
		return
	}
	_ = inner.SetDeadline(time.Time{})
	go c.manageLanes(ctx, flowSession, flow.sessionID, flow.flowID, flow.kind)
	c.metrics.FlowStarted()
	stats, err := flowSession.run(ctx)
	// A peer may close the last outer lane immediately after the application
	// bytes and FIN exchange complete. Both direction flags are the same
	// correctness proof used by the server tombstone path; classify a late
	// socket EOF as a completed logical flow rather than a transport failure.
	flowComplete := err == nil || (ctx.Err() == nil && flowSession.finSent.Load() && flowSession.remoteFinSeen.Load())
	c.metrics.FlowFinished(stats.BytesSent, stats.BytesRead, !flowComplete && err != nil && !errors.Is(err, context.Canceled))
	if !flowComplete && err != nil && !errors.Is(err, context.Canceled) {
		c.cfg.Logger.Debug("local flow ended with error", "error", err, "bytes_up", stats.BytesSent, "bytes_down", stats.BytesRead, "lane_bytes", stats.LaneBytes)
		return
	}
	c.cfg.Logger.Info("local flow complete", "bytes_up", stats.BytesSent, "bytes_down", stats.BytesRead, "duration", stats.Ended.Sub(stats.Started), "lane_bytes", stats.LaneBytes)
}

type openedFlow struct {
	fc        *frameConn
	outer     streamConn
	sessionID [16]byte
	flowID    uint64
	laneID    uint64
	kind      TransportKind
}

type authenticatedLane struct {
	fc        *frameConn
	outer     streamConn
	sessionID [16]byte
	kind      TransportKind
	laneID    uint64
	fastOpen  bool
}

func (c *Client) openFlow(ctx context.Context, destination string) (*openedFlow, error) {
	return c.openFlowMode(ctx, destination, false)
}

func (c *Client) openFlowMode(ctx context.Context, destination string, fastRetry bool) (*openedFlow, error) {
	payload, err := session.EncodeDestination(destination)
	if err != nil {
		return nil, err
	}
	lane, err := c.chooseAuthenticatedLane(ctx)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*openedFlow, error) {
		_ = lane.fc.Close()
		return nil, err
	}
	_ = lane.outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	flowID, err := randomFlowID()
	if err != nil {
		return fail(err)
	}
	openType := protocol.TypeOpen
	if lane.fastOpen {
		openType = protocol.TypeOpenFast
	}
	if err := lane.fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: openType, SessionID: lane.sessionID, FlowID: flowID, Class: protocol.ClassNew},
		Payload: payload,
	}); err != nil {
		return fail(fmt.Errorf("send flow open: %w", err))
	}
	response, err := lane.fc.Read()
	if err != nil {
		return fail(fmt.Errorf("read flow open acknowledgement: %w", err))
	}
	if response.Header.SessionID != lane.sessionID || response.Header.FlowID != flowID {
		return fail(errors.New("flow open acknowledgement identity mismatch"))
	}
	if response.Header.Type == protocol.TypeReset {
		if !fastRetry && lane.fastOpen && resetCode(response.Payload) == session.ResetProtocol {
			c.disableQUICPoolFast()
			_ = lane.fc.Close()
			return c.openFlowMode(ctx, destination, true)
		}
		return fail(errors.New("remote destination unavailable"))
	}
	if response.Header.Type != protocol.TypeOpenOK || len(response.Payload) != 0 {
		return fail(errors.New("invalid flow open acknowledgement"))
	}
	_ = lane.outer.SetDeadline(time.Time{})
	return &openedFlow{fc: lane.fc, outer: lane.outer, sessionID: lane.sessionID, flowID: flowID, laneID: lane.laneID, kind: lane.kind}, nil
}

func resetCode(payload []byte) session.ResetCode {
	if len(payload) == 0 {
		return 0
	}
	return session.ResetCode(payload[0])
}

func (c *Client) disableQUICPoolFast() {
	c.quicMu.Lock()
	c.quicPoolFast = false
	c.quicMu.Unlock()
}

func (c *Client) dialAuthenticatedLane(ctx context.Context, kind TransportKind) (*authenticatedLane, error) {
	sessionID, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	return c.dialLane(ctx, kind, sessionID, 0, session.HelloNew)
}

func (c *Client) dialJoinLane(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64) (*authenticatedLane, error) {
	return c.dialLaneMode(ctx, kind, sessionID, laneID, session.HelloJoin, false)
}

func (c *Client) dialLane(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64, helloKind session.HelloKind) (*authenticatedLane, error) {
	return c.dialLaneMode(ctx, kind, sessionID, laneID, helloKind, c.cfg.EnableQUICPool)
}

// dialLaneMode uses the shared QUIC stream pool only for a flow's initial
// control stream. Additional lanes are independent QUIC connections: they
// provide true bulk capacity and independent loss paths, while the pooled
// control stream remains available for short/interactive traffic.
func (c *Client) dialLaneMode(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64, helloKind session.HelloKind, pooled bool) (*authenticatedLane, error) {
	dialStarted := time.Now()
	var outer streamConn
	var err error
	fastOpen := false
	alreadyAuthenticated := false
	switch kind {
	case TransportTCP:
		outer, err = dialTCP(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress)
	case TransportQUIC:
		ccfg := congestionConfig{
			kind: c.cfg.Congestion, brutalBytesPerSecond: c.cfg.BrutalBytesPerSec,
			adaptiveMinBytesPerSec: c.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: c.cfg.AdaptiveMaxBytesSec,
		}
		if pooled {
			outer, fastOpen, alreadyAuthenticated, err = c.dialPooledQUICLane(ctx, ccfg, sessionID)
		} else {
			outer, err = dialQUIC(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress, ccfg)
		}
	default:
		return nil, fmt.Errorf("cannot dial transport %q", kind)
	}
	if err != nil {
		return nil, transportError(kind, err)
	}
	outerReady := time.Now()
	fail := func(err error) (*authenticatedLane, error) {
		_ = outer.Close()
		return nil, transportError(kind, err)
	}
	_ = outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	fc := newFrameConn(outer, c.cfg.MaxPayload)
	if !alreadyAuthenticated {
		if err := clientAuthenticateKind(fc, c.cfg.Secret, sessionID, laneID, helloKind, time.Now()); err != nil {
			return fail(err)
		}
	}
	_ = outer.SetDeadline(time.Time{})
	c.cfg.Logger.Debug("outer lane authenticated", "transport", kind, "dial_duration", outerReady.Sub(dialStarted), "authentication_duration", time.Since(outerReady), "pooled", pooled, "fast_open", fastOpen)
	return &authenticatedLane{fc: fc, outer: outer, sessionID: sessionID, kind: kind, laneID: laneID, fastOpen: fastOpen}, nil
}

// dialPooledQUICLane opens a stream on the client's shared QUIC connection.
// The mutex covers connection creation and stream-limit admission, so two
// simultaneous first flows cannot create competing pools. A stream-open
// failure caused by a dead connection clears the pool and lets the caller's
// normal AUTO fallback/retry policy establish a fresh transport.
func (c *Client) dialPooledQUICLane(ctx context.Context, ccfg congestionConfig, sessionID [16]byte) (streamConn, bool, bool, error) {
	dialCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.DialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, c.cfg.DialTimeout)
		defer cancel()
	}
	c.quicMu.Lock()
	defer c.quicMu.Unlock()

	if c.quicConn == nil || c.quicConn.Context().Err() != nil {
		if c.quicConn != nil {
			_ = c.quicConn.CloseWithError(0, "wanopt stale pooled connection")
		}
		if c.quicPacket != nil {
			_ = c.quicPacket.Close()
		}
		c.quicPoolFast, c.quicPoolAuthenticated = false, false
		conn, packet, err := dialQUICConnection(dialCtx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress)
		if err != nil {
			c.quicConn, c.quicPacket, c.quicController = nil, nil, nil
			c.quicPoolFast, c.quicPoolAuthenticated = false, false
			return nil, false, false, err
		}
		controller := configureQUICController(conn, ccfg)
		c.quicConn, c.quicPacket, c.quicController = conn, packet, controller
	}
	stream, err := c.quicConn.OpenStreamSync(dialCtx)
	if err != nil {
		if c.quicConn.Context().Err() != nil {
			_ = c.quicConn.CloseWithError(0, "wanopt pooled connection failed")
			if c.quicPacket != nil {
				_ = c.quicPacket.Close()
			}
			c.quicConn, c.quicPacket, c.quicController = nil, nil, nil
			c.quicPoolFast, c.quicPoolAuthenticated = false, false
		}
		return nil, false, false, err
	}
	outer := &quicStreamConn{stream: stream, conn: c.quicConn, controller: c.quicController, closeConn: false}
	// Authenticate the first stream while holding the pool mutex. This makes
	// connection-level authentication atomic: a second stream cannot race a
	// not-yet-authenticated connection. Subsequent streams on a capable server
	// skip Hello and begin with TypeOpenFast.
	if !c.quicPoolAuthenticated && c.quicConn.Context().Err() == nil {
		_ = outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
		ok, authErr := clientAuthenticateKindResult(newFrameConn(outer, c.cfg.MaxPayload), c.cfg.Secret, sessionID, 0, session.HelloNew, time.Now())
		if authErr != nil {
			_ = outer.Close()
			if c.quicConn.Context().Err() != nil {
				c.quicConn, c.quicPacket, c.quicController, c.quicPoolFast = nil, nil, nil, false
			}
			return nil, false, false, authErr
		}
		c.quicPoolFast = ok.Capabilities&session.CapabilityFastStreams != 0
		c.quicPoolAuthenticated = true
		_ = outer.SetDeadline(time.Time{})
		return outer, false, true, nil
	}
	// A capable peer has authenticated the QUIC connection, so this stream
	// must skip the per-stream Hello and start with OPEN_FAST. An older peer
	// advertises no capability and deliberately keeps the legacy Hello path.
	return outer, c.quicPoolFast, c.quicPoolFast, nil
}

func (c *Client) openJoinLane(ctx context.Context, kind TransportKind, sessionID [16]byte, flowID, laneID uint64) (*mpLane, error) {
	if kind != TransportQUIC && kind != TransportTCP {
		return nil, fmt.Errorf("unsupported join transport %q", kind)
	}
	lane, err := c.dialJoinLane(ctx, kind, sessionID, laneID)
	if err != nil {
		return nil, err
	}
	if err := lane.fc.Write(protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeOpen, SessionID: sessionID, FlowID: flowID, Class: protocol.ClassBulk,
	}}); err != nil {
		_ = lane.fc.Close()
		return nil, err
	}
	response, err := lane.fc.Read()
	if err != nil {
		_ = lane.fc.Close()
		return nil, err
	}
	if response.Header.Type == protocol.TypeReset && response.Header.SessionID == sessionID && response.Header.FlowID == flowID {
		_ = lane.fc.Close()
		if len(response.Payload) > 1 {
			return nil, fmt.Errorf("lane join rejected: %s", string(response.Payload[1:]))
		}
		return nil, errors.New("lane join rejected")
	}
	if response.Header.Type != protocol.TypeOpenOK || response.Header.SessionID != sessionID || response.Header.FlowID != flowID || len(response.Payload) != 0 {
		_ = lane.fc.Close()
		return nil, errors.New("invalid lane join acknowledgement")
	}
	return &mpLane{id: laneID, kind: kind, fc: lane.fc}, nil
}

func (c *Client) openAdditionalLanes(ctx context.Context, flow *multipathFlow, sessionID [16]byte, flowID uint64, initialKind TransportKind) {
	if initialKind != TransportQUIC {
		return
	}
	for flow.laneCount() < c.cfg.InitialLanes {
		if ctx.Err() != nil {
			return
		}
		laneID, err := flow.allocateJoinID()
		if err != nil {
			return
		}
		lane, err := c.openJoinLane(ctx, TransportQUIC, sessionID, flowID, laneID)
		if err != nil {
			// A flow can finish while a speculative join is in the handshake.
			// That is an expected shutdown race, not evidence that the UDP path
			// is unhealthy; feeding it into the global cooldown causes unrelated
			// new flows to fall back to TCP unnecessarily.
			if ctx.Err() != nil || flow.doneChanClosed() {
				return
			}
			c.udpHealth.failure(time.Now())
			c.cfg.Logger.Warn("additional lane unavailable", "lane", laneID, "error", err)
			// Keep the already-authenticated lane usable. Retrying the same
			// join synchronously can delay SOCKS CONNECT indefinitely when UDP
			// is filtered or the path is degraded; the adaptive manager may
			// attempt replacement later under its normal health policy.
			return
		}
		if err := flow.addLane(lane); err != nil {
			_ = lane.fc.Close()
			return
		}
	}
}

func (c *Client) manageLanes(ctx context.Context, flow *multipathFlow, sessionID [16]byte, flowID uint64, initialKind TransportKind) {
	if initialKind != TransportQUIC {
		return
	}
	planner := scheduler.New(scheduler.Config{
		MaxLanes: c.cfg.MaxLanes, InteractiveLanes: 1, BulkStartLanes: c.cfg.BulkStartLanes,
		MinimumMarginalGain: c.cfg.MinimumMarginalGain, InteractiveRTTBudget: 40 * time.Millisecond,
	})
	manageCtx, manageCancel := context.WithCancel(ctx)
	defer manageCancel()
	go func() {
		select {
		case <-flow.doneChan():
			manageCancel()
		case <-manageCtx.Done():
		}
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var previous float64
	var lastDecision time.Time
	var recoveryBackoff time.Duration
	var nextRecovery time.Time
	recoveryAttempts := 0
	var lastRecoveryAttempt time.Time
	for {
		select {
		case <-flow.doneChan():
			return
		case <-manageCtx.Done():
			return
		case <-ticker.C:
			// The remote completion watcher can close its lanes just before
			// this scheduler tick. Both FIN directions are already known at
			// that point, so joining a tombstoned/unknown session would only
			// create noisy warnings and transient UDP-health penalties.
			if flow.finSent.Load() && flow.remoteFinSeen.Load() {
				flow.closeAll()
				return
			}
			snapshot := flow.snapshot()
			if snapshot.HealthyLanes == 0 {
				if flow.doneChanClosed() || recoveryAttempts >= maxLaneRecoveryAttempts {
					return
				}
				now := time.Now()
				if !nextRecovery.IsZero() && now.Before(nextRecovery) {
					continue
				}
				recoveryAttempts++
				lastRecoveryAttempt = now
				if err := c.openRecoveryLane(manageCtx, flow, sessionID, flowID); err != nil {
					if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
						c.cfg.Logger.Warn("lane recovery unavailable", "error", err)
					}
					if recoveryBackoff == 0 {
						recoveryBackoff = time.Second
					} else if recoveryBackoff < 15*time.Second {
						recoveryBackoff *= 2
						if recoveryBackoff > 15*time.Second {
							recoveryBackoff = 15 * time.Second
						}
					}
					nextRecovery = now.Add(recoveryBackoff)
				} else {
					// A replacement that succeeds its handshake can still fail
					// immediately. Keep a bounded exponential delay between all
					// attempts, not only failed handshakes.
					if recoveryBackoff == 0 {
						recoveryBackoff = time.Second
					}
					nextRecovery = now.Add(recoveryBackoff)
					if recoveryBackoff < 15*time.Second {
						recoveryBackoff *= 2
						if recoveryBackoff > 15*time.Second {
							recoveryBackoff = 15 * time.Second
						}
					}
				}
				continue
			}
			// A replacement that survives one 500-ms scheduler tick is not yet
			// stable. Reset the lifetime budget only after a sustained healthy
			// dwell; otherwise accept-then-close peers can bypass the cap by
			// keeping each replacement alive very briefly.
			if recoveryAttempts > 0 && !lastRecoveryAttempt.IsZero() && time.Since(lastRecoveryAttempt) >= laneRecoveryResetAfter {
				recoveryAttempts = 0
				recoveryBackoff = 0
				nextRecovery = time.Time{}
				lastRecoveryAttempt = time.Time{}
			}
			// Once a TCP rescue lane is installed, keep the session on that
			// reliable lane. TCP/QUIC striping compounds head-of-line blocking
			// and would make the fallback less predictable.
			for _, lane := range flow.healthyLanes() {
				if lane.kind == TransportTCP {
					return
				}
			}
			if !lastDecision.IsZero() && time.Since(lastDecision) < 2*time.Second {
				continue
			}
			lastDecision = time.Now()
			if snapshot.Class != classifier.ClassBulk {
				continue
			}
			goodput := 0.0
			if snapshot.Elapsed > 0 {
				goodput = float64(snapshot.Bytes) / snapshot.Elapsed.Seconds()
			}
			gain := 0.0
			if previous > 0 {
				gain = (goodput - previous) / previous
			}
			previous = goodput
			decision := planner.Decide(snapshot.Class, scheduler.Metrics{
				CurrentLanes: snapshot.CurrentLanes, HealthyLanes: snapshot.HealthyLanes,
				AvailableLanes: c.cfg.MaxLanes, MarginalGain: gain,
				BaselineRTT: snapshot.BaselineRTT, CurrentRTT: snapshot.CurrentRTT,
				UDPHealthy: c.udpHealth.allow(time.Now()),
			})
			for flow.laneCount() > decision.TargetLanes && flow.laneCount() > 1 {
				if !flow.retireLeastProductiveLane() {
					break
				}
			}
			// Open at most one speculative lane per scheduler tick.  A flow can
			// finish while a join is in flight; if the peer has already started
			// tearing down the session, repeatedly filling the target in one tick
			// creates an unbounded stream of authenticated joins (and can leave
			// dozens of zero-byte lanes behind).  One bounded probe per tick keeps
			// growth observable, gives the completion watcher a chance to stop the
			// manager, and makes the configured lane cap a real resource bound.
			if flow.laneCount() < decision.TargetLanes && !flow.doneChanClosed() &&
				!(flow.finSent.Load() && flow.remoteFinSeen.Load()) {
				laneID, err := flow.allocateJoinID()
				if err != nil {
					return
				}
				lane, err := c.openJoinLane(manageCtx, TransportQUIC, sessionID, flowID, laneID)
				if err != nil {
					if manageCtx.Err() != nil || flow.doneChanClosed() {
						return
					}
					c.udpHealth.failure(time.Now())
					c.cfg.Logger.Warn("adaptive lane unavailable", "lane", laneID, "error", err)
				} else if err := flow.addLane(lane); err != nil {
					_ = lane.fc.Close()
					return
				}
			}
		}
	}
}

func (c *Client) openRecoveryLane(ctx context.Context, flow *multipathFlow, sessionID [16]byte, flowID uint64) error {
	// A replacement handshake must not outlive its logical flow. Without this
	// bound, a dead UDP flow can keep dialing a session that the server has
	// already unregistered after the application completed.
	recoveryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if flow.finSent.Load() && flow.remoteFinSeen.Load() {
		return context.Canceled
	}
	go func() {
		select {
		case <-flow.doneChan():
			cancel()
		case <-recoveryCtx.Done():
		}
	}()
	laneID, err := flow.allocateJoinID()
	if err != nil {
		return err
	}
	kind := TransportQUIC
	if c.cfg.Transport == TransportAuto {
		// Losing every QUIC lane is a stronger signal than a failed probe.
		// Install TCP immediately so recovery fits inside the bounded grace
		// period; later new flows may probe UDP again through the health race.
		kind = TransportTCP
		c.udpHealth.failure(time.Now())
	}
	lane, err := c.openJoinLane(recoveryCtx, kind, sessionID, flowID, laneID)
	if err != nil {
		if recoveryCtx.Err() != nil || flow.doneChanClosed() {
			return context.Canceled
		}
		return err
	}
	if kind == TransportQUIC {
		c.udpHealth.success()
	}
	if err := flow.addLane(lane); err != nil {
		_ = lane.fc.Close()
		return err
	}
	c.metrics.LaneReplacement()
	return nil
}

func (c *Client) chooseAuthenticatedLane(ctx context.Context) (*authenticatedLane, error) {
	switch c.cfg.Transport {
	case TransportTCP:
		return c.dialAuthenticatedLane(ctx, TransportTCP)
	case TransportQUIC:
		lane, err := c.dialAuthenticatedLane(ctx, TransportQUIC)
		if err != nil {
			c.udpHealth.failure(time.Now())
			return nil, err
		}
		c.udpHealth.success()
		return lane, nil
	case TransportAuto:
		if !c.udpHealth.allow(time.Now()) {
			c.metrics.Fallback()
			return c.dialAuthenticatedLane(ctx, TransportTCP)
		}
		return c.raceUDPAndTCP(ctx)
	default:
		return nil, fmt.Errorf("unsupported transport %q", c.cfg.Transport)
	}
}

type laneResult struct {
	lane *authenticatedLane
	err  error
}

func (c *Client) raceUDPAndTCP(ctx context.Context) (*authenticatedLane, error) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	quicResult := make(chan laneResult, 1)
	go func() {
		lane, err := c.dialAuthenticatedLane(raceCtx, TransportQUIC)
		quicResult <- laneResult{lane: lane, err: err}
	}()
	timer := time.NewTimer(c.cfg.FallbackDelay)
	defer timer.Stop()
	select {
	case result := <-quicResult:
		if result.err == nil {
			c.udpHealth.success()
			return result.lane, nil
		}
		c.udpHealth.failure(time.Now())
		c.metrics.Fallback()
		return c.dialAuthenticatedLane(ctx, TransportTCP)
	case <-timer.C:
	case <-ctx.Done():
		closeLateLane(quicResult)
		return nil, ctx.Err()
	}

	tcpResult := make(chan laneResult, 1)
	go func() {
		lane, err := c.dialAuthenticatedLane(raceCtx, TransportTCP)
		tcpResult <- laneResult{lane: lane, err: err}
	}()
	var quicErr, tcpErr error
	for quicResult != nil || tcpResult != nil {
		select {
		case result := <-quicResult:
			quicResult = nil
			if result.err == nil {
				c.udpHealth.success()
				cancel()
				closeLateLane(tcpResult)
				return result.lane, nil
			}
			quicErr = result.err
			c.udpHealth.failure(time.Now())
		case result := <-tcpResult:
			tcpResult = nil
			if result.err == nil {
				c.metrics.Fallback()
				cancel()
				closeLateLane(quicResult)
				return result.lane, nil
			}
			tcpErr = result.err
		case <-ctx.Done():
			closeLateLane(quicResult)
			closeLateLane(tcpResult)
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("QUIC failed (%v); TCP fallback failed (%v)", quicErr, tcpErr)
}

func closeLateLane(ch <-chan laneResult) {
	if ch == nil {
		return
	}
	go func() {
		result := <-ch
		if result.lane != nil {
			_ = result.lane.fc.Close()
		}
	}()
}
