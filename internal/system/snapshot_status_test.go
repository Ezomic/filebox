package system

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodSnapshotStatus = `filebox-snapshot 1
state on
checked 1794000000
daily 1793925000 ok
lowspace 1793990000 3 no
scope _family yes 17 2026-10-04T0030 2026-11-07T0030 7340032 -
scope amy yes 23 2026-06-30T0030 2026-11-07T0030 - snapshot_failed
scope bob no 0 - - 0 -
scope cleo.deleted-20261101093000 gone 4 2026-10-20T0030 2026-10-31T0030 1024 -
scope dave unchecked 2 2026-11-06T0030 2026-11-07T0030 - -
something new 1 2 3
`

func TestSnapshotStatusIsReadLineByLine(t *testing.T) {
	status, err := ParseSnapshotStatus(strings.NewReader(goodSnapshotStatus))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Enabled || status.Reason != "" || !status.Checked.Equal(time.Unix(1794000000, 0)) || !status.DailyOK {
		t.Fatalf("got %+v", status)
	}
	if low := status.LowSpace; low == nil || low.Deleted != 3 || low.StillFull || !low.At.Equal(time.Unix(1793990000, 0)) {
		t.Fatalf("lowspace %+v", low)
	}
	if len(status.Folders) != 5 || status.Folders[4].Protection != "unchecked" {
		t.Fatalf("folders %+v", status.Folders)
	}
	family, amy, bob, cleo := status.Folders[0], status.Folders[1], status.Folders[2], status.Folders[3]
	if family.Name != FamilyDir || family.Count != 17 || *family.Held != 7340032 || family.Error != "" ||
		!family.Oldest.Equal(time.Date(2026, 10, 4, 0, 30, 0, 0, time.Local)) {
		t.Fatalf("family %+v", family)
	}
	if amy.Held != nil || amy.Error != "snapshot_failed" || amy.Protection != "yes" {
		t.Fatalf("amy %+v", amy)
	}
	if bob.Protection != "no" || bob.Count != 0 || !bob.Oldest.IsZero() || !bob.Newest.IsZero() {
		t.Fatalf("bob %+v", bob)
	}
	if cleo.Protection != "gone" || cleo.Count != 4 {
		t.Fatalf("cleo %+v", cleo)
	}
}

func TestSnapshotStatusOffSaysWhy(t *testing.T) {
	status, err := ParseSnapshotStatus(strings.NewReader("filebox-snapshot 1\nstate off not_btrfs\nchecked 1794000000\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.Enabled || status.Reason != "not_btrfs" || len(status.Folders) != 0 || !status.Daily.IsZero() || status.LowSpace != nil {
		t.Fatalf("got %+v", status)
	}
}

func TestSnapshotStatusRefusesWhatItDoesNotExpect(t *testing.T) {
	head := "filebox-snapshot 1\n"
	for name, text := range map[string]string{
		"no header":          "state on\n",
		"newer format":       "filebox-snapshot 2\nstate on\n",
		"unknown state":      head + "state maybe\n",
		"unknown reason":     head + "state off because\n",
		"bad time":           head + "checked yesterday\n",
		"negative time":      head + "checked -5\n",
		"padded time":        head + "checked 0017\n",
		"bad daily":          head + "daily 1 fine\n",
		"bad lowspace":       head + "lowspace 1 2 maybe\n",
		"short scope":        head + "scope amy yes 1\n",
		"path as name":       head + "scope ../amy yes 0 - - 0 -\n",
		"file name as name":  head + "scope report.pdf yes 0 - - 0 -\n",
		"bad protection":     head + "scope amy maybe 0 - - 0 -\n",
		"bad count":          head + "scope amy yes many - - 0 -\n",
		"bad date":           head + "scope amy yes 1 2026-13-01T0030 - 0 -\n",
		"bad held":           head + "scope amy yes 1 - - lots -\n",
		"free text as error": head + "scope amy yes 1 - - 0 disk_on_fire\n",
	} {
		if _, err := ParseSnapshotStatus(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted %q", name, text)
		}
	}
}

func TestSnapshotStatusFileThatIsNotThereYet(t *testing.T) {
	if _, err := ReadSnapshotStatus(filepath.Join(t.TempDir(), "status")); !errors.Is(err, ErrNoSnapshotStatus) {
		t.Fatalf("got %v", err)
	}
	path := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(path, []byte(goodSnapshotStatus), 0o640); err != nil {
		t.Fatal(err)
	}
	if status, err := ReadSnapshotStatus(path); err != nil || len(status.Folders) != 5 {
		t.Fatalf("got %+v %v", status, err)
	}
}
