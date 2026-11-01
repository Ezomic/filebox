package system

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"time"
)

// photoDateBudget bounds how long one listing spends reading dates from files it has not seen before.
// A very large library is then finished over the next requests instead of timing out the first.
var photoDateBudget = 8 * time.Second

type thumbIndex struct {
	Dates map[string]int64 `json:"dates"`
}

func (d *dirFiles) ListPhotos(after string, limit int) (PhotoPage, error) {
	if after != "" {
		if _, _, err := parsePhotoCursor(after); err != nil {
			return PhotoPage{}, err
		}
	}
	photos, err := d.collectPhotos()
	if err != nil {
		return PhotoPage{}, err
	}
	d.resolveDates(photos)

	return pagePhotos(photos, after, limit)
}

func (d *dirFiles) collectPhotos() ([]Photo, error) {
	var photos []Photo
	err := d.walkPhotos("", 0, &photos)

	return photos, err
}

func (d *dirFiles) walkPhotos(dir string, depth int, photos *[]Photo) error {
	dirFile, err := d.root.Open(rootRelative(dir))
	if err != nil {
		if dir == "" {
			return d.classify(dir, err)
		}

		return nil
	}
	defer dirFile.Close()
	entries, err := dirFile.ReadDir(-1)
	if err != nil && dir == "" {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if hiddenName(name) || entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		full := path.Join(dir, name)
		switch {
		case entry.IsDir():
			if depth < maxPhotoDepth {
				if err := d.walkPhotos(full, depth+1, photos); err != nil {
					return err
				}
			}
		case entry.Type().IsRegular():
			if photo, ok := d.photoFromEntry(full, entry); ok && len(*photos) < maxPhotoEntries {
				*photos = append(*photos, photo)
			}
		}
	}

	return nil
}

func (d *dirFiles) photoFromEntry(full string, entry fs.DirEntry) (Photo, bool) {
	kind, _, ok := mediaKind(entry.Name())
	if !ok {
		return Photo{}, false
	}
	if _, err := CleanPath(full); err != nil {
		return Photo{}, false
	}
	info, err := entry.Info()
	if err != nil || info.Size() == 0 {
		return Photo{}, false
	}

	return d.newPhoto(full, kind, info), true
}

func (d *dirFiles) newPhoto(full, kind string, info fs.FileInfo) Photo {
	return Photo{
		Path: full, Kind: kind, Size: info.Size(), Date: info.ModTime().UTC(), Modified: info.ModTime(),
		Key: photoKey(full, info.Size(), info.ModTime()), Scope: d.root.Name(),
	}
}

func (d *dirFiles) loadThumbIndex() map[string]int64 {
	file, err := d.root.Open(ThumbsDir + "/" + thumbIndexName)
	if err != nil {
		return map[string]int64{}
	}
	defer file.Close()

	var index thumbIndex
	if err := json.NewDecoder(io.LimitReader(file, maxThumbIndex)).Decode(&index); err != nil || index.Dates == nil {
		return map[string]int64{}
	}

	return index.Dates
}

func (d *dirFiles) resolveDates(photos []Photo) {
	known := d.loadThumbIndex()
	current := make(map[string]int64, len(photos))
	deadline := time.Now().Add(photoDateBudget)
	learned := false

	for i := range photos {
		photo := &photos[i]
		if unix, ok := known[photo.Key]; ok {
			photo.Date = time.Unix(unix, 0).UTC()
			current[photo.Key] = unix

			continue
		}
		if time.Now().After(deadline) {
			continue
		}
		if date, ok := d.readExifDate(*photo); ok {
			photo.Date = date
		}
		current[photo.Key] = photo.Date.Unix()
		learned = true
	}

	if learned || len(current) != len(known) {
		d.storeThumbIndex(current)
	}
}

