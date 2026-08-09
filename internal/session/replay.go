package session

import (
	"errors"
	"sync"
	"time"
)

// ReplayGuard remembers recently accepted authenticated nonces. The TLS layer
// prevents passive capture, but explicit replay rejection also protects
// against credentialed clients accidentally or maliciously reusing a HELLO.
type ReplayGuard struct {
	mu      sync.Mutex
	entries map[[16]byte]time.Time
	ttl     time.Duration
	max     int
}

func NewReplayGuard(ttl time.Duration, maxEntries int) *ReplayGuard {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if maxEntries <= 0 {
		maxEntries = 100_000
	}
	return &ReplayGuard{entries: make(map[[16]byte]time.Time), ttl: ttl, max: maxEntries}
}

func (g *ReplayGuard) Accept(nonce [16]byte, now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if expiry, ok := g.entries[nonce]; ok && expiry.After(now) {
		return errors.New("replayed session hello")
	}
	if len(g.entries) >= g.max {
		for key, expiry := range g.entries {
			if !expiry.After(now) {
				delete(g.entries, key)
			}
		}
		if len(g.entries) >= g.max {
			return errors.New("authentication replay cache is full")
		}
	}
	g.entries[nonce] = now.Add(g.ttl)
	return nil
}
