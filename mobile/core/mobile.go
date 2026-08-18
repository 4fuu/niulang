package mobilecore

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/metrics"
	"github.com/bojieli/queqiao/internal/pep"
)

const (
	StateStopped  = "stopped"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateFailed   = "failed"
)

// Protector is implemented by Android's VpnService. Protect must synchronously
// exempt fd from the VPN before the socket is bound or connected.
type Protector interface {
	Protect(fd int64) bool
}

// Observer receives serialized lifecycle and diagnostic callbacks. Callbacks
// can arrive on arbitrary Go threads; platform implementations must marshal UI
// changes onto their main thread.
type Observer interface {
	OnStateChanged(state string)
	OnLog(level, message string)
	// OnProfileUpdated must durably replace the platform's encrypted profile.
	// Returning false leaves the current in-memory certificate active and
	// causes renewal to be retried on the next maintenance interval.
	OnProfileUpdated(profileJSON string) bool
}

// Session owns exactly one mobile tunnel. A Session may be restarted after it
// has fully stopped, but Start and Stop are serialized and idempotent.
type Session struct {
	opMu      sync.Mutex
	mu        sync.Mutex
	state     string
	observer  Observer
	protector Protector
	cancel    context.CancelFunc
	listener  net.Listener
	packet    *packetStack
	client    *pep.Client
	metrics   *metrics.Registry
	done      chan struct{}
	runErr    error
}

func NewSession(observer Observer, protector Protector) *Session {
	return &Session{state: StateStopped, observer: observer, protector: protector}
}

// Start activates a full-device tunnel over a platform-provided TUN descriptor.
// packetOffset is normally 0; 4 is supported for callers that already own an
// Apple utun descriptor, though StartPacketFlow is the public-API iOS path.
func (s *Session) Start(profileJSON string, tunFD, packetOffset, mtu int64, requireSocketProtection bool) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if tunFD < 0 || packetOffset < 0 || packetOffset > 4 || mtu < 0 || mtu > maximumMTU {
		return errors.New("invalid tunnel descriptor configuration")
	}
	return s.start(profileJSON, requireSocketProtection,
		func(ctx context.Context, proxy socksClient, log func(string, string)) (*packetStack, error) {
			return newPacketStack(ctx, int(tunFD), int(packetOffset), int(mtu), defaultMaxSessions, proxy, log)
		})
}

// StartPacketFlow activates an iOS tunnel over NEPacketTunnelFlow callbacks.
// It avoids private utun descriptor access and takes ownership of packetIO
// after the packet engine has been created successfully.
func (s *Session) StartPacketFlow(profileJSON string, packetIO PacketIO, mtu int64) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if packetIO == nil || mtu < 0 || mtu > maximumMTU {
		return errors.New("invalid packet-flow configuration")
	}
	return s.start(profileJSON, false,
		func(ctx context.Context, proxy socksClient, log func(string, string)) (*packetStack, error) {
			return newPacketStackWithDevice(ctx, &callbackPacketDevice{packetIO: packetIO}, 0, int(mtu), defaultMaxSessions, proxy, log)
		})
}

type packetStackFactory func(context.Context, socksClient, func(string, string)) (*packetStack, error)

