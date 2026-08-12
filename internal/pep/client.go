package pep

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
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
	// A rejected speculative lane must not be reopened on every scheduler tick.
	// The backoff is intentionally shorter than a normal bulk request timeout,
	// but long enough to collect a fresh throughput/RTT sample on the surviving
	// control lane before trying another independent QUIC path.
	minLaneProbeBackoff  = 10 * time.Second
	maxLaneProbeBackoff  = 60 * time.Second
	maxLaneProbeAttempts = 4
	// laneDecisionInterval spaces the lane probe's samples. It bounds how fast
	// the search can converge: a baseline costs two decisions and judging a
	// probed lane costs three more, so a flow reaches its second bulk lane
	// about five intervals after it is classified as bulk. Shorter intervals
	// make each goodput sample noisier -- at a 200ms RTT, 500ms is roughly two
	// round trips of evidence -- and the probe's bias against striping is what
	// keeps that noise from producing false positives.
	laneDecisionInterval = 500 * time.Millisecond
	// Start the authenticated join slightly before the classifier's final bulk
	// transition. The lane is attached but carries no NEW/interactive DATA, so
	// this only overlaps its lossy QUIC handshake with the tail of classification.
	bulkLanePrewarmBytes = 64 * 1024
	bulkLanePrewarmAge   = 500 * time.Millisecond
	bulkLaneAsymmetry    = 8
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
	EnableQUICPool bool
	// OptimisticOpen returns SOCKS success after the authenticated OPEN has
	// been queued, without waiting for OPEN_OK. The flow reader validates the
	// eventual OPEN_OK (or propagates a typed RESET), allowing application
	// request bytes to overlap the destination dial and one WAN RTT. It is
	// opt-in until deployment campaigns prove acceptable destination-error
	// behavior for all callers.
	OptimisticOpen                bool
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
	// ReplayMemoryBytes bounds the total memory all local flows may hold in
	// their replay windows. Zero selects the package default.
	ReplayMemoryBytes   uint64
	Metrics             *metrics.Registry
	FallbackDelay       time.Duration
	UDPFailureThreshold int
	UDPCooldown         time.Duration
	InitialLanes        int
	MaxLanes            int
	BulkStartLanes      int
	MinimumMarginalGain float64
	Logger              *slog.Logger
}

type Client struct {
	cfg       ClientConfig
	udpHealth *udpHealth
	budget    *limiter.Budget
	metrics   *metrics.Registry
	// replayBudget bounds the memory all local flows may hold in their
	// replay windows.
	replayBudget *replayBudget

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
	quicPoolControl       bool
	quicPoolAuthenticated bool
	// quicPoolActive counts flows currently sharing the pooled control
	// connection. A bulk flow only needs to move off it when another flow
	// would otherwise queue behind its congestion window.
	quicPoolActive atomic.Int64
	// peerAckRanges records whether the server advertised that it can consume
	// range acknowledgements.
	peerAckRanges atomic.Bool

	// bulkMu protects a bounded set of pre-authenticated secondary QUIC
	// connections used only for fast lane joins. Keeping them separate from
	// the control pool preserves the control lane's congestion state while
	// avoiding a fresh QUIC handshake at the bulk-promotion boundary.
	//
	// Each connection carries at most one lane at a time. Multiplexing several
	// lanes of one flow onto a single connection would give them one 4-tuple
	// and one congestion controller, which is what a single TUIC connection
	// already provides: measured on a path that polices per source address,
	// striping over a shared connection produced no gain at all. A connection
	// is retained after its lane is released so a later flow still skips the
	// handshake.
	bulkMu                 sync.Mutex
	bulkConns              []*bulkConn
	bulkJoinUnavailableTil time.Time
}

// bulkConn is one pre-authenticated secondary QUIC connection reserved for
// bulk lane joins.
type bulkConn struct {
	conn       *quic.Conn
	packet     net.PacketConn
	controller wancongestion.TelemetryProvider
	busy       bool
	idleTimer  *time.Timer
}

func (b *bulkConn) close(reason string) {
	if b.idleTimer != nil {
		b.idleTimer.Stop()
		b.idleTimer = nil
	}
	if b.conn != nil {
		_ = b.conn.CloseWithError(0, reason)
	}
	if b.packet != nil {
		_ = b.packet.Close()
	}
}

const bulkPoolIdleTimeout = 30 * time.Second
const bulkJoinCapabilityRetry = 60 * time.Second

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
		// This is a ceiling on a measured search, not a target. A flow starts
		// on one bulk lane and reaches this bound only by demonstrating, one
		// controlled probe at a time, that each added lane raised its goodput;
		// a path that does not reward striping stops after a single probe and
		// stays at one lane. Four is where the measured benefit flattened on a
		// per-flow-policed path, and independent congestion controllers past
		// that point mostly add loss.
		cfg.MaxLanes = 4
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
		metrics:      cfg.Metrics,
		replayBudget: newReplayBudget(int64(cfg.ReplayMemoryBytes)),
	}, nil
}

