package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/icourses-dev/wanopt/internal/pep"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type options struct {
	mode                          string
	listen                        string
	remote                        string
	localAddress                  string
	serverName                    string
	secretFile                    string
	certFile                      string
	keyFile                       string
	rootCAFile                    string
	maxSessions                   int
	maxPayload                    uint
	chunkSize                     int
	dialTimeout                   time.Duration
	handshakeTimeout              time.Duration
	flowIdleTimeout               time.Duration
	flowMaxLifetime               time.Duration
	transport                     string
	quicPool                      bool
	optimisticOpen                bool
	congestion                    string
	codedLanes                    bool
	brutalBytesPerSec             uint64
	adaptiveMinBytesSec           uint64
	adaptiveMaxBytesSec           uint64
	aggregateBytesPerSec          uint64
	interactiveReserveBytesPerSec uint64
	fallbackDelay                 time.Duration
	udpFailureThreshold           int
	udpCooldown                   time.Duration
	initialLanes                  int
	maxLanes                      int
	bulkStartLanes                int
	allowPrivate                  bool
	logLevel                      string
	jsonLogs                      bool
	metricsListen                 string
	showVersion                   bool
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "wanoptd: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	opts, err := parseOptions(args)
	if err != nil {
		return err
	}
	if opts.showVersion {
		fmt.Printf("wanoptd %s commit=%s built=%s go=%s\n", version, commit, buildDate, goVersion())
		return nil
	}
	logger, err := newLogger(opts.logLevel, opts.jsonLogs)
	if err != nil {
		return err
	}
	secret, err := loadSecret(opts.secretFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch opts.mode {
	case "local":
		roots, err := loadRootCAs(opts.rootCAFile)
		if err != nil {
			return err
		}
		client, err := pep.NewClient(pep.ClientConfig{
			ListenAddr: opts.listen, RemoteAddr: opts.remote, ServerName: opts.serverName,
			LocalAddress: opts.localAddress,
			Secret:       secret, RootCAs: roots, MaxPayload: uint32(opts.maxPayload), ChunkSize: opts.chunkSize,
			DialTimeout: opts.dialTimeout, HandshakeTimeout: opts.handshakeTimeout,
			FlowIdleTimeout: opts.flowIdleTimeout, FlowMaxLifetime: opts.flowMaxLifetime,
			MaxSessions: opts.maxSessions, Transport: pep.TransportKind(opts.transport),
			EnableQUICPool: opts.quicPool,
			OptimisticOpen: opts.optimisticOpen,
			Congestion:     pep.CongestionControlKind(opts.congestion), BrutalBytesPerSec: opts.brutalBytesPerSec,
			CodedLanes:          opts.codedLanes,
			AdaptiveMinBytesSec: opts.adaptiveMinBytesSec, AdaptiveMaxBytesSec: opts.adaptiveMaxBytesSec,
			AggregateBytesPerSec: opts.aggregateBytesPerSec, InteractiveReserveBytesPerSec: opts.interactiveReserveBytesPerSec,
			FallbackDelay: opts.fallbackDelay, UDPFailureThreshold: opts.udpFailureThreshold,
			UDPCooldown: opts.udpCooldown, InitialLanes: opts.initialLanes,
			MaxLanes: opts.maxLanes, BulkStartLanes: opts.bulkStartLanes, Logger: logger,
		})
		if err != nil {
			return err
		}
		stopMetrics, err := serveMetrics(opts.metricsListen, client.Metrics(), logger)
		if err != nil {
			return err
		}
		defer stopMetrics()
		return client.Serve(ctx)
	case "server":
		certificate, err := tls.LoadX509KeyPair(opts.certFile, opts.keyFile)
		if err != nil {
			return fmt.Errorf("load server TLS certificate: %w", err)
		}
		server, err := pep.NewServer(pep.ServerConfig{
			ListenAddr: opts.listen, Certificate: certificate, Secret: secret,
			MaxPayload: uint32(opts.maxPayload), ChunkSize: opts.chunkSize,
			HandshakeTimeout: opts.handshakeTimeout, FlowIdleTimeout: opts.flowIdleTimeout,
			FlowMaxLifetime: opts.flowMaxLifetime, MaxSessions: opts.maxSessions,
			DestinationPolicy: pep.DestinationPolicy{AllowPrivate: opts.allowPrivate, DialTimeout: opts.dialTimeout},
			EnableTCP:         opts.transport == string(pep.TransportTCP) || opts.transport == string(pep.TransportAuto),
			EnableQUIC:        opts.transport == string(pep.TransportQUIC) || opts.transport == string(pep.TransportAuto),
			Congestion:        pep.CongestionControlKind(opts.congestion), BrutalBytesPerSec: opts.brutalBytesPerSec,
			AdaptiveMinBytesSec: opts.adaptiveMinBytesSec, AdaptiveMaxBytesSec: opts.adaptiveMaxBytesSec,
			AggregateBytesPerSec: opts.aggregateBytesPerSec, InteractiveReserveBytesPerSec: opts.interactiveReserveBytesPerSec,
			MaxLanes: opts.maxLanes,
			Logger:   logger,
		})
		if err != nil {
			return err
		}
		stopMetrics, err := serveMetrics(opts.metricsListen, server.Metrics(), logger)
		if err != nil {
			return err
		}
		defer stopMetrics()
		return server.Serve(ctx)
	default:
		return errors.New("--mode must be local or server")
	}
}

