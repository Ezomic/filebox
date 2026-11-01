package system

import (
	"slices"
	"strings"
	"time"
)

type memoryTrashed struct {
	item    TrashItem
	entries map[string]*memoryEntry
}

func (f *FakeFiles) Trash(path string) (TrashItem, error) {
	path, err := cleanNonRoot(path)
	if err != nil {
		return TrashItem{}, err
	}
	id, err := newUploadID()
	if err != nil {
		return TrashItem{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.parentError(path); err != nil {
		return TrashItem{}, err
	}
	entry, ok := f.entries[path]
	if !ok {
		return TrashItem{}, ErrNotFound
	}

	moved := map[string]*memoryEntry{}
	var size int64
	for other, inner := range f.entries {
		if other == path || isInside(other, path) {
			moved[strings.TrimPrefix(other, path)] = inner
			size += int64(len(inner.data))
			delete(f.entries, other)
		}
	}
	if f.trash == nil {
		f.trash = map[string]*memoryTrashed{}
	}
	item := TrashItem{ID: id, Path: path, DeletedAt: time.Now(), Size: size, IsDir: entry.isDir}
	f.trash[id] = &memoryTrashed{item: item, entries: moved}

	return item, nil
}

func (f *FakeFiles) ListTrash() ([]TrashItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	items := make([]TrashItem, 0, len(f.trash))
	for _, trashed := range f.trash {
		items = append(items, trashed.item)
	}
	slices.SortFunc(items, func(a, b TrashItem) int { return b.DeletedAt.Compare(a.DeletedAt) })

	return items, nil
}

func (f *FakeFiles) RestoreTrash(id string) (string, error) {
	if !uploadIDPattern.MatchString(id) {
		return "", ErrNotFound
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	trashed, ok := f.trash[id]
	if !ok {
		return "", ErrNotFound
	}
	target := trashed.item.Path
	for _, prefix := range parentChain(parentOf(target)) {
		entry, exists := f.entries[prefix]
		switch {
		case !exists:
			f.entries[prefix] = &memoryEntry{isDir: true, modified: time.Now()}
		case !entry.isDir:
			return "", ErrNotDir
		}
	}

	for attempt := 0; attempt < maxRestoreAttempts; attempt++ {
		candidate := restoreCandidate(target, trashed.item.IsDir, attempt)
		if _, err := cleanNonRoot(candidate); err != nil {
			return "", err
		}
		if _, taken := f.entries[candidate]; taken {
			continue
		}
		for suffix, entry := range trashed.entries {
			f.entries[candidate+suffix] = entry
		}
		delete(f.trash, id)

		return candidate, nil
	}

	return "", ErrExists
}

func (f *FakeFiles) PurgeTrash(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.trash[id]; !ok || !uploadIDPattern.MatchString(id) {
		return ErrNotFound
	}
	delete(f.trash, id)

	return nil
}

func (f *FakeFiles) EmptyTrash() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	removed := len(f.trash)
	f.trash = nil

	return removed, nil
}

func (f *FakeFiles) SweepTrash(cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	swept := 0
	for id, trashed := range f.trash {
		if trashed.item.DeletedAt.Before(cutoff) {
			delete(f.trash, id)
			swept++
		}
	}

	return swept, nil
}
