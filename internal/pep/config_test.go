package pep

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
	"github.com/bojieli/queqiao/internal/limiter"
	"github.com/bojieli/queqiao/internal/protocol"
)

func TestClientRejectsUnserviceableConfiguration(t *testing.T) {
	_, credentials := testCertificate(t)
	base := ClientConfig{ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", Credentials: credentials}
	for name, mutate := range map[string]func(*ClientConfig){
		"too many sessions":      func(c *ClientConfig) { c.MaxSessions = maxConfiguredSessions + 1 },
		"too many pending opens": func(c *ClientConfig) { c.MaxPendingOpens = maxConfiguredSessions + 1 },
		"invalid local address":  func(c *ClientConfig) { c.LocalAddress = "not-an-address" },
		"empty local interface":  func(c *ClientConfig) { c.LocalAddress = "if:" },
		"too many TCP lanes":     func(c *ClientConfig) { c.TCPFallbackLanes = maxTCPFallbackLanes + 1 },
		"adaptive bounds":        func(c *ClientConfig) { c.AdaptiveMinBytesSec = 2; c.AdaptiveMaxBytesSec = 1 },
		"reserve without budget": func(c *ClientConfig) {
			c.InteractiveReserveBytesPerSec = 1
		},
		"reserve above budget": func(c *ClientConfig) {
			c.AggregateBytesPerSec = 1
			c.InteractiveReserveBytesPerSec = 2
		},
		"reserve consumes whole budget": func(c *ClientConfig) {
			c.AggregateBytesPerSec = 2
			c.InteractiveReserveBytesPerSec = 2
		},
		"idle exceeds lifetime": func(c *ClientConfig) {
			c.FlowIdleTimeout = 2 * time.Second
			c.FlowMaxLifetime = time.Second
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := NewClient(cfg); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestClientAdmissionDefaultsAndPendingOpenBound(t *testing.T) {
	_, credentials := testCertificate(t)
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", Credentials: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.cfg.MaxSessions != defaultClientMaxSessions {
		t.Fatalf("client default sessions = %d, want %d", client.cfg.MaxSessions, defaultClientMaxSessions)
	}
	if client.cfg.MaxPendingOpens != defaultMaxPendingOpens || cap(client.pendingOpens) != defaultMaxPendingOpens {
		t.Fatalf("client default pending opens = %d/%d, want %d", client.cfg.MaxPendingOpens, cap(client.pendingOpens), defaultMaxPendingOpens)
	}

	client.pendingOpens = make(chan struct{}, 2)
	for admitted := 0; admitted < 2; admitted++ {
		if !client.admitPendingOpen() {
			t.Fatalf("configured pending-open capacity stopped after %d admissions", admitted)
		}
	}
	if client.admitPendingOpen() {
		t.Fatal("pending-open capacity was exceeded")
	}
	client.releasePendingOpen()
	if !client.admitPendingOpen() {
		t.Fatal("released pending-open capacity was not reusable")
	}
}

func TestClientCredentialUpdateCannotChangeIdentity(t *testing.T) {
	_, credentials := testCertificate(t)
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", Credentials: credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateCredentials(credentials); err != nil {
		t.Fatalf("same-device credential refresh failed: %v", err)
	}
	_, other := testCertificate(t)
	if err := client.UpdateCredentials(other); err == nil {
		t.Fatal("credential update changed the client trust domain and device")
	}
}

func TestCodedDataGetsOneReliableSafetyCopyBeforeOpenConfirmation(t *testing.T) {
	frameConn := &frameConn{}
	unconfirmed := true
	frameConn.setOpenSafetyPolicy(func() bool { return unconfirmed })
	data := protocol.Frame{Header: protocol.Header{Type: protocol.TypeData}}
	if !frameConn.needsOpenSafetyCopy(data) {
		t.Fatal("first pre-confirmation coded frame had no reliable safety copy")
	}
	if frameConn.needsOpenSafetyCopy(data) {
		t.Fatal("pre-confirmation safety copy was not bounded to one frame")
	}
	frameConn.setOpenSafetyPolicy(func() bool { return false })
	if frameConn.needsOpenSafetyCopy(data) {
		t.Fatal("confirmed flow retained an unnecessary safety copy")
	}
}

func TestPerUserSessionLimitSpansDevicesAndReleases(t *testing.T) {
	store, err := identity.NewStore(filepath.Join(t.TempDir(), "authorization.json"))
	if err != nil || store.Initialize() != nil {
		t.Fatal(err)
	}
	now := time.Now()
	account, err := store.AddAccount("alice", time.Time{}, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, _, _ := ed25519.GenerateKey(rand.Reader)
	_, token, _ := store.CreateInvite(account.ID, time.Hour, now)
	_, device, err := store.ConsumeInvite(token, "laptop", publicKey, now)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{AccountID: account.ID, DeviceID: device.ID, PublicKey: publicKey}
	server := &Server{cfg: ServerConfig{Credentials: identity.ServerCredentials{Store: store}}, accountSessions: make(map[string]int)}
	if err := server.acquireAccountSession(principal); err != nil {
		t.Fatal(err)
	}
	if err := server.acquireAccountSession(principal); err == nil {
		t.Fatal("per-user session limit was exceeded")
	}
	server.releaseAccountSession(account.ID)
	if err := server.acquireAccountSession(principal); err != nil {
		t.Fatalf("released per-user session slot was not reusable: %v", err)
	}
	server.releaseAccountSession(account.ID)
}

func TestTUICAlignedCongestionConfigurationIsAccepted(t *testing.T) {
	_, credentials := testCertificate(t)
	base := ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1",
		Credentials: credentials, Congestion: CongestionBBRTUIC,
	}
	if _, err := NewClient(base); err != nil {
		t.Fatalf("bbr-tuic configuration rejected: %v", err)
	}
}

func TestServerRejectsUnserviceableConfiguration(t *testing.T) {
	credentials, _ := testCertificate(t)
	base := ServerConfig{ListenAddr: "127.0.0.1:0", Credentials: credentials}
	for name, mutate := range map[string]func(*ServerConfig){
		"too many sessions": func(c *ServerConfig) { c.MaxSessions = maxConfiguredSessions + 1 },
		"adaptive bounds":   func(c *ServerConfig) { c.AdaptiveMinBytesSec = 2; c.AdaptiveMaxBytesSec = 1 },
		"too many TCP lanes": func(c *ServerConfig) {
			c.TCPFallbackLanes = maxTCPFallbackLanes + 1
		},
		"invalid TCP congestion name": func(c *ServerConfig) { c.TCPCongestion = "bbr;no" },
		"reserve without budget": func(c *ServerConfig) {
			c.InteractiveReserveBytesPerSec = 1
		},
		"reserve above budget": func(c *ServerConfig) {
			c.AggregateBytesPerSec = 1
			c.InteractiveReserveBytesPerSec = 2
		},
		"reserve consumes whole budget": func(c *ServerConfig) {
			c.AggregateBytesPerSec = 2
			c.InteractiveReserveBytesPerSec = 2
		},
		"idle exceeds lifetime": func(c *ServerConfig) {
			c.FlowIdleTimeout = 2 * time.Second
			c.FlowMaxLifetime = time.Second
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := NewServer(cfg); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
}

func TestTCPFallbackRoleDefaultsAreConservativeAtTheClient(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", Credentials: clientCredentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.cfg.TCPFallbackLanes != 1 {
		t.Fatalf("client default TCP lanes = %d, want conservative one", client.cfg.TCPFallbackLanes)
	}

	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.cfg.TCPFallbackLanes != maxTCPFallbackLanes {
		t.Fatalf("server default TCP lane ceiling = %d, want %d", server.cfg.TCPFallbackLanes, maxTCPFallbackLanes)
	}
}

func TestTCPFallbackCongestionNameNormalization(t *testing.T) {
	for input, want := range map[string]string{"": "system", " SYSTEM ": "system", " BBR ": "bbr", "bbr2": "bbr2"} {
		got, err := normalizeTCPCongestion(input)
		if err != nil {
			t.Fatalf("normalize %q: %v", input, err)
		}
		if got != want {
			t.Fatalf("normalize %q = %q, want %q", input, got, want)
		}
	}
}

func TestQUICConnectionsHaveAnAdmissionBound(t *testing.T) {
	credentials, _ := testCertificate(t)
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Credentials: credentials, MaxSessions: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !server.admitConnection() {
		t.Fatal("the first configured connection slot was not admitted")
	}
	if !server.admitConnection() {
		t.Fatal("the configured connection capacity was not admitted")
	}
	if server.admitConnection() {
		t.Fatal("an unauthenticated QUIC connection exceeded the admission bound")
	}
	server.releaseConnection()
	if !server.admitConnection() {
		t.Fatal("released QUIC connection capacity was not reusable")
	}
}

func TestServerQUICStreamCapacitySupportsMobileSessions(t *testing.T) {
	config := quicServerConfig(flowWindows{})
	if config.MaxIncomingStreams < 1024 {
		t.Fatalf("server QUIC stream capacity = %d, want at least 1024", config.MaxIncomingStreams)
	}
}

// A chunk larger than the aggregate budget's burst can never be admitted: the
// budget refuses it outright rather than pacing it, the DATA frame carrying it
// fails to enqueue, and the lane that was carrying it is failed and retired.
// The configured chunk size is corrected to what the budget can carry so that
// an operator's rate limit slows the endpoint down instead of taking its lanes
// apart.
func TestChunkSizeIsCappedByAggregateBurst(t *testing.T) {
	serverCredentials, clientCredentials := testCertificate(t)
	// 128 KiB chunks against 64 KiB/s: a quarter-second burst is well under
	// the 64 KiB floor, so the floor is the ceiling and every chunk as
	// configured would have been refused.
	const rate = 64 * 1024
	newEndpoints := func(t *testing.T, aggregate uint64) (int, *limiter.Budget, int, *limiter.Budget) {
		t.Helper()
		client, err := NewClient(ClientConfig{
			ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", Credentials: clientCredentials,
			ChunkSize: protocol.MaxPayload, AggregateBytesPerSec: aggregate,
		})
		if err != nil {
			t.Fatal(err)
		}
		server, err := NewServer(ServerConfig{
			ListenAddr: "127.0.0.1:0", Credentials: serverCredentials,
			ChunkSize: protocol.MaxPayload, AggregateBytesPerSec: aggregate,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client.cfg.ChunkSize, client.budget, server.cfg.ChunkSize, server.budget
	}

	clientChunk, clientBudget, serverChunk, serverBudget := newEndpoints(t, rate)
	for name, endpoint := range map[string]struct {
		chunkSize int
		budget    *limiter.Budget
	}{
		"client": {clientChunk, clientBudget},
		"server": {serverChunk, serverBudget},
	} {
		t.Run(name, func(t *testing.T) {
			if endpoint.chunkSize >= protocol.MaxPayload {
				t.Fatalf("chunk size %d was not capped by the aggregate burst", endpoint.chunkSize)
			}
			// A cap, not a collapse: a budget that admits bulk at all admits a
			// whole 64 KiB frame of it, so the correction never leaves the
			// endpoint sending uselessly small chunks.
			if endpoint.chunkSize < 64*1024 {
				t.Fatalf("chunk size %d narrowed below the burst floor", endpoint.chunkSize)
			}
			if err := endpoint.budget.Wait(context.Background(), endpoint.chunkSize, false); err != nil {
				t.Fatalf("a full chunk was not admitted by its own budget: %v", err)
			}
		})
	}

	// Without a budget there is nothing to fit inside, and the chunk size the
	// operator asked for is the one they get.
	unpacedClient, _, unpacedServer, _ := newEndpoints(t, 0)
	if unpacedClient != protocol.MaxPayload || unpacedServer != protocol.MaxPayload {
		t.Fatalf("unpaced chunk sizes = %d/%d, want %d", unpacedClient, unpacedServer, protocol.MaxPayload)
	}
}
