package system

import (
	"testing"
	"time"
)

type meterClock struct {
	now     time.Time
	samples []cpuSample
	reads   int
}

func (c *meterClock) read() (cpuSample, error) {
	sample := c.samples[c.reads]
	c.reads++

	return sample, nil
}

func (c *meterClock) meter() *cpuMeter {
	return &cpuMeter{
		read:  c.read,
		now:   func() time.Time { return c.now },
		sleep: func(d time.Duration) { c.now = c.now.Add(d) },
	}
}

func TestCPUMeterReusesReadingWithinAnInterval(t *testing.T) {
	c := &meterClock{now: time.Unix(1000, 0), samples: []cpuSample{
		{idle: 0, total: 0},
		{idle: 50, total: 100},
		{idle: 60, total: 200},
	}}
	m := c.meter()

	first, _ := m.Percent()
	if first != 50 {
		t.Fatalf("first reading %v", first)
	}
	for range 5 {
		c.now = c.now.Add(100 * time.Millisecond)
		if got, _ := m.Percent(); got != first {
			t.Fatalf("got %v, want the cached %v", got, first)
		}
	}
	if c.reads != 2 {
		t.Fatalf("cached calls must not read /proc/stat, reads=%d", c.reads)
	}

	c.now = c.now.Add(time.Second)
	if got, _ := m.Percent(); got != 90 {
		t.Fatalf("got %v after the interval", got)
	}
}
