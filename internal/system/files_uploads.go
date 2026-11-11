package system

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var uploadIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type uploadMeta struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Overwrite bool   `json:"overwrite"`
	Visitor   bool   `json:"visitor,omitempty"`
}

// The portal opens a new dirFiles per request, so upload locks are shared per root and id instead of living on the value.
var uploadLocks = struct {
	sync.Mutex
	held map[string]*uploadLock
}{held: map[string]*uploadLock{}}

type uploadLock struct {
	mu   sync.Mutex
	refs int
}

func lockUpload(key string) (unlock func()) {
	uploadLocks.Lock()
	lock := uploadLocks.held[key]
	if lock == nil {
		lock = &uploadLock{}
		uploadLocks.held[key] = lock
	}
	lock.refs++
	uploadLocks.Unlock()

	lock.mu.Lock()

	return func() {
		lock.mu.Unlock()
		uploadLocks.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(uploadLocks.held, key)
		}
		uploadLocks.Unlock()
	}
}

func newUploadID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}

	return hex.EncodeToString(raw), nil
}

func (d *dirFiles) partPath(id string) string { return UploadsDir + "/" + id + ".part" }
func (d *dirFiles) metaPath(id string) string { return UploadsDir + "/" + id + ".json" }

func (d *dirFiles) CreateUpload(name string, size int64, overwrite bool) (UploadInfo, error) {
	return d.createUpload(uploadMeta{Path: name, Size: size, Overwrite: overwrite})
}

func (d *dirFiles) CreateVisitorUpload(name string, size int64) (UploadInfo, error) {
	return d.createUpload(uploadMeta{Path: name, Size: size, Visitor: true})
}

func (d *dirFiles) createUpload(meta uploadMeta) (UploadInfo, error) {
	name, err := cleanNonRoot(meta.Path)
	if err != nil {
		return UploadInfo{}, err
	}
	if meta.Size < 0 {
		return UploadInfo{}, ErrInvalidPath
	}
	if err := d.checkParent(name); err != nil && !(meta.Visitor && errors.Is(err, ErrNotFound)) {
		return UploadInfo{}, err
	}
	if err := d.checkWritable(name, meta.Overwrite); err != nil {
		return UploadInfo{}, err
	}
	if err := d.root.Mkdir(UploadsDir, 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
		return UploadInfo{}, d.classify(UploadsDir, err)
	}
	id, err := newUploadID()
	if err != nil {
		return UploadInfo{}, err
	}

	part, err := d.root.OpenFile(d.partPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
	if err != nil {
		return UploadInfo{}, d.classify(UploadsDir, err)
	}
	if err := part.Close(); err != nil {
		_ = d.root.Remove(d.partPath(id))

		return UploadInfo{}, err
	}
	meta.Path = name
	if err := d.writeMeta(id, meta); err != nil {
		_ = d.root.Remove(d.partPath(id))

		return UploadInfo{}, quotaError(err)
	}

	return UploadInfo{ID: id, Path: name, Size: meta.Size, Overwrite: meta.Overwrite, Visitor: meta.Visitor}, nil
}

func (d *dirFiles) writeMeta(id string, meta uploadMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	file, err := d.root.OpenFile(d.metaPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)

	return errors.Join(writeErr, file.Close())
}

func (d *dirFiles) loadUpload(id string) (UploadInfo, error) {
	if !uploadIDPattern.MatchString(id) {
		return UploadInfo{}, ErrNotFound
	}
	data, err := d.root.ReadFile(d.metaPath(id))
	if err != nil {
		return UploadInfo{}, notFoundOr(err)
	}
	var meta uploadMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return UploadInfo{}, ErrNotFound
	}
	stat, err := d.root.Stat(d.partPath(id))
	if err != nil {
		return UploadInfo{}, notFoundOr(err)
	}

	return UploadInfo{ID: id, Path: meta.Path, Size: meta.Size, Offset: stat.Size(), Overwrite: meta.Overwrite, Visitor: meta.Visitor}, nil
}

func notFoundOr(err error) error {
	if errors.Is(err, fs.ErrNotExist) || isEscape(err) {
		return ErrNotFound
	}

	return err
}

func (d *dirFiles) UploadStatus(id string) (UploadInfo, error) {
	return d.loadUpload(id)
}

