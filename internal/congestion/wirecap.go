package congestion

import (
	"sync"
	"sync/atomic"
	"time"

	quiccongestion "github.com/apernet/quic-go/congestion"
	"github.com/apernet/quic-go/monotime"
)

// WireScheduler is one burst-bounded QUIC packet-byte budget shared by every
// connection on a path. It is deliberately separate from the path model: the
// model measures erasure and delivery, while this scheduler enforces an
// operator-supplied ceiling without replacing that measurement.
type WireScheduler struct {
	mu              sync.Mutex
	total           wireBucket
	bulk            wireBucket
	maxDatagramSize uint64
}

type wireBucket struct {
	rate uint64
	next monotime.Time
}

// NewWireScheduler returns a scheduler whose reserve is unavailable to bulk
// connections. Callers validate that total is positive and reserve < total.
func NewWireScheduler(total, reserve uint64) *WireScheduler {
	return &WireScheduler{
		total:           wireBucket{rate: total},
		bulk:            wireBucket{rate: total - reserve},
		maxDatagramSize: uint64(quiccongestion.InitialPacketSize),
	}
}

func pacingDuration(bytes, rate uint64) time.Duration {
	nanos := bytes * uint64(time.Second) / rate
	if bytes*uint64(time.Second)%rate != 0 {
		nanos++
	}
	return time.Duration(nanos)
}

func (s *WireScheduler) deadline(bucket wireBucket, now monotime.Time, nextBytes uint64) monotime.Time {
	if bucket.next.IsZero() {
		return 0
	}
	burst := pacingDuration(maxBurstPackets*s.maxDatagramSize, bucket.rate)
	deadline := bucket.next.Add(pacingDuration(nextBytes, bucket.rate) - burst)
	if deadline <= now {
		return 0
	}
	return deadline
}

func later(a, b monotime.Time) monotime.Time {
	if a > b {
		return a
	}
	return b
}

func (s *WireScheduler) timeUntilSend(now monotime.Time, bulk bool) monotime.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline := s.deadline(s.total, now, s.maxDatagramSize)
	if bulk {
		deadline = later(deadline, s.deadline(s.bulk, now, s.maxDatagramSize))
	}
	return deadline
}

func chargeBucket(bucket *wireBucket, sent monotime.Time, bytes uint64) {
	if bucket.next.IsZero() || bucket.next < sent {
		bucket.next = sent
	}
	bucket.next = bucket.next.Add(pacingDuration(bytes, bucket.rate))
}

// charge records actual packet bytes. It returns true when quic-go sent while
// the aggregate scheduler was in debt, which can happen for an ACK/PTO bypass
// or for the one-packet race between concurrent connection send loops. The
// debt remains in next and is therefore repaid by later packets.
func (s *WireScheduler) charge(sent monotime.Time, bytes uint64, bulk bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	overshoot := !s.deadline(s.total, sent, bytes).IsZero()
	if bulk {
		overshoot = overshoot || !s.deadline(s.bulk, sent, bytes).IsZero()
	}
	chargeBucket(&s.total, sent, bytes)
	if bulk {
		chargeBucket(&s.bulk, sent, bytes)
	}
	return overshoot
}

func (s *WireScheduler) setMaxDatagramSize(size uint64) {
	if size == 0 {
		return
	}
	s.mu.Lock()
	if size > s.maxDatagramSize {
		s.maxDatagramSize = size
	}
	s.mu.Unlock()
}

func (s *WireScheduler) debt(now monotime.Time, bulk bool) time.Duration {
	deadline := s.timeUntilSend(now, bulk)
	if deadline.IsZero() {
		return 0
	}
	return deadline.Sub(now)
}

// WireCapSender combines a connection's own controller with a shared path
// budget. Every congestion and erasure callback still reaches the inner
// controller unchanged, so the path model and adaptive FEC remain active.
type WireCapSender struct {
	inner     quiccongestion.CongestionControlEx
	telemetry TelemetryProvider
	scheduler *WireScheduler
	totalRate uint64
	bulkRate  uint64
	bulk      atomic.Bool
	charged   atomic.Uint64
	overshoot atomic.Uint64
}

