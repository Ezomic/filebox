package system

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"time"
)

type trashMeta struct {
	Path    string    `json:"path"`
	Deleted time.Time `json:"deleted"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
}

func trashItemPath(id string) string { return TrashDir + "/" + id }
func trashMetaPath(id string) string { return TrashDir + "/" + id + ".json" }

func (d *dirFiles) lockTrash(id string) func() {
	return lockUpload(d.root.Name() + "\x00trash\x00" + id)
}

func (d *dirFiles) Trash(name string) (TrashItem, error) {
	name, err := cleanNonRoot(name)
	if err != nil {
		return TrashItem{}, err
	}
	info, err := d.root.Lstat(name)
	if err != nil {
		return TrashItem{}, d.classify(name, err)
	}
	size := info.Size()
	if info.IsDir() {
		size = d.treeSize(name)
	}
	if err := d.root.Mkdir(TrashDir, 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
		return TrashItem{}, d.classify(TrashDir, err)
	}
	id, err := newUploadID()
	if err != nil {
		return TrashItem{}, err
	}
	defer d.lockTrash(id)()

	meta := trashMeta{Path: name, Deleted: time.Now().UTC(), Size: size, IsDir: info.IsDir()}
	if err := d.writeTrashMeta(id, meta); err != nil {
		return TrashItem{}, err
	}
	if err := d.root.Rename(name, trashItemPath(id)); err != nil {
		_ = d.root.Remove(trashMetaPath(id))

		return TrashItem{}, d.classify(name, err)
	}

	return meta.item(id), nil
}

func (m trashMeta) item(id string) TrashItem {
	return TrashItem{ID: id, Path: m.Path, DeletedAt: m.Deleted, Size: m.Size, IsDir: m.IsDir}
}

// Symlinks are counted as links, never followed, and an unreadable corner must not stop a delete.
func (d *dirFiles) treeSize(name string) int64 {
	var total int64
	_ = fs.WalkDir(d.root.FS(), name, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += info.Size()
		}

		return nil
	})

	return total
}

func (d *dirFiles) writeTrashMeta(id string, meta trashMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	file, err := d.root.OpenFile(trashMetaPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
	if err != nil {
		return d.classify(TrashDir, err)
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = d.root.Remove(trashMetaPath(id))

		return err
	}

	return nil
}

func (d *dirFiles) readTrashMeta(id string) (trashMeta, error) {
	data, err := d.readCapped(trashMetaPath(id))
	if err != nil {
		return trashMeta{}, notFoundOr(err)
	}
	var meta trashMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return trashMeta{}, ErrNotFound
	}

	return meta, nil
}

func (d *dirFiles) trashEntries() ([]fs.DirEntry, error) {
	dir, err := d.root.Open(TrashDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, d.classify(TrashDir, err)
	}
	defer dir.Close()

	return dir.ReadDir(-1)
}

func (d *dirFiles) ListTrash() ([]TrashItem, error) {
	entries, err := d.trashEntries()
	if err != nil {
		return nil, err
	}

	items := []TrashItem{}
	for _, entry := range entries {
		match := trashEntryPattern.FindStringSubmatch(entry.Name())
		if match == nil || match[2] == "" {
			continue
		}
		meta, err := d.readTrashMeta(match[1])
		if err != nil {
			continue
		}
		if _, err := d.root.Lstat(trashItemPath(match[1])); err != nil {
			continue
		}
		items = append(items, meta.item(match[1]))
	}
	slices.SortFunc(items, func(a, b TrashItem) int { return b.DeletedAt.Compare(a.DeletedAt) })

	return items, nil
}

func (d *dirFiles) RestoreTrash(id string) (string, error) {
	if !uploadIDPattern.MatchString(id) {
		return "", ErrNotFound
	}
	defer d.lockTrash(id)()

	meta, err := d.readTrashMeta(id)
	if err != nil {
		return "", err
	}
	itemPath := trashItemPath(id)
	info, err := d.root.Lstat(itemPath)
	if err != nil {
		return "", notFoundOr(err)
	}
	target, err := cleanNonRoot(meta.Path)
	if err != nil {
		return "", err
	}
	if err := d.ensureParents(parentOf(target)); err != nil {
		return "", err
	}

	for attempt := 0; attempt < maxRestoreAttempts; attempt++ {
		candidate := restoreCandidate(target, info.IsDir(), attempt)
		if _, err := cleanNonRoot(candidate); err != nil {
			return "", err
		}
		err := d.moveWithoutClobbering(itemPath, candidate, info.Mode().IsRegular())
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", d.classify(candidate, err)
		}
		_ = d.root.Remove(trashMetaPath(id))
		_ = d.root.RemoveAll(itemPath)

		return candidate, nil
	}

	return "", ErrExists
}

func (d *dirFiles) ensureParents(dir string) error {
	for _, prefix := range parentChain(dir) {
		info, exists, err := d.existing(prefix)
		switch {
		case err != nil:
			return err
		case !exists:
			if err := d.root.Mkdir(prefix, 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
				return d.classify(prefix, err)
			}
		case !info.IsDir():
			return ErrNotDir
		}
	}

	return nil
}

// A hard link fails when the target exists, so a file that appears at the last moment is never overwritten.
// Folders and links cannot use that, so they rely on a check right before the rename.
func (d *dirFiles) moveWithoutClobbering(from, to string, regular bool) error {
	if regular {
		err := d.root.Link(from, to)
		if err == nil {
			_ = d.root.Remove(from)

			return nil
		}
		if errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if _, err := d.root.Lstat(to); err == nil {
		return fs.ErrExist
	}

	return d.root.Rename(from, to)
}

func (d *dirFiles) PurgeTrash(id string) error {
	if !uploadIDPattern.MatchString(id) {
		return ErrNotFound
	}
	defer d.lockTrash(id)()

	removed, err := d.removeTrashEntry(id)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotFound
	}

	return nil
}

func (d *dirFiles) removeTrashEntry(id string) (bool, error) {
	_, itemErr := d.root.Lstat(trashItemPath(id))
	_, metaErr := d.root.Lstat(trashMetaPath(id))
	if itemErr != nil && metaErr != nil {
		return false, nil
	}
	if itemErr == nil {
		if err := d.root.RemoveAll(trashItemPath(id)); err != nil {
			return false, err
		}
	}
	if err := d.root.Remove(trashMetaPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}

	return true, nil
}

func (d *dirFiles) EmptyTrash() (int, error) {
	entries, err := d.trashEntries()
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	removed := 0
	var errs []error
	for _, entry := range entries {
		match := trashEntryPattern.FindStringSubmatch(entry.Name())
		if match == nil || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		unlock := d.lockTrash(match[1])
		gone, err := d.removeTrashEntry(match[1])
		unlock()
		switch {
		case err != nil:
			errs = append(errs, err)
		case gone:
			removed++
		}
	}

	return removed, errors.Join(errs...)
}

func (d *dirFiles) SweepTrash(cutoff time.Time) (int, error) {
	entries, err := d.trashEntries()
	if err != nil {
		return 0, err
	}
	swept := 0
	var errs []error
	for _, entry := range entries {
		match := trashEntryPattern.FindStringSubmatch(entry.Name())
		if match == nil || match[2] == "" {
			continue
		}
		id := match[1]
		deleted, ok := d.trashedAt(id)
		if !ok || !deleted.Before(cutoff) {
			continue
		}
		unlock := d.lockTrash(id)
		if again, ok := d.trashedAt(id); !ok || !again.Before(cutoff) {
			unlock()
			continue
		}
		gone, err := d.removeTrashEntry(id)
		unlock()
		switch {
		case err != nil:
			errs = append(errs, err)
		case gone:
			swept++
		}
	}

	return swept, errors.Join(errs...)
}

// An entry whose metadata is unreadable ages by the metadata file's own time, so it can never linger forever.
func (d *dirFiles) trashedAt(id string) (time.Time, bool) {
	if meta, err := d.readTrashMeta(id); err == nil && !meta.Deleted.IsZero() {
		return meta.Deleted, true
	}
	info, err := d.root.Lstat(trashMetaPath(id))
	if err != nil {
		return time.Time{}, false
	}

	return info.ModTime(), true
}

const maxTrashMetaBytes = 64 << 10

func (d *dirFiles) readCapped(name string) ([]byte, error) {
	file, err := d.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return io.ReadAll(io.LimitReader(file, maxTrashMetaBytes))
}
