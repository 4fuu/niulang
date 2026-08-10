package congestion

// tuicMinMax is the constant-space, three-sample max filter used by TUIC's
// quinn-congestions BBR implementation. The clock is a packet-timed round
// number rather than wall time. Keeping the filter round based makes it
// insensitive to ACK coalescing and gives it a bounded ten-round memory.
type tuicMinMax struct {
	window  uint64
	samples [3]tuicMinMaxSample
}

type tuicMinMaxSample struct {
	round uint64
	value uint64
}

func newTUICMinMax() tuicMinMax {
	return tuicMinMax{window: 10}
}

func (m *tuicMinMax) get() uint64 { return m.samples[0].value }

func (m *tuicMinMax) reset() {
	m.samples = [3]tuicMinMaxSample{}
}

func (m *tuicMinMax) updateMax(round, value uint64) {
	if value == 0 {
		return
	}
	current := tuicMinMaxSample{round: round, value: value}
	oldest := m.samples[2].round
	// A round counter is monotonic in normal operation. The explicit round
	// comparison also makes the filter safe if a test or a timeout resets it.
	windowExpired := round >= oldest && round-oldest > m.window
	if m.samples[0].value == 0 || value >= m.samples[0].value || windowExpired {
		m.samples = [3]tuicMinMaxSample{current, current, current}
		return
	}
	if value >= m.samples[1].value {
		m.samples[2] = current
		m.samples[1] = current
	} else if value >= m.samples[2].value {
		m.samples[2] = current
	}
	m.subWindowUpdate(current)
}

func (m *tuicMinMax) subWindowUpdate(sample tuicMinMaxSample) {
	if sample.round < m.samples[0].round {
		return
	}
	delta := sample.round - m.samples[0].round
	if delta > m.window {
		m.samples[0] = m.samples[1]
		m.samples[1] = m.samples[2]
		m.samples[2] = sample
		if sample.round >= m.samples[0].round && sample.round-m.samples[0].round > m.window {
			m.samples[0] = m.samples[1]
			m.samples[1] = m.samples[2]
			m.samples[2] = sample
		}
	} else if m.samples[1].round == m.samples[0].round && delta > m.window/4 {
		m.samples[2] = sample
		m.samples[1] = sample
	} else if m.samples[2].round == m.samples[1].round && delta > m.window/2 {
		m.samples[2] = sample
	}
}
