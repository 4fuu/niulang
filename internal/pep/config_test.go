package pep

import (
	"testing"
)

func TestClientRejectsUnserviceableConfiguration(t *testing.T) {
	base := ClientConfig{ListenAddr: "127.0.0.1:0", RemoteAddr: "127.0.0.1:1", ServerName: "wanopt.test", Secret: []byte("0123456789abcdef")}
	for name, mutate := range map[string]func(*ClientConfig){
		"too many sessions": func(c *ClientConfig) { c.MaxSessions = maxConfiguredSessions + 1 },
		"adaptive bounds":   func(c *ClientConfig) { c.AdaptiveMinBytesSec = 2; c.AdaptiveMaxBytesSec = 1 },
		"reserve without budget": func(c *ClientConfig) {
			c.InteractiveReserveBytesPerSec = 1
		},
		"reserve above budget": func(c *ClientConfig) {
			c.AggregateBytesPerSec = 1
			c.InteractiveReserveBytesPerSec = 2
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
