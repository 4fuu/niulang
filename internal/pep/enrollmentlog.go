package pep

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bojieli/queqiao/internal/identity"
)

const enrollmentRecordInterval = 10 * time.Second

// admissionRefused is recorded like an outcome but is this gateway's own
// answer: the enrollment slots were full, so the attempt was dropped before
// any invitation was read. It is the one enrollment ceiling that used to close
// a connection without saying anything, while the session and QUIC ceilings
// beside it both warn.
const admissionRefused = "admission_refused"

// enrollmentLog rate-limits the enrollment record, the way refusalLog does for
// lane joins and for the same reason: the attempts are a stranger's to
// generate, and a storm of them is exactly when the record matters most.
//
// The key space is this gateway's own outcome set rather than anything a peer
// chooses, so a map is bounded by construction. Acceptances are deliberately
// not rate-limited - a device joining a provider is a rare, durable change to
// who can use this gateway, and it should never be summarized away.
type enrollmentLog struct {
	mu    sync.Mutex
	state map[string]*enrollmentCounter
}

type enrollmentCounter struct {
	last       time.Time
	suppressed uint64
	total      uint64
}

// due reports whether this outcome should be written now, how many of its
// attempts went unwritten since the last time it was, and how many there have
// been in total. The total is carried for the same reason refusalLog carries
// it: a storm that stops otherwise leaves its tail in a record nobody writes.
func (l *enrollmentLog) due(outcome string, now time.Time) (write bool, suppressed, total uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == nil {
		l.state = make(map[string]*enrollmentCounter)
	}
	counter, ok := l.state[outcome]
	if !ok {
		counter = &enrollmentCounter{}
		l.state[outcome] = counter
	}
	counter.total++
	total = counter.total
	if !counter.last.IsZero() && now.Sub(counter.last) < enrollmentRecordInterval {
		counter.suppressed++
		return false, 0, total
	}
	suppressed = counter.suppressed
	counter.last, counter.suppressed = now, 0
	return true, suppressed, total
}

// recordEnrollment says what an enrollment or renewal attempt did, where an
// operator will see it.
//
// Every one of these attempts used to be discarded at the call site with `_ =`,
// so a gateway refusing every enrollment it received was indistinguishable
// from one receiving none. The reason for a refusal reached only the client -
// the one place it cannot be acted on - and arrived there flattened into a
// sentence about the invitation even when the real cause was that this
// gateway could not open its own authorization store.
func (s *Server) recordEnrollment(kind string, result identity.EnrollmentResult, err error) {
	if result.Outcome == identity.EnrollmentAccepted {
		s.cfg.Logger.LogAttrs(context.Background(), slog.LevelInfo, kind+" accepted",
			slog.String("account_id", result.AccountID),
			slog.String("device_id", result.DeviceID),
			slog.String("device_name", result.DeviceName))
		return
	}
	level := slog.LevelInfo
	switch result.Outcome {
	case identity.EnrollmentRejected:
		// A live invitation that this gateway declined. Routine on its own,
		// but a run of them is a provisioning problem someone is waiting on.
		level = slog.LevelWarn
	case identity.EnrollmentUnavailable:
		// Not a judgement about the invitation at all: the store could not be
		// read, locked, or written. Nothing the enrolling user does will fix
		// it, so it belongs at the level an outage is reported at.
		level = slog.LevelError
	}
	outcome := string(result.Outcome)
	write, suppressed, total := s.enrollLog.due(outcome, time.Now())
	if !write {
		return
	}
	attrs := []slog.Attr{
		slog.String("outcome", outcome),
		slog.Uint64("suppressed", suppressed),
		slog.Uint64("total", total),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	// Present only once mutual TLS has named the device, which is renewal.
	if result.AccountID != "" {
		attrs = append(attrs, slog.String("account_id", result.AccountID))
	}
	if result.DeviceID != "" {
		attrs = append(attrs, slog.String("device_id", result.DeviceID))
	}
	s.cfg.Logger.LogAttrs(context.Background(), level, kind+" refused", attrs...)
}

// recordEnrollmentAdmission reports an attempt dropped because the enrollment
// slots were full, before any invitation was read.
func (s *Server) recordEnrollmentAdmission(kind string) {
	if write, suppressed, total := s.enrollLog.due(admissionRefused, time.Now()); write {
		s.cfg.Logger.LogAttrs(context.Background(), slog.LevelWarn, kind+" refused",
			slog.String("outcome", admissionRefused),
			slog.Uint64("suppressed", suppressed),
			slog.Uint64("total", total))
	}
}
