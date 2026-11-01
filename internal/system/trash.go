package system

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// TrashMaxAge is how long a deleted item stays restorable.
const TrashMaxAge = 30 * 24 * time.Hour

const maxRestoreAttempts = 1000

var trashEntryPattern = regexp.MustCompile(`^([0-9a-f]{32})(\.json)?$`)

type TrashItem struct {
	ID        string
	Path      string
	DeletedAt time.Time
	Size      int64
	IsDir     bool
}

func (t TrashItem) Name() string { return baseOf(t.Path) }

func (t TrashItem) Folder() string { return parentOf(t.Path) }

// restoredName builds the name used when the original is taken: "a.txt" becomes "a (restored).txt",
// then "a (restored 2).txt". The stem is cut so the result still fits the 255 byte name limit.
func restoredName(name string, isDir bool, attempt int) string {
	suffix := " (restored)"
	if attempt > 1 {
		suffix = fmt.Sprintf(" (restored %d)", attempt)
	}
	stem, ext := name, ""
	if !isDir {
		if found := path.Ext(name); found != name {
			stem, ext = name[:len(name)-len(found)], found
		}
	}
	room := maxSegmentBytes - len(suffix) - len(ext)
	if room < 1 {
		stem, ext = name, ""
		room = maxSegmentBytes - len(suffix)
	}
	for len(stem) > room {
		_, size := utf8.DecodeLastRuneInString(stem)
		stem = stem[:len(stem)-size]
	}

	return stem + suffix + ext
}

func restoreCandidate(target string, isDir bool, attempt int) string {
	if attempt == 0 {
		return target
	}
	name := restoredName(baseOf(target), isDir, attempt)
	if parent := parentOf(target); parent != "" {
		return parent + "/" + name
	}

	return name
}

func parentChain(dir string) []string {
	if dir == "" {
		return nil
	}
	parts := strings.Split(dir, "/")
	chain := make([]string, len(parts))
	for i := range parts {
		chain[i] = strings.Join(parts[:i+1], "/")
	}

	return chain
}
