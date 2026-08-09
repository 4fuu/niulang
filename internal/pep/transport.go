package pep

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/apernet/quic-go"
	wancongestion "github.com/icourses-dev/wanopt/internal/congestion"
)

type TransportKind string

const (
	TransportTCP  TransportKind = "tcp"
	TransportQUIC TransportKind = "quic"
	TransportAuto TransportKind = "auto"
)

const defaultALPN = "wanopt/1"

const (
	defaultAdaptiveMinBytesPerSec = 64 * 1024
	defaultAdaptiveMaxBytesPerSec = 200 * 1024 * 1024
	maxConfiguredSessions         = 1 << 16
)

// CongestionControlKind selects the QUIC sender. Reno leaves the apNet
// quic-go default untouched and is the safe control. Adaptive is a
// rate-estimating controller for unknown paths. Brutal is a fixed-rate mode
// for controlled experiments where the operator knows the per-lane budget.
type CongestionControlKind string

const (
	CongestionReno     CongestionControlKind = "reno"
	CongestionAdaptive CongestionControlKind = "adaptive"
	CongestionBrutal   CongestionControlKind = "brutal"
)

type congestionConfig struct {
	kind                   CongestionControlKind
	brutalBytesPerSecond   uint64
	adaptiveMinBytesPerSec uint64
	adaptiveMaxBytesPerSec uint64
}

type udpHealth struct {
	mu        sync.Mutex
	failures  int
	threshold int
	cooldown  time.Duration
	blockedTo time.Time
}

func newUDPHealth(threshold int, cooldown time.Duration) *udpHealth {
	if threshold <= 0 {
		threshold = 3
	}
	if cooldown <= 0 {
		cooldown = 30 * time.Second
	}
	return &udpHealth{threshold: threshold, cooldown: cooldown}
}

func (h *udpHealth) allow(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !now.Before(h.blockedTo)
}

func (h *udpHealth) success() {
	h.mu.Lock()
	h.failures = 0
	h.blockedTo = time.Time{}
	h.mu.Unlock()
}

func (h *udpHealth) failure(now time.Time) {
	h.mu.Lock()
	h.failures++
	if h.failures >= h.threshold {
		h.blockedTo = now.Add(h.cooldown)
		h.failures = 0
	}
	h.mu.Unlock()
}

type streamConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// laneTransportStats is intentionally a small internal projection of QUIC's
// connection counters.  Keeping the QUIC type out of the flow and metrics
// packages lets TCP rescue lanes remain dependency-independent.
type laneTransportStats struct {
	latestRTT, smoothedRTT   time.Duration
	bytesSent, bytesReceived uint64
	bytesLost, packetsLost   uint64
}

type laneStatsProvider interface {
	transportStats() laneTransportStats
}

type quicStreamConn struct {
	stream *quic.Stream
	conn   *quic.Conn
	packet net.PacketConn
	once   sync.Once
}

func (c *quicStreamConn) transportStats() laneTransportStats {
	if c == nil || c.conn == nil {
		return laneTransportStats{}
	}
	s := c.conn.ConnectionStats()
	return laneTransportStats{
		latestRTT: s.LatestRTT, smoothedRTT: s.SmoothedRTT,
		bytesSent: s.BytesSent, bytesReceived: s.BytesReceived,
		bytesLost: s.BytesLost, packetsLost: s.PacketsLost,
	}
}

func (c *quicStreamConn) Read(p []byte) (int, error)  { return c.stream.Read(p) }
func (c *quicStreamConn) Write(p []byte) (int, error) { return c.stream.Write(p) }
func (c *quicStreamConn) SetDeadline(t time.Time) error {
	return c.stream.SetDeadline(t)
}
func (c *quicStreamConn) SetWriteDeadline(t time.Time) error {
	return c.stream.SetWriteDeadline(t)
}
func (c *quicStreamConn) Close() error {
	var err error
	c.once.Do(func() {
		// Closing the connection as well as the stream ensures that a lane
		// cannot remain alive after a flow has been terminated. A future
		// session manager may share a QUIC connection and will use a different
		// wrapper with stream-only lifetime.
		_ = c.stream.Close()
		err = c.conn.CloseWithError(0, "wanopt lane closed")
		if c.packet != nil {
			_ = c.packet.Close()
		}
	})
	return err
}

func tlsClientConfig(serverName string, roots *x509.CertPool) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, NextProtos: []string{defaultALPN}}
	if roots != nil {
		cfg.RootCAs = roots
	}
	return cfg
}

