package system

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

const (
	healthHeader   = "filebox-health 1"
	maxHealthBytes = 4 << 20
	maxHealthLine  = 1 << 20
	// HealthMaxAge is how old the report may be. filebox-health.timer writes it every 10 minutes, so an older one
	// means it stopped, and its answers no longer count.
	HealthMaxAge = time.Hour
)

var (
	ErrNoHealthStatus = errors.New("filebox-health has not reported yet")
	ErrHealthStale    = errors.New("the last disk health report is more than an hour old")
	errBadHealth      = errors.New("unexpected disk health report")
)

// HealthStatus is what filebox-health last wrote: SMART for every disk and the state of the btrfs data disk.
type HealthStatus struct {
	Checked time.Time
	// Disks is nil when smartctl could not run at all.
	Disks []DiskHealth
	Btrfs BtrfsHealth
}

func ReadHealthStatus(path string) (HealthStatus, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return HealthStatus{}, ErrNoHealthStatus
	}
	if err != nil {
		return HealthStatus{}, err
	}
	defer file.Close()

	return ParseHealthStatus(io.LimitReader(file, maxHealthBytes))
}

// ParseHealthStatus is strict about the lines it knows and skips the ones it does not, so a newer helper can add some.
// A "smart" line is one smartctl JSON document; one that does not parse is skipped, like a disk that did not answer.
func ParseHealthStatus(r io.Reader) (HealthStatus, error) {
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 0, 64<<10), maxHealthLine)
	if !lines.Scan() || lines.Text() != healthHeader {
		return HealthStatus{}, fmt.Errorf("%w: missing header", errBadHealth)
	}
	status := HealthStatus{Disks: []DiskHealth{}}
	devicesUnknown := false
	for lines.Scan() {
		keyword, rest, _ := bytes.Cut(lines.Bytes(), []byte(" "))
		fields := strings.Fields(string(rest))
		var err error
		switch string(keyword) {
		case "checked":
			status.Checked, err = unixField(fields, 1)
		case "smart":
			err = status.parseSmart(rest)
		case "state":
			err = status.Btrfs.parseState(fields)
		case "device":
			err = status.Btrfs.parseDevice(fields)
		case "devices":
			devicesUnknown = len(fields) == 1 && fields[0] == "unknown"
			if !devicesUnknown {
				err = errors.New("devices")
			}
		case "scrub":
			status.Btrfs.Scrub, err = parseScrub(fields)
		}
		if err != nil {
			return HealthStatus{}, fmt.Errorf("%w: %v", errBadHealth, err)
		}
	}
	if err := lines.Err(); err != nil {
		return HealthStatus{}, err
	}
	if status.Checked.IsZero() || status.Btrfs.State == "" {
		return HealthStatus{}, fmt.Errorf("%w: incomplete", errBadHealth)
	}
	if devicesUnknown || status.Btrfs.State != BtrfsOn {
		status.Btrfs.Devices = nil
	}

	return status, nil
}

func (s *HealthStatus) parseSmart(rest []byte) error {
	if string(rest) == "unavailable" {
		s.Disks = nil
		return nil
	}
	if disk, ok := parseSmartReport(rest); ok && s.Disks != nil {
		s.Disks = append(s.Disks, disk)
	}

	return nil
}

// Health hands the last report of filebox-health to the dashboard and the alerts. The panel cannot read the disks
// itself: its sandbox hides them.
type Health struct {
	read func() (HealthStatus, error)
	now  func() time.Time
}

func (h *Health) current() (HealthStatus, error) {
	status, err := h.read()
	if err != nil {
		return HealthStatus{}, err
	}
	if h.now().Sub(status.Checked) > HealthMaxAge {
		return HealthStatus{}, ErrHealthStale
	}

	return status, nil
}

// Disks makes Health the Smart of the dashboard.
func (h *Health) Disks() ([]DiskHealth, error) {
	status, err := h.current()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSmartUnavailable, err)
	}
	if status.Disks == nil {
		return nil, fmt.Errorf("%w: smartctl did not run", ErrSmartUnavailable)
	}

	return status.Disks, nil
}

func (h *Health) Btrfs() (BtrfsHealth, error) {
	status, err := h.current()
	if err != nil {
		return BtrfsHealth{}, fmt.Errorf("%w: %v", ErrBtrfsHealthUnavailable, err)
	}

	return status.Btrfs, nil
}