func NewWireCapSender(inner quiccongestion.CongestionControlEx, telemetry TelemetryProvider, scheduler *WireScheduler, totalRate, reserveRate uint64, bulk bool) *WireCapSender {
	sender := &WireCapSender{
		inner: inner, telemetry: telemetry, scheduler: scheduler,
		totalRate: totalRate, bulkRate: totalRate - reserveRate,
	}
	sender.bulk.Store(bulk)
	return sender
}

// SetBulk changes the whole connection's service class. A server calls it
// only after validating a non-control JOIN, before acknowledging that JOIN.
func (w *WireCapSender) SetBulk(bulk bool) { w.bulk.Store(bulk) }

func (w *WireCapSender) SetRTTStatsProvider(provider quiccongestion.RTTStatsProvider) {
	w.inner.SetRTTStatsProvider(provider)
}

func (w *WireCapSender) TimeUntilSend(bytesInFlight quiccongestion.ByteCount) monotime.Time {
	inner := w.inner.TimeUntilSend(bytesInFlight)
	wire := w.scheduler.timeUntilSend(monotime.Now(), w.bulk.Load())
	return later(inner, wire)
}

func (w *WireCapSender) HasPacingBudget(now monotime.Time) bool {
	return w.inner.HasPacingBudget(now) && w.scheduler.timeUntilSend(now, w.bulk.Load()).IsZero()
}

func (w *WireCapSender) OnPacketSent(sentTime monotime.Time, bytesInFlight quiccongestion.ByteCount, packetNumber quiccongestion.PacketNumber, bytes quiccongestion.ByteCount, isRetransmittable bool) {
	if w.scheduler.charge(sentTime, uint64(bytes), w.bulk.Load()) {
		w.overshoot.Add(1)
	}
	w.charged.Add(uint64(bytes))
	w.inner.OnPacketSent(sentTime, bytesInFlight, packetNumber, bytes, isRetransmittable)
}

func (w *WireCapSender) CanSend(bytesInFlight quiccongestion.ByteCount) bool {
	return w.inner.CanSend(bytesInFlight)
}

func (w *WireCapSender) MaybeExitSlowStart() { w.inner.MaybeExitSlowStart() }

func (w *WireCapSender) OnPacketAcked(number quiccongestion.PacketNumber, ackedBytes, priorInFlight quiccongestion.ByteCount, eventTime monotime.Time) {
	w.inner.OnPacketAcked(number, ackedBytes, priorInFlight, eventTime)
}

func (w *WireCapSender) OnCongestionEvent(number quiccongestion.PacketNumber, lostBytes, priorInFlight quiccongestion.ByteCount) {
	w.inner.OnCongestionEvent(number, lostBytes, priorInFlight)
}

func (w *WireCapSender) OnCongestionEventEx(priorInFlight quiccongestion.ByteCount, eventTime monotime.Time, acked []quiccongestion.AckedPacketInfo, lost []quiccongestion.LostPacketInfo) {
	w.inner.OnCongestionEventEx(priorInFlight, eventTime, acked, lost)
}

func (w *WireCapSender) OnRetransmissionTimeout(packetsRetransmitted bool) {
	w.inner.OnRetransmissionTimeout(packetsRetransmitted)
}

func (w *WireCapSender) SetMaxDatagramSize(size quiccongestion.ByteCount) {
	w.scheduler.setMaxDatagramSize(uint64(size))
	w.inner.SetMaxDatagramSize(size)
}

func (w *WireCapSender) InSlowStart() bool { return w.inner.InSlowStart() }
func (w *WireCapSender) InRecovery() bool  { return w.inner.InRecovery() }
func (w *WireCapSender) GetCongestionWindow() quiccongestion.ByteCount {
	return w.inner.GetCongestionWindow()
}

func (w *WireCapSender) Telemetry() ControllerTelemetry {
	snapshot := w.telemetry.Telemetry()
	snapshot.WireCapRate = w.totalRate
	snapshot.WireCapBulkRate = w.bulkRate
	snapshot.WireCapBytes = w.charged.Load()
	snapshot.WireCapOvershootPackets = w.overshoot.Load()
	snapshot.WireCapDebt = w.scheduler.debt(monotime.Now(), w.bulk.Load())
	snapshot.WireCapBulk = w.bulk.Load()
	return snapshot
}

var _ quiccongestion.CongestionControlEx = (*WireCapSender)(nil)
var _ TelemetryProvider = (*WireCapSender)(nil)
