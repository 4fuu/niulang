package mobilecore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/pep"
	"github.com/bojieli/queqiao/internal/socks5"
)

func TestValidateLoopbackListenAddr(t *testing.T) {
	for _, test := range []struct {
		name      string
		address   string
		wantError bool
	}{
		{name: "ephemeral loopback", address: "127.0.0.1:0"},
		{name: "fixed loopback", address: "127.0.0.1:1080"},
		{name: "loopback alias", address: "127.0.0.53:1080"},
		{name: "loopback IPv6", address: "[::1]:1080"},
		{name: "wildcard IPv4", address: "0.0.0.0:1080", wantError: true},
		{name: "wildcard IPv6", address: "[::]:1080", wantError: true},
		{name: "routable address", address: "192.168.1.10:1080", wantError: true},
		// A name is rejected rather than resolved: resolution depends on state
		// this process does not own, and pointing "localhost" elsewhere is a
		// documented way to turn a private listener into a public one.
		{name: "hostname", address: "localhost:1080", wantError: true},
		{name: "privileged port", address: "127.0.0.1:80", wantError: true},
		{name: "no port", address: "127.0.0.1", wantError: true},
		{name: "empty", address: "", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateLoopbackListenAddr(test.address)
			if test.wantError && err == nil {
				t.Fatalf("validateLoopbackListenAddr(%q) unexpectedly succeeded", test.address)
			}
			if !test.wantError && err != nil {
				t.Fatalf("validateLoopbackListenAddr(%q): %v", test.address, err)
			}
		})
	}
}

// recordingObserver captures the lifecycle callbacks a platform would receive.
type recordingObserver struct {
	mu     sync.Mutex
	states []string
}

func (o *recordingObserver) OnStateChanged(state string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.states = append(o.states, state)
}

func (o *recordingObserver) OnLog(string, string) {}

func (o *recordingObserver) OnProfileUpdated(string) bool { return true }

func (o *recordingObserver) seen(state string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, got := range o.states {
		if got == state {
			return true
		}
	}
	return false
}

// testGateway is a complete provider and gateway on loopback: the same trust
// domain, enrollment path, and data plane a real deployment uses, so an export
// session under test is exercised end to end rather than against a stub.
type testGateway struct {
	profileJSON string
}

