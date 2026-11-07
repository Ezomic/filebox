package system

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	maxSnapshotStatusBytes = 1 << 20
	snapshotStatusHeader   = "filebox-snapshot 1"
)

var (
	ErrNoSnapshotStatus  = errors.New("filebox-snapshot has not reported yet")
	errBadSnapshotStatus = errors.New("unexpected snapshot status")

	deletedFolderPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}\.deleted-[0-9]{14}$`)
)

// SnapshotStatus is what filebox-snapshot last wrote for the admin panel: names, counts, dates and sizes, never file names.
type SnapshotStatus struct {
	// Enabled is false when the box cannot take snapshots, and Reason says why: "not_btrfs" or "no_btrfs_progs".
	Enabled bool
	Reason  string
	Checked time.Time
	// Daily is the last nightly run, zero before the first one.
	Daily   time.Time
	DailyOK bool
	// LowSpace is the last time a 95% full disk made it delete earlier versions, nil if that never happened.
	LowSpace *SnapshotLowSpace
	Folders  []SnapshotFolder
}

type SnapshotLowSpace struct {
	At        time.Time
	Deleted   int
	StillFull bool
}

// SnapshotFolder is one user's files, the family folder (FamilyDir), or the earlier versions of a deleted account
// ("name.deleted-<time>").
type SnapshotFolder struct {
	Name string
	// Protection is "yes" for a subvolume that gets snapshots, "no" for a plain folder, "gone" for a deleted account, and
	// "unchecked" when the helper could not list the accounts.
	Protection string
	Count      int
	// Oldest and Newest are zero when there are no snapshots.
	Oldest time.Time
	Newest time.Time
	// Held is nil when the box has no btrfs quotas to measure it with.
	Held *uint64
	// Error is "" or the code of the last failure: "snapshot_failed" or "prune_failed".
	Error string
}

func ReadSnapshotStatus(path string) (SnapshotStatus, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return SnapshotStatus{}, ErrNoSnapshotStatus
	}
	if err != nil {
		return SnapshotStatus{}, err
	}
	defer file.Close()

	return ParseSnapshotStatus(io.LimitReader(file, maxSnapshotStatusBytes))
}

// ParseSnapshotStatus is strict about the lines it knows and skips the ones it does not, so a newer helper can add some.
func ParseSnapshotStatus(r io.Reader) (SnapshotStatus, error) {
	lines := bufio.NewScanner(r)
	if !lines.Scan() || lines.Text() != snapshotStatusHeader {
		return SnapshotStatus{}, fmt.Errorf("%w: missing header", errBadSnapshotStatus)
	}
	var status SnapshotStatus
	for lines.Scan() {
		if err := status.parseLine(strings.Fields(lines.Text())); err != nil {
			return SnapshotStatus{}, fmt.Errorf("%w: %v", errBadSnapshotStatus, err)
		}
	}
	if err := lines.Err(); err != nil {
		return SnapshotStatus{}, err
	}

	return status, nil
}

func (s *SnapshotStatus) parseLine(fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	var err error
	switch fields[0] {
	case "state":
		err = s.parseState(fields[1:])
	case "checked":
		s.Checked, err = unixField(fields[1:], 1)
	case "daily":
		err = s.parseDaily(fields[1:])
	case "lowspace":
		err = s.parseLowSpace(fields[1:])
	case "scope":
		var folder SnapshotFolder
		if folder, err = parseFolder(fields[1:]); err == nil {
			s.Folders = append(s.Folders, folder)
		}
	}

	return err
}

func (s *SnapshotStatus) parseState(fields []string) error {
	switch {
	case len(fields) == 1 && fields[0] == "on":
		s.Enabled, s.Reason = true, ""
	case len(fields) == 2 && fields[0] == "off" && (fields[1] == "not_btrfs" || fields[1] == "no_btrfs_progs"):
		s.Enabled, s.Reason = false, fields[1]
	default:
		return errors.New("state")
	}

	return nil
}

func (s *SnapshotStatus) parseDaily(fields []string) error {
	at, err := unixField(fields, 2)
	if err != nil || (fields[1] != "ok" && fields[1] != "failed") {
		return errors.New("daily")
	}
	s.Daily, s.DailyOK = at, fields[1] == "ok"

	return nil
}

func (s *SnapshotStatus) parseLowSpace(fields []string) error {
	at, err := unixField(fields, 3)
	if err != nil {
		return err
	}
	deleted, ok := parseCanonical(fields[1])
	if !ok || deleted > 1<<20 || (fields[2] != "yes" && fields[2] != "no") {
		return errors.New("lowspace")
	}
	s.LowSpace = &SnapshotLowSpace{At: at, Deleted: int(deleted), StillFull: fields[2] == "yes"}

	return nil
}

func unixField(fields []string, want int) (time.Time, error) {
	if len(fields) != want {
		return time.Time{}, errors.New("field count")
	}
	seconds, ok := parseCanonical(fields[0])
	if !ok || seconds > 1<<40 {
		return time.Time{}, errors.New("time")
	}

	return time.Unix(int64(seconds), 0), nil
}

// scope <name> <yes|no|gone> <count> <oldest|-> <newest|-> <held|-> <error|->
func parseFolder(fields []string) (SnapshotFolder, error) {
	if len(fields) != 7 || !snapshotFolderName(fields[0]) {
		return SnapshotFolder{}, errors.New("scope name")
	}
	folder := SnapshotFolder{Name: fields[0], Protection: fields[1]}
	switch folder.Protection {
	case "yes", "no", "gone", "unchecked":
	default:
		return SnapshotFolder{}, errors.New("scope protection")
	}
	count, ok := parseCanonical(fields[2])
	if !ok || count > 1<<20 {
		return SnapshotFolder{}, errors.New("scope count")
	}
	folder.Count = int(count)
	var err error
	if folder.Oldest, err = optionalVersionTime(fields[3]); err != nil {
		return SnapshotFolder{}, err
	}
	if folder.Newest, err = optionalVersionTime(fields[4]); err != nil {
		return SnapshotFolder{}, err
	}
	if fields[5] != "-" {
		held, ok := parseCanonical(fields[5])
		if !ok {
			return SnapshotFolder{}, errors.New("scope held")
		}
		folder.Held = &held
	}
	switch fields[6] {
	case "-":
	case "snapshot_failed", "prune_failed":
		folder.Error = fields[6]
	default:
		return SnapshotFolder{}, errors.New("scope error")
	}

	return folder, nil
}

// Deleted reports whether these are the earlier versions of a deleted account, moved aside under a name no account can have.
func (f SnapshotFolder) Deleted() bool {
	return deletedFolderPattern.MatchString(f.Name)
}

func snapshotFolderName(name string) bool {
	return name == FamilyDir || ValidUsername(name) || deletedFolderPattern.MatchString(name)
}

func optionalVersionTime(field string) (time.Time, error) {
	if field == "-" {
		return time.Time{}, nil
	}
	taken, ok := versionTime(field)
	if !ok {
		return time.Time{}, errors.New("scope date")
	}

	return taken, nil
}
