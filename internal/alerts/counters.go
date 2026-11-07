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
	// LowerAfter is how long readings below the count must last before they are the new baseline, so a disk that
	// answers 0 once in a while neither ends a rise nor makes the next normal reading look like a new one.
	LowerAfter = 24 * time.Hour
)

// Counter follows a count that stays put on a healthy disk, such as reallocated sectors or read errors. What a count
// means differs per disk and vendor, so only a rise against the disk's own earlier value is a problem.
type Counter struct {
	Count uint64 `json:"count"`
	// From is the count before the latest rise began.
	From   uint64    `json:"from"`
	RoseAt time.Time `json:"roseAt,omitzero"`
	// LowSince is when the readings fell below Count, zero while they do not.
	LowSince time.Time `json:"lowSince,omitzero"`
}

// Rising reports whether the count went up within the last RiseWindow.
func (c Counter) Rising(now time.Time) bool {
	return !c.RoseAt.IsZero() && now.Sub(c.RoseAt) < RiseWindow
}

// observe is the counter after reading count at now. The first reading is the baseline, and rises within RiseWindow
// of each other are one rise. A lower reading changes nothing until lower readings have lasted LowerAfter (a reset,
// or a replaced disk); then the lower count is the new baseline.
func observe(prev Counter, seen bool, count uint64, now time.Time) Counter {
	switch {
	case !seen:
		return Counter{Count: count, From: count}
	case count > prev.Count && prev.Rising(now):
		return Counter{Count: count, From: prev.From, RoseAt: now}
	case count > prev.Count:
		return Counter{Count: count, From: prev.Count, RoseAt: now}
	case count == prev.Count:
		prev.LowSince = time.Time{}
	case prev.LowSince.IsZero():
		prev.LowSince = now
	case now.Sub(prev.LowSince) >= LowerAfter:
		return Counter{Count: count, From: count}
	}

	return prev
}

// Counters keeps every Counter in the panel data directory, so a restart neither forgets a rise nor takes the
// current count as a new baseline.
type Counters struct {
	store *keyed[Counter]
}

func OpenCounters(dir string) (*Counters, error) {
	store, err := openKeyed[Counter](dir, countersFile)
	if err != nil {
		return nil, err
	}

	return &Counters{store: store}, nil
}

// Observe records a reading and returns the counter after it.
func (c *Counters) Observe(key string, count uint64, now time.Time) Counter {
	var next Counter
	c.store.change(key, func(prev Counter, seen bool) (Counter, bool) {
		next = observe(prev, seen, count, now)
		return next, true
	})

	return next
}

// Get returns the counter without a new reading, for a disk that could not be read this time.
func (c *Counters) Get(key string) (Counter, bool) {
	return c.store.get(key)
}

// keyed is a small map kept in a JSON file in the panel data directory, written whenever it changes.
type keyed[T comparable] struct {
	path string

	mu    sync.Mutex
	items map[string]T
}

func openKeyed[T comparable](dir, name string) (*keyed[T], error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store := &keyed[T]{path: filepath.Join(dir, name), items: map[string]T{}}
	content, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(content, &store.items); err != nil || store.items == nil {
		log.Printf("alerts: %s is damaged, starting over", store.path)
		store.items = map[string]T{}
	}

	return store, nil
}

func (k *keyed[T]) get(key string) (T, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	item, found := k.items[key]

	return item, found
}

func (k *keyed[T]) all() map[string]T {
	k.mu.Lock()
	defer k.mu.Unlock()

	return maps.Clone(k.items)
}

// change sets key to what update returns, or removes it when update says not to keep it.
func (k *keyed[T]) change(key string, update func(prev T, seen bool) (T, bool)) {
	k.mu.Lock()
	defer k.mu.Unlock()

	prev, seen := k.items[key]
	next, keep := update(prev, seen)
	if keep == seen && (!keep || next == prev) {
		return
	}
	updated := maps.Clone(k.items)
	if keep {
		updated[key] = next
	} else {
		delete(updated, key)
	}
	if err := writeFileAtomic(k.path, updated); err != nil {
		log.Printf("alerts: save %s: %v", k.path, err)
	}
	k.items = updated
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
