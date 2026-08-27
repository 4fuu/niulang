package pep

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/protocol"
)

func requireFlowGrant(t *testing.T, grant *flowGrant) {
	t.Helper()
	select {
	case <-grant.ready:
	case <-time.After(time.Second):
		t.Fatal("flow scheduling grant did not become ready")
	}
}

func requireFlowGrantPending(t *testing.T, grant *flowGrant) {
	t.Helper()
	select {
	case <-grant.ready:
		t.Fatal("flow scheduling grant became ready early")
	default:
	}
}

func TestFlowSchedulerBoundsStartupPreference(t *testing.T) {
	scheduler := newFlowScheduler(flowSchedulerConfig{
		startupBytes: 100, perFlow: 1, maxActive: 1,
		interactiveBurst: 3, bulkAge: time.Hour,
	})
	blocker := scheduler.request(99, protocol.ClassNew, 1)
	requireFlowGrant(t, blocker)

	startup := scheduler.request(1, protocol.ClassBulk, 100)
	bulk := scheduler.request(1, protocol.ClassBulk, 100)
	if !startup.high {
		t.Fatal("a flow's bounded startup service was not preferred")
	}
	if bulk.high {
		t.Fatal("a flow remained preferred after consuming its startup service")
	}
	requireFlowGrantPending(t, startup)
	requireFlowGrantPending(t, bulk)

	scheduler.release(blocker)
	requireFlowGrant(t, startup)
	scheduler.closeFlow(99)
	if !scheduler.closeFlow(1) {
		t.Fatal("closing the only remaining flow did not empty the scheduler")
	}
}

func TestFlowSchedulerGivesBulkOneGrantInFour(t *testing.T) {
	scheduler := newFlowScheduler(flowSchedulerConfig{
		startupBytes: 1, perFlow: 1, maxActive: 1,
		interactiveBurst: 3, bulkAge: time.Hour,
	})
	blocker := scheduler.request(99, protocol.ClassNew, 1)
	requireFlowGrant(t, blocker)
	high := []*flowGrant{
		scheduler.request(1, protocol.ClassInteractive, 1),
		scheduler.request(2, protocol.ClassInteractive, 1),
		scheduler.request(3, protocol.ClassInteractive, 1),
		scheduler.request(4, protocol.ClassInteractive, 1),
	}
	scheduler.mu.Lock()
	scheduler.states[10] = &flowScheduleState{
		startupAssigned: 1,
		grants:          make(map[*flowGrant]struct{}),
	}
	scheduler.highRun = 0
	scheduler.mu.Unlock()
	bulk := scheduler.request(10, protocol.ClassBulk, 1)
	if bulk.high {
		t.Fatal("primed bulk flow was queued as startup traffic")
	}

	scheduler.release(blocker)
	for i := 0; i < 3; i++ {
		requireFlowGrant(t, high[i])
		requireFlowGrantPending(t, bulk)
		scheduler.release(high[i])
	}
	requireFlowGrant(t, bulk)
	requireFlowGrantPending(t, high[3])
	scheduler.release(bulk)
	requireFlowGrant(t, high[3])

	for _, flowID := range []uint64{1, 2, 3, 4, 10, 99} {
		scheduler.closeFlow(flowID)
	}
	if !scheduler.empty() {
		t.Fatal("scheduler retained state after every flow closed")
	}
}

func TestFlowSchedulerCloseWakesPendingAndReclaimsGranted(t *testing.T) {
	scheduler := newFlowScheduler(flowSchedulerConfig{
		startupBytes: 1, perFlow: 1, maxActive: 1,
		interactiveBurst: 3, bulkAge: time.Hour,
	})
	granted := scheduler.request(1, protocol.ClassNew, 1)
	pending := scheduler.request(1, protocol.ClassNew, 1)
	requireFlowGrant(t, granted)
	requireFlowGrantPending(t, pending)

	if !scheduler.closeFlow(1) {
		t.Fatal("closing a flow did not reclaim its granted scheduling credit")
	}
	requireFlowGrant(t, pending)
	for _, grant := range []*flowGrant{granted, pending} {
		if !errors.Is(grant.done, errFlowSchedulingClosed) {
			t.Fatalf("closed grant error = %v, want flow scheduling closed", grant.done)
		}
	}
}

func TestFlowSchedulerSetRejectsClosedFlowWithoutRetainingPath(t *testing.T) {
	set := newFlowSchedulerSet(1)
	stop := make(chan struct{})
	close(stop)

	if _, err := set.acquire(context.Background(), stop, "test-path", 1, protocol.ClassNew, 1); !errors.Is(err, errFlowSchedulingClosed) {
		t.Fatalf("closed flow acquire error = %v, want flow scheduling closed", err)
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if len(set.paths) != 0 {
		t.Fatalf("closed flow retained %d path schedulers", len(set.paths))
	}
}
