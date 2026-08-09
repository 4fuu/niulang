package limiter

import (
	"context"
	"testing"
	"time"
)

func TestBulkCannotConsumeInteractiveReserve(t *testing.T) {
	b := New(Config{TotalBytesPerSec: 1000, ReserveBytesPerSec: 500, Burst: time.Second})
	if err := b.Wait(context.Background(), int(b.bulkCap), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx, 100, false); err == nil {
		t.Fatal("bulk unexpectedly consumed reserved capacity")
	}
}

func TestInteractiveReserveSurvivesBulk(t *testing.T) {
	b := New(Config{TotalBytesPerSec: 1000, ReserveBytesPerSec: 500, Burst: time.Second})
	if err := b.Wait(context.Background(), int(b.bulkCap), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx, 250, true); err != nil {
		t.Fatal(err)
	}
}

func TestDisabledBudgetAndCancellation(t *testing.T) {
	if err := (*Budget)(nil).Wait(context.Background(), 1<<30, false); err != nil {
		t.Fatal(err)
	}
	b := New(Config{TotalBytesPerSec: 100, ReserveBytesPerSec: 0, Burst: time.Millisecond})
	if err := b.Wait(context.Background(), int(b.bulkCap), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Wait(ctx, 1000, false); err == nil {
		t.Fatal("canceled wait succeeded")
	}
}
