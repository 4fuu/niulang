package classifier

import (
	"testing"
	"time"
)

func TestNewFlowGetsInteractiveClassAfterBudget(t *testing.T) {
	c := New(DefaultConfig())
	got := c.Observe(Observation{
		Age:                      4 * time.Second,
		SinceLastPayload:         300 * time.Millisecond,
		BytesUp:                  1024,
		BytesDown:                2048,
		Bidirectional:            true,
		SmallBidirectionalBursts: true,
	})
	if got != ClassInteractive {
		t.Fatalf("class = %s, want interactive", got)
	}
}

func TestSustainedOneWayFlowBecomesBulk(t *testing.T) {
	c := New(DefaultConfig())
	got := c.Observe(Observation{
		Age:              3 * time.Second,
		BytesDown:        4 * 1024 * 1024,
		DownRate:         8 * 1024 * 1024,
		SinceLastPayload: 5 * time.Millisecond,
	})
	if got != ClassBulk {
		t.Fatalf("class = %s, want bulk", got)
	}
}

func TestBulkPromotionStartsAtEarlyLaneIsolationBoundary(t *testing.T) {
	c := New(DefaultConfig())
	got := c.Observe(Observation{
		Age: 1 * time.Second, BytesDown: 128 * 1024,
		DownRate: 128 * 1024, SinceLastPayload: 5 * time.Millisecond,
	})
	if got != ClassBulk {
		t.Fatalf("class = %s, want bulk at the configured promotion boundary", got)
	}
}

func TestConstrainedOneWayFlowStillBecomesBulk(t *testing.T) {
	c := New(DefaultConfig())
	got := c.Observe(Observation{
		Age: 4 * time.Second, BytesDown: 4 * 1024 * 1024,
		DownRate: 512 * 1024, SinceLastPayload: 5 * time.Millisecond,
	})
	if got != ClassBulk {
		t.Fatalf("class = %s, want bulk on a constrained path", got)
	}
}

func TestInteractiveBurstIsNotBulk(t *testing.T) {
	c := New(DefaultConfig())
	got := c.Observe(Observation{
		Age:                      30 * time.Second,
		BytesUp:                  8 * 1024 * 1024,
		BytesDown:                8 * 1024 * 1024,
		UpRate:                   32 * 1024,
		DownRate:                 32 * 1024,
		Bidirectional:            true,
		SmallBidirectionalBursts: true,
	})
	if got != ClassInteractive {
		t.Fatalf("class = %s, want interactive", got)
	}
}

func TestBulkClassDoesNotFlap(t *testing.T) {
	c := New(DefaultConfig())
	c.Observe(Observation{Age: 4 * time.Second, BytesDown: 4 * 1024 * 1024, DownRate: 8 * 1024 * 1024})
	got := c.Observe(Observation{Age: 12 * time.Second, BytesDown: 4 * 1024 * 1024, SinceLastPayload: 10 * time.Second})
	if got != ClassBulk {
		t.Fatalf("class = %s after idle gap, want bulk", got)
	}
}