func (c *Client) windows() flowWindows {
	return flowWindows{stream: c.cfg.StreamReceiveWindow, connection: c.cfg.ConnectionReceiveWindow}
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
	c.quicPoolFast, c.quicPoolControl, c.quicPoolAuthenticated = false, false, false
	c.quicMu.Unlock()
	if conn != nil {
		_ = conn.CloseWithError(0, "wanopt client stopped")
	}
	if packet != nil {
		_ = packet.Close()
	}
	c.bulkMu.Lock()
	bulkConns := c.bulkConns
	c.bulkConns, c.bulkJoinUnavailableTil = nil, time.Time{}
	c.bulkMu.Unlock()
	for _, entry := range bulkConns {
		entry.close("wanopt bulk pool stopped")
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
	flowSession.replayBudget = c.replayBudget
	flowSession.ackRanges.Store(c.peerAcceptsAckRanges())
	flowSession.idleTimeout = c.cfg.FlowIdleTimeout
	flowSession.maxLifetime = c.cfg.FlowMaxLifetime
	flowSession.openAckPending = flow.openPending
	flowSession.helloAckPending = flow.helloPending
	flowSession.onHelloOK = flow.onHelloOK
	flowSession.reserveControlLane = flow.reserveControl
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
	fc             *frameConn
	outer          streamConn
	sessionID      [16]byte
	flowID         uint64
	laneID         uint64
	kind           TransportKind
	openPending    bool
	reserveControl bool
	// helloPending marks a flow whose authenticated HELLO was pipelined with
	// OPEN and whose HELLO_OK has not been read yet. onHelloOK publishes the
	// negotiated capabilities once the flow reader observes it.
	helloPending bool
	onHelloOK    func(session.HelloOK)
}

type authenticatedLane struct {
	fc             *frameConn
	outer          streamConn
	sessionID      [16]byte
	kind           TransportKind
	laneID         uint64
	fastOpen       bool
	reserveControl bool
	// helloPending is true when this lane's HELLO was written without waiting
	// for HELLO_OK, so the flow reader owns the acknowledgement.
	helloPending bool
	onHelloOK    func(session.HelloOK)
}

func (c *Client) openFlow(ctx context.Context, destination string) (*openedFlow, error) {
	// A fresh, dedicated lane can pipeline HELLO and OPEN on the same stream.
	// The server still validates and acknowledges HELLO first, but it can read
	// OPEN from the already-buffered stream without making the client wait for
	// an extra China-US round trip. Pooled streams deliberately keep their
	// existing capability-negotiation path: the first pool stream must learn
	// HelloOK before later OPEN_FAST streams are admitted.
	if !c.cfg.EnableQUICPool {
		return c.openInitialFlow(ctx, destination)
	}
	return c.openFlowMode(ctx, destination, false)
}

// openInitialFlow establishes a new dedicated flow while writing HELLO and
// OPEN back-to-back. This is wire-compatible with the original server (which
// reads HELLO, writes HELLO_OK, then reads OPEN) and removes one sequential
// request/response exchange from every cold flow. AUTO preserves the normal
// UDP preference and races a delayed TCP candidate against the pipelined
// QUIC candidate.
func (c *Client) openInitialFlow(ctx context.Context, destination string) (*openedFlow, error) {
	payload, err := session.EncodeDestination(destination)
	if err != nil {
		return nil, err
	}
	if c.cfg.Transport == TransportTCP {
		return c.dialPipelinedFlow(ctx, TransportTCP, payload)
	}
	if c.cfg.Transport == TransportQUIC {
		flow, err := c.dialPipelinedFlow(ctx, TransportQUIC, payload)
		if err != nil {
			c.udpHealth.failure(time.Now())
			return nil, err
		}
		c.udpHealth.success()
		return flow, nil
	}
	if c.cfg.Transport != TransportAuto {
		return nil, fmt.Errorf("unsupported transport %q", c.cfg.Transport)
	}
	if !c.udpHealth.allow(time.Now()) {
		c.metrics.Fallback()
		return c.dialPipelinedFlow(ctx, TransportTCP, payload)
	}
	return c.racePipelinedFlow(ctx, payload)
}

func (c *Client) dialPipelinedFlow(ctx context.Context, kind TransportKind, payload []byte) (*openedFlow, error) {
	sessionID, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	flowID, err := randomFlowID()
	if err != nil {
		return nil, err
	}
	lane, err := c.dialLaneMode(ctx, kind, sessionID, 0, session.HelloNew, false, true)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*openedFlow, error) {
		_ = lane.fc.Close()
		return nil, err
	}
	_ = lane.outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	if err := lane.fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeOpen, SessionID: sessionID, FlowID: flowID, Class: protocol.ClassNew},
		Payload: payload,
	}); err != nil {
		return fail(fmt.Errorf("send pipelined flow open: %w", err))
	}
	if c.cfg.OptimisticOpen {
		// The server emits HELLO_OK before OPEN_OK. Neither acknowledgement
		// gates the application's first request bytes, so reading HELLO_OK
		// here would spend one full WAN round trip confirming something the
		// flow reader validates anyway. A rejected HELLO still fails the flow.
		_ = lane.outer.SetDeadline(time.Time{})
		return &openedFlow{
			fc: lane.fc, outer: lane.outer, sessionID: sessionID, flowID: flowID,
			laneID: lane.laneID, kind: lane.kind, openPending: true, helloPending: true,
		}, nil
	}
	// The server emits HELLO_OK before OPEN_OK, even though both client
	// requests were sent without waiting between them.
	helloAck, err := lane.fc.Read()
	if err != nil {
		return fail(fmt.Errorf("read pipelined hello acknowledgement: %w", err))
	}
	if helloAck.Header.Type == protocol.TypeReset {
		return fail(errors.New("server rejected session authentication"))
	}
	if helloAck.Header.Type != protocol.TypeHelloOK || helloAck.Header.SessionID != sessionID || helloAck.Header.FlowID != 0 {
		return fail(errors.New("invalid pipelined session acknowledgement"))
	}
	var helloOK session.HelloOK
	if err := helloOK.UnmarshalBinary(helloAck.Payload); err != nil {
		return fail(fmt.Errorf("decode pipelined session acknowledgement: %w", err))
	}
	c.peerAckRanges.Store(helloOK.Capabilities&session.CapabilityAckRanges != 0)
	openAck, err := lane.fc.Read()
	if err != nil {
		return fail(fmt.Errorf("read pipelined flow acknowledgement: %w", err))
	}
	if openAck.Header.SessionID != sessionID || openAck.Header.FlowID != flowID {
		return fail(errors.New("pipelined flow acknowledgement identity mismatch"))
	}
	if openAck.Header.Type == protocol.TypeReset {
		return fail(errors.New("remote destination unavailable"))
	}
	if openAck.Header.Type != protocol.TypeOpenOK || len(openAck.Payload) != 0 {
		return fail(errors.New("invalid pipelined flow acknowledgement"))
	}
	_ = lane.outer.SetDeadline(time.Time{})
	return &openedFlow{fc: lane.fc, outer: lane.outer, sessionID: sessionID, flowID: flowID, laneID: lane.laneID, kind: lane.kind}, nil
}

