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
	"sort"
	"strings"
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
// quic-go default untouched and is the safe control. BBR is the original
// wanopt controller. BBRTUIC is a faithful Go port of TUIC's
// quinn-congestions BBR model and remains opt-in until matched path campaigns
// establish that it is an improvement. Adaptive is a conservative
// rate-estimating controller for unknown paths. Brutal is a fixed-rate mode
// for controlled experiments where the operator knows the per-lane budget.
type CongestionControlKind string

const (
	CongestionReno     CongestionControlKind = "reno"
	CongestionBBR      CongestionControlKind = "bbr"
	CongestionBBRTUIC  CongestionControlKind = "bbr-tuic"
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
	controller               wancongestion.ControllerTelemetry
}

type laneStatsProvider interface {
	transportStats() laneTransportStats
}

type quicStreamConn struct {
	stream     *quic.Stream
	conn       *quic.Conn
	packet     net.PacketConn
	controller wancongestion.TelemetryProvider
	// closeConn is true for a dedicated lane. Streams obtained from the
	// client pool and streams accepted by the server must only close their
	// stream; closing the connection would tear down unrelated flows.
	closeConn bool
	once      sync.Once
}

func (c *quicStreamConn) transportStats() laneTransportStats {
	if c == nil || c.conn == nil {
		return laneTransportStats{}
	}
	s := c.conn.ConnectionStats()
	stats := laneTransportStats{
		latestRTT: s.LatestRTT, smoothedRTT: s.SmoothedRTT,
		bytesSent: s.BytesSent, bytesReceived: s.BytesReceived,
		bytesLost: s.BytesLost, packetsLost: s.PacketsLost,
	}
	if c.controller != nil {
		stats.controller = c.controller.Telemetry()
	}
	return stats
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
		_ = c.stream.Close()
		if c.closeConn {
			// Dedicated lanes own their QUIC connection and socket. Pooled
			// streams deliberately leave both alive for other flows.
			err = c.conn.CloseWithError(0, "wanopt lane closed")
			if c.packet != nil {
				_ = c.packet.Close()
			}
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
		ip, err := resolveLocalAddress(localAddress)
		if err != nil {
			return nil, err
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

// Flow-control windows are the single largest measured determinant of
// long-haul goodput for this transport, so they are named constants rather
// than inline literals.
//
// quic-go auto-tunes its receive windows upward from an initial value, but the
// growth heuristic requires the receiver to consume a large fraction of the
// window within a small multiple of the RTT. On a 200 ms path with a few
// percent packet loss, loss recovery delays consumption enough that the
// window stops growing, and the *receive window* rather than congestion
// control becomes the binding constraint. Measured with cmd/wanoptbench at
// 200 ms RTT and 1--5% loss, a 512 KiB initial stream window cost 30--40%
// goodput against an otherwise identical TUIC-shaped reference.
//
// TUIC (via quinn) instead uses a fixed 8 MiB stream receive window and a
// 16 MiB connection send window with no ramp at all. These constants match it
// exactly, initial and maximum alike, so no ramp remains.
//
// Allowing the window to auto-tune *above* that point was measured and
// rejected. It bought a little bulk goodput (58.5--64.8 against 55.4--58.5
// Mbit/s on a 50 MiB transfer) by letting the sender hold a deeper standing
// queue at the bottleneck, and it cost far more than it bought at the tail:
// interactive requests issued during that transfer went from a 489--701 ms
// 95th percentile to 976--1062 ms, and from an 883 ms worst case to 1339 ms.
// Protecting interactive latency under bulk load is the point of this
// transport, so the ceiling stays where TUIC puts it.
//
// The connection window is what bounds per-connection receive memory: it caps
// the aggregate across all streams, so a large per-stream window does not
// multiply by the stream limit.
const (
	initialStreamReceiveWindow     = 8 * 1024 * 1024
	initialConnectionReceiveWindow = 16 * 1024 * 1024
	// A bounded stream fan-out lets one QUIC connection carry multiple
	// independent PEP flows, like TUIC, without an unbounded stream commitment.
	maxIncomingStreams = 128
)

// flowWindows selects the QUIC receive windows. A zero field takes the
// default. They are configurable because they are the single largest measured
// determinant of long-haul goodput and because their correct value is a
// property of the path, not of the code: the defaults match TUIC, which is the
// right answer for the paths this project targets, but a much fatter or much
// thinner path wants a different one.
type flowWindows struct {
	stream     uint64
	connection uint64
}

func (w flowWindows) resolved() (stream, connection uint64) {
	stream, connection = w.stream, w.connection
	if stream == 0 {
		stream = initialStreamReceiveWindow
	}
	if connection == 0 {
		connection = initialConnectionReceiveWindow
	}
	return stream, connection
}

func quicConfig(windows flowWindows) *quic.Config {
	streamWindow, connectionWindow := windows.resolved()
	return &quic.Config{
		HandshakeIdleTimeout: 10 * time.Second,
		// Existing-flow TCP rescue cannot begin until QUIC declares the
		// black-holed lane dead. Keep this bound well below application-level
		// request timeouts while allowing several PTOs on a 200 ms WAN.
		MaxIdleTimeout:                 15 * time.Second,
		KeepAlivePeriod:                5 * time.Second,
		InitialStreamReceiveWindow:     streamWindow,
		MaxStreamReceiveWindow:         streamWindow,
		InitialConnectionReceiveWindow: connectionWindow,
		MaxConnectionReceiveWindow:     connectionWindow,
		MaxIncomingStreams:             maxIncomingStreams,
		MaxIncomingUniStreams:          0,
		// The China path has a smaller effective UDP MTU than this host's
		// interface. Disable probing until path-specific MTU discovery is
		// available; otherwise a successful probe can raise packets above the
		// path MTU and stall a long response.
		DisablePathMTUDiscovery: true,
		InitialPacketSize:       1200,
	}
}

func dialQUIC(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration, localAddress string, ccfg congestionConfig, windows flowWindows) (streamConn, error) {
	conn, packetConn, err := dialQUICConnection(ctx, remote, serverName, roots, dialTimeout, localAddress, windows)
	if err != nil {
		return nil, err
	}
	controller := configureQUICController(conn, ccfg)
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "unable to open wanopt stream")
		if packetConn != nil {
			_ = packetConn.Close()
		}
		return nil, err
	}
	return &quicStreamConn{stream: stream, conn: conn, packet: packetConn, controller: controller, closeConn: true}, nil
}

// dialQUICConnection establishes only the QUIC connection. Keeping this
// separate from stream creation allows the client to pool one connection and
// open a stream for each logical flow without paying another handshake.
func dialQUICConnection(ctx context.Context, remote, serverName string, roots *x509.CertPool, dialTimeout time.Duration, localAddress string, windows flowWindows) (*quic.Conn, net.PacketConn, error) {
	dialCtx := ctx
	var cancel context.CancelFunc
	if dialTimeout > 0 {
		dialCtx, cancel = context.WithTimeout(ctx, dialTimeout)
		defer cancel()
	}
	tlsCfg := tlsClientConfig(serverName, roots)
	if localAddress != "" {
		ip, parseErr := resolveLocalAddress(localAddress)
		if parseErr != nil {
			return nil, nil, parseErr
		}
		remoteAddr, resolveErr := net.ResolveUDPAddr("udp", remote)
		if resolveErr != nil {
			return nil, nil, resolveErr
		}
		packetConn, listenErr := net.ListenUDP("udp", &net.UDPAddr{IP: ip.AsSlice()})
		if listenErr != nil {
			return nil, nil, listenErr
		}
		conn, dialErr := quic.Dial(dialCtx, packetConn, remoteAddr, tlsCfg, quicConfig(windows))
		if dialErr != nil {
			_ = packetConn.Close()
			return nil, nil, dialErr
		}
		return conn, packetConn, nil
	}
	conn, err := quic.DialAddr(dialCtx, remote, tlsCfg, quicConfig(windows))
	if err != nil {
		return nil, nil, err
	}
	return conn, nil, nil
}

// validateLocalAddressSpec checks syntax without requiring the address or
// interface to be present at process startup. DHCP and interface state can
// change after startup; resolution is therefore repeated for every outer
// dial by resolveLocalAddress.
func validateLocalAddressSpec(spec string) error {
	if spec == "" || spec == "auto" {
		return nil
	}
	if strings.HasPrefix(spec, "if:") {
		if strings.TrimSpace(strings.TrimPrefix(spec, "if:")) == "" {
			return errors.New("local interface name must not be empty")
		}
		return nil
	}
	if _, err := netip.ParseAddr(spec); err != nil {
		return fmt.Errorf("parse local address %q: %w", spec, err)
	}
	return nil
}

type localAddressCandidate struct {
	interfaceName string
	address       netip.Addr
}

// resolveLocalAddress supports a literal IP, `if:NAME`, or `auto`. Interface
// and automatic modes deliberately consider only IPv4 addresses on active,
// non-loopback, non-point-to-point interfaces: the fixed deployment endpoint
// is IPv4, and excluding point-to-point links prevents selecting the Clash
// TUN itself. Ambiguity is an error rather than silently routing the optimizer
// through an unintended NIC.
func resolveLocalAddress(spec string) (netip.Addr, error) {
	if err := validateLocalAddressSpec(spec); err != nil {
		return netip.Addr{}, err
	}
	if spec != "auto" && !strings.HasPrefix(spec, "if:") {
		return netip.ParseAddr(spec)
	}

	wantedInterface := ""
	if strings.HasPrefix(spec, "if:") {
		wantedInterface = strings.TrimSpace(strings.TrimPrefix(spec, "if:"))
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("enumerate local interfaces: %w", err)
	}
	candidates := make([]localAddressCandidate, 0, 2)
	for _, iface := range interfaces {
		if wantedInterface != "" && iface.Name != wantedInterface {
			continue
		}
		if iface.Flags&net.FlagUp == 0 || iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addresses, addressErr := iface.Addrs()
		if addressErr != nil {
			continue
		}
		for _, raw := range addresses {
			prefix, parseErr := netip.ParsePrefix(raw.String())
			if parseErr != nil {
				continue
			}
			address := prefix.Addr().Unmap()
			if !address.Is4() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
				continue
			}
			candidates = append(candidates, localAddressCandidate{interfaceName: iface.Name, address: address})
		}
	}
	if len(candidates) == 0 {
		if wantedInterface != "" {
			return netip.Addr{}, fmt.Errorf("local interface %q has no active IPv4 address", wantedInterface)
		}
		return netip.Addr{}, errors.New("no active non-tunnel IPv4 address found")
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].interfaceName != candidates[j].interfaceName {
			return candidates[i].interfaceName < candidates[j].interfaceName
		}
		return candidates[i].address.Less(candidates[j].address)
	})
	first := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.address != first.address {
			return netip.Addr{}, fmt.Errorf("multiple physical IPv4 addresses found (%s on %s and %s on %s); use a literal IP or if:NAME", first.address, first.interfaceName, candidate.address, candidate.interfaceName)
		}
	}
	return first.address, nil
}

