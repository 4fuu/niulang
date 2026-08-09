// Package classifier implements the transport-independent flow classifier.
// It intentionally uses observable flow statistics rather than HTTPS
// decryption or a TLS MITM.
package classifier

import "time"

// Class is the scheduling class assigned to a flow.
type Class uint8

const (
	ClassNew Class = iota
	ClassInteractive
	ClassBulk
)

func (c Class) String() string {
	switch c {
	case ClassNew:
		return "new"
	case ClassInteractive:
		return "interactive"
	case ClassBulk:
		return "bulk"
	default:
		return "unknown"
	}
}

// Config controls transition thresholds. Values are deliberately explicit so
// deployments can tune them from measurements rather than hiding policy in
// the implementation.
type Config struct {
	NewBytes            uint64
	NewAge              time.Duration
	BulkBytes           uint64
	BulkRateBytesPerSec float64
	BulkMinimumAge      time.Duration
	InteractiveMaxRate  float64
	InteractiveIdleGap  time.Duration
}

func DefaultConfig() Config {
	return Config{
		NewBytes:            64 * 1024,
		NewAge:              3 * time.Second,
		BulkBytes:           2 * 1024 * 1024,
		BulkRateBytesPerSec: 2 * 1024 * 1024,
		BulkMinimumAge:      2 * time.Second,
		InteractiveMaxRate:  1 * 1024 * 1024,
		InteractiveIdleGap:  250 * time.Millisecond,
	}
}

// Observation contains statistics accumulated by the flow observer. Rates
// are bytes per second over a recent, implementation-defined window.
type Observation struct {
	BytesUp, BytesDown       uint64
	UpRate, DownRate         float64
	Age                      time.Duration
	SinceLastPayload         time.Duration
	Bidirectional            bool
	SmallBidirectionalBursts bool
}

// Classifier is stateful. Once a flow becomes bulk it remains bulk until it
// closes; this hysteresis prevents queue policy from flapping during a short
// idle gap in a large transfer.
type Classifier struct {
	cfg   Config
	class Class
}

func New(cfg Config) *Classifier {
	if cfg.NewBytes == 0 || cfg.NewAge <= 0 || cfg.BulkBytes == 0 ||
		cfg.BulkRateBytesPerSec <= 0 || cfg.BulkMinimumAge <= 0 {
		cfg = DefaultConfig()
	}
	return &Classifier{cfg: cfg, class: ClassNew}
}

func (c *Classifier) Class() Class { return c.class }

// Observe advances the flow class. The caller should call this at a bounded
// cadence (for example, once per scheduler tick), not once per packet.
func (c *Classifier) Observe(o Observation) Class {
	if c.class == ClassBulk {
		return c.class
	}
	if c.isBulk(o) {
		c.class = ClassBulk
		return c.class
	}
	if c.class == ClassNew && o.Age >= c.cfg.NewAge {
		if c.isInteractive(o) || o.BytesUp+o.BytesDown >= c.cfg.NewBytes {
			c.class = ClassInteractive
		}
	}
	return c.class
}

func (c *Classifier) isBulk(o Observation) bool {
	return o.Age >= c.cfg.BulkMinimumAge &&
		o.BytesUp+o.BytesDown >= c.cfg.BulkBytes &&
		max(o.UpRate, o.DownRate) >= c.cfg.BulkRateBytesPerSec &&
		(!o.Bidirectional || !o.SmallBidirectionalBursts)
}

func (c *Classifier) isInteractive(o Observation) bool {
	if o.Bidirectional && o.SmallBidirectionalBursts {
		return true
	}
	return o.SinceLastPayload >= c.cfg.InteractiveIdleGap &&
		max(o.UpRate, o.DownRate) <= c.cfg.InteractiveMaxRate
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