func (s *Session) start(profileJSON string, requireSocketProtection bool, makePacketStack packetStackFactory) error {
	s.mu.Lock()
	if s.state != StateStopped {
		state := s.state
		s.mu.Unlock()
		return fmt.Errorf("cannot start tunnel while state is %s", state)
	}
	if requireSocketProtection && s.protector == nil {
		s.mu.Unlock()
		return errors.New("socket protection is required for this tunnel")
	}
	s.state = StateStarting
	s.runErr = nil
	s.mu.Unlock()
	s.notifyState(StateStarting)

	profile, err := decodeProfile(profileJSON)
	if err != nil {
		return s.startFailed(err)
	}
	credentials, err := profile.Credentials()
	if err != nil {
		return s.startFailed(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return s.startFailed(fmt.Errorf("open private SOCKS listener: %w", err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	registry := metrics.New()
	logger := slog.New(newObserverHandler(s.observer, slog.LevelInfo))
	client, err := pep.NewClient(pep.ClientConfig{
		// Leave source-address selection to the platform. Android's socket
		// protector must run before bind/connect; resolving and binding an
		// "auto" interface here would bypass that contract and can select the
		// VPN itself after its default route is installed.
		ListenAddr: listener.Addr().String(), RemoteAddr: profile.Endpoint, LocalAddress: "",
		SocketControl: s.socketControl(requireSocketProtection), Credentials: credentials,
		MaxPayload: 256 * 1024, ChunkSize: 32 * 1024,
		DialTimeout: 10 * time.Second, HandshakeTimeout: 30 * time.Second,
		FlowIdleTimeout: 30 * time.Minute, FlowMaxLifetime: 24 * time.Hour,
		MaxSessions: defaultMaxSessions, Transport: pep.TransportAuto,
		TCPFallbackLanes: 0, EnableQUICPool: true, WaitForOpenAcknowledgement: false,
		UDPOnStream: false, Congestion: pep.CongestionErasure,
		AdaptiveMinBytesSec: 64 * 1024, AdaptiveMaxBytesSec: 200 * 1024 * 1024,
		FallbackDelay: 300 * time.Millisecond, FallbackGrace: 2 * time.Second,
		UDPFailureThreshold: 3, UDPCooldown: 30 * time.Second,
		Metrics: registry, Logger: logger,
	})
	if err != nil {
		cancel()
		_ = listener.Close()
		return s.startFailed(err)
	}
	packet, err := makePacketStack(ctx,
		socksClient{address: listener.Addr().String(), handshakeTimeout: 10 * time.Second}, s.notifyLog)
	if err != nil {
		cancel()
		_ = listener.Close()
		return s.startFailed(err)
	}
	done := make(chan struct{})
	s.mu.Lock()
	if s.state != StateStarting {
		s.mu.Unlock()
		cancel()
		_ = listener.Close()
		_ = packet.Close()
		return errors.New("tunnel start was interrupted")
	}
	s.cancel, s.listener, s.packet, s.client, s.metrics, s.done = cancel, listener, packet, client, registry, done
	s.state = StateRunning
	s.mu.Unlock()

	packet.start()
	go s.maintainIdentity(ctx, profile, client)
	go s.run(ctx, cancel, client, listener, packet, done)
	s.notifyState(StateRunning)
	return nil
}

const identityMaintenanceInterval = time.Hour

func (s *Session) maintainIdentity(ctx context.Context, profile identity.ClientProfile, client *pep.Client) {
	ticker := time.NewTicker(identityMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
		needsRenewal, err := profile.NeedsRenewal(time.Now(), 7*24*time.Hour)
		if err != nil {
			s.notifyLog("error", fmt.Sprintf("check device identity lifetime: %v", err))
			continue
		}
		if !needsRenewal {
			continue
		}
		renewalContext, cancel := context.WithTimeout(ctx, 15*time.Second)
		renewed, err := identity.RenewProfile(renewalContext, profile, 15*time.Second)
		cancel()
		if err != nil {
			s.notifyLog("warning", fmt.Sprintf("automatic certificate renewal failed; will retry: %v", err))
			continue
		}
		credentials, err := renewed.Credentials()
		if err != nil {
			s.notifyLog("error", fmt.Sprintf("load renewed device identity: %v", err))
			continue
		}
		encoded, err := encodeJSON(renewed)
		if err != nil {
			s.notifyLog("error", fmt.Sprintf("encode renewed device identity: %v", err))
			continue
		}
		if !s.notifyProfileUpdated(encoded) {
			s.notifyLog("warning", "persist renewed device identity; will retry")
			continue
		}
		if err := client.UpdateCredentials(credentials); err != nil {
			s.notifyLog("error", fmt.Sprintf("activate renewed device identity: %v", err))
			continue
		}
		profile = renewed
		s.notifyLog("info", "device identity renewed")
	}
}

func (s *Session) run(ctx context.Context, cancel context.CancelFunc, client *pep.Client, listener net.Listener, packet *packetStack, done chan struct{}) {
	clientResult := make(chan error, 1)
	go func() { clientResult <- client.ServeListener(ctx, listener) }()
	var err error
	unexpected := false
	select {
	case err = <-clientResult:
		unexpected = ctx.Err() == nil
		if err == nil && unexpected {
			err = errors.New("queqiao client stopped unexpectedly")
		}
	case <-packet.ctx.Done():
		unexpected = ctx.Err() == nil
		if unexpected {
			err = errors.New("packet engine stopped unexpectedly")
		}
		cancel()
		_ = listener.Close()
		clientErr := <-clientResult
		if err == nil {
			err = clientErr
		}
	}
	cancel()
	_ = listener.Close()
	packetErr := packet.Close()
	if packetErr != nil && unexpected {
		err = packetErr
	}
	if err != nil && unexpected {
		s.notifyLog("error", fmt.Sprintf("Queqiao client stopped: %v", err))
	}
	s.mu.Lock()
	if err != nil && unexpected {
		s.runErr = err
		s.state = StateFailed
	} else {
		s.state = StateStopped
	}
	state := s.state
	s.cancel, s.listener, s.packet, s.client, s.done = nil, nil, nil, nil, nil
	s.mu.Unlock()
	close(done)
	s.notifyState(state)
}

func (s *Session) Stop() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	switch s.state {
	case StateStopped:
		err := s.runErr
		s.mu.Unlock()
		return err
	case StateFailed:
		err := s.runErr
		s.state = StateStopped
		s.mu.Unlock()
		s.notifyState(StateStopped)
		return err
	case StateStopping:
		done := s.done
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		return s.lastError()
	default:
		s.state = StateStopping
		cancel, listener, done := s.cancel, s.listener, s.done
		s.mu.Unlock()
		s.notifyState(StateStopping)
		if cancel != nil {
			cancel()
		}
		if listener != nil {
			_ = listener.Close()
		}
		if done != nil {
			<-done
		}
		return s.lastError()
	}
}

func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) MetricsJSON() string {
	s.mu.Lock()
	state, registry, packet := s.state, s.metrics, s.packet
	s.mu.Unlock()
	var transport any = struct{}{}
	if registry != nil {
		transport = registry.Snapshot()
	}
	var packets any = struct{}{}
	if packet != nil {
		packets = packet.snapshot()
	}
	encoded, err := json.Marshal(struct {
		Version   int    `json:"version"`
		State     string `json:"state"`
		Packets   any    `json:"packets"`
		Transport any    `json:"transport"`
	}{Version: 1, State: state, Packets: packets, Transport: transport})
	if err != nil {
		return `{"version":1,"state":"failed"}`
	}
	return string(encoded)
}

func (s *Session) startFailed(err error) error {
	s.mu.Lock()
	s.state, s.runErr = StateStopped, err
	s.mu.Unlock()
	s.notifyLog("error", err.Error())
	s.notifyState(StateStopped)
	return err
}

func (s *Session) lastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

func (s *Session) socketControl(required bool) func(string, string, syscall.RawConn) error {
	if !required {
		return nil
	}
	return func(_, _ string, raw syscall.RawConn) error {
		var protectionErr error
		err := raw.Control(func(fd uintptr) {
			defer func() {
				if recovered := recover(); recovered != nil {
					protectionErr = fmt.Errorf("socket protector panicked: %v", recovered)
				}
			}()
			// File descriptors are signed C ints on every supported mobile OS;
			// reject an invalid value before crossing gomobile's int64 boundary.
			if fd > uintptr(1<<31-1) {
				protectionErr = errors.New("platform returned an out-of-range socket descriptor")
				return
			}
			if s.protector == nil || !s.protector.Protect(int64(fd)) {
				protectionErr = errors.New("platform refused to exempt Queqiao socket from VPN")
			}
		})
		return errors.Join(err, protectionErr)
	}
}

func (s *Session) notifyState(state string) {
	if s.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.observer.OnStateChanged(state)
}

func (s *Session) notifyLog(level, message string) {
	if s.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	s.observer.OnLog(level, message)
}

func (s *Session) notifyProfileUpdated(profileJSON string) (stored bool) {
	if s.observer == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			stored = false
		}
	}()
	return s.observer.OnProfileUpdated(profileJSON)
}

