package pep

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/metrics"
	"github.com/4fuu/niulang/internal/protocol"
	"github.com/4fuu/niulang/internal/stripe"
)

func TestLaneCongestionSamplerFollowsTraceGate(t *testing.T) {
	wasEnabled := laneTrace.Load()
	t.Cleanup(func() { laneTrace.Store(wasEnabled) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flow := &multipathFlow{done: make(chan struct{})}

	laneTrace.Store(false)
	if flow.startLaneCongestionSampler(ctx) {
		t.Fatal("trace-off flow started a lane congestion sampler")
	}
	laneTrace.Store(true)
	if !flow.startLaneCongestionSampler(ctx) {
		t.Fatal("trace-on flow did not start a lane congestion sampler")
	}
}

func newPullerTestFlow() (*multipathFlow, *mpLane, *stripe.Scheduler) {
	control := &mpLane{id: 0}
	data := &mpLane{id: 1}
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}), reserveControlLane: true,
		lanes:             map[uint64]*mpLane{0: control, 1: data},
		controlLaneShared: func() bool { return true },
	}
	flow.class.Store(uint32(protocol.ClassBulk))
	return flow, control, stripe.New(strings.NewReader(""), stripe.DefaultConfig())
}

func runTestPuller(ctx context.Context, flow *multipathFlow, lane *mpLane, sched *stripe.Scheduler) <-chan struct{} {
	done := make(chan struct{})
	lane.pulling.Store(true)
	go func() {
		flow.runLanePuller(ctx, lane, sched)
		close(done)
	}()
	return done
}

func awaitPuller(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("lane puller did not stop after %s", what)
	}
}

func TestLanePullerRechecksEligibility(t *testing.T) {
	flow, lane, sched := newPullerTestFlow()
	checked := make(chan struct{})
	var checkedOnce sync.Once
	flow.controlLaneShared = func() bool {
		checkedOnce.Do(func() { close(checked) })
		return true
	}
	done := runTestPuller(context.Background(), flow, lane, sched)
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("lane puller did not perform its initial eligibility check")
	}
	// Class is atomic and is the production eligibility transition: the
	// reserved control lane carries interactive data but not isolated bulk.
	flow.class.Store(uint32(protocol.ClassInteractive))
	// A closed scheduler makes Next return immediately once the lane observes
	// that transition. While ineligible, the puller deliberately does not ask
	// the scheduler for work, so this also proves that it performed a recheck.
	sched.Close()
	awaitPuller(t, done, "the lane became eligible")
	if lane.pulling.Load() {
		t.Fatal("eligible lane retained its pulling state after scheduler completion")
	}
}

func TestIneligibleLanePullerStopsForLifecycleSignals(t *testing.T) {
	tests := []struct {
		name string
		stop func(context.CancelFunc, *multipathFlow, *mpLane)
	}{
		{name: "context", stop: func(cancel context.CancelFunc, _ *multipathFlow, _ *mpLane) { cancel() }},
		{name: "flow done", stop: func(_ context.CancelFunc, flow *multipathFlow, _ *mpLane) { flow.signalDone() }},
		{name: "lane close", stop: func(_ context.CancelFunc, _ *multipathFlow, lane *mpLane) { lane.closed.Store(true) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow, lane, sched := newPullerTestFlow()
			defer sched.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runTestPuller(ctx, flow, lane, sched)
			tt.stop(cancel, flow, lane)
			awaitPuller(t, done, tt.name)
			if lane.pulling.Load() {
				t.Fatal("stopped lane retained its pulling state")
			}
		})
	}
}

func newCompletionTestFlow(t *testing.T, grace time.Duration) (*multipathFlow, *metrics.Registry, net.Conn) {
	t.Helper()
	inner, peer := net.Pipe()
	registry := metrics.New()
	flow := &multipathFlow{
		ctx: context.Background(), inner: inner, done: make(chan struct{}),
		lanes: make(map[uint64]*mpLane), metrics: registry, completionGrace: grace,
		completionWake: make(chan struct{}, 1),
	}
	return flow, registry, peer
}