func startTestGateway(t *testing.T) *testGateway {
	t.Helper()
	// The provider pins its endpoint at initialization and the invitation
	// carries it, so the ports have to exist before the trust domain does. QUIC
	// and the TCP fallback share one port number, as they do in deployment.
	packet, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = packet.Close() })
	endpoint := packet.LocalAddr().String()
	listener, err := net.Listen("tcp", endpoint)
	if err != nil {
		t.Skipf("gateway TCP port %s is already taken: %v", endpoint, err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	now := time.Now()
	provider, err := identity.InitProvider(filepath.Join(t.TempDir(), "provider"), "test provider", endpoint, now)
	if err != nil {
		t.Fatal(err)
	}
	account, err := provider.Store.AddAccount("test account", time.Time{}, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	_, invitation, err := provider.CreateInvitation(account.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	server, err := pep.NewServer(pep.ServerConfig{
		ListenAddr: endpoint, Credentials: provider.ServerCredentials(),
		Enrollment:        &identity.EnrollmentService{Provider: provider},
		DestinationPolicy: pep.DestinationPolicy{AllowPrivate: true},
		EnableTCP:         true, EnableQUIC: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = server.ServeListener(ctx, listener) }()
	go func() { defer wg.Done(); _ = server.ServePacketConn(ctx, packet) }()
	// One cleanup, because cancelling and waiting are not independent: cleanups
	// run last-registered-first, and a separate wait would run before the
	// cancel that lets it return.
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})

	enrollCtx, cancelEnroll := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelEnroll()
	profile, err := identity.Enroll(enrollCtx, invitation, "test device", 20*time.Second)
	if err != nil {
		t.Fatalf("enroll device: %v", err)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	return &testGateway{profileJSON: string(encoded)}
}

// startEchoOrigin is the destination an export-mode consumer ultimately reaches.
func startEchoOrigin(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr().String()
}

func startUDPEchoOrigin(t *testing.T) string {
	t.Helper()
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, peer, readErr := socket.ReadFromUDP(buffer)
			if readErr != nil {
				return
			}
			_, _ = socket.WriteToUDP(buffer[:n], peer)
		}
	}()
	return socket.LocalAddr().String()
}

// consumer is one app on the device talking to the exported listener, in the
// shape v2rayNG, mihomo, and sing-box all use: RFC 1929 then one request.
type consumer struct {
	conn net.Conn
}

func dialConsumer(t *testing.T, listen string) *consumer {
	t.Helper()
	conn, err := net.DialTimeout("tcp", listen, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return &consumer{conn: conn}
}

// greet performs method selection and, when a username is supplied, the RFC 1929
// sub-negotiation. It returns the sub-negotiation status so a test can assert on
// a rejection rather than only on a downstream failure.
func (c *consumer) greet(method byte, username, password string) (byte, error) {
	if _, err := c.conn.Write([]byte{5, 1, method}); err != nil {
		return 0, err
	}
	var selected [2]byte
	if _, err := io.ReadFull(c.conn, selected[:]); err != nil {
		return 0, err
	}
	if selected[0] != 5 {
		return 0, errors.New("gateway replied with a non-SOCKS5 version")
	}
	if selected[1] != method {
		return 0, errors.New("listener refused the offered authentication method")
	}
	if method != 2 {
		return 0, nil
	}
	request := []byte{1, byte(len(username))}
	request = append(request, username...)
	request = append(request, byte(len(password)))
	request = append(request, password...)
	if _, err := c.conn.Write(request); err != nil {
		return 0, err
	}
	var status [2]byte
	if _, err := io.ReadFull(c.conn, status[:]); err != nil {
		return 0, err
	}
	return status[1], nil
}

func (c *consumer) request(command byte, destination string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(destination)
	if err != nil {
		return nil, err
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, errors.New("test destinations are IPv4")
	}
	message := append([]byte{5, command, 0, 1}, ip...)
	message = binary.BigEndian.AppendUint16(message, uint16(port))
	if _, err := c.conn.Write(message); err != nil {
		return nil, err
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(c.conn, reply); err != nil {
		return nil, err
	}
	if reply[1] != socks5.ReplySucceeded {
		return nil, fmt.Errorf("SOCKS reply code %d", reply[1])
	}
	return reply, nil
}

// TestExportModeEndToEnd is the whole export product in one test: a real trust
// domain, a real gateway, a session started through the gomobile entry point
// Android calls, and a consumer that authenticates the way a configured proxy
// client does. It also pins the negative cases, because on Android this
// listener shares loopback with every other installed app and a listener that
// can be talked to without credentials is the failure that matters.
func TestExportModeEndToEnd(t *testing.T) {
	gateway := startTestGateway(t)
	origin := startEchoOrigin(t)
	udpOrigin := startUDPEchoOrigin(t)

	observer := &recordingObserver{}
	session := NewSession(observer, nil)
	// No Protector: without a VpnService of its own the app cannot call
	// protect(), and export mode must not require one.
	if err := session.StartProxy(gateway.profileJSON, "127.0.0.1:0", "queqiao", "s3cret-token"); err != nil {
		t.Fatalf("start export session: %v", err)
	}
	t.Cleanup(func() { _ = session.Stop() })

	if state := session.State(); state != StateRunning {
		t.Fatalf("state = %s, want %s", state, StateRunning)
	}
	if !observer.seen(StateStarting) || !observer.seen(StateRunning) {
		t.Fatal("observer did not see the start transitions")
	}
	listen := session.ListenAddress()
	if listen == "" {
		t.Fatal("export session reported no listen address")
	}
	if err := validateLoopbackListenAddr(listen); err != nil {
		t.Fatalf("bound address is not loopback: %v", err)
	}

	t.Run("authenticated CONNECT reaches the origin through the gateway", func(t *testing.T) {
		client := dialConsumer(t, listen)
		status, err := client.greet(2, "queqiao", "s3cret-token")
		if err != nil {
			t.Fatal(err)
		}
		if status != 0 {
			t.Fatalf("authentication status = %d", status)
		}
		if _, err := client.request(socks5.CommandConnect, origin); err != nil {
			t.Fatal(err)
		}
		// Large enough to cross a single frame, so this exercises the data path
		// rather than only the handshake.
		payload := bytes.Repeat([]byte("queqiao-export-"), 4096)
		if _, err := client.conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if tcp, ok := client.conn.(*net.TCPConn); ok {
			if err := tcp.CloseWrite(); err != nil {
				t.Fatal(err)
			}
		}
		got, err := io.ReadAll(client.conn)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("echo mismatch: got %d bytes, want %d", len(got), len(payload))
		}
	})

	t.Run("authenticated UDP ASSOCIATE relays datagrams", func(t *testing.T) {
		client := dialConsumer(t, listen)
		status, err := client.greet(2, "queqiao", "s3cret-token")
		if err != nil {
			t.Fatal(err)
		}
		if status != 0 {
			t.Fatalf("authentication status = %d", status)
		}
		reply, err := client.request(socks5.CommandUDPAssociate, "0.0.0.0:0")
		if err != nil {
			t.Fatal(err)
		}
		relay := net.JoinHostPort(net.IP(reply[4:8]).String(),
			strconv.Itoa(int(binary.BigEndian.Uint16(reply[8:10]))))
		socket, err := net.Dial("udp", relay)
		if err != nil {
			t.Fatal(err)
		}
		defer socket.Close()
		if err := socket.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
			t.Fatal(err)
		}
		payload := []byte("export-datagram")
		var datagram bytes.Buffer
		if err := socks5.WriteUDPDatagram(&datagram, udpOrigin, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := socket.Write(datagram.Bytes()); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 2048)
		n, err := socket.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		echoed, err := socks5.ReadUDPDatagram(buffer[:n])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(echoed.Payload, payload) {
			t.Fatalf("UDP echo = %q, want %q", echoed.Payload, payload)
		}
	})

	t.Run("a wrong password is rejected", func(t *testing.T) {
		client := dialConsumer(t, listen)
		status, err := client.greet(2, "queqiao", "wrong")
		if err != nil {
			t.Fatal(err)
		}
		if status == 0 {
			t.Fatal("listener accepted a wrong password")
		}
	})

	t.Run("an unauthenticated app on the same loopback is rejected", func(t *testing.T) {
		client := dialConsumer(t, listen)
		if _, err := client.greet(0, "", ""); err == nil {
			t.Fatal("listener accepted a client that offered no authentication")
		}
	})

	t.Run("metrics report export mode rather than an idle tunnel", func(t *testing.T) {
		var report struct {
			Version int    `json:"version"`
			State   string `json:"state"`
			Mode    string `json:"mode"`
			Listen  string `json:"listen"`
			Packets any    `json:"packets"`
		}
		if err := json.Unmarshal([]byte(session.MetricsJSON()), &report); err != nil {
			t.Fatal(err)
		}
		if report.Mode != ModeProxy {
			t.Fatalf("mode = %q, want %q", report.Mode, ModeProxy)
		}
		if report.State != StateRunning {
			t.Fatalf("state = %q", report.State)
		}
		if report.Listen != listen {
			t.Fatalf("listen = %q, want %q", report.Listen, listen)
		}
		// An export session has no packet engine at all. Reporting zeroes would
		// let a UI draw a stalled tunnel that does not exist.
		if counters, ok := report.Packets.(map[string]any); !ok || len(counters) != 0 {
			t.Fatalf("packets = %#v, want an empty object", report.Packets)
		}
	})

	if err := session.Stop(); err != nil {
		t.Fatalf("stop export session: %v", err)
	}
	if state := session.State(); state != StateStopped {
		t.Fatalf("state after stop = %s", state)
	}
	if session.ListenAddress() != "" {
		t.Fatal("stopped session still reports a listen address")
	}
}

// TestExportModeRejectsUnsafeConfiguration covers what a platform integration
// can get wrong before any traffic flows.
func TestExportModeRejectsUnsafeConfiguration(t *testing.T) {
	session := NewSession(&recordingObserver{}, nil)
	for _, test := range []struct {
		name     string
		listen   string
		username string
		password string
	}{
		{name: "off-host listener", listen: "0.0.0.0:1080", username: "u", password: "p"},
		{name: "routable listener", listen: "10.0.0.5:1080", username: "u", password: "p"},
		{name: "no username", listen: "127.0.0.1:0", username: "", password: "p"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := session.StartProxy(`{"version":1}`, test.listen, test.username, test.password)
			if err == nil {
				t.Fatal("StartProxy accepted an unsafe configuration")
			}
			if state := session.State(); state != StateStopped {
				t.Fatalf("state = %s after a rejected start", state)
			}
		})
	}
}
