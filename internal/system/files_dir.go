package system

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

const (
	uploadTempPrefix = ".upload-"
	uploadTempSuffix = ".tmp"
)

type dirFiles struct {
	root *os.Root
}

func NewDirFiles(dir string) (Files, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}

	return &dirFiles{root: root}, nil
}

func (d *dirFiles) Close() error {
	return d.root.Close()
}

func (d *dirFiles) List(dir string) ([]FileInfo, error) {
	dir, err := CleanPath(dir)
	if err != nil {
		return nil, err
	}
	dirFile, err := d.root.Open(rootRelative(dir))
	if err != nil {
		return nil, d.classify(dir, err)
	}
	defer dirFile.Close()

	info, err := dirFile.Stat()
	if err != nil {
		return nil, d.classify(dir, err)
	}
	if !info.IsDir() {
		return nil, ErrNotDir
	}
	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		return nil, err
	}

	listing := make([]FileInfo, 0, len(entries))
	for _, entry := range entries {
		entryInfo, err := entry.Info()
		if err != nil {
			continue
		}
		if entryInfo.Mode()&fs.ModeSymlink != 0 {
			if resolved, err := d.root.Stat(path.Join(rootRelative(dir), entry.Name())); err == nil {
				entryInfo = resolved
			}
		}
		listing = append(listing, FileInfo{Name: entry.Name(), IsDir: entryInfo.IsDir(), Size: entryInfo.Size(), Modified: entryInfo.ModTime()})
	}
	sortListing(listing)

	return listing, nil
}

func (d *dirFiles) Open(name string) (File, FileInfo, error) {
	name, err := cleanNonRoot(name)
	if err != nil {
		return nil, FileInfo{}, err
	}
	file, err := d.root.Open(name)
	if err != nil {
		return nil, FileInfo{}, d.classify(name, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return nil, FileInfo{}, d.classify(name, err)
	}
	if info.IsDir() {
		_ = file.Close()

		return nil, FileInfo{}, ErrIsDir
	}

	return file, FileInfo{Name: info.Name(), Size: info.Size(), Modified: info.ModTime()}, nil
}

func (d *dirFiles) Write(name string, content io.Reader, overwrite bool) error {
	name, err := cleanNonRoot(name)
	if err != nil {
		return err
	}
	if err := d.checkParent(name); err != nil {
		return err
	}
	if err := d.checkWritable(name, overwrite); err != nil {
		return err
	}

	temp, err := d.copyToTemp(parentOf(name), content)
	if err != nil {
		return err
	}
	// Checked again because the upload may have taken a long time.
	if err := d.checkWritable(name, overwrite); err != nil {
		_ = d.root.Remove(temp)

		return err
	}
	if err := d.root.Rename(temp, name); err != nil {
		_ = d.root.Remove(temp)

		return d.classify(name, err)
	}

	return nil
}

func (d *dirFiles) checkWritable(name string, overwrite bool) error {
	info, exists, err := d.existing(name)
	switch {
	case err != nil:
		return err
	case !exists:
		return nil
	case info.IsDir():
		return ErrIsDir
	case !overwrite:
		return ErrExists
	}

	return nil
}

func (d *dirFiles) copyToTemp(dir string, content io.Reader) (string, error) {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	temp := path.Join(rootRelative(dir), uploadTempPrefix+hex.EncodeToString(suffix)+uploadTempSuffix)

	file, err := d.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
	if err != nil {
		return "", d.classify(dir, err)
	}
	_, copyErr := io.Copy(file, content)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		_ = d.root.Remove(temp)

		return "", err
	}

	return temp, nil
}