func TestCompletionWatchdogFINOrdersAndDuplicateNotifications(t *testing.T) {
	orders := []struct {
		name   string
		notify func(*multipathFlow)
	}{
		{name: "local then remote", notify: func(flow *multipathFlow) {
			flow.noteLocalFINSent()
			flow.noteRemoteFINSeen()
		}},
		{name: "remote then local", notify: func(flow *multipathFlow) {
			flow.noteRemoteFINSeen()
			flow.noteLocalFINSent()
		}},
		{name: "duplicate and before watcher", notify: func(flow *multipathFlow) {
			flow.noteLocalFINSent()
			flow.noteLocalFINSent()
			flow.noteRemoteFINSeen()
			flow.noteRemoteFINSeen()
		}},
	}
	for _, order := range orders {
		t.Run(order.name, func(t *testing.T) {
			flow, registry, peer := newCompletionTestFlow(t, 5*time.Millisecond)
			defer peer.Close()
			stop := make(chan struct{})
			if order.name == "duplicate and before watcher" {
				order.notify(flow)
				go flow.completionWatchdog(stop)
			} else {
				go flow.completionWatchdog(stop)
				order.notify(flow)
			}
			select {
			case <-flow.done:
			case <-time.After(time.Second):
				close(stop)
				t.Fatal("completion grace did not close the proven-complete flow")
			}
			close(stop)
			if got := registry.Snapshot().CompletionTimeouts; got != 1 {
				t.Fatalf("completion timeout metric = %d, want exactly one", got)
			}
		})
	}
}

func TestCompletionWatchdogStopsBeforeFINPair(t *testing.T) {
	tests := []struct {
		name string
		stop func(chan struct{}, context.CancelFunc)
	}{
		{name: "stop", stop: func(stop chan struct{}, _ context.CancelFunc) { close(stop) }},
		{name: "context", stop: func(_ chan struct{}, cancel context.CancelFunc) { cancel() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow, registry, peer := newCompletionTestFlow(t, 5*time.Millisecond)
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			flow.ctx = ctx
			defer cancel()
			stop := make(chan struct{})
			watcherDone := make(chan struct{})
			go func() {
				flow.completionWatchdog(stop)
				close(watcherDone)
			}()
			flow.noteLocalFINSent()
			tt.stop(stop, cancel)
			awaitPuller(t, watcherDone, tt.name)
			flow.noteRemoteFINSeen()
			select {
			case <-flow.done:
				t.Fatal("stopped completion watchdog closed the flow")
			default:
			}
			if got := registry.Snapshot().CompletionTimeouts; got != 0 {
				t.Fatalf("stopped watchdog exported %d completion timeouts", got)
			}
			flow.closeAll()
		})
	}
}

func TestCompletionWatchdogStopsDuringGrace(t *testing.T) {
	tests := []struct {
		name string
		stop func(chan struct{}, context.CancelFunc)
	}{
		{name: "stop", stop: func(stop chan struct{}, _ context.CancelFunc) { close(stop) }},
		{name: "context", stop: func(_ chan struct{}, cancel context.CancelFunc) { cancel() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flow, registry, peer := newCompletionTestFlow(t, time.Second)
			defer peer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			flow.ctx = ctx
			defer cancel()
			stop := make(chan struct{})
			watcherDone := make(chan struct{})
			flow.noteLocalFINSent()
			flow.noteRemoteFINSeen()
			go func() {
				flow.completionWatchdog(stop)
				close(watcherDone)
			}()
			tt.stop(stop, cancel)
			awaitPuller(t, watcherDone, tt.name+" during grace")
			select {
			case <-flow.done:
				t.Fatal("completion watchdog closed the flow after cancellation")
			default:
			}
			if got := registry.Snapshot().CompletionTimeouts; got != 0 {
				t.Fatalf("cancelled grace exported %d completion timeouts", got)
			}
			flow.closeAll()
		})
	}
}

func TestCompletionWatchdogFlowTeardownStopsIdleWatcher(t *testing.T) {
	flow, registry, peer := newCompletionTestFlow(t, 5*time.Millisecond)
	defer peer.Close()
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		flow.completionWatchdog(stop)
		close(watcherDone)
	}()
	flow.closeAll()
	awaitPuller(t, watcherDone, "flow teardown")
	close(stop)
	if got := registry.Snapshot().CompletionTimeouts; got != 0 {
		t.Fatalf("idle teardown exported %d completion timeouts", got)
	}
}

func BenchmarkIneligibleLanePuller(b *testing.B) {
	for i := 0; i < b.N; i++ {
		flow, lane, sched := newPullerTestFlow()
		ctx, cancel := context.WithTimeout(context.Background(), 105*time.Millisecond)
		flow.runLanePuller(ctx, lane, sched)
		cancel()
		sched.Close()
	}
}

func BenchmarkTraceOffLaneCongestionSampler(b *testing.B) {
	wasEnabled := laneTrace.Load()
	laneTrace.Store(false)
	b.Cleanup(func() { laneTrace.Store(wasEnabled) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flow := &multipathFlow{done: make(chan struct{})}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if flow.startLaneCongestionSampler(ctx) {
			b.Fatal("trace-off sampler started")
		}
	}
}

func BenchmarkIdleCompletionWatchdog(b *testing.B) {
	stop := make(chan struct{})
	close(stop)
	flow := &multipathFlow{
		ctx: context.Background(), done: make(chan struct{}),
		completionWake: make(chan struct{}, 1),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		flow.completionWatchdog(stop)
	}
}
