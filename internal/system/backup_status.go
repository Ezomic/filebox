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
	maxBackupStatusBytes = 64 << 10
	backupStatusHeader   = "filebox-backup 1"
)

// The results of a nightly run of filebox-backup.
const (
	// BackupOK is a run that made a backup and removed the old ones.
	BackupOK = "ok"
	// BackupStopped is a run that reached the end of the night's window first; the next night carries on.
	BackupStopped = "stopped"
	// BackupIncomplete is a run that made a backup without some files it could not read.
	BackupIncomplete = "incomplete"
	// BackupFailed is a run that made no backup, for example because the destination could not be reached.
	BackupFailed = "backup_failed"
	// BackupPruneFailed is a run that made a backup but could not remove the old ones.
	BackupPruneFailed = "prune_failed"
	// BackupPrepareFailed is a run that could not list the accounts or prepare the folders to read.
	BackupPrepareFailed = "prepare_failed"
)

var (
	ErrNoBackupStatus  = errors.New("filebox-backup has not reported yet")
	errBadBackupStatus = errors.New("unexpected backup status")

	backupKindPattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	backupHostPattern = regexp.MustCompile(`^[A-Za-z0-9.:_-]{1,253}$`)
)

// BackupStatus is what filebox-backup last wrote for the admin panel: the destination host, times, sizes and results,
// never a file name or a secret.
type BackupStatus struct {
	// Configured is false until "filebox-backup setup" has run.
	Configured bool
	Checked    time.Time
	// Kind is the kind of storage, such as "sftp", "s3", "b2" or "local", and Host its host name, "" when it has none.
	Kind string
	Host string
	// Run is the last nightly run, nil before the first one.
	Run *BackupRun
	// Latest is when the newest complete backup was started, zero until the first one has finished.
	Latest time.Time
	// Size is the size of the repository, nil until it was first measured after a complete backup.
	Size *RepositorySize
	// Check is the last monthly check that finished, nil before the first one.
	Check *RepositoryCheck
}

type BackupRun struct {
	Started  time.Time
	Duration time.Duration
	// Result is one of the Backup* results.
	Result string
	// Added is what the run stored, nil when restic did not say, as in a stopped run.
	Added *uint64
}

type RepositorySize struct {
	Measured time.Time
	Bytes    uint64
}

type RepositoryCheck struct {
	At time.Time
	OK bool
}

func ReadBackupStatus(path string) (BackupStatus, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return BackupStatus{}, ErrNoBackupStatus
	}
	if err != nil {
		return BackupStatus{}, err
	}
	defer file.Close()

	return ParseBackupStatus(io.LimitReader(file, maxBackupStatusBytes))
}

// ParseBackupStatus is strict about the lines it knows and skips the ones it does not, so a newer helper can add some.
func ParseBackupStatus(r io.Reader) (BackupStatus, error) {
	lines := bufio.NewScanner(r)
	if !lines.Scan() || lines.Text() != backupStatusHeader {
		return BackupStatus{}, fmt.Errorf("%w: missing header", errBadBackupStatus)
	}
	var status BackupStatus
	stated := false
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) > 0 && fields[0] == "state" {
			stated = true
		}
		if err := status.parseLine(fields); err != nil {
			return BackupStatus{}, fmt.Errorf("%w: %v", errBadBackupStatus, err)
		}
	}
	if err := lines.Err(); err != nil {
		return BackupStatus{}, err
	}
	if !stated || status.Checked.IsZero() {
		return BackupStatus{}, fmt.Errorf("%w: incomplete", errBadBackupStatus)
	}

	return status, nil
}

func (s *BackupStatus) parseLine(fields []string) error {
	if len(fields) == 0 {
		return nil
	}
	var err error
	switch fields[0] {
	case "checked":
		s.Checked, err = unixField(fields[1:], 1)
	case "state":
		err = s.parseState(fields[1:])
	case "destination":
		err = s.parseDestination(fields[1:])
	case "run":
		s.Run, err = parseBackupRun(fields[1:])
	case "latest":
		s.Latest, err = unixField(fields[1:], 1)
	case "size":
		s.Size, err = parseRepositorySize(fields[1:])
	case "check":
		s.Check, err = parseRepositoryCheck(fields[1:])
	}

	return err
}

func (s *BackupStatus) parseState(fields []string) error {
	if len(fields) != 1 || (fields[0] != "on" && fields[0] != "unconfigured") {
		return errors.New("state")
	}
	s.Configured = fields[0] == "on"

	return nil
}

// destination <kind> <host|->
func (s *BackupStatus) parseDestination(fields []string) error {
	if len(fields) != 2 || !backupKindPattern.MatchString(fields[0]) || (fields[1] != "-" && !backupHostPattern.MatchString(fields[1])) {
		return errors.New("destination")
	}
	s.Kind, s.Host = fields[0], fields[1]
	if s.Host == "-" {
		s.Host = ""
	}

	return nil
}

// run <started> <seconds> <result> <added|->
func parseBackupRun(fields []string) (*BackupRun, error) {
	if len(fields) != 4 {
		return nil, errors.New("run")
	}
	started, err := unixField(fields[:1], 1)
	if err != nil {
		return nil, err
	}
	seconds, ok := parseCanonical(fields[1])
	if !ok || seconds > 1<<32 {
		return nil, errors.New("run duration")
	}
	run := &BackupRun{Started: started, Duration: time.Duration(seconds) * time.Second, Result: fields[2]}
	switch run.Result {
	case BackupOK, BackupStopped, BackupIncomplete, BackupFailed, BackupPruneFailed, BackupPrepareFailed:
	default:
		return nil, errors.New("run result")
	}
	if fields[3] != "-" {
		added, ok := parseCanonical(fields[3])
		if !ok {
			return nil, errors.New("run added")
		}
		run.Added = &added
	}

	return run, nil
}

// size <measured> <bytes>
func parseRepositorySize(fields []string) (*RepositorySize, error) {
	if len(fields) != 2 {
		return nil, errors.New("size")
	}
	measured, err := unixField(fields[:1], 1)
	if err != nil {
		return nil, err
	}
	bytes, ok := parseCanonical(fields[1])
	if !ok {
		return nil, errors.New("size bytes")
	}

	return &RepositorySize{Measured: measured, Bytes: bytes}, nil
}

// check <at> <ok|failed>
func parseRepositoryCheck(fields []string) (*RepositoryCheck, error) {
	if len(fields) != 2 || (fields[1] != "ok" && fields[1] != "failed") {
		return nil, errors.New("check")
	}
	at, err := unixField(fields[:1], 1)
	if err != nil {
		return nil, err
	}

	return &RepositoryCheck{At: at, OK: fields[1] == "ok"}, nil
}
