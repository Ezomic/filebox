package system

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodBackupStatus = `filebox-backup 1
checked 1794000000
state on
destination sftp u123456.your-storagebox.de
run 1793926800 5400 ok 734003200
latest 1793926805
size 1793932300 412316860416
check 1791334800 failed
something new 1 2 3
`

func TestBackupStatusIsReadLineByLine(t *testing.T) {
	status, err := ParseBackupStatus(strings.NewReader(goodBackupStatus))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Configured || status.Kind != "sftp" || status.Host != "u123456.your-storagebox.de" || !status.Checked.Equal(time.Unix(1794000000, 0)) {
		t.Fatalf("got %+v", status)
	}
	if run := status.Run; run == nil || !run.Started.Equal(time.Unix(1793926800, 0)) || run.Duration != 90*time.Minute ||
		run.Result != BackupOK || run.Added == nil || *run.Added != 734003200 {
		t.Fatalf("run %+v", status.Run)
	}
	if !status.Latest.Equal(time.Unix(1793926805, 0)) {
		t.Fatalf("latest %v", status.Latest)
	}
	if size := status.Size; size == nil || size.Bytes != 412316860416 || !size.Measured.Equal(time.Unix(1793932300, 0)) {
		t.Fatalf("size %+v", status.Size)
	}
	if check := status.Check; check == nil || check.OK || !check.At.Equal(time.Unix(1791334800, 0)) {
		t.Fatalf("check %+v", status.Check)
	}
}

func TestBackupStatusBeforeSetupAndTheFirstBackup(t *testing.T) {
	status, err := ParseBackupStatus(strings.NewReader("filebox-backup 1\nchecked 1794000000\nstate unconfigured\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured || status.Run != nil || !status.Latest.IsZero() || status.Size != nil || status.Check != nil {
		t.Fatalf("got %+v", status)
	}

	stopped := "filebox-backup 1\nchecked 1794000000\nstate on\ndestination b2 -\nrun 1793926800 21600 stopped -\n"
	status, err = ParseBackupStatus(strings.NewReader(stopped))
	if err != nil {
		t.Fatal(err)
	}
	if status.Kind != "b2" || status.Host != "" || status.Run.Result != BackupStopped || status.Run.Added != nil || !status.Latest.IsZero() {
		t.Fatalf("got %+v %+v", status, status.Run)
	}
}

func TestBackupStatusRefusesWhatItDoesNotExpect(t *testing.T) {
	head := "filebox-backup 1\nchecked 1794000000\nstate on\n"
	for name, text := range map[string]string{
		"no header":           "checked 1794000000\nstate on\n",
		"newer format":        "filebox-backup 2\nchecked 1794000000\nstate on\n",
		"no state":            "filebox-backup 1\nchecked 1794000000\n",
		"no checked":          "filebox-backup 1\nstate on\n",
		"unknown state":       "filebox-backup 1\nchecked 1794000000\nstate paused\n",
		"user in destination": head + "destination sftp u123456@u123456.your-storagebox.de\n",
		"path in destination": head + "destination sftp host/filebox\n",
		"no host":             head + "destination sftp\n",
		"bad kind":            head + "destination S3! host\n",
		"short run":           head + "run 1793926800 5400 ok\n",
		"unknown result":      head + "run 1793926800 5400 fine -\n",
		"padded duration":     head + "run 1793926800 05400 ok -\n",
		"bad added":           head + "run 1793926800 5400 ok lots\n",
		"bad latest":          head + "latest yesterday\n",
		"bad size":            head + "size 1793932300 big\n",
		"bad check":           head + "check 1791334800 maybe\n",
		"short check":         head + "check ok\n",
	} {
		if _, err := ParseBackupStatus(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted %q", name, text)
		}
	}
}

func TestBackupStatusFileThatIsNotThereYet(t *testing.T) {
	if _, err := ReadBackupStatus(filepath.Join(t.TempDir(), "status")); !errors.Is(err, ErrNoBackupStatus) {
		t.Fatalf("got %v", err)
	}
	path := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(path, []byte(goodBackupStatus), 0o640); err != nil {
		t.Fatal(err)
	}
	if status, err := ReadBackupStatus(path); err != nil || status.Run == nil {
		t.Fatalf("got %+v %v", status, err)
	}
}
