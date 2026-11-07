package system

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// SnapshotsDir holds the earlier versions at the data root: a folder per user and one for the family folder, each with
// the read-only btrfs snapshots filebox-snapshot takes every night. It is outside every SFTP chroot.
const SnapshotsDir = ".snapshots"

// A version is named after the moment it was taken, in the box's own time zone.
const versionLayout = "2006-01-02T1504"

var versionPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{4}$`)

// Version is one earlier version of a Files root.
type Version struct {
	ID    string
	Taken time.Time
}

// Versions are the earlier versions of one Files root.
type Versions interface {
	// List returns the versions newest first. A root that has none yet has an empty list, not an error.
	List() ([]Version, error)
	// Open returns one version's files, which are read-only on disk. An unknown id is ErrNotFound.
	Open(id string) (Files, error)
}

// Restored is what a restore did: the path it filled, and the trash item that now holds what was there before.
// TrashID is empty when the path was free.
type Restored struct {
	Path    string
	TrashID string
}

type dirVersions struct {
	dir string
}

// OpenUserVersions gives the earlier versions of one user's files folder. Like OpenUserFiles it checks the name again,
// so no value can reach another folder under the snapshots, such as a deleted account's.
func OpenUserVersions(base, user string) (Versions, error) {
	if !ValidUsername(user) {
		return nil, ErrInvalidUsername
	}

	return dirVersions{dir: filepath.Join(base, SnapshotsDir, user)}, nil
}

func OpenFamilyVersions(base string) Versions {
	return dirVersions{dir: filepath.Join(base, SnapshotsDir, FamilyDir)}
}

func (v dirVersions) List() ([]Version, error) {
	entries, err := os.ReadDir(v.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Version{}, nil
	}
	if err != nil {
		return nil, err
	}
	versions := []Version{}
	for _, entry := range entries {
		if taken, ok := versionTime(entry.Name()); ok && entry.IsDir() {
			versions = append(versions, Version{ID: entry.Name(), Taken: taken})
		}
	}
	sortVersions(versions)

	return versions, nil
}

func (v dirVersions) Open(id string) (Files, error) {
	if _, ok := versionTime(id); !ok {
		return nil, ErrNotFound
	}
	dir := filepath.Join(v.dir, id)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, ErrNotFound
	}

	return NewDirFiles(dir)
}

func versionTime(id string) (time.Time, bool) {
	if !versionPattern.MatchString(id) {
		return time.Time{}, false
	}
	taken, err := time.ParseInLocation(versionLayout, id, time.Local)

	return taken, err == nil && taken.Format(versionLayout) == id
}

func sortVersions(versions []Version) {
	slices.SortFunc(versions, func(a, b Version) int {
		if c := b.Taken.Compare(a.Taken); c != 0 {
			return c
		}

		return strings.Compare(b.ID, a.ID)
	})
}

// VersionSize is how many bytes restoring path from an earlier version writes, for the quota check before a restore.
// It counts exactly what RestoreVersion copies.
func VersionSize(earlier Files, path string) (int64, error) {
	info, err := restorableItem(earlier, path)
	if err != nil {
		return 0, err
	}

	return treeBytes(earlier, path, info)
}

func treeBytes(files Files, path string, info FileInfo) (int64, error) {
	if !info.IsDir {
		return info.Size, nil
	}
	entries, err := files.List(path)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if !copied(entry) {
			continue
		}
		size, err := treeBytes(files, path+"/"+entry.Name, entry)
		if err != nil {
			return 0, err
		}
		total += size
	}

	return total, nil
}

// Symlinks and names the portal cannot address are left out of a restore, the same as from a zip download.
func copied(entry FileInfo) bool {
	return !entry.Symlink && validSegment(entry.Name)
}

func restorableItem(earlier Files, path string) (FileInfo, error) {
	path, err := cleanNonRoot(path)
	if err != nil {
		return FileInfo{}, err
	}
	info, err := earlier.Lstat(path)
	if err != nil {
		return FileInfo{}, err
	}
	if info.Symlink {
		return FileInfo{}, ErrNotFound
	}

	return info, nil
}

// RestoreVersion copies path from an earlier version back to the same path in live. Whatever live has at that path now
// goes to the trash first, so the restore can be undone from there. If the copy fails halfway, the half copy is
// deleted and the trashed item is put back.
func RestoreVersion(live, earlier Files, path string) (Restored, error) {
	info, err := restorableItem(earlier, path)
	if err != nil {
		return Restored{}, err
	}
	if err := ensureFolders(live, parentOf(path)); err != nil {
		return Restored{}, err
	}
	restored := Restored{Path: path}
	switch _, err := live.Lstat(path); {
	case err == nil:
		item, err := live.Trash(path)
		if err != nil {
			return Restored{}, err
		}
		restored.TrashID = item.ID
	case !errors.Is(err, ErrNotFound):
		return Restored{}, err
	}
	if created, err := copyVersion(live, earlier, path, info); err != nil {
		undoRestore(live, path, created, restored.TrashID)

		return Restored{}, err
	}

	return restored, nil
}

func ensureFolders(live Files, dir string) error {
	for _, prefix := range parentChain(dir) {
		info, err := live.Stat(prefix)
		switch {
		case errors.Is(err, ErrNotFound):
			if err := live.Mkdir(prefix); err != nil && !errors.Is(err, ErrExists) {
				return err
			}
		case err != nil:
			return err
		case !info.IsDir:
			return ErrNotDir
		}
	}

	return nil
}

// copyVersion reports whether it created anything at path, so a failed restore knows what to clear away.
func copyVersion(live, earlier Files, path string, info FileInfo) (bool, error) {
	if info.IsDir {
		if err := live.Mkdir(path); err != nil {
			return false, err
		}

		return true, copyFolder(live, earlier, path, info.Modified)
	}
	file, _, err := earlier.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := live.Write(path, file, false); err != nil {
		return false, err
	}

	return true, live.SetModified(path, info.Modified)
}

func copyFolder(live, earlier Files, dir string, modified time.Time) error {
	entries, err := earlier.List(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !copied(entry) {
			continue
		}
		if _, err := copyVersion(live, earlier, dir+"/"+entry.Name, entry); err != nil {
			return err
		}
	}

	// Last, because filling the folder changed its time.
	return live.SetModified(dir, modified)
}

// The half copy is deleted directly: a restore usually fails for lack of space, and then the trash cannot take it.
func undoRestore(live Files, path string, created bool, trashID string) {
	if created {
		if err := live.RemoveAll(path); err != nil {
			log.Printf("restore: could not delete the half restored copy: %v", withoutPath(err))
		}
	}
	if trashID != "" {
		if _, err := live.RestoreTrash(trashID); err != nil {
			log.Printf("restore: could not put back the item it moved to the trash: %v", withoutPath(err))
		}
	}
}

// The panel and the portal share one journal, so a logged error must not carry a file name.
func withoutPath(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err
	}

	return err
}