func (c *Client) racePipelinedFlow(ctx context.Context, payload []byte) (*openedFlow, error) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	quicResult := make(chan openedFlowResult, 1)
	go func() {
		flow, err := c.dialPipelinedFlow(raceCtx, TransportQUIC, payload)
		quicResult <- openedFlowResult{flow: flow, err: err}
	}()
	timer := time.NewTimer(c.cfg.FallbackDelay)
	defer timer.Stop()
	select {
	case result := <-quicResult:
		if result.err == nil {
			c.udpHealth.success()
			return result.flow, nil
		}
		c.udpHealth.failure(time.Now())
		c.metrics.Fallback()
		return c.dialPipelinedFlow(ctx, TransportTCP, payload)
	case <-timer.C:
	case <-ctx.Done():
		closeLateFlow(quicResult)
		return nil, ctx.Err()
	}
	tcpResult := make(chan openedFlowResult, 1)
	go func() {
		flow, err := c.dialPipelinedFlow(raceCtx, TransportTCP, payload)
		tcpResult <- openedFlowResult{flow: flow, err: err}
	}()
	var quicErr, tcpErr error
	for quicResult != nil || tcpResult != nil {
		select {
		case result := <-quicResult:
			quicResult = nil
			if result.err == nil {
				c.udpHealth.success()
				cancel()
				closeLateFlow(tcpResult)
				return result.flow, nil
			}
			quicErr = result.err
			c.udpHealth.failure(time.Now())
		case result := <-tcpResult:
			tcpResult = nil
			if result.err == nil {
				c.metrics.Fallback()
				cancel()
				closeLateFlow(quicResult)
				return result.flow, nil
			}
			tcpErr = result.err
		case <-ctx.Done():
			closeLateFlow(quicResult)
			closeLateFlow(tcpResult)
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("QUIC failed (%v); TCP fallback failed (%v)", quicErr, tcpErr)
}

type openedFlowResult struct {
	flow *openedFlow
	err  error
}

func closeLateFlow(ch <-chan openedFlowResult) {
	if ch == nil {
		return
	}
	go func() {
		result := <-ch
		if result.flow != nil {
			_ = result.flow.fc.Close()
		}
	}()
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
	openFlags := uint16(0)
	if lane.reserveControl {
		openFlags |= protocol.FlagReserveControl
	}
	if err := lane.fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: openType, Flags: openFlags, SessionID: lane.sessionID, FlowID: flowID, Class: protocol.ClassNew},
		Payload: payload,
	}); err != nil {
		return fail(fmt.Errorf("send flow open: %w", err))
	}
	if c.cfg.OptimisticOpen {
		_ = lane.outer.SetDeadline(time.Time{})
		return &openedFlow{
			fc: lane.fc, outer: lane.outer, sessionID: lane.sessionID, flowID: flowID,
			laneID: lane.laneID, kind: lane.kind, openPending: true, reserveControl: lane.reserveControl,
			helloPending: lane.helloPending, onHelloOK: lane.onHelloOK,
		}, nil
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
	return &openedFlow{fc: lane.fc, outer: lane.outer, sessionID: lane.sessionID, flowID: flowID, laneID: lane.laneID, kind: lane.kind, reserveControl: lane.reserveControl}, nil
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
	return c.dialLaneMode(ctx, kind, sessionID, laneID, session.HelloJoin, false, false)
}

func (c *Client) dialLane(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64, helloKind session.HelloKind) (*authenticatedLane, error) {
	// Under optimistic open the initial control stream pipelines HELLO with
	// OPEN, so the pooled bootstrap must not block on HELLO_OK either.
	return c.dialLaneMode(ctx, kind, sessionID, laneID, helloKind, c.cfg.EnableQUICPool, c.cfg.OptimisticOpen)
}

// dialLaneMode uses the shared QUIC stream pool only for a flow's initial
// control stream. Additional lanes are independent QUIC connections: they
// provide true bulk capacity and independent loss paths, while the pooled
// control stream remains available for short/interactive traffic.
func (c *Client) dialLaneMode(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64, helloKind session.HelloKind, pooled bool, pipelineHello bool) (*authenticatedLane, error) {
	dialStarted := time.Now()
	var outer streamConn
	var err error
	fastOpen := false
	reserveControl := false
	alreadyAuthenticated := false
	var publishCapabilities func(session.HelloOK)
	switch kind {
	case TransportTCP:
		outer, err = dialTCP(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress)
	case TransportQUIC:
		ccfg := congestionConfig{
			kind: c.cfg.Congestion, brutalBytesPerSecond: c.cfg.BrutalBytesPerSec,
			adaptiveMinBytesPerSec: c.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: c.cfg.AdaptiveMaxBytesSec,
		}
		if pooled {
			outer, fastOpen, alreadyAuthenticated, reserveControl, publishCapabilities, err = c.dialPooledQUICLane(ctx, ccfg, sessionID, pipelineHello)
		} else {
			outer, err = dialQUIC(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress, ccfg, c.windows())
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
		if pipelineHello {
			hello, helloErr := session.NewHello(c.cfg.Secret, sessionID, laneID, helloKind, time.Now())
			if helloErr != nil {
				return fail(helloErr)
			}
			if err := fc.Write(protocol.Frame{
				Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeHello, SessionID: sessionID, Class: protocol.ClassNew},
				Payload: hello.MarshalBinary(),
			}); err != nil {
				return fail(fmt.Errorf("send pipelined session hello: %w", err))
			}
		} else if err := clientAuthenticateKind(fc, c.cfg.Secret, sessionID, laneID, helloKind, time.Now()); err != nil {
			return fail(err)
		}
	}
	_ = outer.SetDeadline(time.Time{})
	c.cfg.Logger.Debug("outer lane authenticated", "transport", kind, "dial_duration", outerReady.Sub(dialStarted), "authentication_duration", time.Since(outerReady), "pooled", pooled, "fast_open", fastOpen)
	return &authenticatedLane{
		fc: fc, outer: outer, sessionID: sessionID, kind: kind, laneID: laneID,
		fastOpen: fastOpen, reserveControl: reserveControl,
		helloPending: !alreadyAuthenticated && pipelineHello, onHelloOK: publishCapabilities,
	}, nil
}