// ValidateInvitation validates structure, pin and expiry without consuming the
// one-time invitation token.
func ValidateInvitation(invitationURI string) error {
	_, err := identity.ParseInvitation(invitationURI, time.Now())
	return err
}

// PrepareEnrollment creates the permanent Ed25519 key before the one-time
// token is sent. The caller must persist the returned draft securely before
// invoking CompleteEnrollment so a lost response can be retried safely.
func PrepareEnrollment(invitationURI, deviceName string) (string, error) {
	invitation, err := identity.ParseInvitation(invitationURI, time.Now())
	if err != nil {
		return "", err
	}
	draft, err := identity.NewEnrollmentDraft(invitation, deviceName)
	if err != nil {
		return "", err
	}
	return encodeJSON(draft)
}

func CompleteEnrollment(draftJSON string) (string, error) {
	var draft identity.EnrollmentDraft
	if err := decodeStrictJSON(draftJSON, &draft); err != nil {
		return "", fmt.Errorf("decode enrollment draft: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	profile, err := draft.Enroll(ctx, 15*time.Second)
	if err != nil {
		return "", err
	}
	return encodeJSON(profile)
}

func ValidateProfile(profileJSON string) error {
	_, err := decodeProfile(profileJSON)
	return err
}

func ProfileSummaryJSON(profileJSON string) (string, error) {
	profile, err := decodeProfile(profileJSON)
	if err != nil {
		return "", err
	}
	credentials, err := profile.Credentials()
	if err != nil {
		return "", err
	}
	leaf, err := x509.ParseCertificate(credentials.Certificate.Certificate[0])
	if err != nil {
		return "", err
	}
	return encodeJSON(struct {
		Version           int    `json:"version"`
		Name              string `json:"name"`
		Endpoint          string `json:"endpoint"`
		ProviderID        string `json:"provider_id"`
		GatewayID         string `json:"gateway_id"`
		AccountID         string `json:"account_id"`
		DeviceID          string `json:"device_id"`
		DeviceName        string `json:"device_name"`
		CertificateExpiry string `json:"certificate_expiry"`
	}{
		Version: 1, Name: profile.Name, Endpoint: profile.Endpoint,
		ProviderID: profile.ProviderID, GatewayID: profile.GatewayID,
		AccountID: profile.AccountID, DeviceID: profile.DeviceID,
		DeviceName: profile.DeviceName, CertificateExpiry: leaf.NotAfter.UTC().Format(time.RFC3339),
	})
}

// ProfileNeedsRenewal returns 1 when renewal is due and 0 otherwise. An integer
// avoids Objective-C's collision between a Go boolean result and the BOOL used
// by gomobile to report NSError success.
func ProfileNeedsRenewal(profileJSON string) (int64, error) {
	profile, err := decodeProfile(profileJSON)
	if err != nil {
		return 0, err
	}
	needsRenewal, err := profile.NeedsRenewal(time.Now(), 7*24*time.Hour)
	if err != nil || !needsRenewal {
		return 0, err
	}
	return 1, nil
}

// RenewProfile must be called before establishing the platform VPN so its
// renewal socket follows the ordinary system route.
func RenewProfile(profileJSON string) (string, error) {
	profile, err := decodeProfile(profileJSON)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	renewed, err := identity.RenewProfile(ctx, profile, 15*time.Second)
	if err != nil {
		return "", err
	}
	return encodeJSON(renewed)
}

func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "development"
}

func decodeProfile(encoded string) (identity.ClientProfile, error) {
	var profile identity.ClientProfile
	if err := decodeStrictJSON(encoded, &profile); err != nil {
		return identity.ClientProfile{}, fmt.Errorf("decode client profile: %w", err)
	}
	if _, err := profile.Credentials(); err != nil {
		return identity.ClientProfile{}, err
	}
	return profile, nil
}

func decodeStrictJSON(encoded string, destination any) error {
	if len(encoded) == 0 || len(encoded) > 256*1024 {
		return errors.New("JSON document is empty or exceeds 256 KiB")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON document contains trailing data")
	}
	return nil
}

func encodeJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

type observerHandler struct {
	observer Observer
	level    slog.Level
}

func newObserverHandler(observer Observer, level slog.Level) *observerHandler {
	return &observerHandler{observer: observer, level: level}
}

func (h *observerHandler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }

func (h *observerHandler) Handle(_ context.Context, record slog.Record) error {
	if h.observer == nil {
		return nil
	}
	message := record.Message
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "error" {
			message += ": " + attr.Value.String()
		}
		return true
	})
	defer func() { _ = recover() }()
	h.observer.OnLog(record.Level.String(), message)
	return nil
}

func (h *observerHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *observerHandler) WithGroup(_ string) slog.Handler      { return h }
