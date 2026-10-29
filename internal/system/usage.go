package system

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const usageTTL = 10 * time.Minute

type Usage interface {
	// Sizes returns bytes used per SFTP user, and false until the first scan has finished.
	Sizes() (map[string]uint64, bool)
}

// Scanning every home is slow on an old disk, so it runs in the background and callers get the last result.
type dirUsage struct {
	root string
	now  func() time.Time

	mu        sync.Mutex
	sizes     map[string]uint64
	scannedAt time.Time
	running   bool
}

func NewUsage(root string) Usage {
	return &dirUsage{root: root, now: time.Now}
}

func (u *dirUsage) Sizes() (map[string]uint64, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !u.running && (u.scannedAt.IsZero() || u.now().Sub(u.scannedAt) > usageTTL) {
		u.running = true
		go u.refresh()
	}

	return maps.Clone(u.sizes), !u.scannedAt.IsZero()
}

func (u *dirUsage) refresh() {
	sizes := u.scan()

	u.mu.Lock()
	defer u.mu.Unlock()
	u.sizes, u.scannedAt, u.running = sizes, u.now(), false
}

func (u *dirUsage) scan() map[string]uint64 {
	sizes := map[string]uint64{}
	entries, err := os.ReadDir(u.root)
	if err != nil {
		return sizes
	}
	for _, entry := range entries {
		if entry.IsDir() && ValidUsername(entry.Name()) {
			sizes[entry.Name()] = directorySize(filepath.Join(u.root, entry.Name(), "files"))
		}
	}

	return sizes
}

// Symlinks are counted as the link itself and never followed; unreadable parts are skipped.
func directorySize(dir string) uint64 {
	var total uint64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil && entry != nil && entry.IsDir():
			return fs.SkipDir
		case err != nil || entry.IsDir():
			return nil
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += uint64(info.Size())
		}

		return nil
	})

	return total
}

type FakeUsage struct {
	Bytes map[string]uint64
	Ready bool
}

func (f *FakeUsage) Sizes() (map[string]uint64, bool) {
	return maps.Clone(f.Bytes), f.Ready
}