// dialPooledQUICLane opens a stream on the client's shared QUIC connection.
// The mutex covers connection creation and stream-limit admission, so two
// simultaneous first flows cannot create competing pools. A stream-open
// failure caused by a dead connection clears the pool and lets the caller's
// normal AUTO fallback/retry policy establish a fresh transport.
func (c *Client) dialPooledQUICLane(ctx context.Context, ccfg congestionConfig, sessionID [16]byte, deferAuthentication bool) (streamConn, bool, bool, bool, func(session.HelloOK), error) {
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
		c.quicPoolFast, c.quicPoolControl, c.quicPoolAuthenticated = false, false, false
		conn, packet, err := dialQUICConnection(dialCtx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress, c.windows())
		if err != nil {
			c.quicConn, c.quicPacket, c.quicController = nil, nil, nil
			c.quicPoolFast, c.quicPoolControl, c.quicPoolAuthenticated = false, false, false
			return nil, false, false, false, nil, err
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
			c.quicPoolFast, c.quicPoolControl, c.quicPoolAuthenticated = false, false, false
		}
		return nil, false, false, false, nil, err
	}
	// Track how many flows share the control connection. Bulk isolation is
	// only worth its cost when there is something to protect.
	c.quicPoolActive.Add(1)
	outer := &controlPoolStreamConn{
		quicStreamConn: &quicStreamConn{stream: stream, conn: c.quicConn, controller: c.quicController, closeConn: false},
		owner:          c,
	}
	if !c.quicPoolAuthenticated && c.quicConn.Context().Err() == nil {
		if deferAuthentication {
			// The caller pipelines HELLO with OPEN and lets the flow reader
			// consume HELLO_OK, which removes one China-US round trip from
			// every cold connection. Capabilities stay unpublished until that
			// acknowledgement arrives; a stream opened in the meantime simply
			// performs its own pipelined HELLO, which costs a few bytes rather
			// than a round trip. publishPoolCapabilities is bound to this
			// connection so a late acknowledgement from a replaced pool cannot
			// mark a newer connection authenticated.
			conn := c.quicConn
			return outer, false, false, false, func(ok session.HelloOK) { c.publishPoolCapabilities(conn, ok) }, nil
		}
		// Authenticate the first stream while holding the pool mutex. This
		// makes connection-level authentication atomic: a second stream cannot
		// race a not-yet-authenticated connection. Subsequent streams on a
		// capable server skip Hello and begin with TypeOpenFast.
		_ = outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
		ok, authErr := clientAuthenticateKindResult(newFrameConn(outer, c.cfg.MaxPayload), c.cfg.Secret, sessionID, 0, session.HelloNew, time.Now())
		if authErr != nil {
			_ = outer.Close()
			if c.quicConn.Context().Err() != nil {
				c.quicConn, c.quicPacket, c.quicController = nil, nil, nil
				c.quicPoolFast, c.quicPoolControl, c.quicPoolAuthenticated = false, false, false
			}
			return nil, false, false, false, nil, authErr
		}
		c.quicPoolFast = ok.Capabilities&session.CapabilityFastStreams != 0
		c.quicPoolControl = ok.Capabilities&session.CapabilityReserveControl != 0
		c.quicPoolAuthenticated = true
		c.peerAckRanges.Store(ok.Capabilities&session.CapabilityAckRanges != 0)
		_ = outer.SetDeadline(time.Time{})
		return outer, false, true, c.quicPoolControl, nil, nil
	}
	// A capable peer has authenticated the QUIC connection, so this stream
	// must skip the per-stream Hello and start with OPEN_FAST. An older peer
	// advertises no capability and deliberately keeps the legacy Hello path.
	return outer, c.quicPoolFast, c.quicPoolFast, c.quicPoolControl, nil, nil
}

// publishPoolCapabilities records the negotiated connection capabilities once
// a deferred HELLO_OK arrives. It is a no-op when the pool has already been
// replaced, so a slow acknowledgement cannot resurrect a dead connection's
// negotiated state.
func (c *Client) publishPoolCapabilities(conn *quic.Conn, ok session.HelloOK) {
	c.quicMu.Lock()
	defer c.quicMu.Unlock()
	if c.quicConn != conn || c.quicPoolAuthenticated {
		return
	}
	c.quicPoolFast = ok.Capabilities&session.CapabilityFastStreams != 0
	c.quicPoolControl = ok.Capabilities&session.CapabilityReserveControl != 0
	c.quicPoolAuthenticated = true
	c.peerAckRanges.Store(ok.Capabilities&session.CapabilityAckRanges != 0)
}

// peerAcceptsAckRanges reports whether the server advertised that it can
// consume byte ranges alongside the cumulative acknowledgement. A peer that
// did not say so must never be sent them.
//
// A flow opened before the first HELLO_OK arrives starts without them and does
// not retrofit: the capability is per flow, and the only cost of being late is
// that one early flow keeps the cumulative-only behavior.
func (c *Client) peerAcceptsAckRanges() bool { return c.peerAckRanges.Load() }

// laneJoinResult carries an asynchronous lane join back to the decision loop.
type laneJoinResult struct {
	lane *mpLane
	id   uint64
	err  error
}

// errLaneJoinRejected reports that the peer answered a lane join and refused
// it, as opposed to the join failing to complete. The distinction decides two
// things: a refusal is not evidence that UDP is unhealthy -- the handshake
// completed and the peer replied, so marking the transport down here would
// eventually push unrelated flows onto the TCP fallback -- and it is a policy
// answer that will not change during this flow, so the search should stop
// rather than back off and retry. A peer pinned to a lower lane ceiling than
// this endpoint is the ordinary way to reach it.
var errLaneJoinRejected = errors.New("lane join rejected")

