// Package multipath contains transport-independent pieces of the striped
// flow session. The transport package is intentionally unaware of ordering:
// QUIC lanes may deliver frames in different orders, while the application
// stream must remain strictly ordered.
package multipath

import (
	"errors"
	"fmt"
	"sort"
)

var (
	ErrWindowExceeded = errors.New("reassembly window exceeded")
	ErrSequence       = errors.New("invalid reassembly sequence")
)

type Config struct {
	MaxBufferedBytes  uint64
	MaxBufferedFrames int
}

func DefaultConfig() Config {
	return Config{MaxBufferedBytes: 8 * 1024 * 1024, MaxBufferedFrames: 4096}
}

type Segment struct {
	Sequence uint64
	Payload  []byte
	Final    bool
}

type Reassembler struct {
	cfg     Config
	next    uint64
	buffer  map[uint64]Segment
	bytes   uint64
	finalAt *uint64
}

func NewReassembler(cfg Config) *Reassembler {
	if cfg.MaxBufferedBytes == 0 || cfg.MaxBufferedFrames <= 0 {
		cfg = DefaultConfig()
	}
	return &Reassembler{cfg: cfg, buffer: make(map[uint64]Segment)}
}

func (r *Reassembler) NextSequence() uint64  { return r.next }
func (r *Reassembler) BufferedBytes() uint64 { return r.bytes }
func (r *Reassembler) BufferedFrames() int   { return len(r.buffer) }
func (r *Reassembler) Closed() bool          { return r.finalAt != nil && r.next >= *r.finalAt }

// Insert adds one non-empty data segment or a zero-length FIN. It returns all
// newly contiguous bytes. The caller owns the returned byte slice. Overlapping
// segments are rejected rather than silently merging untrusted input.
func (r *Reassembler) Insert(segment Segment) ([]byte, bool, error) {
	if segment.Final && len(segment.Payload) != 0 {
		return nil, false, errors.New("FIN segment must not carry payload")
	}
	if !segment.Final && len(segment.Payload) == 0 {
		return nil, false, errors.New("empty data segment")
	}
	if segment.Sequence > ^uint64(0)-uint64(len(segment.Payload)) {
		return nil, false, ErrSequence
	}
	end := segment.Sequence + uint64(len(segment.Payload))
	if segment.Final {
		end = segment.Sequence
	}
	if end < r.next || (segment.Final && segment.Sequence < r.next) {
		// A duplicate segment is harmless if it is wholly before the receive
		// cursor. FIN duplicates are also harmless.
		if end <= r.next {
			return nil, r.Closed(), nil
		}
		return nil, false, errors.New("segment overlaps receive cursor")
	}
	if existing, ok := r.buffer[segment.Sequence]; ok {
		if existing.Final == segment.Final && string(existing.Payload) == string(segment.Payload) {
			return nil, r.Closed(), nil
		}
		return nil, false, errors.New("conflicting duplicate segment")
	}
	for _, existing := range r.buffer {
		existingEnd := existing.Sequence + uint64(len(existing.Payload))
		if existing.Final {
			existingEnd = existing.Sequence
		}
		if segment.Sequence < existingEnd && existing.Sequence < end {
			return nil, false, errors.New("overlapping reassembly segments")
		}
		if segment.Final && existing.Sequence == segment.Sequence {
			return nil, false, errors.New("FIN conflicts with buffered segment")
		}
	}
	if segment.Sequence != r.next {
		if uint64(len(r.buffer))+1 > uint64(r.cfg.MaxBufferedFrames) || r.bytes+uint64(len(segment.Payload)) > r.cfg.MaxBufferedBytes {
			return nil, false, ErrWindowExceeded
		}
		segment.Payload = append([]byte(nil), segment.Payload...)
		r.buffer[segment.Sequence] = segment
		r.bytes += uint64(len(segment.Payload))
		if segment.Final {
			at := segment.Sequence
			r.finalAt = &at
		}
		return nil, r.Closed(), nil
	}
	return r.consumeContiguous(segment)
}

func (r *Reassembler) consumeContiguous(first Segment) ([]byte, bool, error) {
	var output []byte
	current := first
	for {
		if current.Sequence != r.next {
			return nil, false, fmt.Errorf("internal reassembly cursor mismatch")
		}
		if current.Final {
			at := current.Sequence
			r.finalAt = &at
			return output, true, nil
		}
		output = append(output, current.Payload...)
		r.next += uint64(len(current.Payload))
		if next, ok := r.buffer[r.next]; ok {
			delete(r.buffer, r.next)
			r.bytes -= uint64(len(next.Payload))
			current = next
			continue
		}
		if r.finalAt != nil && r.next >= *r.finalAt {
			return output, true, nil
		}
		return output, false, nil
	}
}

// BufferedSequences is intended for diagnostics and deterministic tests.
func (r *Reassembler) BufferedSequences() []uint64 {
	sequences := make([]uint64, 0, len(r.buffer))
	for sequence := range r.buffer {
		sequences = append(sequences, sequence)
	}
	sort.Slice(sequences, func(i, j int) bool { return sequences[i] < sequences[j] })
	return sequences
}

// ReceivedRanges reports the byte ranges held out of order, merged and sorted,
// capped at max entries.
//
// A striped flow's sender otherwise learns only the contiguous receive point,
// which sits behind whatever the slowest lane has not delivered. Its retention
// window then has to cover the whole reorder span rather than the bytes
// actually outstanding. Reporting what the receiver already holds lets the
// sender release those frames, which is what keeps the window proportional to
// the data in flight.
//
// Ranges are returned lowest first so a truncated report still describes the
// bytes closest to the contiguous point, which are the ones the sender is most
// likely to still be holding.
func (r *Reassembler) ReceivedRanges(max int) [][2]uint64 {
	if max <= 0 || len(r.buffer) == 0 {
		return nil
	}
	starts := make([]uint64, 0, len(r.buffer))
	for sequence := range r.buffer {
		starts = append(starts, sequence)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	ranges := make([][2]uint64, 0, len(starts))
	for _, start := range starts {
		segment := r.buffer[start]
		end := start + uint64(len(segment.Payload))
		if segment.Final {
			// A buffered FIN covers no bytes; reporting it would tell the
			// sender a zero-length range it cannot act on.
			continue
		}
		if last := len(ranges) - 1; last >= 0 && ranges[last][1] == start {
			ranges[last][1] = end
			continue
		}
		ranges = append(ranges, [2]uint64{start, end})
	}
	if len(ranges) > max {
		ranges = ranges[:max]
	}
	return ranges
}
