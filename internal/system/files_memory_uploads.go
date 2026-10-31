package system

import (
	"io"
	"time"
)

type memoryUpload struct {
	info    UploadInfo
	data    []byte
	touched time.Time
}

func (f *FakeFiles) Stat(path string) (FileInfo, error) {
	path, err := cleanNonRoot(path)
	if err != nil {
		return FileInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.parentError(path); err != nil {
		return FileInfo{}, err
	}
	entry, ok := f.entries[path]
	if !ok {
		return FileInfo{}, ErrNotFound
	}

	return FileInfo{Name: baseOf(path), IsDir: entry.isDir, Size: int64(len(entry.data)), Modified: entry.modified}, nil
}

func (f *FakeFiles) CreateUpload(path string, size int64, overwrite bool) (UploadInfo, error) {
	path, err := cleanNonRoot(path)
	if err != nil {
		return UploadInfo{}, err
	}
	if size < 0 {
		return UploadInfo{}, ErrInvalidPath
	}
	id, err := newUploadID()
	if err != nil {
		return UploadInfo{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.writeAllowed(path, overwrite); err != nil {
		return UploadInfo{}, err
	}
	info := UploadInfo{ID: id, Path: path, Size: size, Overwrite: overwrite}
	if f.uploads == nil {
		f.uploads = map[string]*memoryUpload{}
	}
	f.uploads[id] = &memoryUpload{info: info, touched: time.Now()}

	return info, nil
}

func (f *FakeFiles) upload(id string) (*memoryUpload, error) {
	if !uploadIDPattern.MatchString(id) {
		return nil, ErrNotFound
	}
	upload, ok := f.uploads[id]
	if !ok {
		return nil, ErrNotFound
	}

	return upload, nil
}

func (u *memoryUpload) current() UploadInfo {
	info := u.info
	info.Offset = int64(len(u.data))

	return info
}

func (f *FakeFiles) UploadStatus(id string) (UploadInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	upload, err := f.upload(id)
	if err != nil {
		return UploadInfo{}, err
	}

	return upload.current(), nil
}

func (f *FakeFiles) AppendUpload(id string, offset, length int64, content io.Reader) (UploadInfo, error) {
	f.mu.Lock()
	upload, err := f.upload(id)
	if err != nil {
		f.mu.Unlock()

		return UploadInfo{}, err
	}
	info := upload.current()
	switch {
	case offset != info.Offset:
		f.mu.Unlock()

		return info, ErrUploadOffset
	case length <= 0:
		f.mu.Unlock()

		return info, ErrUploadShort
	case length > info.Size-info.Offset:
		f.mu.Unlock()

		return info, ErrUploadTooLarge
	}
	f.mu.Unlock()

	data, readErr := io.ReadAll(io.LimitReader(content, length+1))

	f.mu.Lock()
	defer f.mu.Unlock()
	upload, err = f.upload(id)
	if err != nil {
		return UploadInfo{}, err
	}
	if int64(len(upload.data)) != offset {
		return upload.current(), ErrUploadOffset
	}
	if int64(len(data)) > length {
		return upload.current(), ErrUploadTooLarge
	}
	upload.data = append(upload.data, data...)
	upload.touched = time.Now()
	switch {
	case readErr != nil:
		return upload.current(), readErr
	case int64(len(data)) < length:
		return upload.current(), ErrUploadShort
	}

	return upload.current(), nil
}

func (f *FakeFiles) FinishUpload(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	upload, err := f.upload(id)
	if err != nil {
		return err
	}
	info := upload.current()
	if info.Offset != info.Size {
		return ErrUploadIncomplete
	}
	if err := f.writeAllowed(info.Path, info.Overwrite); err != nil {
		return err
	}
	f.entries[info.Path] = &memoryEntry{data: upload.data, modified: time.Now()}
	delete(f.uploads, id)

	return nil
}

func (f *FakeFiles) SweepUploads(cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	swept := 0
	for id, upload := range f.uploads {
		if upload.touched.Before(cutoff) {
			delete(f.uploads, id)
			swept++
		}
	}

	return swept, nil
}