func (c *Client) openJoinLane(ctx context.Context, kind TransportKind, sessionID [16]byte, flowID, laneID uint64) (*mpLane, error) {
	if kind != TransportQUIC && kind != TransportTCP {
		return nil, fmt.Errorf("unsupported join transport %q", kind)
	}
	if kind == TransportQUIC && c.cfg.EnableQUICPool {
		if lane, fastErr := c.openFastJoinLane(ctx, sessionID, flowID, laneID); fastErr == nil {
			return lane, nil
		} else {
			// A missing capability, stale secondary pool, or a transient UDP
			// failure must not make an otherwise healthy flow fail. Fall back to
			// the established authenticated dedicated-lane path.
			c.cfg.Logger.Debug("fast lane join unavailable; using dedicated lane", "error", fastErr)
		}
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
			return nil, fmt.Errorf("%w: %s", errLaneJoinRejected, string(response.Payload[1:]))
		}
		return nil, errLaneJoinRejected
	}
	if response.Header.Type != protocol.TypeOpenOK || response.Header.SessionID != sessionID || response.Header.FlowID != flowID || len(response.Payload) != 0 {
		_ = lane.fc.Close()
		return nil, errors.New("invalid lane join acknowledgement")
	}
	return &mpLane{id: laneID, kind: kind, fc: lane.fc}, nil
}

// openFastJoinLane uses a bounded, separately authenticated QUIC connection
// for bulk streams. The connection handshake and PSK exchange are amortized
// across joins; the per-flow operation is one stream-open plus one
// OPEN_JOIN_FAST/OpenOK exchange. If the peer lacks the negotiated capability
// the caller transparently falls back to the legacy dedicated lane.
func (c *Client) openFastJoinLane(ctx context.Context, sessionID [16]byte, flowID, laneID uint64) (*mpLane, error) {
	started := time.Now()
	outer, err := c.openBulkPoolStream(ctx)
	if err != nil {
		return nil, err
	}
	fc := newFrameConn(outer, c.cfg.MaxPayload)
	var payload [8]byte
	binary.BigEndian.PutUint64(payload[:], laneID)
	_ = outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	if err := fc.Write(protocol.Frame{Header: protocol.Header{
		Version: protocol.Version, Type: protocol.TypeOpenJoinFast, Flags: protocol.FlagLaneJoin,
		SessionID: sessionID, FlowID: flowID, Class: protocol.ClassBulk,
	}, Payload: payload[:]}); err != nil {
		_ = outer.Close()
		return nil, fmt.Errorf("send fast lane join: %w", err)
	}
	response, err := fc.Read()
	if err != nil {
		_ = outer.Close()
		return nil, fmt.Errorf("read fast lane join acknowledgement: %w", err)
	}
	if response.Header.SessionID != sessionID || response.Header.FlowID != flowID {
		_ = outer.Close()
		return nil, errors.New("fast lane join acknowledgement identity mismatch")
	}
	if response.Header.Type == protocol.TypeReset {
		_ = outer.Close()
		if len(response.Payload) > 1 {
			return nil, fmt.Errorf("fast lane join rejected: %s", string(response.Payload[1:]))
		}
		return nil, errors.New("fast lane join rejected")
	}
	if response.Header.Type != protocol.TypeOpenOK || len(response.Payload) != 0 {
		_ = outer.Close()
		return nil, errors.New("invalid fast lane join acknowledgement")
	}
	_ = outer.SetDeadline(time.Time{})
	c.cfg.Logger.Debug("fast bulk lane joined", "lane", laneID, "duration", time.Since(started))
	return &mpLane{id: laneID, kind: TransportQUIC, fc: fc}, nil
}

// openBulkPoolStream reserves one secondary connection and opens its lane
// stream. Connections are created lazily, so one-shot and interactive-only
// clients pay no extra QUIC handshake, and each is reserved exclusively for
// the lane it carries so concurrent lanes keep independent 4-tuples and
// congestion state.
func (c *Client) openBulkPoolStream(ctx context.Context) (streamConn, error) {
	started := time.Now()
	dialCtx := ctx
	var cancel context.CancelFunc
	if c.cfg.DialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, c.cfg.DialTimeout)
		defer cancel()
	}
	entry, err := c.reserveBulkConn(dialCtx)
	if err != nil {
		return nil, err
	}
	stream, err := entry.conn.OpenStreamSync(dialCtx)
	if err != nil {
		c.releaseBulkConn(entry, entry.conn.Context().Err() != nil)
		return nil, err
	}
	c.cfg.Logger.Debug("bulk pool stream opened", "duration", time.Since(started), "connections", c.bulkConnCount())
	return &bulkPoolStreamConn{
		quicStreamConn: &quicStreamConn{stream: stream, conn: entry.conn, controller: entry.controller, closeConn: false},
		owner:          c, entry: entry,
	}, nil
}

// reserveBulkConn returns an idle authenticated connection, or establishes a
// new one when every existing connection is already carrying a lane.
func (c *Client) reserveBulkConn(ctx context.Context) (*bulkConn, error) {
	c.bulkMu.Lock()
	if time.Now().Before(c.bulkJoinUnavailableTil) {
		c.bulkMu.Unlock()
		return nil, errors.New("peer fast lane join capability is temporarily unavailable")
	}
	live := c.bulkConns[:0]
	for _, entry := range c.bulkConns {
		if entry.conn.Context().Err() != nil && !entry.busy {
			entry.close("wanopt stale bulk pool")
			continue
		}
		live = append(live, entry)
	}
	c.bulkConns = live
	for _, entry := range c.bulkConns {
		if !entry.busy && entry.conn.Context().Err() == nil {
			entry.busy = true
			if entry.idleTimer != nil {
				entry.idleTimer.Stop()
				entry.idleTimer = nil
			}
			c.bulkMu.Unlock()
			return entry, nil
		}
	}
	if len(c.bulkConns) >= c.maxBulkConns() {
		c.bulkMu.Unlock()
		return nil, errors.New("bulk lane connection limit reached")
	}
	c.bulkMu.Unlock()

	// The handshake is deliberately performed without the pool mutex so that
	// one slow secondary handshake cannot block every other lane join.
	entry, err := c.dialBulkConn(ctx)
	if err != nil {
		return nil, err
	}
	c.bulkMu.Lock()
	if len(c.bulkConns) >= c.maxBulkConns() {
		c.bulkMu.Unlock()
		entry.close("wanopt bulk pool limit reached")
		return nil, errors.New("bulk lane connection limit reached")
	}
	entry.busy = true
	c.bulkConns = append(c.bulkConns, entry)
	c.bulkMu.Unlock()
	return entry, nil
}

