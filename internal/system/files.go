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
)

type FileInfo struct {
	Name     string    `json:"name"`
	IsDir    bool      `json:"isDir"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
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
	if segment == "" || segment == "." || segment == ".." || len(segment) > maxSegmentBytes {
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
