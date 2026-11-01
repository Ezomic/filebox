package system

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"sync"
	"time"
)

const (
	// customDatesName sits next to the thumbnail index. Like it, it is not a valid thumbnail name, so no route can serve it.
	customDatesName    = "dates.json"
	maxCustomDatesFile = 1 << 20
	// A file with this many entries is pruned on the next write, which keeps it far below maxCustomDatesFile.
	maxCustomDates  = 5000
	customDayLayout = "2006-01-02"
)

var ErrInvalidDate = errors.New("invalid date")

var customDayPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

var earliestCustomDay = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)

// customDatesMu serializes the read-modify-write of every dates file. Files are opened per request, so it can not live on them.
var customDatesMu sync.Mutex

type customDatesFile struct {
	Dates map[string]string `json:"dates"`
}

func validPhotoKind(kind string) bool {
	return kind == "" || kind == PhotoImage || kind == PhotoVideo
}

func filterKind(photos []Photo, kind string) []Photo {
	if kind == "" {
		return photos
	}
	kept := photos[:0]
	for _, photo := range photos {
		if photo.Kind == kind {
			kept = append(kept, photo)
		}
	}

	return kept
}

// customDay reduces when to the UTC day it falls on. A photo can not be from the future, so tomorrow (UTC) is the latest day taken.
func customDay(when time.Time) (time.Time, error) {
	if when.IsZero() {
		return time.Time{}, ErrInvalidDate
	}
	day := when.UTC().Truncate(24 * time.Hour)
	if day.Before(earliestCustomDay) || day.After(time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)) {
		return time.Time{}, ErrInvalidDate
	}

	return day, nil
}

// ParsePhotoDay accepts exactly YYYY-MM-DD with ASCII digits, and only a day customDay accepts.
func ParsePhotoDay(value string) (time.Time, error) {
	if !customDayPattern.MatchString(value) {
		return time.Time{}, ErrInvalidDate
	}
	parsed, err := time.Parse(customDayLayout, value)
	if err != nil {
		return time.Time{}, ErrInvalidDate
	}

	return customDay(parsed)
}

// decodeCustomDates keeps only entries that hold a valid day. A file that is not what we wrote counts as empty.
func decodeCustomDates(source io.Reader) map[string]string {
	var file customDatesFile
	if err := json.NewDecoder(io.LimitReader(source, maxCustomDatesFile)).Decode(&file); err != nil {
		return map[string]string{}
	}
	dates := make(map[string]string, len(file.Dates))
	for key, value := range file.Dates {
		if _, err := ParsePhotoDay(value); err == nil {
			dates[key] = value
		}
	}

	return dates
}

func encodeCustomDates(dates map[string]string) ([]byte, error) {
	return json.Marshal(customDatesFile{Dates: dates})
}

func applyCustomDates(photos []Photo, dates map[string]string) {
	for i := range photos {
		if day, err := ParsePhotoDay(dates[photos[i].Key]); err == nil {
			photos[i].Date = day
			photos[i].Custom = true
		}
	}
}

// pruneCustomDates drops the dates of photos that were renamed, moved, replaced or deleted and reports whether it dropped any.
func pruneCustomDates(dates map[string]string, live map[string]bool) bool {
	pruned := false
	for key := range dates {
		if !live[key] {
			delete(dates, key)
			pruned = true
		}
	}

	return pruned
}

func liveKeys(photos []Photo) map[string]bool {
	live := make(map[string]bool, len(photos))
	for _, photo := range photos {
		live[photo.Key] = true
	}

	return live
}