// maxBulkConns bounds the secondary connections one client may hold. A flow
// never needs more than its lane budget, and lane zero is not a bulk lane.
func (c *Client) maxBulkConns() int {
	if c.cfg.MaxLanes <= 1 {
		return 1
	}
	return c.cfg.MaxLanes
}

func (c *Client) bulkConnCount() int {
	c.bulkMu.Lock()
	defer c.bulkMu.Unlock()
	return len(c.bulkConns)
}

func (c *Client) dialBulkConn(ctx context.Context) (*bulkConn, error) {
	started := time.Now()
	conn, packet, err := dialQUICConnection(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress, c.windows())
	if err != nil {
		return nil, err
	}
	entry := &bulkConn{conn: conn, packet: packet}
	entry.controller = configureQUICController(conn, congestionConfig{
		kind: c.cfg.Congestion, brutalBytesPerSecond: c.cfg.BrutalBytesPerSec,
		adaptiveMinBytesPerSec: c.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: c.cfg.AdaptiveMaxBytesSec,
	})
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		entry.close("wanopt bulk pool bootstrap failed")
		return nil, err
	}
	bootstrap, err := session.NewSessionID()
	if err != nil {
		entry.close("wanopt bulk pool bootstrap failed")
		return nil, err
	}
	outer := &quicStreamConn{stream: stream, conn: conn, controller: entry.controller, closeConn: false}
	_ = outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	ok, err := clientAuthenticateKindResult(newFrameConn(outer, c.cfg.MaxPayload), c.cfg.Secret, bootstrap, 0, session.HelloPool, time.Now())
	_ = outer.Close()
	if err != nil {
		entry.close("wanopt bulk pool authentication failed")
		return nil, err
	}
	if ok.Capabilities&session.CapabilityFastLaneJoin == 0 {
		entry.close("wanopt bulk pool unsupported")
		c.bulkMu.Lock()
		c.bulkJoinUnavailableTil = time.Now().Add(bulkJoinCapabilityRetry)
		c.bulkMu.Unlock()
		return nil, errors.New("peer does not support fast lane join")
	}
	c.cfg.Logger.Debug("bulk QUIC pool authenticated", "duration", time.Since(started), "capabilities", ok.Capabilities)
	return entry, nil
}

// controlPoolStreamConn keeps the count of flows sharing the pooled control
// connection accurate, which is what decides whether a bulk flow should move
// off it.
type controlPoolStreamConn struct {
	*quicStreamConn
	owner *Client
	once  sync.Once
}

func (s *controlPoolStreamConn) Close() error {
	err := s.quicStreamConn.Close()
	s.once.Do(func() {
		if remaining := s.owner.quicPoolActive.Add(-1); remaining < 0 {
			s.owner.quicPoolActive.Store(0)
		}
	})
	return err
}

type bulkPoolStreamConn struct {
	*quicStreamConn
	owner *Client
	entry *bulkConn
	once  sync.Once
}

func (s *bulkPoolStreamConn) Close() error {
	err := s.quicStreamConn.Close()
	s.once.Do(func() { s.owner.releaseBulkConn(s.entry, s.entry.conn.Context().Err() != nil) })
	return err
}

// releaseBulkConn returns a connection to the idle set, or discards it when
// its transport is already dead. An idle connection is retained briefly so a
// following flow can skip the handshake, then closed.
func (c *Client) releaseBulkConn(entry *bulkConn, dead bool) {
	c.bulkMu.Lock()
	entry.busy = false
	if dead {
		remaining := c.bulkConns[:0]
		for _, existing := range c.bulkConns {
			if existing != entry {
				remaining = append(remaining, existing)
			}
		}
		c.bulkConns = remaining
		c.bulkMu.Unlock()
		entry.close("wanopt bulk pool failed")
		return
	}
	if entry.idleTimer != nil {
		entry.idleTimer.Stop()
	}
	entry.idleTimer = time.AfterFunc(bulkPoolIdleTimeout, func() { c.expireBulkConn(entry) })
	c.bulkMu.Unlock()
}

func (c *Client) expireBulkConn(entry *bulkConn) {
	c.bulkMu.Lock()
	if entry.busy {
		c.bulkMu.Unlock()
		return
	}
	remaining := c.bulkConns[:0]
	found := false
	for _, existing := range c.bulkConns {
		if existing == entry {
			found = true
			continue
		}
		remaining = append(remaining, existing)
	}
	c.bulkConns = remaining
	c.bulkMu.Unlock()
	if found {
		entry.close("wanopt bulk pool idle")
	}
}

func (c *Client) openAdditionalLanes(ctx context.Context, flow *multipathFlow, sessionID [16]byte, flowID uint64, initialKind TransportKind) {
	if initialKind != TransportQUIC {
		return
	}
	// Every additional lane is an independent authenticated QUIC connection,
	// so each costs a full handshake. Opening them one after another charges
	// SOCKS CONNECT the sum of those handshakes: with four configured lanes on
	// a 200 ms path that measured 1.8 s before any application byte moved.
	// They are independent operations, so open them concurrently and pay one
	// handshake of latency instead.
	wanted := c.cfg.InitialLanes - flow.laneCount()
	if wanted <= 0 {
		return
	}
	type joined struct {
		lane *mpLane
		err  error
		id   uint64
	}
	results := make(chan joined, wanted)
	started := 0
	for range wanted {
		if ctx.Err() != nil {
			break
		}
		laneID, err := flow.allocateJoinID()
		if err != nil {
			break
		}
		started++
		go func(laneID uint64) {
			lane, err := c.openJoinLane(ctx, TransportQUIC, sessionID, flowID, laneID)
			results <- joined{lane: lane, err: err, id: laneID}
		}(laneID)
	}
	for range started {
		result := <-results
		if result.err != nil {
			// A flow can finish while a speculative join is in the handshake.
			// That is an expected shutdown race, not evidence that the UDP path
			// is unhealthy; feeding it into the global cooldown causes unrelated
			// new flows to fall back to TCP unnecessarily.
			if ctx.Err() != nil || flow.doneChanClosed() {
				continue
			}
			c.udpHealth.failure(time.Now())
			c.cfg.Logger.Warn("additional lane unavailable", "lane", result.id, "error", result.err)
			continue
		}
		if err := flow.addLane(result.lane); err != nil {
			_ = result.lane.fc.Close()
		}
	}
}

