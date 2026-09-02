package extproxy

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// queqiaoLaunch describes the long-running sides of a released Queqiao pair.
// startQueqiao owns the provider initialization and enrollment required before
// the client command can use this profile.
func queqiaoLaunch(cfg Config) (Launch, error) {
	provider := filepath.Join(cfg.WorkDir, "provider")
	profile := filepath.Join(cfg.WorkDir, "client.json")
	common := []string{
		"--transport", "quic",
		"--congestion", cfg.Congestion,
		"--log-level", "error",
		"--log-file", "none",
		"--telemetry-log-interval", "0",
	}
	return Launch{
		ServerArgs: append([]string{
			"server", "--state", provider,
			"--listen", cfg.ServerListen,
			// Enrollment is TCP, so the provider listens on both protocols.
			"--transport", "auto",
			"--allow-private-destinations",
		}, common[2:]...),
		ClientArgs: append([]string{
			"client", "--profile", profile,
			"--listen", cfg.SOCKSListen,
			"--local-address", "127.0.0.1",
			"--no-auto-renew",
		}, common...),
	}, nil
}

// startQueqiao exercises the released binary's real trust path: initialize a
// provider, create a user and one-time invitation, start the gateway, enroll a
// device through the companion TCP relay, and only then start the QUIC client.
// Provisioning finishes before the measured request, so identity management is
// not charged to transport latency.
func startQueqiao(ctx context.Context, cfg Config, launch Launch) (*Pair, error) {
	provider := filepath.Join(cfg.WorkDir, "provider")
	profile := filepath.Join(cfg.WorkDir, "client.json")
	if _, err := runOneShot(ctx, launch.ServerBinary,
		"provider", "init", "--state", provider,
		"--name", "benchmark", "--endpoint", cfg.ClientRemote); err != nil {
		return nil, fmt.Errorf("initialize queqiao provider: %w", err)
	}
	if _, err := runOneShot(ctx, launch.ServerBinary,
		"provider", "add-user", "--state", provider,
		"--name", "benchmark"); err != nil {
		return nil, fmt.Errorf("create queqiao benchmark user: %w", err)
	}
	invitation, err := runOneShot(ctx, launch.ServerBinary,
		"provider", "invite", "--state", provider,
		"--user", "benchmark")
	if err != nil {
		return nil, fmt.Errorf("create queqiao invitation: %w", err)
	}
	invitation = strings.TrimSpace(invitation)
	if !strings.HasPrefix(invitation, "queqiao://enroll/") {
		return nil, fmt.Errorf("queqiao invitation command returned an unexpected value")
	}

	server, err := startProcess(ctx, launch.ServerBinary, launch.ServerEnv, launch.ServerArgs...)
	if err != nil {
		return nil, err
	}
	// The auto server's TCP listener is a stronger readiness check than the
	// UDP settle used by ordinary QUIC stacks, and enrollment needs it next.
	if err := waitForListener(ctx, cfg.ServerListen, "tcp", 10*time.Second); err != nil {
		server.stop()
		return nil, fmt.Errorf("queqiao server did not listen: %w\n%s", err, server.output())
	}
	if _, err := runOneShot(ctx, launch.ClientBinary,
		"enroll", invitation,
		"--profile", profile,
		"--device-name", "benchmark",
		"--timeout", "15s",
		"--local-address", "127.0.0.1"); err != nil {
		server.stop()
		return nil, fmt.Errorf("enroll queqiao benchmark device: %w\n%s", err, server.output())
	}

	client, err := startProcess(ctx, launch.ClientBinary, launch.ClientEnv, launch.ClientArgs...)
	if err != nil {
		server.stop()
		return nil, err
	}
	if err := waitForListener(ctx, cfg.SOCKSListen, "tcp", 10*time.Second); err != nil {
		client.stop()
		server.stop()
		return nil, fmt.Errorf("queqiao client did not listen: %w\n%s", err, client.output())
	}
	return &Pair{server: server, client: client}, nil
}

func runOneShot(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}
