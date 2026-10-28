package system

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxLogLines     = 500
	filterWindow    = 5000
	maxLogLine      = 2000
	maxJournalBytes = 4 << 20
	journalTimeout  = 10 * time.Second
	journalctlPath  = "/usr/bin/journalctl"
)

var (
	ErrInvalidLogRequest = errors.New("invalid log request")
	ErrLogsUnavailable   = errors.New("logs unavailable")
)

type LogSource string

const (
	LogSSHD  LogSource = "sshd"
	LogPanel LogSource = "panel"
)

// The only units this package will ever ask journalctl about.
var logUnits = map[LogSource]string{
	LogSSHD:  "ssh",
	LogPanel: "filebox-panel",
}

type Logs interface {
	// Read returns the newest lines that contain filter, oldest first.
	Read(source LogSource, lines int, filter string) ([]string, error)
}

func ParseLogSource(name string) (LogSource, bool) {
	source := LogSource(name)
	_, ok := logUnits[source]

	return source, ok
}

func validateLogRequest(source LogSource, lines int) error {
	if _, ok := logUnits[source]; !ok || lines < 1 || lines > MaxLogLines {
		return ErrInvalidLogRequest
	}

	return nil
}

func journalArgs(source LogSource, lines int) []string {
	return []string{"-u", logUnits[source], "-n", fmt.Sprint(lines), "--no-pager", "-o", "short-iso", "-r"}
}

// The filter is a plain substring on purpose: it never reaches a command line or a regexp.
func filterTail(lines []string, filter string, limit int) []string {
	needle := strings.ToLower(filter)
	matched := []string{}
	for i := len(lines) - 1; i >= 0 && len(matched) < limit; i-- {
		if needle == "" || strings.Contains(strings.ToLower(lines[i]), needle) {
			matched = append(matched, shorten(lines[i]))
		}
	}
	for left, right := 0, len(matched)-1; left < right; left, right = left+1, right-1 {
		matched[left], matched[right] = matched[right], matched[left]
	}

	return matched
}

func shorten(line string) string {
	runes := []rune(line)
	if len(runes) <= maxLogLine {
		return line
	}

	return string(runes[:maxLogLine]) + "…"
}