func (d *dirFiles) Rename(from, to string) error {
	from, err := cleanNonRoot(from)
	if err != nil {
		return err
	}
	to, err = cleanNonRoot(to)
	if err != nil {
		return err
	}
	if isInside(to, from) {
		return ErrIntoSelf
	}
	if _, err := d.root.Lstat(from); err != nil {
		return d.classify(from, err)
	}
	if err := d.checkParent(to); err != nil {
		return err
	}
	if err := d.requireAbsent(to); err != nil {
		return err
	}

	if err := d.root.Rename(from, to); err != nil {
		return d.classify(to, err)
	}

	return nil
}

func (d *dirFiles) Remove(name string) error {
	name, err := cleanNonRoot(name)
	if err != nil {
		return err
	}
	info, err := d.root.Lstat(name)
	if err != nil {
		return d.classify(name, err)
	}
	if info.IsDir() {
		if err := d.requireEmpty(name); err != nil {
			return err
		}
	}
	if err := d.root.Remove(name); err != nil {
		return d.classify(name, err)
	}

	return nil
}

func (d *dirFiles) requireEmpty(name string) error {
	dir, err := d.root.Open(name)
	if err != nil {
		return d.classify(name, err)
	}
	defer dir.Close()

	if _, err := dir.ReadDir(1); err == nil {
		return ErrNotEmpty
	} else if !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}

func (d *dirFiles) Mkdir(name string) error {
	name, err := cleanNonRoot(name)
	if err != nil {
		return err
	}
	if err := d.checkParent(name); err != nil {
		return err
	}
	if err := d.requireAbsent(name); err != nil {
		return err
	}
	if err := d.root.Mkdir(name, 0o775); err != nil {
		return d.classify(name, err)
	}

	return nil
}

func (d *dirFiles) checkParent(name string) error {
	parent := parentOf(name)
	info, err := d.root.Stat(rootRelative(parent))
	if err != nil {
		return d.classify(name, err)
	}
	if !info.IsDir() {
		return ErrNotDir
	}

	return nil
}

// A symlink is described by what it points at, so overwriting never writes through a link
// and a link to a folder is not mistaken for a file.
func (d *dirFiles) existing(name string) (fs.FileInfo, bool, error) {
	info, err := d.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, d.classify(name, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := d.root.Stat(name)
		switch {
		case err == nil:
			info = target
		case !errors.Is(err, fs.ErrNotExist):
			return nil, false, d.classify(name, err)
		}
	}

	return info, true, nil
}

// os.Root has no exported sentinel for a path that leaves the root, so it is matched by
// message. TestDirFilesRefusesSymlinkEscape fails if a Go upgrade changes it.
func isEscape(err error) bool {
	return strings.Contains(err.Error(), "path escapes from parent")
}

// Raw OS errors never go further than this package's callers' logs.
func (d *dirFiles) classify(name string, err error) error {
	switch {
	case isEscape(err):
		return ErrNotFound
	case errors.Is(err, fs.ErrNotExist):
		if d.ancestorIsFile(name) {
			return ErrNotDir
		}

		return ErrNotFound
	case errors.Is(err, fs.ErrExist):
		return ErrExists
	case errors.Is(err, syscall.ENOTDIR):
		return ErrNotDir
	case errors.Is(err, syscall.ENOTEMPTY):
		return ErrNotEmpty
	}

	return err
}

// Windows reports a path through a file as "not found" rather than "not a directory".
func (d *dirFiles) ancestorIsFile(name string) bool {
	for parent := parentOf(name); parent != ""; parent = parentOf(parent) {
		if info, err := d.root.Stat(parent); err == nil && !info.IsDir() {
			return true
		}
	}

	return false
}

func (d *dirFiles) requireAbsent(name string) error {
	_, exists, err := d.existing(name)
	if err != nil {
		return err
	}
	if exists {
		return ErrExists
	}

	return nil
}

func rootRelative(dir string) string {
	if dir == "" {
		return "."
	}

	return dir
}

func sortListing(listing []FileInfo) {
	slices.SortFunc(listing, func(a, b FileInfo) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}

			return 1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}

		return strings.Compare(a.Name, b.Name)
	})
}
