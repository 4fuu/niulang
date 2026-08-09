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

	"github.com/icourses-dev/wanopt/internal/classifier"
	"github.com/icourses-dev/wanopt/internal/protocol"
	"github.com/icourses-dev/wanopt/internal/scheduler"
	"github.com/icourses-dev/wanopt/internal/session"
	"github.com/icourses-dev/wanopt/internal/socks5"
)

type ClientConfig struct {
	ListenAddr          string
	RemoteAddr          string
	ServerName          string
	LocalAddress        string
	Secret              []byte
	RootCAs             *x509.CertPool
	MaxPayload          uint32
	ChunkSize           int
	DialTimeout         time.Duration
	HandshakeTimeout    time.Duration
	MaxSessions         int
	Transport           TransportKind
	Congestion          CongestionControlKind
	BrutalBytesPerSec   uint64
	AdaptiveMinBytesSec uint64
	AdaptiveMaxBytesSec uint64
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
}

func NewClient(cfg ClientConfig) (*Client, error) {
	if cfg.ListenAddr == "" || cfg.RemoteAddr == "" || cfg.ServerName == "" {
		return nil, errors.New("client listen, remote, and TLS server name are required")
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
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 1024
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
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
	if cfg.Congestion != CongestionReno && cfg.Congestion != CongestionAdaptive && cfg.Congestion != CongestionBrutal {
		return nil, fmt.Errorf("unsupported QUIC congestion controller %q", cfg.Congestion)
	}
	if cfg.Congestion == CongestionBrutal && cfg.BrutalBytesPerSec == 0 {
		return nil, errors.New("brutal congestion requires a positive per-lane byte rate")
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
		cfg.MaxLanes = 8
	}
	if cfg.MaxLanes > 8 {
		return nil, errors.New("maximum lane count must not exceed 8")
	}
	if cfg.InitialLanes > cfg.MaxLanes {
		return nil, errors.New("initial lane count cannot exceed maximum lane count")
	}
	if cfg.BulkStartLanes <= 0 {
		cfg.BulkStartLanes = 2
	}
	if cfg.BulkStartLanes > cfg.MaxLanes {
		cfg.BulkStartLanes = cfg.MaxLanes
	}
	if cfg.MinimumMarginalGain <= 0 {
		cfg.MinimumMarginalGain = 0.10
	}
	return &Client{cfg: cfg, udpHealth: newUDPHealth(cfg.UDPFailureThreshold, cfg.UDPCooldown)}, nil
}

func (c *Client) Serve(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	listener, err := lc.Listen(ctx, "tcp", c.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on local SOCKS5 address: %w", err)
	}
	return c.ServeListener(ctx, listener)
}

// ServeListener is primarily useful for tests and service managers which
// provide an already-bound socket. The listener is closed when the context is
// cancelled or the method returns.
func (c *Client) ServeListener(ctx context.Context, listener net.Listener) error {
	defer listener.Close()

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

func (c *Client) handleLocal(ctx context.Context, inner net.Conn) {
	defer inner.Close()
	_ = inner.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	req, err := socks5.ReadRequest(inner)
	if err != nil {
		c.cfg.Logger.Debug("SOCKS5 negotiation failed", "error", err)
		return
	}
	flow, err := c.openFlow(ctx, req.Destination)
	if err != nil {
		_ = socks5.WriteReply(inner, socks5.ReplyHostUnreachable, nil)
		c.cfg.Logger.Warn("remote flow open failed", "error", err)
		return
	}
	flowSession := newMultipathFlow(ctx, inner, flow.sessionID, flow.flowID, c.cfg.ChunkSize, protocol.FlagAckUp, protocol.FlagAckDown)
	if err := flowSession.addLane(&mpLane{id: flow.laneID, kind: flow.kind, fc: flow.fc}); err != nil {
		_ = flow.fc.Close()
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
		_ = flow.fc.Close()
		return
	}
	_ = inner.SetDeadline(time.Time{})
	go c.manageLanes(ctx, flowSession, flow.sessionID, flow.flowID, flow.kind)
	stats, err := flowSession.run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
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
}

func (c *Client) openFlow(ctx context.Context, destination string) (*openedFlow, error) {
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
	if err := lane.fc.Write(protocol.Frame{
		Header:  protocol.Header{Version: protocol.Version, Type: protocol.TypeOpen, SessionID: lane.sessionID, FlowID: flowID, Class: protocol.ClassNew},
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
		return fail(errors.New("remote destination unavailable"))
	}
	if response.Header.Type != protocol.TypeOpenOK || len(response.Payload) != 0 {
		return fail(errors.New("invalid flow open acknowledgement"))
	}
	_ = lane.outer.SetDeadline(time.Time{})
	return &openedFlow{fc: lane.fc, outer: lane.outer, sessionID: lane.sessionID, flowID: flowID, laneID: lane.laneID, kind: lane.kind}, nil
}

func (c *Client) dialAuthenticatedLane(ctx context.Context, kind TransportKind) (*authenticatedLane, error) {
	sessionID, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	return c.dialLane(ctx, kind, sessionID, 0, session.HelloNew)
}

func (c *Client) dialJoinLane(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64) (*authenticatedLane, error) {
	return c.dialLane(ctx, kind, sessionID, laneID, session.HelloJoin)
}

func (c *Client) dialLane(ctx context.Context, kind TransportKind, sessionID [16]byte, laneID uint64, helloKind session.HelloKind) (*authenticatedLane, error) {
	var outer streamConn
	var err error
	switch kind {
	case TransportTCP:
		outer, err = dialTCP(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress)
	case TransportQUIC:
		outer, err = dialQUIC(ctx, c.cfg.RemoteAddr, c.cfg.ServerName, c.cfg.RootCAs, c.cfg.DialTimeout, c.cfg.LocalAddress, congestionConfig{
			kind: c.cfg.Congestion, brutalBytesPerSecond: c.cfg.BrutalBytesPerSec,
			adaptiveMinBytesPerSec: c.cfg.AdaptiveMinBytesSec, adaptiveMaxBytesPerSec: c.cfg.AdaptiveMaxBytesSec,
		})
	default:
		return nil, fmt.Errorf("cannot dial transport %q", kind)
	}
	if err != nil {
		return nil, transportError(kind, err)
	}
	fail := func(err error) (*authenticatedLane, error) {
		_ = outer.Close()
		return nil, transportError(kind, err)
	}
	_ = outer.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	fc := newFrameConn(outer, c.cfg.MaxPayload)
	if err := clientAuthenticateKind(fc, c.cfg.Secret, sessionID, laneID, helloKind, time.Now()); err != nil {
		return fail(err)
	}
	_ = outer.SetDeadline(time.Time{})
	return &authenticatedLane{fc: fc, outer: outer, sessionID: sessionID, kind: kind, laneID: laneID}, nil
}

func (c *Client) openJoinLane(ctx context.Context, sessionID [16]byte, flowID, laneID uint64) (*mpLane, error) {
	// Striping over TCP is intentionally disabled. The session's fallback lane
	// is reliable, but several nested TCP congestion controllers can amplify
	// head-of-line blocking under loss.
	lane, err := c.dialJoinLane(ctx, TransportQUIC, sessionID, laneID)
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
	if response.Header.Type != protocol.TypeOpenOK || response.Header.SessionID != sessionID || response.Header.FlowID != flowID || len(response.Payload) != 0 {
		_ = lane.fc.Close()
		return nil, errors.New("invalid lane join acknowledgement")
	}
	return &mpLane{id: laneID, kind: TransportQUIC, fc: lane.fc}, nil
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
		lane, err := c.openJoinLane(ctx, sessionID, flowID, laneID)
		if err != nil {
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
	if initialKind != TransportQUIC || c.cfg.MaxLanes <= 1 {
		return
	}
	planner := scheduler.New(scheduler.Config{
		MaxLanes: c.cfg.MaxLanes, InteractiveLanes: 1, BulkStartLanes: c.cfg.BulkStartLanes,
		MinimumMarginalGain: c.cfg.MinimumMarginalGain, InteractiveRTTBudget: 40 * time.Millisecond,
	})
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var previous float64
	for {
		select {
		case <-flow.doneChan():
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			snapshot := flow.snapshot()
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
				AvailableLanes: c.cfg.MaxLanes, MarginalGain: gain, UDPHealthy: c.udpHealth.allow(time.Now()),
			})
			for flow.laneCount() < decision.TargetLanes {
				laneID, err := flow.allocateJoinID()
				if err != nil {
					return
				}
				lane, err := c.openJoinLane(ctx, sessionID, flowID, laneID)
				if err != nil {
					c.cfg.Logger.Warn("adaptive lane unavailable", "lane", laneID, "error", err)
					break
				}
				if err := flow.addLane(lane); err != nil {
					_ = lane.fc.Close()
					return
				}
			}
		}
	}
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
		return c.dialAuthenticatedLane(ctx, TransportTCP)
	case <-timer.C:
	case <-ctx.Done():
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
				cancel()
				closeLateLane(quicResult)
				return result.lane, nil
			}
			tcpErr = result.err
		case <-ctx.Done():
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
