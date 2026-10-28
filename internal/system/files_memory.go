package system

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
)

type memoryEntry struct {
	isDir    bool
	data     []byte
	modified time.Time
}

type FakeFiles struct {
	mu      sync.Mutex
	entries map[string]*memoryEntry
}

func NewFakeFiles() *FakeFiles {
	return &FakeFiles{entries: map[string]*memoryEntry{}}
}

func NewSampleFiles() *FakeFiles {
	files := NewFakeFiles()
	for _, dir := range []string{"alice", "alice/files", "bob", "bob/files", "family-share", "family-share/files"} {
		_ = files.Mkdir(dir)
	}
	samples := map[string]string{
		"alice/files/holiday-notes.txt":      "Packing list and ferry times.\n",
		"alice/files/budget.csv":             "month,total\njan,120\nfeb,98\n",
		"family-share/files/readme.txt":      "Shared with everyone in the family.\n",
		"family-share/files/recipe-soup.pdf": "%PDF-1.4 sample\n",
	}
	for name, content := range samples {
		_ = files.Write(name, strings.NewReader(content), false)
	}

	return files
}

// dirExists treats the root as a folder that always exists.
func (f *FakeFiles) dirExists(path string) (exists, isDir bool) {
	if path == "" {
		return true, true
	}
	entry, ok := f.entries[path]

	return ok, ok && entry.isDir
}

func (f *FakeFiles) parentError(path string) error {
	exists, isDir := f.dirExists(parentOf(path))
	switch {
	case !exists:
		return ErrNotFound
	case !isDir:
		return ErrNotDir
	}

	return nil
}

func (f *FakeFiles) List(dir string) ([]FileInfo, error) {
	dir, err := CleanPath(dir)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	exists, isDir := f.dirExists(dir)
	switch {
	case !exists:
		return nil, ErrNotFound
	case !isDir:
		return nil, ErrNotDir
	}

	listing := []FileInfo{}
	for path, entry := range f.entries {
		if parentOf(path) == dir && !isUploadTemp(baseOf(path)) {
			listing = append(listing, FileInfo{Name: baseOf(path), IsDir: entry.isDir, Size: int64(len(entry.data)), Modified: entry.modified})
		}
	}
	sortListing(listing)

	return listing, nil
}

type memoryFile struct {
	*bytes.Reader
}

func (memoryFile) Close() error { return nil }

func (f *FakeFiles) Open(path string) (File, FileInfo, error) {
	path, err := cleanNonRoot(path)
	if err != nil {
		return nil, FileInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.parentError(path); err != nil {
		return nil, FileInfo{}, err
	}
	entry, ok := f.entries[path]
	switch {
	case !ok:
		return nil, FileInfo{}, ErrNotFound
	case entry.isDir:
		return nil, FileInfo{}, ErrIsDir
	}

	return memoryFile{bytes.NewReader(entry.data)}, FileInfo{Name: baseOf(path), Size: int64(len(entry.data)), Modified: entry.modified}, nil
}

// The body is read before anything is stored, like the real implementation's temp file.
func (f *FakeFiles) Write(path string, content io.Reader, overwrite bool) error {
	path, err := cleanNonRoot(path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	if err := f.writeAllowed(path, overwrite); err != nil {
		f.mu.Unlock()

		return err
	}
	f.mu.Unlock()

	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.writeAllowed(path, overwrite); err != nil {
		return err
	}
	f.entries[path] = &memoryEntry{data: data, modified: time.Now()}

	return nil
}

func (f *FakeFiles) writeAllowed(path string, overwrite bool) error {
	if err := f.parentError(path); err != nil {
		return err
	}
	entry, ok := f.entries[path]
	switch {
	case !ok:
		return nil
	case entry.isDir:
		return ErrIsDir
	case !overwrite:
		return ErrExists
	}

	return nil
}

func (f *FakeFiles) Mkdir(path string) error {
	path, err := cleanNonRoot(path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.parentError(path); err != nil {
		return err
	}
	if _, ok := f.entries[path]; ok {
		return ErrExists
	}
	f.entries[path] = &memoryEntry{isDir: true, modified: time.Now()}

	return nil
}

func (f *FakeFiles) Rename(from, to string) error {
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
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.entries[from]; !ok {
		return ErrNotFound
	}
	if err := f.parentError(to); err != nil {
		return err
	}
	if _, ok := f.entries[to]; ok {
		return ErrExists
	}

	moved := map[string]*memoryEntry{}
	for path, entry := range f.entries {
		if path == from || isInside(path, from) {
			moved[to+strings.TrimPrefix(path, from)] = entry
			delete(f.entries, path)
		}
	}
	for path, entry := range moved {
		f.entries[path] = entry
	}

	return nil
}

func (f *FakeFiles) Remove(path string) error {
	path, err := cleanNonRoot(path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.parentError(path); err != nil {
		return err
	}
	entry, ok := f.entries[path]
	if !ok {
		return ErrNotFound
	}
	if entry.isDir {
		for other := range f.entries {
			if isInside(other, path) {
				return ErrNotEmpty
			}
		}
	}
	delete(f.entries, path)

	return nil
}
