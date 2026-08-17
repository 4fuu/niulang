package pep

import (
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/session"
)

func TestClientRejectsUnserviceableConfiguration(t *testing.T) {
	base := ClientConfig{ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", ServerName: "queqiao.test", Secret: []byte("0123456789abcdef")}
	for name, mutate := range map[string]func(*ClientConfig){
		"too many sessions":     func(c *ClientConfig) { c.MaxSessions = maxConfiguredSessions + 1 },
		"invalid local address": func(c *ClientConfig) { c.LocalAddress = "not-an-address" },
		"empty local interface": func(c *ClientConfig) { c.LocalAddress = "if:" },
		"too many TCP lanes":    func(c *ClientConfig) { c.TCPFallbackLanes = maxTCPFallbackLanes + 1 },
		"adaptive bounds":       func(c *ClientConfig) { c.AdaptiveMinBytesSec = 2; c.AdaptiveMaxBytesSec = 1 },
		"reserve without budget": func(c *ClientConfig) {
			c.InteractiveReserveBytesPerSec = 1
		},
		"reserve above budget": func(c *ClientConfig) {
			c.AggregateBytesPerSec = 1
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

func TestTUICAlignedCongestionConfigurationIsAccepted(t *testing.T) {
	base := ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", ServerName: "queqiao.test",
		Secret: []byte("0123456789abcdef"), Congestion: CongestionBBRTUIC,
	}
	if _, err := NewClient(base); err != nil {
		t.Fatalf("bbr-tuic configuration rejected: %v", err)
	}
}

func TestServerRejectsUnserviceableConfiguration(t *testing.T) {
	certificate, _ := testCertificate(t)
	base := ServerConfig{ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: []byte("0123456789abcdef")}
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
	client, err := NewClient(ClientConfig{
		ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", ServerName: "queqiao.test",
		Secret: []byte("0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.cfg.TCPFallbackLanes != 1 {
		t.Fatalf("client default TCP lanes = %d, want legacy-safe one", client.cfg.TCPFallbackLanes)
	}

	certificate, _ := testCertificate(t)
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate, Secret: []byte("0123456789abcdef"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.cfg.TCPFallbackLanes != maxTCPFallbackLanes {
		t.Fatalf("server default TCP lane ceiling = %d, want %d", server.cfg.TCPFallbackLanes, maxTCPFallbackLanes)
	}
	if server.tcpCapabilities&session.CapabilityTCPStriping == 0 {
		t.Fatal("server with a multi-lane ceiling did not advertise TCP striping")
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
	certificate, _ := testCertificate(t)
	server, err := NewServer(ServerConfig{
		ListenAddr: "127.0.0.1:0", Certificate: certificate,
		Secret: []byte("0123456789abcdef"), MaxSessions: 2,
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
