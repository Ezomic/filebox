package system

import (
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxPathBytes    = 4096
	maxSegmentBytes = 255
)

var (
	ErrInvalidPath = errors.New("invalid path")
	ErrNotFound    = errors.New("not found")
	ErrExists      = errors.New("already exists")
	ErrNotDir      = errors.New("not a folder")
	ErrIsDir       = errors.New("is a folder")
	ErrNotEmpty    = errors.New("folder is not empty")
	ErrIntoSelf    = errors.New("cannot move a folder into itself")

	ErrUploadOffset     = errors.New("upload offset does not match")
	ErrUploadTooLarge   = errors.New("chunk goes past the declared size")
	ErrUploadShort      = errors.New("chunk ended before its declared length")
	ErrUploadIncomplete = errors.New("upload is not complete")
)

// UploadsDir holds partial uploads at the top of a Files root. It is invisible to every file operation.
const UploadsDir = ".uploads"

// TrashDir holds deleted items at the top of a Files root. Like UploadsDir it is invisible to every file operation.
const TrashDir = ".trash"

type FileInfo struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"isDir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
	Symlink  bool      `json:"-"`
}

// UploadInfo describes a partial upload. Offset is how many bytes the server holds.
type UploadInfo struct {
	ID        string
	Path      string
	Size      int64
	Offset    int64
	Overwrite bool
}

type File interface {
	io.ReadSeekCloser
}

// Every path is relative to the data root and slash separated. The empty path is the root.
type Files interface {
	List(dir string) ([]FileInfo, error)
	Open(path string) (File, FileInfo, error)
	Write(path string, content io.Reader, overwrite bool) error
	Rename(from, to string) error
	Remove(path string) error
	Mkdir(path string) error
	Stat(path string) (FileInfo, error)

	CreateUpload(path string, size int64, overwrite bool) (UploadInfo, error)
	UploadStatus(id string) (UploadInfo, error)
	// AppendUpload adds exactly length bytes at offset. On ErrUploadOffset, ErrUploadTooLarge and
	// ErrUploadShort the returned info carries the offset the server now holds.
	AppendUpload(id string, offset, length int64, content io.Reader) (UploadInfo, error)
	FinishUpload(id string) error
	// SweepUploads deletes partial uploads that were last written before cutoff and reports how many.
	SweepUploads(cutoff time.Time) (int, error)

	// Trash moves a file or a whole folder into the hidden trash as one item. Nothing is copied or followed.
	Trash(path string) (TrashItem, error)
	ListTrash() ([]TrashItem, error)
	// RestoreTrash puts an item back where it was and returns the path it landed on. It never overwrites:
	// a clash gets a "(restored)" name instead.
	RestoreTrash(id string) (string, error)
	PurgeTrash(id string) error
	EmptyTrash() (int, error)
	// SweepTrash deletes items that were trashed before cutoff and reports how many.
	SweepTrash(cutoff time.Time) (int, error)
}

// CleanPath is a cheap first filter. The real confinement is os.Root, so this only
// has to keep odd input from reaching the filesystem at all.
func CleanPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if len(path) > maxPathBytes || !utf8.ValidString(path) {
		return "", ErrInvalidPath
	}
	for _, segment := range strings.Split(path, "/") {
		if !validSegment(segment) {
			return "", ErrInvalidPath
		}
	}

	return path, nil
}

// Names ending in a dot or space are refused because Windows silently strips them,
// which would make two different names address the same file.
func validSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." || len(segment) > maxSegmentBytes || strings.EqualFold(segment, UploadsDir) || strings.EqualFold(segment, TrashDir) {
		return false
	}
	if last := segment[len(segment)-1]; last == '.' || last == ' ' {
		return false
	}
	for _, r := range segment {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}

	return true
}

func cleanNonRoot(path string) (string, error) {
	clean, err := CleanPath(path)
	if err != nil || clean == "" {
		return "", ErrInvalidPath
	}

	return clean, nil
}

func parentOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}

	return ""
}

func baseOf(path string) string {
	return path[strings.LastIndexByte(path, '/')+1:]
}

func isInside(path, dir string) bool {
	return strings.HasPrefix(path, dir+"/")
}
