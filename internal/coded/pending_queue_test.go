package coded

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func newPendingTestPath(capacity int) *Path {
	path := &Path{
		pending: make([]outboundFrame, capacity),
		done:    make(chan struct{}),
	}
	path.sendReady = sync.NewCond(&path.sendMu)
	path.sendSpace = sync.NewCond(&path.sendMu)
	return path
}

func TestPendingQueuePreservesFIFOThroughWraparound(t *testing.T) {
	path := newPendingTestPath(3)
	push := func(value string) {
		path.sendMu.Lock()
		path.pushPendingLocked(outboundFrame{frame: []byte(value)})
		path.sendReady.Signal()
		path.sendMu.Unlock()
	}
	pop := func(want string) {
		t.Helper()
		frame, ok := path.takePending(false)
		if !ok {
			t.Fatalf("pending queue was empty, want %q", want)
		}
		if got := string(frame.frame); got != want {
			t.Fatalf("pending queue returned %q, want %q", got, want)
		}
	}

	push("a")
	push("b")
	push("c")
	pop("a")
	pop("b")
	push("d")
	push("e")
	pop("c")
	pop("d")
	pop("e")
}

func TestPendingQueueWakesBlockedConsumer(t *testing.T) {
	path := newPendingTestPath(1)
	result := make(chan outboundFrame, 1)
	go func() {
		frame, ok := path.takePending(true)
		if ok {
			result <- frame
		}
	}()

	path.sendMu.Lock()
	path.pushPendingLocked(outboundFrame{frame: []byte("ready")})
	path.sendReady.Signal()
	path.sendMu.Unlock()
	select {
	case frame := <-result:
		if got := string(frame.frame); got != "ready" {
			t.Fatalf("woken consumer received %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("empty pending queue did not wake its consumer")
	}
}

func TestPendingQueueCloseWakesBlockedConsumer(t *testing.T) {
	path := newPendingTestPath(1)
	result := make(chan bool, 1)
	go func() {
		_, ok := path.takePending(true)
		result <- ok
	}()

	path.closePath()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("closed pending queue returned a frame")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake the blocked consumer")
	}
}

func TestAbandonPendingReleasesWrappedAndPackedCallbacksExactlyOnce(t *testing.T) {
	path := newPendingTestPath(3)
	calls := make([]int, 4)
	callback := func(index int) func() {
		return func() {
			// A callback that can take sendMu proves abandonPending released the
			// queue lock before handing ownership back to its caller.
			path.sendMu.Lock()
			path.sendMu.Unlock()
			calls[index]++
		}
	}
	push := func(done func()) {
		path.sendMu.Lock()
		path.pushPendingLocked(outboundFrame{done: done})
		path.sendMu.Unlock()
	}

	push(nil)
	push(nil)
	push(callback(0))
	if _, ok := path.takePending(false); !ok {
		t.Fatal("first queued frame was not available")
	}
	if _, ok := path.takePending(false); !ok {
		t.Fatal("second queued frame was not available")
	}
	push(callback(1))
	push(callback(2))
	path.packedDone = append(path.packedDone, callback(3))

	path.abandonPending()
	path.abandonPending()
	for i, got := range calls {
		if got != 1 {
			t.Fatalf("callback %d called %d times, want exactly once", i, got)
		}
	}
}

func TestPendingQueueSpaceAdvancesAllBlockedProducers(t *testing.T) {
	const producers = 8
	carrier := newTrackedCarrier()
	path := New(carrier, Config{Pending: 1})
	t.Cleanup(func() { _ = path.Close() })
	if err := path.SendOwnedTracked([]byte("in carrier"), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-carrier.started:
	case <-time.After(time.Second):
		t.Fatal("coded sender did not reach the carrier")
	}
	if err := path.SendOwnedTracked([]byte("queued"), nil); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, producers)
	for i := 0; i < producers; i++ {
		go func(i int) {
			results <- path.SendOwnedTracked([]byte(fmt.Sprintf("producer-%d", i)), nil)
		}(i)
	}

	seen := make(map[string]bool, producers+1)
	deadline := time.After(2 * time.Second)
	for len(seen) < producers+1 {
		if frame, ok := path.takePending(false); ok {
			seen[string(frame.frame)] = true
			continue
		}
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatalf("only %d of %d queued frames made progress", len(seen), producers+1)
		}
	}
	for i := 0; i < producers; i++ {
		if !seen[fmt.Sprintf("producer-%d", i)] {
			t.Fatalf("producer %d never entered the queue", i)
		}
	}
}

func TestPendingQueueCloseWakesAllBlockedProducers(t *testing.T) {
	const producers = 8
	carrier := newTrackedCarrier()
	path := New(carrier, Config{Pending: 1})
	if err := path.SendOwnedTracked([]byte("in carrier"), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-carrier.started:
	case <-time.After(time.Second):
		t.Fatal("coded sender did not reach the carrier")
	}
	if err := path.SendOwnedTracked([]byte("queued"), nil); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{}, producers)
	results := make(chan error, producers)
	for i := 0; i < producers; i++ {
		go func() {
			started <- struct{}{}
			results <- path.SendOwnedTracked([]byte("blocked"), nil)
		}()
	}
	for i := 0; i < producers; i++ {
		<-started
	}
	if err := path.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < producers; i++ {
		select {
		case err := <-results:
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("blocked send returned %v, want ErrClosed", err)
			}
		case <-time.After(time.Second):
			t.Fatal("close did not wake every blocked producer")
		}
	}
	if err := path.SendOwnedTracked([]byte("after close"), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("send after close returned %v, want ErrClosed", err)
	}
}

func BenchmarkPendingQueue(b *testing.B) {
	b.Run("transition", func(b *testing.B) {
		path := &Path{
			pending: make([]outboundFrame, 1),
		}
		path.sendReady = sync.NewCond(&path.sendMu)
		path.sendSpace = sync.NewCond(&path.sendMu)
		frame := outboundFrame{frame: []byte("frame")}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			path.sendMu.Lock()
			path.pushPendingLocked(frame)
			path.sendReady.Signal()
			path.sendMu.Unlock()
			if _, ok := path.takePending(false); !ok {
				b.Fatal("queued frame was not available")
			}
		}
	})

	b.Run("large_queue_dequeue", func(b *testing.B) {
		const capacity = 4096
		path := &Path{
			pending:    make([]outboundFrame, capacity),
			pendingLen: capacity,
		}
		path.sendReady = sync.NewCond(&path.sendMu)
		path.sendSpace = sync.NewCond(&path.sendMu)
		frame := outboundFrame{frame: []byte("frame")}
		for i := range path.pending {
			path.pending[i] = frame
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, ok := path.takePending(false); !ok {
				b.Fatal("queued frame was not available")
			}
			path.sendMu.Lock()
			path.pushPendingLocked(frame)
			path.sendReady.Signal()
			path.sendMu.Unlock()
		}
	})
}
