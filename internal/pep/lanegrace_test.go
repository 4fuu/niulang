package pep

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/bojieli/queqiao/internal/protocol"
)

// The replacement grace covers the time a replacement lane needs to arrive.
// The flow used to end it only on an explicit refusal, which arrives when a
// rescue handshake completes -- and on a path lossy enough to have killed
// every lane, the rescue handshake is usually what fails instead. So the
// answer often never came and the application waited in silence: 25s on
// average and 573s at worst in the reported incident.
func TestWaitEndsWhenNoReplacementWillBeAttempted(t *testing.T) {
	flow := newGraceTestFlow(t)
	waits := make(chan error, 1)
	go func() { waits <- flow.waitForHealthyLane(context.Background(), laneReplacementWait) }()

	select {
	case err := <-waits:
		t.Fatalf("the wait ended with no evidence at all: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	flow.noteReplacementAbandoned()
	select {
	case err := <-waits:
		if !errors.Is(err, errReplacementAbandoned) {
			t.Fatalf("wait error = %v, want %v", err, errReplacementAbandoned)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the flow waited out its grace after replacement was abandoned")
	}
}

// A flow that already knows nothing is coming must not enter the wait at all.
func TestWaitIsNotEnteredOnceReplacementIsAbandoned(t *testing.T) {
	flow := newGraceTestFlow(t)
	flow.noteReplacementAbandoned()
	start := time.Now()
	if err := flow.waitForHealthyLane(context.Background(), laneReplacementWait); !errors.Is(err, errReplacementAbandoned) {
		t.Fatalf("wait error = %v, want %v", err, errReplacementAbandoned)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the wait took %s to report what it already knew", elapsed)
	}
}

// A healthy lane is still the answer, whatever the manager has stopped doing.
func TestAbandonedReplacementDoesNotFailAFlowWithALane(t *testing.T) {
	flow := newGraceTestFlow(t)
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close(); _ = remote.Close() })
	flow.lanes[1] = &mpLane{id: 1, kind: TransportQUIC, fc: newFrameConn(local)}
	flow.noteReplacementAbandoned()
	if err := flow.waitForHealthyLane(context.Background(), laneReplacementWait); err != nil {
		t.Fatalf("a flow with a healthy lane failed: %v", err)
	}
}

// The wait is a client-side conclusion drawn from the client's own behaviour.
// A server flow never draws it: the rescue is still somebody's to send, and
// its grace is what gives the peer time to send it.
func TestTheLaneManagerIsWhatAbandonsReplacement(t *testing.T) {
	flow := newGraceTestFlow(t)
	if flow.replacementAbandoned.Load() {
		t.Fatal("a fresh flow starts with its replacement grace already spent")
	}
	client := &Client{cfg: ClientConfig{Logger: slog.New(slog.NewTextHandler(discardWriter{}, nil))}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.manageQUICLanes(ctx, flow, flow.sessionID, flow.flowID)
	if !flow.replacementAbandoned.Load() {
		t.Fatal("the lane manager returned without ending the flow's replacement grace")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func newGraceTestFlow(t *testing.T) *multipathFlow {
	t.Helper()
	inner, peer := net.Pipe()
	t.Cleanup(func() { _ = inner.Close(); _ = peer.Close() })
	return newMultipathFlow(context.Background(), inner, [16]byte{2}, 3, defaultChunkSize, protocol.FlagAckUp, protocol.FlagAckDown, nil, nil)
}