func (d *dirFiles) readExifDate(photo Photo) (time.Time, bool) {
	file, err := d.root.Open(photo.Path)
	if err != nil {
		return time.Time{}, false
	}
	defer file.Close()

	return exifDate(file, photo.Size, mediaExt(photo.Path))
}

func (d *dirFiles) storeThumbIndex(dates map[string]int64) {
	data, err := json.Marshal(thumbIndex{Dates: dates})
	if err != nil {
		return
	}
	_ = d.writeThumbFile(thumbIndexName, data)
}

func (d *dirFiles) OpenPhoto(name string) (File, Photo, error) {
	name, err := cleanNonRoot(name)
	if err != nil {
		return nil, Photo{}, err
	}
	kind, _, ok := mediaKind(name)
	if !ok {
		return nil, Photo{}, ErrNotMedia
	}
	for parent := name; parent != ""; parent = parentOf(parent) {
		info, err := d.root.Lstat(parent)
		if err != nil {
			return nil, Photo{}, d.classify(name, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, Photo{}, ErrNotFound
		}
		if parent == name && !info.Mode().IsRegular() {
			return nil, Photo{}, ErrNotMedia
		}
	}

	file, err := d.root.Open(name)
	if err != nil {
		return nil, Photo{}, d.classify(name, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return nil, Photo{}, d.classify(name, err)
	}

	return file, d.newPhoto(name, kind, info), nil
}

func (d *dirFiles) ReadThumb(name string) (File, error) {
	if _, ok := thumbKeyOf(name); !ok {
		return nil, ErrInvalidPath
	}
	file, err := d.root.Open(ThumbsDir + "/" + name)
	if err != nil {
		return nil, d.classify(ThumbsDir, err)
	}

	return file, nil
}

func (d *dirFiles) WriteThumb(name string, data []byte) error {
	if _, ok := thumbKeyOf(name); !ok {
		return ErrInvalidPath
	}

	return d.writeThumbFile(name, data)
}

// writeThumbFile writes through a temp file so a reader never sees half a thumbnail.
func (d *dirFiles) writeThumbFile(name string, data []byte) error {
	if err := d.root.Mkdir(ThumbsDir, 0o775); err != nil && !errors.Is(err, fs.ErrExist) {
		return d.classify(ThumbsDir, err)
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	temp := ThumbsDir + "/" + thumbTempPrefix + hex.EncodeToString(suffix)
	file, err := d.root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
	if err != nil {
		return d.classify(ThumbsDir, err)
	}
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = d.root.Remove(temp)

		return err
	}
	if err := d.root.Rename(temp, ThumbsDir+"/"+name); err != nil {
		_ = d.root.Remove(temp)

		return d.classify(ThumbsDir, err)
	}

	return nil
}

func (d *dirFiles) SweepThumbs(now time.Time) (int, error) {
	dir, err := d.root.Open(ThumbsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, d.classify(ThumbsDir, err)
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return 0, err
	}
	photos, err := d.collectPhotos()
	if err != nil {
		return 0, err
	}
	live := make(map[string]bool, len(photos))
	for _, photo := range photos {
		live[photo.Key] = true
	}

	removed := 0
	var errs []error
	for _, entry := range entries {
		if !orphanThumb(entry, live, now) {
			continue
		}
		switch err := d.root.Remove(ThumbsDir + "/" + entry.Name()); {
		case err == nil:
			removed++
		case !errors.Is(err, fs.ErrNotExist):
			errs = append(errs, err)
		}
	}

	return removed, errors.Join(errs...)
}

func orphanThumb(entry fs.DirEntry, live map[string]bool, now time.Time) bool {
	if entry.IsDir() {
		return false
	}
	if key, ok := thumbKeyOf(entry.Name()); ok {
		return !live[key]
	}
	if len(entry.Name()) > len(thumbTempPrefix) && entry.Name()[:len(thumbTempPrefix)] == thumbTempPrefix {
		info, err := entry.Info()

		return err == nil && now.Sub(info.ModTime()) > staleTempAfter
	}

	return false
}
