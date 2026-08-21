package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/metrics"
	"github.com/bojieli/queqiao/internal/pep"
)

type providerManifest struct {
	Version   int                     `json:"version"`
	Providers []providerManifestEntry `json:"providers"`
}

type providerManifestEntry struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
	Listen  string `json:"listen"`
}

type providerClientConfig struct {
	name, profilePath, listen string
	profile                   identity.ClientProfile
}

type providerClientRuntime struct {
	config   providerClientConfig
	client   *pep.Client
	listener net.Listener
	logger   *slog.Logger
}

func loadProviderClients(manifestPath string) ([]providerClientConfig, error) {
	manifestPath, err := filepath.Abs(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("resolve provider manifest path: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read provider manifest %q: %w", manifestPath, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest providerManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode provider manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("provider manifest contains trailing data")
	}
	if manifest.Version != 1 {
		return nil, fmt.Errorf("unsupported provider manifest version %d", manifest.Version)
	}
	if len(manifest.Providers) == 0 {
		return nil, errors.New("provider manifest must contain at least one provider")
	}

	configs := make([]providerClientConfig, 0, len(manifest.Providers))
	names := make(map[string]struct{}, len(manifest.Providers))
	listeners := make(map[string]struct{}, len(manifest.Providers))
	profilePaths := make(map[string]struct{}, len(manifest.Providers))
	for i, entry := range manifest.Providers {
		if entry.Name == "" || entry.Name != strings.TrimSpace(entry.Name) || len(entry.Name) > 128 {
			return nil, fmt.Errorf("provider %d has an invalid name", i+1)
		}
		if _, exists := names[entry.Name]; exists {
			return nil, fmt.Errorf("duplicate provider name %q", entry.Name)
		}
		names[entry.Name] = struct{}{}

		listen, err := normalizeProviderListener(entry.Listen)
		if err != nil {
			return nil, fmt.Errorf("provider %q listen address: %w", entry.Name, err)
		}
		if _, exists := listeners[listen]; exists {
			return nil, fmt.Errorf("duplicate provider listener %q", listen)
		}
		listeners[listen] = struct{}{}

		if entry.Profile == "" {
			return nil, fmt.Errorf("provider %q profile path is required", entry.Name)
		}
		profilePath := entry.Profile
		if !filepath.IsAbs(profilePath) {
			profilePath = filepath.Join(filepath.Dir(manifestPath), profilePath)
		}
		profilePath, err = filepath.Abs(profilePath)
		if err != nil {
			return nil, fmt.Errorf("resolve provider %q profile path: %w", entry.Name, err)
		}
		profilePath = filepath.Clean(profilePath)
		if _, exists := profilePaths[profilePath]; exists {
			return nil, fmt.Errorf("duplicate provider profile path %q", profilePath)
		}
		profilePaths[profilePath] = struct{}{}
		configs = append(configs, providerClientConfig{
			name: entry.Name, profilePath: profilePath, listen: listen,
		})
	}

	profileInfos := make([]os.FileInfo, 0, len(configs))
	for _, config := range configs {
		info, err := os.Stat(config.profilePath)
		if err != nil {
			return nil, fmt.Errorf("inspect provider %q profile: %w", config.name, err)
		}
		for _, previous := range profileInfos {
			if os.SameFile(previous, info) {
				return nil, fmt.Errorf("provider %q profile duplicates another profile file", config.name)
			}
		}
		profileInfos = append(profileInfos, info)
	}
	for i := range configs {
		profile, err := identity.LoadClientProfile(configs[i].profilePath)
		if err != nil {
			return nil, fmt.Errorf("load provider %q profile %q: %w", configs[i].name, configs[i].profilePath, err)
		}
		configs[i].profile = profile
	}
	return configs, nil
}

func normalizeProviderListener(address string) (string, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return "", errors.New("must be a TCP host:port address")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("must use a literal loopback IP; SOCKS has no remote authentication")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return "", errors.New("must use a numeric port between 0 and 65535")
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
}

func newRuntimeClient(profile identity.ClientProfile, listen string, opts runtimeOptions, logger *slog.Logger, registry *metrics.Registry, sessionLimit *pep.SessionLimit) (*pep.Client, error) {
	credentials, err := profile.Credentials()
	if err != nil {
		return nil, err
	}
	return pep.NewClient(pep.ClientConfig{
		ListenAddr: listen, RemoteAddr: profile.Endpoint, LocalAddress: opts.localAddress,
		Credentials: credentials, ChunkSize: opts.chunkSize,
		DialTimeout: opts.dialTimeout, HandshakeTimeout: opts.handshakeTimeout,
		FlowIdleTimeout: opts.flowIdleTimeout, FlowMaxLifetime: opts.flowMaxLifetime,
		MaxSessions: opts.maxSessions, SessionLimit: sessionLimit, MaxPendingOpens: opts.maxPendingOpens, Transport: pep.TransportKind(opts.transport),
		TCPFallbackLanes: opts.tcpFallbackLanes, EnableQUICPool: opts.quicPool,
		WaitForOpenAcknowledgement: opts.waitForOpenAck, UDPOnStream: opts.udpOnStream,
		Congestion: pep.CongestionControlKind(opts.congestion), BrutalBytesPerSec: opts.brutalBytesPerSec,
		AdaptiveMinBytesSec: opts.adaptiveMinBytesSec, AdaptiveMaxBytesSec: opts.adaptiveMaxBytesSec,
		AggregateBytesPerSec: opts.aggregateBytesPerSec, InteractiveReserveBytesPerSec: opts.interactiveReserveBytesPerSec,
		FallbackDelay: opts.fallbackDelay, FallbackGrace: opts.fallbackGrace,
		UDPFailureThreshold: opts.udpFailureThreshold, UDPCooldown: opts.udpCooldown,
		Metrics: registry, Logger: logger,
	})
}

func renewClientProfile(ctx context.Context, profilePath string, profile identity.ClientProfile, opts runtimeOptions, logger *slog.Logger) (identity.ClientProfile, error) {
	needs, err := profile.NeedsRenewal(time.Now(), 7*24*time.Hour)
	if err != nil {
		return profile, err
	}
	if !needs {
		return profile, nil
	}
	renewed, err := identity.RenewProfileWithOptions(ctx, profile, identity.DialOptions{Timeout: opts.handshakeTimeout, LocalAddress: opts.localAddress})
	if err != nil {
		logger.Warn("automatic certificate renewal failed; continuing with current valid identity", "error", err)
		return profile, nil
	}
	if err := renewed.Save(profilePath); err != nil {
		return profile, fmt.Errorf("save renewed profile: %w", err)
	}
	logger.Info("device identity renewed")
	return renewed, nil
}

func runProviderClients(manifestPath string, noAutoRenew bool, opts runtimeOptions, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runProviderClientsContext(ctx, manifestPath, noAutoRenew, opts, logger)
}

func runProviderClientsContext(ctx context.Context, manifestPath string, noAutoRenew bool, opts runtimeOptions, logger *slog.Logger) error {
	configs, err := loadProviderClients(manifestPath)
	if err != nil {
		return err
	}
	registry := metrics.New()
	sessionLimit, err := pep.NewSessionLimit(opts.maxSessions)
	if err != nil {
		return err
	}
	runtimes := make([]providerClientRuntime, 0, len(configs))
	closeListeners := func() {
		for _, runtime := range runtimes {
			if runtime.listener != nil {
				_ = runtime.listener.Close()
			}
		}
	}

	for _, config := range configs {
		providerLogger := logger.With("provider", config.name, "listener", config.listen)
		profile := config.profile
		if !noAutoRenew {
			profile, err = renewClientProfile(ctx, config.profilePath, profile, opts, providerLogger)
			if err != nil {
				providerLogger.Warn("automatic certificate renewal failed; continuing with current valid identity", "error", err)
				profile = config.profile
			}
		}
		config.profile = profile
		client, err := newRuntimeClient(profile, config.listen, opts, providerLogger, registry, sessionLimit)
		if err != nil {
			return fmt.Errorf("configure provider %q: %w", config.name, err)
		}
		providerOpts := opts
		providerOpts.listen = config.listen
		logRuntimeConfiguration(providerLogger, providerOpts, true)
		runtimes = append(runtimes, providerClientRuntime{config: config, client: client, logger: providerLogger})
	}

	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	for i := range runtimes {
		listener, err := lc.Listen(ctx, "tcp", runtimes[i].config.listen)
		if err != nil {
			closeListeners()
			return fmt.Errorf("bind provider %q listener %q: %w", runtimes[i].config.name, runtimes[i].config.listen, err)
		}
		runtimes[i].listener = listener
	}
	logger.Info("all provider listeners bound", "providers", len(runtimes))

	stopMetrics, err := serveMetrics(opts.metricsListen, registry, logger)
	if err != nil {
		closeListeners()
		return err
	}
	defer stopMetrics()
	stopTelemetry := startTelemetryLog(ctx, opts.telemetryLogInterval, registry, logger)
	defer stopTelemetry()

	type serveResult struct {
		name string
		err  error
	}
	results := make(chan serveResult, len(runtimes))
	for i := range runtimes {
		runtime := &runtimes[i]
		if !noAutoRenew {
			go maintainClientIdentity(ctx, runtime.config.profilePath, runtime.config.profile, runtime.client, opts.handshakeTimeout, opts.localAddress, runtime.logger)
		}
		go func() {
			results <- serveResult{name: runtime.config.name, err: runtime.client.ServeListener(ctx, runtime.listener)}
		}()
	}

	remaining := len(runtimes)
	var firstErr error
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			if result.err != nil {
				logger.Error("provider client stopped", "provider", result.name, "error", result.err)
				if firstErr == nil {
					firstErr = fmt.Errorf("provider %q stopped: %w", result.name, result.err)
				}
			}
		case <-ctx.Done():
			for remaining > 0 {
				<-results
				remaining--
			}
			return firstErr
		}
	}
	return firstErr
}
