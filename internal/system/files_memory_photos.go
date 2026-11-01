package system

import (
	"bytes"
	"strings"
	"time"
)

func (f *FakeFiles) ListPhotos(after string, limit int) (PhotoPage, error) {
	if after != "" {
		if _, _, err := parsePhotoCursor(after); err != nil {
			return PhotoPage{}, err
		}
	}
	f.mu.Lock()
	var photos []Photo
	for name, entry := range f.entries {
		if photo, ok := f.photoOf(name, entry); ok {
			photos = append(photos, photo)
		}
	}
	f.mu.Unlock()

	return pagePhotos(photos, after, limit)
}

func (f *FakeFiles) photoOf(name string, entry *memoryEntry) (Photo, bool) {
	kind, ext, ok := mediaKind(name)
	if !ok || entry.isDir || len(entry.data) == 0 || len(name) > maxPathBytes {
		return Photo{}, false
	}
	for _, segment := range strings.Split(name, "/") {
		if !validSegment(segment) {
			return Photo{}, false
		}
	}
	photo := fakePhoto(name, kind, entry)
	if date, ok := exifDate(bytes.NewReader(entry.data), photo.Size, ext); ok {
		photo.Date = date
	}

	return photo, true
}

func fakePhoto(name, kind string, entry *memoryEntry) Photo {
	size := int64(len(entry.data))

	return Photo{Path: name, Kind: kind, Size: size, Date: entry.modified.UTC(), Modified: entry.modified, Key: photoKey(name, size, entry.modified), Scope: "fake"}
}

func (f *FakeFiles) OpenPhoto(name string) (File, Photo, error) {
	name, err := cleanNonRoot(name)
	if err != nil {
		return nil, Photo{}, err
	}
	kind, _, ok := mediaKind(name)
	if !ok {
		return nil, Photo{}, ErrNotMedia
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.parentError(name); err != nil {
		return nil, Photo{}, err
	}
	entry, ok := f.entries[name]
	switch {
	case !ok:
		return nil, Photo{}, ErrNotFound
	case entry.isDir:
		return nil, Photo{}, ErrNotMedia
	}

	return memoryFile{bytes.NewReader(entry.data)}, fakePhoto(name, kind, entry), nil
}

// SetModified lets a demo or a test place a file at a chosen time.
func (f *FakeFiles) SetModified(name string, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.entries[name]; ok {
		entry.modified = modified
	}
}

func (f *FakeFiles) ReadThumb(name string) (File, error) {
	if _, ok := thumbKeyOf(name); !ok {
		return nil, ErrInvalidPath
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	data, ok := f.thumbs[name]
	if !ok {
		return nil, ErrNotFound
	}

	return memoryFile{bytes.NewReader(data)}, nil
}

func (f *FakeFiles) WriteThumb(name string, data []byte) error {
	if _, ok := thumbKeyOf(name); !ok {
		return ErrInvalidPath
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.thumbs == nil {
		f.thumbs = map[string][]byte{}
	}
	f.thumbs[name] = bytes.Clone(data)

	return nil
}

func (f *FakeFiles) ThumbNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	names := make([]string, 0, len(f.thumbs))
	for name := range f.thumbs {
		names = append(names, name)
	}

	return names
}

func (f *FakeFiles) SweepThumbs(time.Time) (int, error) {
	f.mu.Lock()
	live := map[string]bool{}
	for name, entry := range f.entries {
		if photo, ok := f.photoOf(name, entry); ok {
			live[photo.Key] = true
		}
	}
	removed := 0
	for name := range f.thumbs {
		if key, _ := thumbKeyOf(name); !live[key] {
			delete(f.thumbs, name)
			removed++
		}
	}
	f.mu.Unlock()

	return removed, nil
}