func configureQUICController(conn *quic.Conn, cfg congestionConfig) wancongestion.TelemetryProvider {
	if conn == nil {
		return nil
	}
	switch cfg.kind {
	case CongestionBBR:
		controller := wancongestion.NewBBRSender(conn.InitialPacketSize())
		conn.SetCongestionControl(controller)
		return controller
	case CongestionBBRTUIC:
		controller := wancongestion.NewTUICBBRSender(conn.InitialPacketSize())
		conn.SetCongestionControl(controller)
		return controller
	case CongestionAdaptive:
		controller := wancongestion.NewAdaptiveSender(conn.InitialPacketSize(), cfg.adaptiveMinBytesPerSec, cfg.adaptiveMaxBytesPerSec)
		conn.SetCongestionControl(controller)
		return controller
	case CongestionBrutal:
		if cfg.brutalBytesPerSecond > 0 {
			controller := wancongestion.NewBrutalSender(cfg.brutalBytesPerSecond, false)
			conn.SetCongestionControl(controller)
			return controller
		}
	case CongestionReno, "":
		// Keep the controller selected by the QUIC implementation.
	default:
		// Configuration is validated before dialing. Fail-safe to the stock
		// controller if a future caller constructs an invalid config directly.
	}
	return nil
}

func quicServerTLSConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, NextProtos: []string{defaultALPN}}
}

func quicServerConfig(windows flowWindows) *quic.Config {
	cfg := quicConfig(windows)
	return cfg
}

func acceptQUICStream(ctx context.Context, conn *quic.Conn, controller wancongestion.TelemetryProvider) (streamConn, error) {
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &quicStreamConn{stream: stream, conn: conn, controller: controller, closeConn: false}, nil
}

func transportError(kind TransportKind, err error) error {
	return fmt.Errorf("%s lane: %w", kind, err)
}
