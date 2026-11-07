package alerts

import (
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	countersFile = "alert-counters.json"

	// RiseWindow is how long a counter that went up stays a problem. After that its new value is the baseline.
	RiseWindow = 7 * 24 * time.Hour
)

// Counter follows a count that stays put on a healthy disk, such as reallocated sectors or read errors. What a count
// means differs per disk and vendor, so only a rise against the disk's own earlier value is a problem.
type Counter struct {
	Count uint64 `json:"count"`
	// From is the count before the latest rise began.
	From   uint64    `json:"from"`
	RoseAt time.Time `json:"roseAt,omitzero"`
}

// Rising reports whether the count went up within the last RiseWindow.
func (c Counter) Rising(now time.Time) bool {
	return !c.RoseAt.IsZero() && now.Sub(c.RoseAt) < RiseWindow
}

// observe is the counter after reading count at now. The first reading is the baseline, rises within RiseWindow of
// each other are one rise, and a count that drops (a reset, or a replaced disk) becomes the new baseline.
func observe(prev Counter, seen bool, count uint64, now time.Time) Counter {
	switch {
	case !seen:
		return Counter{Count: count, From: count}
	case count > prev.Count && prev.Rising(now):
		return Counter{Count: count, From: prev.From, RoseAt: now}
	case count > prev.Count:
		return Counter{Count: count, From: prev.Count, RoseAt: now}
	case count < prev.Count:
		return Counter{Count: count, From: count}
	}

	return prev
}

// Counters keeps every Counter in the panel data directory, so a restart neither forgets a rise nor takes the
// current count as a new baseline.
type Counters struct {
	path string

	mu     sync.Mutex
	counts map[string]Counter
}

func OpenCounters(dir string) (*Counters, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	counters := &Counters{path: filepath.Join(dir, countersFile), counts: map[string]Counter{}}
	content, err := os.ReadFile(counters.path)
	if errors.Is(err, os.ErrNotExist) {
		return counters, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(content, &counters.counts); err != nil || counters.counts == nil {
		log.Printf("alerts: %s is damaged, starting with new baselines", counters.path)
		counters.counts = map[string]Counter{}
	}

	return counters, nil
}

// Observe records a reading and returns the counter after it.
func (c *Counters) Observe(key string, count uint64, now time.Time) Counter {
	c.mu.Lock()
	defer c.mu.Unlock()

	prev, seen := c.counts[key]
	next := observe(prev, seen, count, now)
	if seen && next == prev {
		return next
	}
	updated := maps.Clone(c.counts)
	updated[key] = next
	if err := writeFileAtomic(c.path, updated); err != nil {
		log.Printf("alerts: save %s: %v", c.path, err)
	}
	c.counts = updated

	return next
}

// Get returns the counter without a new reading, for a disk that could not be read this time.
func (c *Counters) Get(key string) (Counter, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	counter, found := c.counts[key]

	return counter, found
}

func writeFileAtomic(path string, value any) error {
	content, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(temp, path)
}