// bulkLaneBudget splits a configured lane maximum into the lanes that may
// carry bulk payload and the separately reserved control lane.
//
// A negotiated control lane is excluded from bulk selection, so it is not part
// of a flow's bulk capacity: --max-lanes bounds the lanes that carry bulk
// payload, and the reserved control lane is additional. Both endpoints must
// agree on this, or the server rejects the bulk lanes the client opens.
//
// This is what makes bulk isolation work. A bulk flow left on the shared
// pooled connection queues interactive traffic behind its congestion window;
// moving it to its own connection is the point of classifying flows at all.
// Measured at 200 ms and 1% loss with a 50 MiB transfer running, isolating
// bulk took interactive requests from a 338 ms median and 545 ms 95th
// percentile to 204 ms and 356 ms, for roughly 8% less bulk goodput. One
// configured lane still means one bulk connection; striping beyond that
// remains an explicit, measured decision.
func bulkLaneBudget(maxLanes int, reserveControl bool) (bulk, controlReserve int) {
	if maxLanes < 1 {
		maxLanes = 1
	}
	if reserveControl {
		return maxLanes, 1
	}
	return maxLanes, 0
}

func (c *Client) manageLanes(ctx context.Context, flow *multipathFlow, sessionID [16]byte, flowID uint64, initialKind TransportKind) {
	if initialKind != TransportQUIC {
		return
	}
	// A negotiated control lane is excluded from bulk selection, so it is not
	// part of the flow's bulk capacity. Planning in total lanes while lane 0
	// cannot carry bulk payload caps a bulk transfer at one lane no matter how
	// large --max-lanes is; measured on the emulated per-flow-policed path,
	// that is the difference between using one lane's share and using several.
	// The planner therefore reasons in bulk lanes, and the reservation is
	// added back when the target is compared against the flow.
	bulkBudget, controlReserve := bulkLaneBudget(c.cfg.MaxLanes, flow.reserveControlLane)
	bulkStartLanes := c.cfg.BulkStartLanes
	if bulkStartLanes < 1 {
		bulkStartLanes = 1
	}
	if bulkStartLanes > bulkBudget {
		bulkStartLanes = bulkBudget
	}
	planner := scheduler.New(scheduler.Config{
		MaxLanes: bulkBudget, InteractiveLanes: 1, BulkStartLanes: bulkStartLanes,
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
	prober := scheduler.NewLaneProbe(scheduler.ProbeConfig{MinGain: c.cfg.MinimumMarginalGain})
	joins := make(chan laneJoinResult, maxLaneProbeAttempts)
	joinPending := false
	var lastSample time.Time
	var lastBytes uint64
	var lastDecision time.Time
	var recoveryBackoff time.Duration
	var nextRecovery time.Time
	recoveryAttempts := 0
	var lastRecoveryAttempt time.Time
	var growthBlockedUntil time.Time
	var growthBackoff time.Duration
	var seenLaneFailures uint64
	growthAttempts := 0
	growthSuppressed := false
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
			for draining := true; draining; {
				select {
				case result := <-joins:
					joinPending = false
					switch {
					case result.err != nil:
						if manageCtx.Err() != nil || flow.doneChanClosed() {
							return
						}
						prober.Cancel()
						if errors.Is(result.err, errLaneJoinRejected) {
							// The peer's ceiling, not a broken path. Stop
							// probing and keep the lanes already held.
							prober.Stop()
							growthSuppressed = true
							c.cfg.Logger.Debug("peer refused lane join; holding lane count", "lane", result.id, "error", result.err)
							break
						}
						c.udpHealth.failure(time.Now())
						c.cfg.Logger.Warn("adaptive lane unavailable", "lane", result.id, "error", result.err)
						if growthBackoff == 0 {
							growthBackoff = minLaneProbeBackoff
						} else if growthBackoff < maxLaneProbeBackoff {
							growthBackoff *= 2
							if growthBackoff > maxLaneProbeBackoff {
								growthBackoff = maxLaneProbeBackoff
							}
						}
						growthBlockedUntil = time.Now().Add(growthBackoff)
					default:
						if err := flow.addLane(result.lane); err != nil {
							_ = result.lane.fc.Close()
							prober.Cancel()
							break
						}
						prober.Confirm()
						if controlReserve > 0 && flow.laneCount() == controlReserve+1 {
							// The flow's first bulk lane is what moves it off
							// the shared control connection, which is what
							// keeps interactive traffic out of a bulk
							// congestion window. Count it so an operator can
							// see the policy act.
							c.metrics.BulkIsolated()
						}
					}
				default:
					draining = false
				}
			}
			snapshot := flow.snapshot()
			now := time.Now()
			// Lane failures are asynchronous. If a speculative lane disappears,
			// suppress immediate re-probing; otherwise a lossy path can churn
			// through authenticated lanes faster than QUIC can declare them dead.
			failures := flow.laneFailureCount()
			if failures != seenLaneFailures {
				seenLaneFailures = failures
				if snapshot.HealthyLanes > 0 {
					if growthBackoff == 0 {
						growthBackoff = minLaneProbeBackoff
					} else if growthBackoff < maxLaneProbeBackoff {
						growthBackoff *= 2
						if growthBackoff > maxLaneProbeBackoff {
							growthBackoff = maxLaneProbeBackoff
						}
					}
					growthBlockedUntil = now.Add(growthBackoff)
				}
			}
			if snapshot.HealthyLanes == 0 {
				if flow.doneChanClosed() || recoveryAttempts >= maxLaneRecoveryAttempts {
					return
				}
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
			// Isolation earns its cost only while another flow shares the
			// control connection. A bulk transfer alone on it has nothing to
			// protect, and moving it would spend a handshake and a fresh
			// congestion window for no benefit: measured on an otherwise idle
			// path that costs about 8% of bulk goodput.
			//
			// Without a negotiated control lane there is no shared connection
			// to leave, so this hold does not apply and lane policy is
			// whatever --max-lanes asks for.
			holdOnControlLane := controlReserve > 0 && c.quicPoolActive.Load() <= 1
			target := bulkStartLanes + controlReserve
			if holdOnControlLane {
				target = 0
			}
			prewarm := flow.laneCount() < target && shouldPrewarmBulkLane(snapshot)
			if snapshot.Class != classifier.ClassBulk && !prewarm {
				continue
			}
			// Do not consume the decision interval while the flow is still NEW or
			// INTERACTIVE. The classifier may cross its bulk byte/age boundary just
			// after such a tick; delaying the first probe by another full interval
			// leaves several MiB on the shared control connection on a fast path.
			if !lastDecision.IsZero() && now.Sub(lastDecision) < laneDecisionInterval {
				continue
			}
			lastDecision = now
			decision := scheduler.Decision{TargetLanes: bulkStartLanes, Class: snapshot.Class, Reason: "bounded bulk-lane prewarm"}
			probeRequested := false
			if snapshot.Class == classifier.ClassBulk {
				// Goodput must be measured over the decision interval.
				// Cumulative average goodput lags by the whole flow history, so
				// a lane that immediately doubles the instantaneous rate still
				// shows a small average change and growth stalls at the
				// bootstrap target.
				goodput := 0.0
				if interval := now.Sub(lastSample); interval > 0 && !lastSample.IsZero() {
					goodput = float64(snapshot.Bytes-lastBytes) / interval.Seconds()
				} else if snapshot.Elapsed > 0 {
					goodput = float64(snapshot.Bytes) / snapshot.Elapsed.Seconds()
				}
				lastSample, lastBytes = now, snapshot.Bytes
				// The probe holds a baseline across the lane it added, so this
				// gain describes that lane rather than whatever the path did
				// between two ticks.
				grow, gain := prober.Observe(goodput)
				probeRequested = grow
				decision = planner.Decide(snapshot.Class, scheduler.Metrics{
					CurrentLanes: snapshot.CurrentLanes - controlReserve, HealthyLanes: snapshot.HealthyLanes - controlReserve,
					AvailableLanes: bulkBudget, MarginalGain: gain, ProbeReady: grow,
					BaselineRTT: snapshot.BaselineRTT, CurrentRTT: snapshot.CurrentRTT,
					UDPHealthy: c.udpHealth.allow(time.Now()),
				})
				if gain != 0 {
					// One line per completed experiment: the measured effect of
					// a lane that was actually added, and whether the search
					// continues. This is the record an operator needs to tell
					// "striping did not help here" from "striping never ran".
					c.cfg.Logger.Debug("lane probe verdict", "gain", gain,
						"lanes", flow.laneCount(), "exhausted", prober.Exhausted(),
						"goodput_mbps", goodput*8/1e6)
				}
			}
			decision.TargetLanes += controlReserve
			if holdOnControlLane && flow.laneCount() <= controlReserve && !probeRequested {
				// Stay on the shared connection while nothing else is using
				// it. A later arrival flips this on the next tick.
				//
				// This hold declines isolation for its own sake, which costs a
				// handshake and a fresh congestion window to protect traffic
				// that is not there. It must not also decline a lane the probe
				// has measured a gain for: the first bulk lane is exactly what
				// moves a flow off the shared connection, so holding it
				// unconditionally means a flow alone on the pool can never
				// stripe no matter how much the path would reward it.
				decision.TargetLanes = flow.laneCount()
			}
			if decision.TargetLanes < flow.laneCount() && snapshot.Class == classifier.ClassBulk {
				// DATA already written on a lane remains in the peer's replay window
				// until cumulatively acknowledged. Closing that lane here turns a
				// policy demotion into a transport failure and causes the peer to
				// retransmit the outstanding window on lane zero. Until the wire
				// protocol has an acknowledged lane-drain exchange, retain every
				// healthy lane for the lifetime of the flow and only suppress further
				// growth. This preserves correctness and avoids replay amplification.
				growthSuppressed = true
			}
			// Open at most one speculative lane per scheduler tick.  A flow can
			// finish while a join is in flight; if the peer has already started
			// tearing down the session, repeatedly filling the target in one tick
			// creates an unbounded stream of authenticated joins (and can leave
			// dozens of zero-byte lanes behind).  One bounded probe per tick keeps
			// growth observable, gives the completion watcher a chance to stop the
			// manager, and makes the configured lane cap a real resource bound.
			mayGrow := flow.laneCount() < decision.TargetLanes && !growthSuppressed && !joinPending &&
				growthAttempts < maxLaneProbeAttempts && !flow.doneChanClosed() &&
				(now.After(growthBlockedUntil) || now.Equal(growthBlockedUntil)) &&
				!(flow.finSent.Load() && flow.remoteFinSeen.Load())
			if !mayGrow {
				// Tell the probe its request went unserved, so it does not
				// spend its one experiment measuring a lane that never opened.
				prober.Cancel()
			}
			if mayGrow {
				laneID, err := flow.allocateJoinID()
				if err != nil {
					return
				}
				growthAttempts++
				joinPending = true
				// Open the lane off the decision loop. On a saturated path the
				// join's own handshake queues behind the flow's data and has
				// been measured taking several seconds; doing it inline stops
				// the sampler for that whole time, which destroys the very
				// windows the probe is comparing and leaves the flow blind to
				// a lane failure meanwhile.
				go func() {
					lane, err := c.openJoinLane(manageCtx, TransportQUIC, sessionID, flowID, laneID)
					select {
					case joins <- laneJoinResult{lane: lane, id: laneID, err: err}:
					case <-manageCtx.Done():
						if lane != nil {
							_ = lane.fc.Close()
						}
					}
				}()
			}
		}
	}
}

func shouldPrewarmBulkLane(snapshot flowSnapshot) bool {
	if snapshot.Elapsed < bulkLanePrewarmAge || snapshot.Bytes < bulkLanePrewarmBytes {
		return false
	}
	smaller, larger := snapshot.BytesUp, snapshot.BytesDown
	if smaller > larger {
		smaller, larger = larger, smaller
	}
	if smaller == 0 {
		return larger >= bulkLanePrewarmBytes
	}
	return larger/smaller >= bulkLaneAsymmetry
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
