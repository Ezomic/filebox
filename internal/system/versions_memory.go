package system

import (
	"strings"
	"sync"
	"time"
)

// FakeVersions keeps earlier versions in memory, each a FakeFiles of its own.
type FakeVersions struct {
	mu       sync.Mutex
	versions map[string]Version
	files    map[string]*FakeFiles
}

func NewFakeVersions() *FakeVersions {
	return &FakeVersions{versions: map[string]Version{}, files: map[string]*FakeFiles{}}
}

// NewSampleVersions gives the sample files two earlier versions, so the portal has something to show on a PC.
func NewSampleVersions(now time.Time) *FakeVersions {
	versions := NewFakeVersions()
	for days, extra := range map[int]string{1: "alice/files/old-notes.txt", 8: "alice/files/first-draft.txt"} {
		files := NewSampleFiles()
		_ = files.Write(extra, strings.NewReader("As it was earlier.\n"), false)
		versions.Add(now.AddDate(0, 0, -days), files)
	}

	return versions
}

// Add stores files as the version taken at taken, to the minute, and returns its id.
func (f *FakeVersions) Add(taken time.Time, files *FakeFiles) string {
	taken = taken.In(time.Local).Truncate(time.Minute)
	id := taken.Format(versionLayout)
	f.mu.Lock()
	defer f.mu.Unlock()

	f.versions[id] = Version{ID: id, Taken: taken}
	f.files[id] = files

	return id
}

func (f *FakeVersions) List() ([]Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	versions := make([]Version, 0, len(f.versions))
	for _, version := range f.versions {
		versions = append(versions, version)
	}
	sortVersions(versions)

	return versions, nil
}

func (f *FakeVersions) Open(id string) (Files, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	files, ok := f.files[id]
	if !ok {
		return nil, ErrNotFound
	}

	return files, nil
}
