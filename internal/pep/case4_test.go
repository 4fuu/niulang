package pep

import (
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/4fuu/niulang/internal/pathsim"
)

// A known falsification case for the controller, which remains partly
// unresolved.
//
// A policer drops what it cannot pass and holds nothing, so overload produces
// loss and no delay. The bandwidth estimator now tracks the policer's
// sustained rate, but an estimate is not a wire cap: probing still sends above
// it, loss is not a congestion response, and there is no queue for the delay
// bound to measure.
//
// This is a characterization test. It retains the resolved estimator bound and
// asserts the remaining defect so behavior cannot change silently in either
// direction. If it starts failing because the sender no longer overdrives and
// loses packets, that is the case being resolved: update this characterization
// and its rationale together.
//
// It matters more than a hypothetical, because internal/pathsim records that
// the live path this project targets is a policer -- "at twice the bottleneck
// rate it shows arrival runs averaging 2.3 packets and loss runs averaging
// 5.7 ... a limiter which passes everything for a while and then drops
// everything for a while".
func TestCase4APolicedPathIsStillUnbraked(t *testing.T) {
	if testing.Short() {
		t.Skip("brings up QUIC across an emulated 300 ms path")
	}
	requireStableImpairmentClock(t)
	const shaped = 250_000
	path := pathsim.Config{
		OneWayDelay:         150 * time.Millisecond,
		RateBytesPerSec:     shaped,
		PolicerRefillPeriod: 8 * time.Millisecond,
		Seed:                53,
	}
	socks, destination := codedPair(t, false, &path)
	conn := socksDial(t, socks, destination, erasureChannelBudget(180*time.Second))
	defer conn.Close()

	payload := make([]byte, 256*1024)
	rand.New(rand.NewSource(29)).Read(payload)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 8; i++ {
			if _, err := conn.Write(payload); err != nil {
				return
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				return
			}
		}
	}()

	var peakPacing, peakBandwidth uint64
	var lanes int64
	var lastMode uint32
	var maxQueue time.Duration
	var maxBrake float64
	var lastLoss float64
	deadline := time.After(24 * time.Second)
	for measuring := true; measuring; {
		select {
		case <-done:
			measuring = false
		case <-deadline:
			measuring = false
		case <-time.After(2 * time.Second):
		}
		s := lastClient.Metrics().Snapshot()
		if s.QUICControllerPacingRate > peakPacing {
			peakPacing = s.QUICControllerPacingRate
		}
		if s.QUICControllerMaxBandwidth > peakBandwidth {
			peakBandwidth = s.QUICControllerMaxBandwidth
		}
		if q := s.QUICSmoothedRTT - s.QUICControllerMinRTT; q > maxQueue {
			maxQueue = q
		}
		if s.QUICDelayBrake > maxBrake {
			maxBrake = s.QUICDelayBrake
		}
		lanes = s.QUICLanes
		lastMode = s.QUICControllerMode
		if s.QUICPacketsSent > 0 {
			lastLoss = 100 * float64(s.QUICLossObservedPackets) / float64(s.QUICPacketsSent)
		}
	}

	t.Logf("policed at %d B/s: peak pacing %d (%.1fx), peak bandwidth estimate %d (%.1fx), "+
		"worst queue %v, strongest brake %.4f, loss %.1f%%, lanes %d, controller mode %d",
		shaped, peakPacing, float64(peakPacing)/shaped,
		peakBandwidth, float64(peakBandwidth)/shaped,
		maxQueue.Round(time.Millisecond), maxBrake, lastLoss, lanes, lastMode)

	if peakPacing == 0 {
		t.Skip("the flow never got going, so this run measured nothing")
	}
	// The estimator component of this falsification is resolved: repeated
	// full-stack runs now track the sustained policer rate rather than a burst.
	// Keep that improvement as an assertion while the independent pacing and
	// loss defect below remains open.
	if float64(peakBandwidth) < shaped*0.8 || float64(peakBandwidth) > shaped*1.2 {
		t.Errorf("the bandwidth estimate no longer tracks the %d B/s policer: %d B/s", shaped, peakBandwidth)
	}
	// Nothing brakes probing above that accurate estimate.
	if maxQueue > 50*time.Millisecond || maxBrake > 0 {
		t.Errorf("a policer produced %v of queue and a brake of %.4f; if it now queues, this "+
			"is no longer the unbraked case and the design document should say so",
			maxQueue, maxBrake)
	}
	// Nine Protocol 3 runs put peak pacing at 4.1--4.2x and sender-induced loss
	// at 17.4--23.7%. Gate both signals: the pacing floor is now materially
	// stronger than the former 1.25x check, while the loss floor leaves enough
	// room for full-suite host-scheduler variation without hiding the residual
	// policer overdrive.
	if float64(peakPacing) < shaped*3.5 || lastLoss < 15 {
		t.Errorf("policed path no longer reproduces the residual overdrive: peak pacing %d "+
			"(%.1fx %d) with %.1f%% loss; re-measure and update the design document",
			peakPacing, float64(peakPacing)/shaped, shaped, lastLoss)
	}
}
