package system

import (
	"errors"
	"math/bits"
	"sync"
)

const (
	WarnPercent     = 85
	CriticalPercent = 95
	FullPercent     = 98
)

type StorageLevel string

const (
	LevelOK       StorageLevel = "ok"
	LevelWarning  StorageLevel = "warning"
	LevelCritical StorageLevel = "critical"
	LevelFull     StorageLevel = "full"
)

var ErrNoDisk = errors.New("disk usage is not available")

type DiskUsage struct {
	Total uint64
	Used  uint64
	Free  uint64
}

// DiskSpace reports the usage of the filesystem that holds every user's files.
type DiskSpace interface {
	Usage() (DiskUsage, error)
}

// Normalize keeps odd filesystem answers from producing nonsense: used and free never exceed the total,
// and together they never exceed it either.
func (d DiskUsage) Normalize() DiskUsage {
	d.Used = min(d.Used, d.Total)
	d.Free = min(d.Free, d.Total-d.Used)

	return d
}

// Full reports whether new uploads must be refused. A filesystem that reports no size is never treated as full.
func (d DiskUsage) Full() bool {
	return reaches(d.Used, d.Total, FullPercent)
}

// LevelOf grades used against total. It never returns LevelFull: that is a property of the whole disk.
func LevelOf(used, total uint64) StorageLevel {
	switch {
	case reaches(used, total, CriticalPercent):
		return LevelCritical
	case reaches(used, total, WarnPercent):
		return LevelWarning
	}

	return LevelOK
}

func (d DiskUsage) Level() StorageLevel {
	if d.Full() {
		return LevelFull
	}

	return LevelOf(d.Used, d.Total)
}

// reaches is used*100 >= total*percent without overflow or float rounding at the boundaries.
func reaches(used, total uint64, percent uint64) bool {
	if total == 0 {
		return false
	}
	usedHi, usedLo := bits.Mul64(used, 100)
	totalHi, totalLo := bits.Mul64(total, percent)

	return usedHi > totalHi || (usedHi == totalHi && usedLo >= totalLo)
}

type FakeDisk struct {
	mu    sync.Mutex
	value DiskUsage
	err   error
	calls int
}

func NewFakeDisk(value DiskUsage) *FakeDisk {
	return &FakeDisk{value: value}
}

func (f *FakeDisk) Set(value DiskUsage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.value = value
}

func (f *FakeDisk) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *FakeDisk) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *FakeDisk) Usage() (DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	return f.value, f.err
}
