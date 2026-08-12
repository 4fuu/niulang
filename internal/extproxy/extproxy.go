// Package extproxy launches third-party proxy implementations so wanopt can be
// measured against them under the same emulated path.
//
// The in-tree reference in internal/baseline deliberately reproduces TUIC's
// data-path shape on wanopt's own QUIC stack, which isolates the transport
// design from the language and library. That is a useful control and a weak
// claim: it is not TUIC, and it says nothing about the other transports people
// actually deploy. This package closes that gap by driving real
// implementations - sing-box for TUIC and Hysteria2, and any binary that can be
// configured to expose SOCKS5 and dial a given server address.
//
// Each transport is a pair of processes: a server bound to a local address that
// the path emulator forwards to, and a client exposing SOCKS5 that dials the
// emulator. Nothing here is used by wanoptd; this is measurement scaffolding.
package extproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Kind names a third-party transport this package can configure.
type Kind string

const (
	// TUIC is the protocol wanopt's in-tree reference imitates. Measuring
	// against the real implementation is the only way to claim parity with it.
	TUIC Kind = "tuic"
	// Hysteria2 is the other widely deployed QUIC-based transport for this
	// kind of path, and its congestion control is deliberately not loss
	// responsive.
	Hysteria2 Kind = "hysteria2"
	// VLESSWebSocket and VLESSTCP are TCP-based. They cannot be compared under
	// emulated packet loss, because a userspace relay carries a byte stream
	// and cannot drop a segment; see the TCP note in docs/BENCHMARKING.md.
	VLESSWebSocket Kind = "vless-ws"
	VLESSTCP       Kind = "vless-tcp"
)

// Transport reports whether a kind runs over UDP, which decides whether the
// UDP path emulator can carry it.
func (k Kind) Transport() string {
	switch k {
	case TUIC, Hysteria2:
		return "udp"
	default:
		return "tcp"
	}
}

// Config describes one measured pair.
type Config struct {
	Kind Kind
	// Binary is the implementation to run. sing-box serves every kind here.
	Binary string
	// ServerListen is the address the server binds; the emulator forwards to it.
	ServerListen string
	// ClientRemote is the emulator address the client dials.
	ClientRemote string
	// SOCKSListen is where the client exposes SOCKS5 for the benchmark.
	SOCKSListen string
	// CertificatePath and KeyPath are the server's TLS material. The client
	// trusts exactly CertificatePath, so no verification bypass is needed.
	CertificatePath string
	KeyPath         string
	// Congestion selects the controller where the implementation exposes one.
	Congestion string
	// WorkDir holds generated configuration. The caller owns its lifetime.
	WorkDir string
	// Credential is the shared secret: a password for TUIC and Hysteria2.
	Credential string
	// UUID identifies the user for TUIC and VLESS.
	UUID string
}

func (c Config) withDefaults() Config {
	if c.Congestion == "" {
		c.Congestion = "bbr"
	}
	if c.Credential == "" {
		c.Credential = "wanopt-benchmark-credential"
	}
	if c.UUID == "" {
		c.UUID = "8c9dbf4a-3e2b-4a1c-9f6d-5b7e0a1c2d3e"
	}
	return c
}

// Pair is a running server and client. Close stops both.
type Pair struct {
	server *process
	client *process
}

func (p *Pair) Close() {
	if p == nil {
		return
	}
	// Stop the client first so it does not log connection errors while the
	// server is going away.
	p.client.stop()
	p.server.stop()
}

// Logs returns whatever the two processes wrote, which is the only diagnostic
// available when a third-party implementation refuses a configuration.
func (p *Pair) Logs() string {
	if p == nil {
		return ""
	}
	return "server:\n" + p.server.output() + "\nclient:\n" + p.client.output()
}

// Start launches the pair and waits until the client's SOCKS port accepts
// connections, so a benchmark never measures a process that is still starting.
func Start(ctx context.Context, cfg Config) (*Pair, error) {
	cfg = cfg.withDefaults()
	if cfg.Binary == "" {
		return nil, errors.New("a proxy binary is required")
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, fmt.Errorf("proxy binary: %w", err)
	}
	serverConfig, clientConfig, err := buildConfigs(cfg)
	if err != nil {
		return nil, err
	}
	serverPath := filepath.Join(cfg.WorkDir, string(cfg.Kind)+"-server.json")
	clientPath := filepath.Join(cfg.WorkDir, string(cfg.Kind)+"-client.json")
	if err := writeJSON(serverPath, serverConfig); err != nil {
		return nil, err
	}
	if err := writeJSON(clientPath, clientConfig); err != nil {
		return nil, err
	}

	server, err := startProcess(ctx, cfg.Binary, "run", "-c", serverPath)
	if err != nil {
		return nil, err
	}
	// The server has to be listening before the client dials, or the client
	// backs off and the first measured request pays that delay.
	if err := waitForListener(ctx, cfg.ServerListen, cfg.Kind.Transport(), 10*time.Second); err != nil {
		server.stop()
		return nil, fmt.Errorf("%s server did not listen: %w\n%s", cfg.Kind, err, server.output())
	}
	client, err := startProcess(ctx, cfg.Binary, "run", "-c", clientPath)
	if err != nil {
		server.stop()
		return nil, err
	}
	if err := waitForListener(ctx, cfg.SOCKSListen, "tcp", 10*time.Second); err != nil {
		client.stop()
		server.stop()
		return nil, fmt.Errorf("%s client did not listen: %w\n%s", cfg.Kind, err, client.output())
	}
	return &Pair{server: server, client: client}, nil
}

func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}

// waitForListener polls until the address accepts a connection. A UDP listener
// cannot be probed this way, so for UDP the check is that the process is still
// alive after a short settle.
func waitForListener(ctx context.Context, address, transport string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if transport == "udp" {
		select {
		case <-time.After(250 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("timed out")
}
