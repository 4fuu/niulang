package pep

import (
	"testing"
	"time"
)

func TestClientRejectsUnserviceableConfiguration(t *testing.T) {
	base := ClientConfig{ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", ServerName: "queqiao.test", Secret: []byte("0123456789abcdef")}
	for name, mutate := range map[string]func(*ClientConfig){
		"too many sessions":     func(c *ClientConfig) { c.MaxSessions = maxConfiguredSessions + 1 },
		"invalid local address": func(c *ClientConfig) { c.LocalAddress = "not-an-address" },
		"empty local interface": func(c *ClientConfig) { c.LocalAddress = "if:" },
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