func parseOptions(args []string) (options, error) {
	var opts options
	fs := flag.NewFlagSet("wanoptd", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&opts.mode, "mode", "", "agent mode: local or server")
	fs.StringVar(&opts.listen, "listen", "", "local SOCKS5 or remote agent listen address")
	fs.StringVar(&opts.remote, "remote", "", "remote agent host:port in local mode")
	fs.StringVar(&opts.localAddress, "local-address", "", "optional local source IP, auto, or if:NAME for outer lanes (bypasses a host TUN route)")
	fs.StringVar(&opts.serverName, "server-name", "", "verified TLS DNS name in local mode")
	fs.StringVar(&opts.secretFile, "secret-file", "", "path to pre-shared session secret")
	fs.StringVar(&opts.certFile, "tls-cert", "", "server TLS certificate path")
	fs.StringVar(&opts.keyFile, "tls-key", "", "server TLS private-key path")
	fs.StringVar(&opts.rootCAFile, "root-ca", "", "optional PEM root CA path in local mode")
	fs.IntVar(&opts.maxSessions, "max-sessions", 1024, "maximum concurrent sessions")
	fs.UintVar(&opts.maxPayload, "max-payload", 256*1024, "maximum frame payload in bytes")
	fs.IntVar(&opts.chunkSize, "chunk-size", 32*1024, "stream data frame size in bytes")
	fs.DurationVar(&opts.dialTimeout, "dial-timeout", 10*time.Second, "destination or remote dial timeout")
	fs.DurationVar(&opts.handshakeTimeout, "handshake-timeout", 10*time.Second, "SOCKS, TLS, and session handshake timeout")
	fs.DurationVar(&opts.flowIdleTimeout, "flow-idle-timeout", 30*time.Minute, "maximum application-idle period before a flow is reset")
	fs.DurationVar(&opts.flowMaxLifetime, "flow-max-lifetime", 24*time.Hour, "maximum lifetime of one logical flow")
	fs.StringVar(&opts.transport, "transport", string(pep.TransportAuto), "outer transport: auto, quic, or tcp")
	fs.BoolVar(&opts.quicPool, "quic-pool", false, "share one persistent QUIC connection for initial/control streams, and move classified bulk flows off it (opt-in)")
	fs.BoolVar(&opts.optimisticOpen, "optimistic-open", false, "return SOCKS success before OPEN_OK; flow validates the eventual response (opt-in)")
	fs.BoolVar(&opts.codedLanes, "coded-lanes", false, "carry flows over QUIC datagrams repaired by an erasure code instead of reliable streams; for interactive traffic on a lossy path")
	fs.StringVar(&opts.congestion, "congestion", string(pep.CongestionReno), "QUIC congestion controller: reno, bbr, bbr-tuic, erasure, adaptive, or brutal")
	fs.Uint64Var(&opts.brutalBytesPerSec, "brutal-bytes-per-sec", 0, "fixed per-lane Brutal target in bytes/s (required with --congestion brutal)")
	fs.Uint64Var(&opts.adaptiveMinBytesSec, "adaptive-min-bytes-per-sec", 64*1024, "Adaptive controller minimum rate in bytes/s")
	fs.Uint64Var(&opts.adaptiveMaxBytesSec, "adaptive-max-bytes-per-sec", 200*1024*1024, "Adaptive controller maximum rate in bytes/s")
	fs.Uint64Var(&opts.aggregateBytesPerSec, "aggregate-bytes-per-sec", 0, "optional aggregate byte budget shared by all lanes and flows (0 disables)")
	fs.Uint64Var(&opts.interactiveReserveBytesPerSec, "interactive-reserve-bytes-per-sec", 0, "reserved aggregate budget for new/interactive traffic")
	fs.DurationVar(&opts.fallbackDelay, "fallback-delay", 300*time.Millisecond, "delay before starting TCP fallback in auto mode")
	fs.IntVar(&opts.udpFailureThreshold, "udp-failure-threshold", 3, "consecutive UDP failures before temporary TCP-only mode")
	fs.DurationVar(&opts.udpCooldown, "udp-cooldown", 30*time.Second, "how long to suppress UDP after repeated failures")
	fs.IntVar(&opts.initialLanes, "initial-lanes", 1, "number of QUIC lanes to open after a flow is established (1-8)")
	fs.IntVar(&opts.maxLanes, "max-lanes", 4, "ceiling on QUIC lanes carrying bulk payload per flow (1-8); lanes are added only when a controlled probe measures a gain, so this is a bound and not a target. A negotiated control lane is additional; set 1 to disable striping")
	fs.IntVar(&opts.bulkStartLanes, "bulk-start-lanes", 1, "bulk lanes to open when a flow becomes bulk (growth beyond this requires measured gain)")
	fs.BoolVar(&opts.allowPrivate, "allow-private-destinations", false, "allow the server to reach private/link-local destinations")
	fs.StringVar(&opts.logLevel, "log-level", "info", "debug, info, warn, or error")
	fs.BoolVar(&opts.jsonLogs, "json-logs", false, "write structured JSON logs")
	fs.StringVar(&opts.metricsListen, "metrics-listen", "", "optional local metrics HTTP listen address (serves /metrics)")
	fs.BoolVar(&opts.showVersion, "version", false, "print build version")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	if fs.NArg() != 0 {
		return opts, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if opts.showVersion {
		return opts, nil
	}
	if opts.mode != "local" && opts.mode != "server" {
		return opts, errors.New("--mode must be local or server")
	}
	if opts.listen == "" {
		return opts, errors.New("--listen is required")
	}
	if opts.secretFile == "" {
		return opts, errors.New("--secret-file is required")
	}
	if opts.maxPayload == 0 || opts.maxPayload > 1<<20 {
		return opts, errors.New("--max-payload must be between 1 and 1048576")
	}
	if opts.chunkSize <= 0 || uint(opts.chunkSize) > opts.maxPayload {
		return opts, errors.New("--chunk-size must be positive and no larger than --max-payload")
	}
	if opts.maxSessions < 1 || opts.maxSessions > 1<<16 {
		return opts, errors.New("--max-sessions must be between 1 and 65536")
	}
	if opts.transport != string(pep.TransportAuto) && opts.transport != string(pep.TransportQUIC) && opts.transport != string(pep.TransportTCP) {
		return opts, errors.New("--transport must be auto, quic, or tcp")
	}
	switch pep.CongestionControlKind(opts.congestion) {
	case pep.CongestionReno, pep.CongestionBBR, pep.CongestionBBRTUIC,
		pep.CongestionErasure, pep.CongestionAdaptive, pep.CongestionBrutal:
	default:
		return opts, errors.New("--congestion must be reno, bbr, bbr-tuic, erasure, adaptive, or brutal")
	}
	if opts.congestion == string(pep.CongestionBrutal) && opts.brutalBytesPerSec == 0 {
		return opts, errors.New("--brutal-bytes-per-sec is required with --congestion brutal")
	}
	if opts.adaptiveMinBytesSec == 0 || opts.adaptiveMaxBytesSec < opts.adaptiveMinBytesSec {
		return opts, errors.New("invalid adaptive byte-rate bounds")
	}
	if opts.aggregateBytesPerSec == 0 && opts.interactiveReserveBytesPerSec != 0 {
		return opts, errors.New("interactive reserve requires an aggregate byte budget")
	}
	if opts.interactiveReserveBytesPerSec > opts.aggregateBytesPerSec {
		return opts, errors.New("interactive reserve cannot exceed aggregate byte budget")
	}
	if opts.fallbackDelay < 0 || opts.udpFailureThreshold < 1 || opts.udpCooldown <= 0 {
		return opts, errors.New("invalid UDP fallback settings")
	}
	if opts.flowIdleTimeout <= 0 || opts.flowMaxLifetime <= 0 || opts.flowIdleTimeout > opts.flowMaxLifetime {
		return opts, errors.New("flow idle timeout must be positive and no longer than flow max lifetime")
	}
	if opts.initialLanes < 1 || opts.initialLanes > 8 {
		return opts, errors.New("--initial-lanes must be between 1 and 8")
	}
	if opts.maxLanes < 1 || opts.maxLanes > 8 || opts.bulkStartLanes < 1 || opts.bulkStartLanes > opts.maxLanes {
		return opts, errors.New("invalid lane settings")
	}
	if opts.mode == "local" {
		if opts.remote == "" || opts.serverName == "" {
			return opts, errors.New("--remote and --server-name are required in local mode")
		}
		if opts.certFile != "" || opts.keyFile != "" || opts.allowPrivate {
			return opts, errors.New("server-only flags used in local mode")
		}
	} else {
		if opts.certFile == "" || opts.keyFile == "" {
			return opts, errors.New("--tls-cert and --tls-key are required in server mode")
		}
		if opts.remote != "" || opts.serverName != "" || opts.rootCAFile != "" || opts.localAddress != "" || opts.quicPool || opts.optimisticOpen {
			return opts, errors.New("local-only flags used in server mode")
		}
	}
	return opts, nil
}

func loadSecret(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session secret: %w", err)
	}
	secret := bytes.TrimSpace(data)
	if len(secret) < 32 {
		return nil, errors.New("session secret file must contain at least 32 non-whitespace bytes")
	}
	return append([]byte(nil), secret...), nil
}

func loadRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read root CA: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("root CA file did not contain a valid PEM certificate")
	}
	return pool, nil
}

func newLogger(level string, json bool) (*slog.Logger, error) {
	var slogLevel slog.Level
	switch strings.ToLower(level) {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn", "warning":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		return nil, fmt.Errorf("unsupported log level %q", level)
	}
	handlerOptions := &slog.HandlerOptions{Level: slogLevel}
	if json {
		return slog.New(slog.NewJSONHandler(os.Stderr, handlerOptions)), nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, handlerOptions)), nil
}

func goVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.GoVersion
	}
	return "unknown"
}

func serveMetrics(addr string, handler http.Handler, logger *slog.Logger) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen metrics endpoint: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics endpoint stopped", "error", err)
		}
	}()
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}, nil
}
