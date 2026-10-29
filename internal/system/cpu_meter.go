package system

import (
	"sync"
	"time"
)

const (
	cpuMinInterval = time.Second
	cpuWarmup      = 200 * time.Millisecond
)

// Percent is measured over at least cpuMinInterval, and callers inside that window
// share the last reading, so several open dashboards do not skew each other.
type cpuMeter struct {
	read  func() (cpuSample, error)
	now   func() time.Time
	sleep func(time.Duration)

	mu       sync.Mutex
	previous cpuSample
	takenAt  time.Time
	cached   float64
}

func (m *cpuMeter) Percent() (float64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.takenAt.IsZero() {
		first, err := m.read()
		if err != nil {
			return 0, err
		}
		m.previous, m.takenAt = first, m.now()
		m.sleep(cpuWarmup)
	} else if m.now().Sub(m.takenAt) < cpuMinInterval {
		return m.cached, nil
	}

	current, err := m.read()
	if err != nil {
		return 0, err
	}
	m.cached = cpuPercent(m.previous, current)
	m.previous, m.takenAt = current, m.now()

	return m.cached, nil
}