func dialTCP(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration, localAddress string) (streamConn, error) {
	var localAddr net.Addr
	if localAddress != "" {
		ip, err := netip.ParseAddr(localAddress)
		if err != nil {
			return nil, fmt.Errorf("parse local address %q: %w", localAddress, err)
		}
		localAddr = &net.TCPAddr{IP: ip.AsSlice()}
	}
	conn, err := (&tls.Dialer{
		NetDialer: &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, LocalAddr: localAddr},
		Config:    tlsClientConfig(serverName, roots),
	}).DialContext(ctx, "tcp", remote)
	if err != nil {
		return nil, err
	}
	tlsConn := conn.(*tls.Conn)
	if tlsConn.ConnectionState().NegotiatedProtocol != defaultALPN {
		_ = tlsConn.Close()
		return nil, errors.New("remote did not negotiate wanopt ALPN")
	}
	return tlsConn, nil
}

func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 10 * time.Second,
		// Existing-flow TCP rescue cannot begin until QUIC declares the
		// black-holed lane dead. Keep this bound well below application-level
		// request timeouts while allowing several PTOs on a 200 ms WAN.
		MaxIdleTimeout:                 15 * time.Second,
		KeepAlivePeriod:                5 * time.Second,
		InitialStreamReceiveWindow:     512 * 1024,
		MaxStreamReceiveWindow:         8 * 1024 * 1024,
		InitialConnectionReceiveWindow: 1 * 1024 * 1024,
		MaxConnectionReceiveWindow:     16 * 1024 * 1024,
		MaxIncomingStreams:             1,
		MaxIncomingUniStreams:          0,
		// The China path has a smaller effective UDP MTU than this host's
		// interface. Disable probing until path-specific MTU discovery is
		// available; otherwise a successful probe can raise packets above the
		// path MTU and stall a long response.
		DisablePathMTUDiscovery: true,
		InitialPacketSize:       1200,
	}
}

func dialQUIC(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration, localAddress string, ccfg congestionConfig) (streamConn, error) {
	dialCtx := ctx
	var cancel context.CancelFunc
	if dialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	tlsCfg := tlsClientConfig(serverName, roots)
	if localAddress != "" {
		ip, parseErr := netip.ParseAddr(localAddress)
		if parseErr != nil {
			return nil, fmt.Errorf("parse local address %q: %w", localAddress, parseErr)
		}
		remoteAddr, resolveErr := net.ResolveUDPAddr("udp", remote)
		if resolveErr != nil {
			return nil, resolveErr
		}
		packetConn, listenErr := net.ListenUDP("udp", &net.UDPAddr{IP: ip.AsSlice()})
		if listenErr != nil {
			return nil, listenErr
		}
		conn, dialErr := quic.Dial(dialCtx, packetConn, remoteAddr, tlsCfg, quicConfig())
		if dialErr != nil {
			_ = packetConn.Close()
			return nil, dialErr
		}
		configureQUICController(conn, ccfg)
		stream, streamErr := conn.OpenStreamSync(dialCtx)
		if streamErr != nil {
			_ = conn.CloseWithError(0, "unable to open wanopt stream")
			_ = packetConn.Close()
			return nil, streamErr
		}
		return &quicStreamConn{stream: stream, conn: conn, packet: packetConn}, nil
	}
	conn, err := quic.DialAddr(dialCtx, remote, tlsCfg, quicConfig())
	if err != nil {
		return nil, err
	}
	configureQUICController(conn, ccfg)
	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		_ = conn.CloseWithError(0, "unable to open wanopt stream")
		return nil, err
	}
	return &quicStreamConn{stream: stream, conn: conn}, nil
}

func configureQUICController(conn *quic.Conn, cfg congestionConfig) {
	if conn == nil {
		return
	}
	switch cfg.kind {
	case CongestionAdaptive:
		conn.SetCongestionControl(wancongestion.NewAdaptiveSender(conn.InitialPacketSize(), cfg.adaptiveMinBytesPerSec, cfg.adaptiveMaxBytesPerSec))
	case CongestionBrutal:
		if cfg.brutalBytesPerSecond > 0 {
			conn.SetCongestionControl(wancongestion.NewBrutalSender(cfg.brutalBytesPerSecond, false))
		}
	case CongestionReno, "":
		// Keep the controller selected by the QUIC implementation.
	default:
		// Configuration is validated before dialing. Fail-safe to the stock
		// controller if a future caller constructs an invalid config directly.
	}
}

func quicServerTLSConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{defaultALPN}}
}

func quicServerConfig() *quic.Config {
	cfg := quicConfig()
	cfg.MaxIncomingStreams = 1
	return cfg
}

func acceptQUICStream(ctx context.Context, conn *quic.Conn) (streamConn, error) {
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStreamConn{stream: stream, conn: conn}, nil
}

func transportError(kind TransportKind, err error) error {
	return fmt.Errorf("%s lane: %w", kind, err)
}
