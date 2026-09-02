package pep

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/identity"
)

func TestEnrollmentAndRenewalShareTheQUICDataEndpoint(t *testing.T) {
	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := identity.InitProvider(
		filepath.Join(t.TempDir(), "provider"),
		"Test Provider",
		packetConn.LocalAddr().String(),
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.Store.AddAccount("alice", time.Time{}, identity.AccountLimits{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, invitation, err := provider.CreateInvitation(account.ID, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	service := &identity.EnrollmentService{Provider: provider}
	server, err := NewServer(ServerConfig{
		ListenAddr:  packetConn.LocalAddr().String(),
		Credentials: provider.ServerCredentials(),
		Enrollment:  service,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serverResult := make(chan error, 1)
	go func() { serverResult <- server.ServePacketConn(ctx, packetConn) }()
	t.Cleanup(func() {
		cancel()
		if err := <-serverResult; err != nil {
			t.Errorf("server shutdown: %v", err)
		}
	})

	profile, err := identity.EnrollWithOptions(ctx, invitation, "laptop", identity.DialOptions{
		Timeout: 3 * time.Second, LocalAddress: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.Credentials(); err != nil {
		t.Fatalf("enrolled profile is invalid: %v", err)
	}
	if _, err := identity.Enroll(ctx, invitation, "replay", 3*time.Second); err == nil {
		t.Fatal("invitation replay enrolled another device")
	}

	// X.509 encodes certificate times to whole seconds. Cross that boundary so
	// an immediate test renewal has a measurably later expiry than enrollment.
	nextSecond := time.Now().Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(nextSecond) + 10*time.Millisecond)
	renewed, err := identity.RenewProfileWithOptions(ctx, profile, identity.DialOptions{
		Timeout: 3 * time.Second, LocalAddress: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.DeviceID != profile.DeviceID || renewed.DevicePrivateKey != profile.DevicePrivateKey || renewed.DeviceCertificate == profile.DeviceCertificate {
		t.Fatal("renewal changed the device identity/key or failed to rotate the certificate")
	}
	if err := provider.Store.RevokeDevice(profile.DeviceID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := identity.RenewProfile(ctx, renewed, 3*time.Second); err == nil {
		t.Fatal("revoked device renewed its certificate")
	}
}