func (d *dirFiles) AppendUpload(id string, offset, length int64, content io.Reader) (UploadInfo, error) {
	if !uploadIDPattern.MatchString(id) {
		return UploadInfo{}, ErrNotFound
	}
	defer lockUpload(d.root.Name() + "\x00" + id)()

	info, err := d.loadUpload(id)
	switch {
	case err != nil:
		return UploadInfo{}, err
	case offset != info.Offset:
		return info, ErrUploadOffset
	case length <= 0:
		return info, ErrUploadShort
	case length > info.Size-info.Offset:
		return info, ErrUploadTooLarge
	}

	part, err := d.root.OpenFile(d.partPath(id), os.O_WRONLY, 0)
	if err != nil {
		return UploadInfo{}, notFoundOr(err)
	}
	written, copyErr := io.Copy(io.NewOffsetWriter(part, offset), io.LimitReader(content, length+1))
	if written > length {
		copyErr = errors.Join(ErrUploadTooLarge, part.Truncate(offset))
		written = 0
	}
	closeErr := part.Close()
	info.Offset = offset + written

	switch {
	case errors.Is(copyErr, ErrUploadTooLarge):
		return info, copyErr
	case errors.Is(copyErr, syscall.EDQUOT) || errors.Is(closeErr, syscall.EDQUOT):
		return info, ErrOverQuota
	case copyErr != nil:
		return info, errors.Join(ErrUploadShort, copyErr)
	case closeErr != nil:
		return info, closeErr
	case written < length:
		return info, ErrUploadShort
	}

	return info, nil
}

func (d *dirFiles) FinishUpload(id string) error {
	if !uploadIDPattern.MatchString(id) {
		return ErrNotFound
	}
	defer lockUpload(d.root.Name() + "\x00" + id)()

	info, err := d.loadUpload(id)
	if err != nil {
		return err
	}
	if info.Offset != info.Size {
		return ErrUploadIncomplete
	}
	if err := d.checkParent(info.Path); err != nil {
		return err
	}
	if err := d.checkWritable(info.Path, info.Overwrite); err != nil {
		return err
	}
	if err := d.syncPart(id); err != nil {
		return err
	}

	if info.Overwrite {
		err = d.root.Rename(d.partPath(id), info.Path)
	} else {
		err = d.root.Link(d.partPath(id), info.Path)
		if err == nil {
			err = d.root.Remove(d.partPath(id))
		}
	}
	if err != nil {
		return d.classify(info.Path, err)
	}
	_ = d.root.Remove(d.metaPath(id))

	return nil
}

func (d *dirFiles) syncPart(id string) error {
	part, err := d.root.OpenFile(d.partPath(id), os.O_WRONLY, 0)
	if err != nil {
		return notFoundOr(err)
	}

	return quotaError(errors.Join(part.Sync(), part.Close()))
}

var uploadFilePattern = regexp.MustCompile(`^([0-9a-f]{32})\.(part|json)$`)

func (d *dirFiles) SweepUploads(cutoff time.Time) (int, error) {
	return d.sweepUploads(cutoff, func(string) bool { return true })
}

func (d *dirFiles) SweepVisitorUploads(cutoff time.Time) (int, error) {
	return d.sweepUploads(cutoff, func(id string) bool {
		var meta uploadMeta
		data, err := d.root.ReadFile(d.metaPath(id))

		return err == nil && json.Unmarshal(data, &meta) == nil && meta.Visitor
	})
}

func (d *dirFiles) sweepUploads(cutoff time.Time, sweepable func(id string) bool) (int, error) {
	dir, err := d.root.Open(UploadsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, d.classify(UploadsDir, err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return 0, err
	}

	newest := map[string]time.Time{}
	for _, entry := range entries {
		match := uploadFilePattern.FindStringSubmatch(entry.Name())
		info, infoErr := entry.Info()
		if match == nil || infoErr != nil {
			continue
		}
		if info.ModTime().After(newest[match[1]]) {
			newest[match[1]] = info.ModTime()
		}
	}

	swept := 0
	for id, touched := range newest {
		if !touched.Before(cutoff) {
			continue
		}
		unlock := lockUpload(d.root.Name() + "\x00" + id)
		if stat, err := d.root.Stat(d.partPath(id)); (err == nil && !stat.ModTime().Before(cutoff)) || !sweepable(id) {
			unlock()
			continue
		}
		partErr := d.root.Remove(d.partPath(id))
		metaErr := d.root.Remove(d.metaPath(id))
		unlock()
		if partErr == nil || metaErr == nil {
			swept++
		}
	}

	return swept, nil
}
