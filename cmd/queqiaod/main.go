package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/netbind"
	"github.com/bojieli/queqiao/internal/pep"
	"github.com/bojieli/queqiao/internal/protocol"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "queqiaod: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("a command is required: provider, enroll, client, server, or version")
	}
	switch args[0] {
	case "version", "--version", "-version":
		if len(args) != 1 {
			return errors.New("version takes no arguments")
		}
		fmt.Printf("queqiaod %s commit=%s built=%s go=%s wire=%d\n", version, commit, buildDate, goVersion(), protocol.Version)
		return nil
	case "provider":
		return runProvider(args[1:])
	case "enroll":
		return runEnroll(args[1:])
	case "client":
		return runClient(args[1:])
	case "server":
		return runServer(args[1:])
	default:
		return fmt.Errorf("unknown command %q; want provider, enroll, client, server, or version", args[0])
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func requireNoArguments(fs *flag.FlagSet) error {
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	return nil
}

func runProvider(args []string) error {
	if len(args) == 0 {
		return errors.New("provider command is required: init, add-user, list-users, invite, list-invites, revoke-invite, list-devices, revoke-device, enable-user, or disable-user")
	}
	switch args[0] {
	case "init":
		fs := newFlagSet("provider init")
		state := fs.String("state", "", "new provider state directory")
		name := fs.String("name", "", "provider display name")
		endpoint := fs.String("endpoint", "", "public gateway host:port")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		if *state == "" || *name == "" || *endpoint == "" {
			return errors.New("--state, --name, and --endpoint are required")
		}
		provider, err := identity.InitProvider(*state, *name, *endpoint, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("Provider %q initialized.\nID: %s\nGateway: %s\nState: %s\n", provider.Metadata.Name, provider.Metadata.ProviderID, provider.Metadata.Endpoint, provider.Directory)
		return nil
	case "add-user":
		fs := newFlagSet("provider add-user")
		state := fs.String("state", "", "provider state directory")
		name := fs.String("name", "", "unique user name")
		expiresIn := fs.Duration("expires-in", 0, "optional account lifetime (0 never expires)")
		maxSessions := fs.Int("max-sessions", 0, "concurrent flows for this user (0 uses provider limit)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		var expires time.Time
		if *expiresIn < 0 {
			return errors.New("--expires-in cannot be negative")
		}
		if *expiresIn > 0 {
			expires = time.Now().Add(*expiresIn)
		}
		account, err := provider.Store.AddAccount(*name, expires, *maxSessions, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("User %q created.\nID: %s\n", account.Name, account.ID)
		return nil
	case "list-users":
		fs := newFlagSet("provider list-users")
		state := fs.String("state", "", "provider state directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		fmt.Println("ID\tNAME\tENABLED\tEXPIRES\tMAX_SESSIONS")
		for _, account := range provider.Store.Accounts() {
			expires := account.ExpiresAt
			if expires == "" {
				expires = "never"
			}
			fmt.Printf("%s\t%s\t%t\t%s\t%d\n", account.ID, account.Name, account.Enabled, expires, account.MaxSessions)
		}
		return nil
	case "invite":
		fs := newFlagSet("provider invite")
		state := fs.String("state", "", "provider state directory")
		user := fs.String("user", "", "user name or ID")
		expiresIn := fs.Duration("expires-in", 24*time.Hour, "one-time invitation lifetime (maximum 7d)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		uri, _, err := provider.CreateInvitation(*user, *expiresIn, time.Now())
		if err != nil {
			return err
		}
		// stdout is intentionally only the importable value, making it safe to
		// pipe into a QR encoder or provider portal.
		fmt.Println(uri)
		return nil
	case "list-invites":
		fs := newFlagSet("provider list-invites")
		state := fs.String("state", "", "provider state directory")
		user := fs.String("user", "", "optional user name or ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		accountID := ""
		if *user != "" {
			account, ok := provider.Store.FindAccount(*user)
			if !ok {
				return errors.New("unknown user")
			}
			accountID = account.ID
		}
		fmt.Println("ID\tACCOUNT_ID\tCREATED\tEXPIRES")
		for _, invitation := range provider.Store.Invites(accountID, time.Now()) {
			fmt.Printf("%s\t%s\t%s\t%s\n", invitation.ID, invitation.AccountID, invitation.CreatedAt, invitation.ExpiresAt)
		}
		return nil
	case "revoke-invite":
		fs := newFlagSet("provider revoke-invite")
		state := fs.String("state", "", "provider state directory")
		invitation := fs.String("invite", "", "outstanding invitation ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		if err := provider.Store.RevokeInvite(*invitation); err != nil {
			return err
		}
		fmt.Printf("Invitation %s revoked.\n", *invitation)
		return nil
	case "list-devices":
		fs := newFlagSet("provider list-devices")
		state := fs.String("state", "", "provider state directory")
		user := fs.String("user", "", "optional user name or ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		accountID := ""
		if *user != "" {
			account, ok := provider.Store.FindAccount(*user)
			if !ok {
				return errors.New("unknown user")
			}
			accountID = account.ID
		}
		fmt.Println("ID\tACCOUNT_ID\tNAME\tENABLED\tCREATED\tREVOKED")
		for _, device := range provider.Store.Devices(accountID) {
			revoked := device.RevokedAt
			if revoked == "" {
				revoked = "-"
			}
			fmt.Printf("%s\t%s\t%s\t%t\t%s\t%s\n", device.ID, device.AccountID, device.Name, device.Enabled, device.CreatedAt, revoked)
		}
		return nil
	case "revoke-device":
		fs := newFlagSet("provider revoke-device")
		state := fs.String("state", "", "provider state directory")
		device := fs.String("device", "", "device ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		if err := provider.Store.RevokeDevice(*device, time.Now()); err != nil {
			return err
		}
		fmt.Printf("Device %s revoked.\n", *device)
		return nil
	case "enable-user", "disable-user":
		fs := newFlagSet("provider " + args[0])
		state := fs.String("state", "", "provider state directory")
		user := fs.String("user", "", "user name or ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if err := requireNoArguments(fs); err != nil {
			return err
		}
		provider, err := loadProviderRequired(*state)
		if err != nil {
			return err
		}
		account, ok := provider.Store.FindAccount(*user)
		if !ok {
			return errors.New("unknown user")
		}
		enabled := args[0] == "enable-user"
		if err := provider.Store.SetAccountEnabled(account.ID, enabled); err != nil {
			return err
		}
		fmt.Printf("User %q enabled=%t.\n", account.Name, enabled)
		return nil
	default:
		return fmt.Errorf("unknown provider command %q", args[0])
	}
}

func loadProviderRequired(state string) (*identity.Provider, error) {
	if strings.TrimSpace(state) == "" {
		return nil, errors.New("--state is required")
	}
	return identity.LoadProvider(state)
}

func runEnroll(args []string) error {
	fs := newFlagSet("enroll")
	inviteFlag := fs.String("invite", "", "one-time queqiao:// invitation URI")
	profilePath := fs.String("profile", "", "output client profile (default: user config directory)")
	deviceName := fs.String("device-name", "", "device label shown to the provider")
	timeout := fs.Duration("timeout", 15*time.Second, "enrollment timeout")
	localAddress := fs.String("local-address", "auto", "outer source: auto, IP, or if:NAME (bypasses host TUN routes)")
	// The share URI is the natural first argument users paste. The standard Go
	// flag parser stops at that positional value, so lift it out first to allow
	// the equally natural `enroll URI --profile PATH` spelling as well as flags
	// before the URI and the explicit --invite form.
	positionalInvitation := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positionalInvitation = args[0]
		parseArgs = args[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("enroll accepts at most one invitation URI")
	}
	invitationText := *inviteFlag
	if positionalInvitation != "" {
		if invitationText != "" {
			return errors.New("provide the invitation as either --invite or one argument, not both")
		}
		invitationText = positionalInvitation
	}
	if fs.NArg() == 1 {
		if invitationText != "" {
			return errors.New("provide the invitation as either --invite or one argument, not both")
		}
		invitationText = fs.Arg(0)
	}
	if invitationText == "" {
		return errors.New("an invitation URI is required")
	}
	if err := netbind.Validate(*localAddress); err != nil {
		return fmt.Errorf("invalid --local-address: %w", err)
	}
	invitation, err := identity.ParseInvitation(invitationText, time.Now())
	if err != nil {
		return err
	}
	if *deviceName == "" {
		*deviceName, _ = os.Hostname()
		if strings.TrimSpace(*deviceName) == "" {
			*deviceName = "device"
		}
	}
	if *profilePath == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("locate user configuration directory: %w", err)
		}
		*profilePath = filepath.Join(configDir, "queqiao", invitation.ProviderID+".json")
	}
	if _, err := os.Stat(*profilePath); err == nil {
		return fmt.Errorf("profile already exists: %s", *profilePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect profile path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(*profilePath), 0o700); err != nil {
		return fmt.Errorf("create profile directory: %w", err)
	}
	pendingPath := *profilePath + ".enrolling"
	var draft identity.EnrollmentDraft
	if _, err := os.Stat(pendingPath); err == nil {
		draft, err = identity.LoadEnrollmentDraft(pendingPath)
		if err != nil {
			return fmt.Errorf("load interrupted enrollment %s: %w", pendingPath, err)
		}
		if draft.Invitation != invitation || draft.DeviceName != *deviceName {
			return fmt.Errorf("%s belongs to another invitation or device name; finish or remove it explicitly", pendingPath)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect interrupted enrollment: %w", err)
	} else {
		draft, err = identity.NewEnrollmentDraft(invitation, *deviceName)
		if err != nil {
			return err
		}
		if err := draft.Save(pendingPath); err != nil {
			return fmt.Errorf("save recoverable enrollment draft: %w", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	profile, err := draft.EnrollWithOptions(ctx, identity.DialOptions{Timeout: *timeout, LocalAddress: *localAddress})
	if err != nil {
		return err
	}
	if err := profile.SaveNew(*profilePath); err != nil {
		return fmt.Errorf("save enrolled profile: %w", err)
	}
	if err := os.Remove(pendingPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("profile was saved successfully at %s, but the completed draft %s still contains private credentials and must be removed: %w", *profilePath, pendingPath, err)
	}
	fmt.Printf("Enrolled %q as device %q.\nProfile: %s\nSOCKS: queqiaod client --profile %q\n", profile.Name, profile.DeviceName, *profilePath, *profilePath)
	return nil
}

type runtimeOptions struct {
	listen, localAddress, transport, tcpCongestion                  string
	maxSessions, tcpFallbackLanes                                   int
	maxPayload                                                      uint
	chunkSize                                                       int
	dialTimeout, handshakeTimeout, flowIdleTimeout, flowMaxLifetime time.Duration
	quicPool, waitForOpenAck, udpOnStream                           bool
	congestion                                                      string
	brutalBytesPerSec, adaptiveMinBytesSec, adaptiveMaxBytesSec     uint64
	aggregateBytesPerSec, interactiveReserveBytesPerSec             uint64
	fallbackDelay, fallbackGrace, udpCooldown                       time.Duration
	udpFailureThreshold                                             int
	allowPrivate                                                    bool
	logLevel                                                        string
	jsonLogs                                                        bool
	metricsListen                                                   string
}

func bindRuntimeFlags(fs *flag.FlagSet, opts *runtimeOptions, client bool) {
	defaultListen := ":443"
	defaultMaxSessions := 4096
	if client {
		defaultListen, defaultMaxSessions = "127.0.0.1:1080", 1024
	}
	fs.StringVar(&opts.listen, "listen", defaultListen, "listen address")
	fs.IntVar(&opts.maxSessions, "max-sessions", defaultMaxSessions, "global concurrent-session limit")
	fs.UintVar(&opts.maxPayload, "max-payload", 256*1024, "maximum frame payload")
	fs.IntVar(&opts.chunkSize, "chunk-size", 32*1024, "stream data frame size")
	fs.DurationVar(&opts.dialTimeout, "dial-timeout", 10*time.Second, "dial timeout")
	fs.DurationVar(&opts.handshakeTimeout, "handshake-timeout", 10*time.Second, "TLS, protocol, and SOCKS handshake timeout")
	fs.DurationVar(&opts.flowIdleTimeout, "flow-idle-timeout", 30*time.Minute, "flow idle timeout")
	fs.DurationVar(&opts.flowMaxLifetime, "flow-max-lifetime", 24*time.Hour, "maximum flow lifetime")
	fs.StringVar(&opts.transport, "transport", string(pep.TransportAuto), "transport: auto, quic, or tcp")
	fs.IntVar(&opts.tcpFallbackLanes, "tcp-fallback-lanes", 0, "TCP lanes per bulk flow (0 uses role default)")
	fs.BoolVar(&opts.udpOnStream, "udp-on-stream", false, "carry UDP packets on streams instead of QUIC datagrams")
	fs.StringVar(&opts.congestion, "congestion", string(pep.CongestionErasure), "QUIC congestion controller")
	fs.Uint64Var(&opts.brutalBytesPerSec, "brutal-bytes-per-sec", 0, "Brutal fixed byte rate")
	fs.Uint64Var(&opts.adaptiveMinBytesSec, "adaptive-min-bytes-per-sec", 64*1024, "Adaptive minimum byte rate")
	fs.Uint64Var(&opts.adaptiveMaxBytesSec, "adaptive-max-bytes-per-sec", 200*1024*1024, "Adaptive maximum byte rate")
	fs.Uint64Var(&opts.aggregateBytesPerSec, "aggregate-bytes-per-sec", 0, "optional aggregate byte budget")
	fs.Uint64Var(&opts.interactiveReserveBytesPerSec, "interactive-reserve-bytes-per-sec", 0, "interactive portion of aggregate budget")
	fs.StringVar(&opts.logLevel, "log-level", "info", "debug, info, warn, or error")
	fs.BoolVar(&opts.jsonLogs, "json-logs", false, "write JSON logs")
	fs.StringVar(&opts.metricsListen, "metrics-listen", "", "optional metrics listen address")
	if client {
		fs.StringVar(&opts.localAddress, "local-address", "auto", "outer source: auto, IP, or if:NAME")
		fs.BoolVar(&opts.quicPool, "quic-pool", true, "reuse a persistent QUIC connection")
		fs.BoolVar(&opts.waitForOpenAck, "wait-for-open-ack", false, "wait for destination confirmation before answering SOCKS")
		fs.DurationVar(&opts.fallbackDelay, "fallback-delay", 300*time.Millisecond, "delay before preparing TCP fallback")
		fs.DurationVar(&opts.fallbackGrace, "fallback-grace", 2*time.Second, "time a ready TCP fallback waits for QUIC")
		fs.IntVar(&opts.udpFailureThreshold, "udp-failure-threshold", 3, "UDP failures before cooldown")
		fs.DurationVar(&opts.udpCooldown, "udp-cooldown", 30*time.Second, "UDP cooldown after repeated failure")
	} else {
		fs.StringVar(&opts.tcpCongestion, "tcp-congestion", "system", "server TCP congestion controller")
		fs.BoolVar(&opts.allowPrivate, "allow-private-destinations", false, "allow private and link-local destinations")
	}
}

func validateRuntime(opts runtimeOptions, client bool) error {
	if opts.listen == "" || opts.maxSessions < 1 || opts.maxSessions > 1<<16 {
		return errors.New("listen address and max-sessions (1-65536) are required")
	}
	if opts.maxPayload == 0 || opts.maxPayload > 1<<20 || opts.chunkSize <= 0 || uint(opts.chunkSize) > opts.maxPayload {
		return errors.New("invalid frame payload or chunk size")
	}
	if opts.transport != string(pep.TransportAuto) && opts.transport != string(pep.TransportQUIC) && opts.transport != string(pep.TransportTCP) {
		return errors.New("--transport must be auto, quic, or tcp")
	}
	if opts.tcpFallbackLanes < 0 || opts.tcpFallbackLanes > 16 {
		return errors.New("--tcp-fallback-lanes must be between 0 and 16")
	}
	if opts.flowIdleTimeout <= 0 || opts.flowMaxLifetime <= 0 || opts.flowIdleTimeout > opts.flowMaxLifetime {
		return errors.New("flow idle timeout must be positive and no longer than flow lifetime")
	}
	if opts.aggregateBytesPerSec == 0 && opts.interactiveReserveBytesPerSec != 0 || opts.interactiveReserveBytesPerSec > opts.aggregateBytesPerSec {
		return errors.New("invalid aggregate/interactive byte budget")
	}
	if opts.adaptiveMinBytesSec == 0 || opts.adaptiveMaxBytesSec < opts.adaptiveMinBytesSec {
		return errors.New("invalid adaptive byte-rate bounds")
	}
	if opts.congestion == string(pep.CongestionBrutal) && opts.brutalBytesPerSec == 0 {
		return errors.New("--brutal-bytes-per-sec is required with brutal congestion")
	}
	if client && (opts.fallbackDelay < 0 || opts.fallbackGrace <= 0 || opts.udpFailureThreshold < 1 || opts.udpCooldown <= 0) {
		return errors.New("invalid fallback settings")
	}
	if client {
		if err := netbind.Validate(opts.localAddress); err != nil {
			return fmt.Errorf("invalid --local-address: %w", err)
		}
	}
	return nil
}

func runClient(args []string) error {
	fs := newFlagSet("client")
	profilePath := fs.String("profile", "", "imported client profile")
	noAutoRenew := fs.Bool("no-auto-renew", false, "disable certificate renewal before expiry")
	var opts runtimeOptions
	bindRuntimeFlags(fs, &opts, true)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArguments(fs); err != nil {
		return err
	}
	if *profilePath == "" {
		return errors.New("--profile is required; import an invitation first with `queqiaod enroll INVITATION`")
	}
	if err := validateRuntime(opts, true); err != nil {
		return err
	}
	logger, err := newLogger(opts.logLevel, opts.jsonLogs)
	if err != nil {
		return err
	}
	profile, err := identity.LoadClientProfile(*profilePath)
	if err != nil {
		return fmt.Errorf("load client profile %q: %w", *profilePath, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !*noAutoRenew {
		needs, err := profile.NeedsRenewal(time.Now(), 7*24*time.Hour)
		if err != nil {
			return err
		}
		if needs {
			renewed, renewErr := identity.RenewProfileWithOptions(ctx, profile, identity.DialOptions{Timeout: opts.handshakeTimeout, LocalAddress: opts.localAddress})
			if renewErr != nil {
				logger.Warn("automatic certificate renewal failed; continuing with current valid identity", "error", renewErr)
			} else if err := renewed.Save(*profilePath); err != nil {
				return fmt.Errorf("save renewed profile: %w", err)
			} else {
				profile = renewed
				logger.Info("device identity renewed")
			}
		}
	}
	credentials, err := profile.Credentials()
	if err != nil {
		return err
	}
	client, err := pep.NewClient(pep.ClientConfig{
		ListenAddr: opts.listen, RemoteAddr: profile.Endpoint, LocalAddress: opts.localAddress,
		Credentials: credentials, MaxPayload: uint32(opts.maxPayload), ChunkSize: opts.chunkSize,
		DialTimeout: opts.dialTimeout, HandshakeTimeout: opts.handshakeTimeout,
		FlowIdleTimeout: opts.flowIdleTimeout, FlowMaxLifetime: opts.flowMaxLifetime,
		MaxSessions: opts.maxSessions, Transport: pep.TransportKind(opts.transport),
		TCPFallbackLanes: opts.tcpFallbackLanes, EnableQUICPool: opts.quicPool,
		WaitForOpenAcknowledgement: opts.waitForOpenAck, UDPOnStream: opts.udpOnStream,
		Congestion: pep.CongestionControlKind(opts.congestion), BrutalBytesPerSec: opts.brutalBytesPerSec,
		AdaptiveMinBytesSec: opts.adaptiveMinBytesSec, AdaptiveMaxBytesSec: opts.adaptiveMaxBytesSec,
		AggregateBytesPerSec: opts.aggregateBytesPerSec, InteractiveReserveBytesPerSec: opts.interactiveReserveBytesPerSec,
		FallbackDelay: opts.fallbackDelay, FallbackGrace: opts.fallbackGrace,
		UDPFailureThreshold: opts.udpFailureThreshold, UDPCooldown: opts.udpCooldown, Logger: logger,
	})
	if err != nil {
		return err
	}
	if !*noAutoRenew {
		go maintainClientIdentity(ctx, *profilePath, profile, client, opts.handshakeTimeout, opts.localAddress, logger)
	}
	stopMetrics, err := serveMetrics(opts.metricsListen, client.Metrics(), logger)
	if err != nil {
		return err
	}
	defer stopMetrics()
	return client.Serve(ctx)
}

func runServer(args []string) error {
	fs := newFlagSet("server")
	state := fs.String("state", "", "provider state directory")
	var opts runtimeOptions
	bindRuntimeFlags(fs, &opts, false)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireNoArguments(fs); err != nil {
		return err
	}
	if err := validateRuntime(opts, false); err != nil {
		return err
	}
	provider, err := loadProviderRequired(*state)
	if err != nil {
		return err
	}
	logger, err := newLogger(opts.logLevel, opts.jsonLogs)
	if err != nil {
		return err
	}
	service := &identity.EnrollmentService{Provider: provider}
	server, err := pep.NewServer(pep.ServerConfig{
		ListenAddr: opts.listen, Credentials: provider.ServerCredentials(), Enrollment: service,
		MaxPayload: uint32(opts.maxPayload), ChunkSize: opts.chunkSize,
		HandshakeTimeout: opts.handshakeTimeout, FlowIdleTimeout: opts.flowIdleTimeout,
		FlowMaxLifetime: opts.flowMaxLifetime, MaxSessions: opts.maxSessions,
		DestinationPolicy: pep.DestinationPolicy{AllowPrivate: opts.allowPrivate, DialTimeout: opts.dialTimeout},
		EnableTCP:         opts.transport == string(pep.TransportTCP) || opts.transport == string(pep.TransportAuto),
		EnableQUIC:        opts.transport == string(pep.TransportQUIC) || opts.transport == string(pep.TransportAuto),
		TCPFallbackLanes:  opts.tcpFallbackLanes, TCPCongestion: opts.tcpCongestion,
		Congestion: pep.CongestionControlKind(opts.congestion), BrutalBytesPerSec: opts.brutalBytesPerSec,
		AdaptiveMinBytesSec: opts.adaptiveMinBytesSec, AdaptiveMaxBytesSec: opts.adaptiveMaxBytesSec,
		AggregateBytesPerSec: opts.aggregateBytesPerSec, InteractiveReserveBytesPerSec: opts.interactiveReserveBytesPerSec,
		Logger: logger, UDPOnStream: opts.udpOnStream,
	})
	if err != nil {
		return err
	}
	stopMetrics, err := serveMetrics(opts.metricsListen, server.Metrics(), logger)
	if err != nil {
		return err
	}
	defer stopMetrics()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go maintainGatewayIdentity(ctx, provider, logger)
	return server.Serve(ctx)
}

const identityMaintenanceInterval = time.Hour

func maintainClientIdentity(ctx context.Context, profilePath string, profile identity.ClientProfile, client *pep.Client, timeout time.Duration, localAddress string, logger *slog.Logger) {
	ticker := time.NewTicker(identityMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			needs, err := profile.NeedsRenewal(time.Now(), 7*24*time.Hour)
			if err != nil {
				logger.Error("check device identity lifetime", "error", err)
				continue
			}
			if !needs {
				continue
			}
			renewed, err := identity.RenewProfileWithOptions(ctx, profile, identity.DialOptions{Timeout: timeout, LocalAddress: localAddress})
			if err != nil {
				logger.Warn("automatic certificate renewal failed; will retry", "error", err)
				continue
			}
			if err := renewed.Save(profilePath); err != nil {
				logger.Error("save renewed device identity; will retry", "error", err)
				continue
			}
			credentials, err := renewed.Credentials()
			if err != nil {
				logger.Error("load renewed device identity", "error", err)
				continue
			}
			if err := client.UpdateCredentials(credentials); err != nil {
				logger.Error("activate renewed device identity", "error", err)
				continue
			}
			profile = renewed
			logger.Info("device identity renewed")
		case <-ctx.Done():
			return
		}
	}
}

func maintainGatewayIdentity(ctx context.Context, provider *identity.Provider, logger *slog.Logger) {
	ticker := time.NewTicker(identityMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			renewed, err := provider.RenewGatewayIdentity(time.Now(), 7*24*time.Hour)
			if err != nil {
				logger.Error("automatic gateway identity renewal failed; will retry", "error", err)
			} else if renewed {
				logger.Info("gateway identity renewed")
			}
		case <-ctx.Done():
			return
		}
	}
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
	options := &slog.HandlerOptions{Level: slogLevel}
	if json {
		return slog.New(slog.NewJSONHandler(os.Stderr, options)), nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, options)), nil
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}, nil
}
