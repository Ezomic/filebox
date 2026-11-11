package alerts

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

func keys(result Result) string {
	var names []string
	for _, problem := range result.Problems {
		name := problem.Key
		if problem.Easing {
			name += " (easing)"
		}
		names = append(names, name)
	}

	return strings.Join(names, ", ")
}

func newCounters(t *testing.T) *Counters {
	t.Helper()
	counters, err := OpenCounters(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return counters
}

func newFailing(t *testing.T, dir string) *FailingDisks {
	t.Helper()
	failing, err := OpenFailingDisks(dir)
	if err != nil {
		t.Fatal(err)
	}

	return failing
}

func TestDiskCheckOpensAt95AndEndsBelow90(t *testing.T) {
	disk := system.NewFakeDisk(system.DiskUsage{})
	check := DiskCheck(disk)
	cases := []struct {
		used uint64
		want string
	}{
		{used: 899, want: ""},
		{used: 900, want: "disk-full (easing)"},
		{used: 949, want: "disk-full (easing)"},
		{used: 950, want: "disk-full"},
		{used: 990, want: "disk-full"},
	}
	for _, c := range cases {
		disk.Set(system.DiskUsage{Total: 1000, Used: c.used, Free: 1000 - c.used})
		result := check(start)
		if !result.Known || keys(result) != c.want {
			t.Errorf("used %d: got %q", c.used, keys(result))
		}
	}

	disk.Set(system.DiskUsage{Total: 1000, Used: 950, Free: 50})
	problem := check(start).Problems[0]
	if problem.Subject != "The data disk is 95% full" || !strings.Contains(problem.Detail, "50 B free of 1000 B") || !strings.Contains(problem.Detail, "At 98%") {
		t.Fatalf("got %+v", problem)
	}

	disk.Fail(errors.New("statfs failed"))
	if result := check(start); result.Known {
		t.Fatal("an unreadable disk must hold its alert, not clear it")
	}
}

func TestWholePercentNeverRoundsUpOrDown(t *testing.T) {
	cases := map[[2]uint64]uint64{{95, 100}: 95, {949, 1000}: 94, {1 << 62, 1 << 62}: 100, {0, 0}: 0, {7, 3}: 100}
	for in, want := range cases {
		if got := wholePercent(in[0], in[1]); got != want {
			t.Errorf("%v: got %d, want %d", in, got, want)
		}
	}
}

func ptr[T any](value T) *T { return &value }

func TestSmartCheckReportsFailingDisks(t *testing.T) {
	smart := &system.FakeSmart{Value: []system.DiskHealth{
		{Device: "/dev/sda", Model: "WDC", Serial: "A1", Healthy: ptr(true)},
		{Device: "/dev/sdb", Model: "ST1000", Serial: "B2", Healthy: ptr(false)},
		{Device: "/dev/sdc", Healthy: nil},
	}}
	result := SmartCheck(smart, newCounters(t), newFailing(t, t.TempDir()))(start)
	if result.Name != "the SMART disk health" || keys(result) != "smart-failing:B2, smart-failing:/dev/sdc (easing)" {
		t.Fatalf("got %q", keys(result))
	}
	failing := result.Problems[0]
	if failing.Subject != "Disk /dev/sdb is failing" || !strings.Contains(failing.Detail, "/dev/sdb (ST1000, serial B2)") {
		t.Fatalf("got %+v", failing)
	}
	if result.Problems[1].Subject != "" {
		t.Fatal("a disk that did not answer keeps the texts of an open alert")
	}

	smart.Err = system.ErrSmartUnavailable
	if SmartCheck(smart, newCounters(t), newFailing(t, t.TempDir()))(start).Known {
		t.Fatal("unreadable SMART must hold the alerts")
	}
}

func TestSmartCheckReportsAGrowingReallocatedCount(t *testing.T) {
	disk := system.DiskHealth{Device: "/dev/sda", Serial: "A1", Healthy: ptr(true), Reallocated: ptr(uint64(8))}
	smart := &system.FakeSmart{Value: []system.DiskHealth{disk}}
	counters := newCounters(t)
	check := SmartCheck(smart, counters, newFailing(t, t.TempDir()))

	if result := check(start); keys(result) != "" {
		t.Fatalf("the first reading is the baseline, whatever its value: %q", keys(result))
	}
	smart.Value[0].Reallocated = ptr(uint64(12))
	rose := start.Add(time.Hour)
	result := check(rose)
	if keys(result) != "reallocated:A1" || !strings.Contains(result.Problems[0].Detail, "went from 8 to 12") {
		t.Fatalf("got %+v", result.Problems)
	}

	smart.Value[0].Reallocated = nil
	if result := check(rose.Add(time.Hour)); keys(result) != "reallocated:A1" {
		t.Fatalf("a sleeping disk keeps its rise: %q", keys(result))
	}
	smart.Value[0].Device = "/dev/sdb"
	smart.Value[0].Reallocated = ptr(uint64(12))
	if result := check(rose.Add(2 * time.Hour)); keys(result) != "reallocated:A1" || result.Problems[0].Subject != "Disk /dev/sdb has more reallocated sectors" {
		t.Fatalf("the serial follows the disk to its new name: %+v", result.Problems)
	}
	if result := check(rose.Add(RiseWindow)); keys(result) != "" {
		t.Fatalf("a count that stays put for a week settles: %q", keys(result))
	}
}

func TestSmartCheckIgnoresAFlakyZeroReading(t *testing.T) {
	smart := &system.FakeSmart{Value: []system.DiskHealth{{Device: "/dev/sda", Serial: "A1", Healthy: ptr(true), Reallocated: ptr(uint64(8))}}}
	check := SmartCheck(smart, newCounters(t), newFailing(t, t.TempDir()))
	check(start)
	smart.Value[0].Reallocated = ptr(uint64(12))
	check(start.Add(time.Hour))

	smart.Value[0].Reallocated = ptr(uint64(0))
	if result := check(start.Add(2 * time.Hour)); keys(result) != "reallocated:A1" {
		t.Fatalf("a zero reading must not end the rise: %q", keys(result))
	}
	smart.Value[0].Reallocated = ptr(uint64(12))
	result := check(start.Add(3 * time.Hour))
	if keys(result) != "reallocated:A1" || !strings.Contains(result.Problems[0].Detail, "went from 8 to 12, last on "+when(start.Add(time.Hour))) {
		t.Fatalf("the next normal reading is no new rise from 0: %+v", result.Problems)
	}
}

func TestSmartCheckKeepsAFailingDiskThatStopsAnswering(t *testing.T) {
	dir := t.TempDir()
	smart := &system.FakeSmart{Value: []system.DiskHealth{
		{Device: "/dev/sda", Serial: "A1", Healthy: ptr(true)},
		{Device: "/dev/sdb", Model: "ST1000", Serial: "B2", Healthy: ptr(false)},
	}}
	check := SmartCheck(smart, newCounters(t), newFailing(t, dir))
	check(start)

	smart.Value = smart.Value[:1]
	away := start.Add(time.Hour)
	restarted := SmartCheck(smart, newCounters(t), newFailing(t, dir))
	result := restarted(away)
	if keys(result) != "smart-failing:B2" || !strings.Contains(result.Problems[0].Detail, "no longer answers since "+when(start)) {
		t.Fatalf("a failing disk that vanished keeps its alert, also after a restart: %+v", result.Problems)
	}
	if !strings.Contains(result.Problems[0].Cleared, "has not been seen for 7 days") {
		t.Fatalf("got %q", result.Problems[0].Cleared)
	}
	if result := restarted(start.Add(GoneDiskAfter - time.Minute)); keys(result) != "smart-failing:B2" {
		t.Fatalf("still within the week: %q", keys(result))
	}

	smart.Value = append(smart.Value, system.DiskHealth{Device: "/dev/sdb", Serial: "B2"})
	restarted(start.Add(2 * 24 * time.Hour))
	smart.Value = smart.Value[:1]
	if result := restarted(start.Add(GoneDiskAfter + time.Hour)); keys(result) != "smart-failing:B2" {
		t.Fatalf("a disk that answered while asleep was seen then: %q", keys(result))
	}
	if result := restarted(start.Add(2*24*time.Hour + GoneDiskAfter)); keys(result) != "" {
		t.Fatalf("a week without the disk ends it: %q", keys(result))
	}

	smart.Value = []system.DiskHealth{{Device: "/dev/sdb", Serial: "B2", Healthy: ptr(false)}}
	restarted(start.Add(30 * 24 * time.Hour))
	smart.Value[0].Healthy = ptr(true)
	restarted(start.Add(31 * 24 * time.Hour))
	smart.Value = nil
	if result := restarted(start.Add(32 * 24 * time.Hour)); keys(result) != "" {
		t.Fatalf("a disk that passed again is no longer remembered as failing: %q", keys(result))
	}
}

func btrfsHealth(devices []system.BtrfsDevice, scrub *system.BtrfsScrub) func() (system.BtrfsHealth, error) {
	return func() (system.BtrfsHealth, error) {
		return system.BtrfsHealth{State: system.BtrfsOn, Devices: devices, Scrub: scrub}, nil
	}
}

func TestMirrorCheckIsQuietOnAHealthyOrNonBtrfsDisk(t *testing.T) {
	healthy := btrfsHealth([]system.BtrfsDevice{{ID: 1, Errors: 3}, {ID: 2}}, &system.BtrfsScrub{Status: "finished"})
	if result := MirrorCheck(healthy, newCounters(t))(start); !result.Known || keys(result) != "" {
		t.Fatalf("got %q", keys(result))
	}
	off := func() (system.BtrfsHealth, error) {
		return system.BtrfsHealth{State: system.BtrfsOff, Reason: "not_btrfs"}, nil
	}
	if result := MirrorCheck(off, newCounters(t))(start); !result.Known || keys(result) != "" {
		t.Fatalf("a box without btrfs has nothing to report: %q", keys(result))
	}
	failed := func() (system.BtrfsHealth, error) { return system.BtrfsHealth{}, system.ErrBtrfsHealthUnavailable }
	if MirrorCheck(failed, newCounters(t))(start).Known {
		t.Fatal("a helper that failed must hold the alerts")
	}
}

func TestMirrorCheckReportsAMissingDisk(t *testing.T) {
	read := btrfsHealth([]system.BtrfsDevice{{ID: 1}, {ID: 2, Missing: true}}, &system.BtrfsScrub{Status: "none"})
	result := MirrorCheck(read, newCounters(t))(start)
	if keys(result) != "mirror-missing" || !strings.Contains(result.Problems[0].Detail, "without btrfs device 2 (1 of its 2 disks)") {
		t.Fatalf("got %+v", result.Problems)
	}

	unknown := btrfsHealth(nil, nil)
	if result := MirrorCheck(unknown, newCounters(t))(start); keys(result) != "mirror-missing (easing), scrub-errors (easing)" {
		t.Fatalf("parts that could not be read hold their alerts: %q", keys(result))
	}
}

func TestMirrorCheckReportsAnUnmountedDataDisk(t *testing.T) {
	read := func() (system.BtrfsHealth, error) { return system.BtrfsHealth{State: system.BtrfsUnmounted}, nil }
	result := MirrorCheck(read, newCounters(t))(start)
	if keys(result) != "data-unmounted, mirror-missing (easing), scrub-errors (easing)" {
		t.Fatalf("got %q", keys(result))
	}
	if !strings.Contains(result.Problems[0].Detail, "If a mirror disk dies") {
		t.Fatalf("got %q", result.Problems[0].Detail)
	}
}

func TestMirrorCheckReportsNewReadAndWriteErrors(t *testing.T) {
	devices := []system.BtrfsDevice{{ID: 1, Errors: 2}, {ID: 2}}
	counters := newCounters(t)
	check := MirrorCheck(func() (system.BtrfsHealth, error) {
		return system.BtrfsHealth{State: system.BtrfsOn, Devices: devices, Scrub: &system.BtrfsScrub{Status: "none"}}, nil
	}, counters)
	if keys(check(start)) != "" {
		t.Fatal("old errors are the baseline")
	}
	devices = []system.BtrfsDevice{{ID: 1, Errors: 2}, {ID: 2, Errors: 1520}}
	result := check(start.Add(time.Hour))
	if keys(result) != "io-errors" || !strings.Contains(result.Problems[0].Detail, "1520 new read, write or flush errors") ||
		!strings.Contains(result.Problems[0].Detail, "device 2: 1520") {
		t.Fatalf("got %+v", result.Problems)
	}
	devices = []system.BtrfsDevice{{ID: 1, Errors: 0}, {ID: 2, Errors: 0}}
	if keys(check(start.Add(2*time.Hour))) != "io-errors" {
		t.Fatal("a zero reading must not end the rise")
	}
	devices = []system.BtrfsDevice{{ID: 1, Errors: 2}, {ID: 2, Errors: 1520}}
	if result := check(start.Add(3 * time.Hour)); keys(result) != "io-errors" || !strings.Contains(result.Problems[0].Detail, "1520 new read") {
		t.Fatalf("the next normal reading is no new rise from 0: %+v", result.Problems)
	}

	devices = []system.BtrfsDevice{{ID: 1, Errors: 2}, {ID: 3}}
	check(start.Add(4 * time.Hour))
	if keys(check(start.Add(4*time.Hour+LowerAfter))) != "" {
		t.Fatal("a replaced disk lowers the count for good after a day and ends the rise")
	}
}

func TestMirrorCheckReportsScrubErrors(t *testing.T) {
	scrubbed := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	read := btrfsHealth([]system.BtrfsDevice{{ID: 1}}, &system.BtrfsScrub{Status: "finished", Started: scrubbed, Errors: 4, Uncorrectable: 1})
	result := MirrorCheck(read, newCounters(t))(start)
	if keys(result) != "scrub-errors" {
		t.Fatalf("got %q", keys(result))
	}
	problem := result.Problems[0]
	if problem.Subject != "The disk check (scrub) found 4 errors" || !strings.Contains(problem.Detail, "The scrub of "+when(scrubbed)+" found 4 errors") ||
		!strings.Contains(problem.Detail, "and 1 could not be repaired") {
		t.Fatalf("got %+v", problem)
	}
}

func snapshotStatus(status system.SnapshotStatus, err error) func() (system.SnapshotStatus, error) {
	return func() (system.SnapshotStatus, error) { return status, err }
}

func TestSnapshotCheckReportsAFailedNight(t *testing.T) {
	night := time.Date(2026, 11, 7, 0, 30, 0, 0, time.UTC)
	status := system.SnapshotStatus{Enabled: true, Daily: night, DailyOK: false, Folders: []system.SnapshotFolder{
		{Name: "amy", Protection: "yes", Error: "snapshot_failed"},
		{Name: system.FamilyDir, Protection: "yes", Error: "prune_failed"},
		{Name: "bob.deleted-20261101120000", Protection: "gone", Error: "prune_failed"},
		{Name: "cleo", Protection: "yes"},
	}}
	result := SnapshotCheck(snapshotStatus(status, nil))(start)
	if keys(result) != "snapshots-failed" {
		t.Fatalf("got %q", keys(result))
	}
	want := "The nightly snapshots failed for amy (no new snapshot was taken), Family (old snapshots could not be deleted), " +
		"the deleted account bob (old snapshots could not be deleted)."
	if !strings.Contains(result.Problems[0].Detail, want) {
		t.Fatalf("got %q", result.Problems[0].Detail)
	}

	status.Folders = nil
	if result := SnapshotCheck(snapshotStatus(status, nil))(start); !strings.Contains(result.Problems[0].Detail, "of "+when(night)+" failed") {
		t.Fatalf("got %q", result.Problems[0].Detail)
	}
	status.DailyOK = true
	if result := SnapshotCheck(snapshotStatus(status, nil))(start); keys(result) != "" {
		t.Fatalf("got %q", keys(result))
	}
}

func TestSnapshotCheckReportsEarlierVersionsDeletedForSpace(t *testing.T) {
	status := system.SnapshotStatus{Enabled: true, DailyOK: true, LowSpace: &system.SnapshotLowSpace{At: start, Deleted: 3, StillFull: true}}
	result := SnapshotCheck(snapshotStatus(status, nil))(start.Add(time.Hour))
	if keys(result) != "snapshots-pruned" || !strings.Contains(result.Problems[0].Detail, "deleted the 3 oldest earlier versions") ||
		!strings.Contains(result.Problems[0].Detail, "still full") {
		t.Fatalf("got %+v", result.Problems)
	}
	if result := SnapshotCheck(snapshotStatus(status, nil))(start.Add(pruneWindow)); keys(result) != "" {
		t.Fatalf("a day later it is over: %q", keys(result))
	}
	status.LowSpace.Deleted = 0
	if result := SnapshotCheck(snapshotStatus(status, nil))(start.Add(time.Hour)); keys(result) != "snapshots-pruned (easing)" {
		t.Fatalf("a run that deleted nothing does not start an alert: %q", keys(result))
	}
}

func TestSnapshotCheckWithoutSnapshots(t *testing.T) {
	off := system.SnapshotStatus{Enabled: false, Reason: "not_btrfs", Daily: start, DailyOK: false}
	if result := SnapshotCheck(snapshotStatus(off, nil))(start); !result.Known || keys(result) != "" {
		t.Fatalf("got %q", keys(result))
	}
	if result := SnapshotCheck(snapshotStatus(system.SnapshotStatus{}, system.ErrNoSnapshotStatus))(start); !result.Known || keys(result) != "" {
		t.Fatal("no report yet is not a problem")
	}
	if SnapshotCheck(snapshotStatus(system.SnapshotStatus{}, errors.New("unreadable")))(start).Known {
		t.Fatal("an unreadable status must hold the alerts")
	}
	if !SnapshotCheck(nil)(start).Known {
		t.Fatal("no status file at all is not a problem")
	}
}

func backupStatus(status system.BackupStatus, err error) func() (system.BackupStatus, error) {
	return func() (system.BackupStatus, error) { return status, err }
}

func TestBackupCheckReportsAFailedNight(t *testing.T) {
	night := start.Add(-8 * time.Hour)
	cases := map[string]string{
		system.BackupFailed:        "The offsite backup failed",
		system.BackupPrepareFailed: "The offsite backup failed",
		system.BackupPruneFailed:   "Old offsite backups could not be removed",
		system.BackupIncomplete:    "",
		system.BackupOK:            "",
		system.BackupStopped:       "",
	}
	for result, want := range cases {
		status := system.BackupStatus{Configured: true, Latest: night, Run: &system.BackupRun{Started: night, Result: result}}
		got := BackupCheck(backupStatus(status, nil))(start)
		if !got.Known || (want == "" && keys(got) != "") || (want != "" && (keys(got) != "backup-failed" || got.Problems[0].Subject != want)) {
			t.Errorf("%s: got %+v", result, got.Problems)
			continue
		}
		if want != "" && !strings.Contains(got.Problems[0].Detail, when(night)) {
			t.Errorf("%s: the detail does not say which night: %q", result, got.Problems[0].Detail)
		}
	}
}

func TestBackupCheckReportsThreeDaysWithoutABackup(t *testing.T) {
	last := start.Add(-backupOverdue)
	status := system.BackupStatus{Configured: true, Latest: last, Run: &system.BackupRun{Started: start.Add(-time.Hour), Result: system.BackupStopped}}
	result := BackupCheck(backupStatus(status, nil))(start)
	if keys(result) != "backup-overdue" || result.Problems[0].Subject != "No offsite backup for 3 days" ||
		!strings.Contains(result.Problems[0].Detail, when(last)) || !strings.Contains(result.Problems[0].Detail, "6 hour limit") {
		t.Fatalf("got %+v", result.Problems)
	}
	if result := BackupCheck(backupStatus(status, nil))(start.Add(-time.Minute)); keys(result) != "" {
		t.Fatalf("just under 3 days is fine: %q", keys(result))
	}
	status.Latest = time.Time{}
	if result := BackupCheck(backupStatus(status, nil))(start.Add(30 * 24 * time.Hour)); keys(result) != "" {
		t.Fatalf("before the first complete backup nothing counts: %q", keys(result))
	}
}

func TestBackupCheckReportsUnreadableFilesOnlyNightAfterNight(t *testing.T) {
	since := start.Add(-50 * time.Hour)
	status := system.BackupStatus{Configured: true, Latest: start, Run: &system.BackupRun{Started: start, Result: system.BackupIncomplete},
		Unreadable: &system.BackupStreak{Since: since, Nights: 2}}
	if result := BackupCheck(backupStatus(status, nil))(start); keys(result) != "" {
		t.Fatalf("two nights are not a problem yet: %q", keys(result))
	}
	status.Unreadable.Nights = 3
	result := BackupCheck(backupStatus(status, nil))(start)
	if keys(result) != "backup-unreadable" || result.Problems[0].Subject != "Some files could not be read for the offsite backup" ||
		!strings.Contains(result.Problems[0].Detail, "The last 3 nightly offsite backups, since "+when(since)) ||
		strings.Contains(result.Problems[0].Detail, "disk") {
		t.Fatalf("got %+v", result.Problems)
	}
}

func TestBackupCheckReportsAFirstBackupThatDoesNotFinish(t *testing.T) {
	setup := start.Add(-firstBackupDue)
	status := system.BackupStatus{Configured: true, Setup: setup, Run: &system.BackupRun{Started: start, Result: system.BackupStopped}}
	result := BackupCheck(backupStatus(status, nil))(start)
	if keys(result) != "backup-first" || result.Problems[0].Subject != "The first offsite backup has not finished after 14 days" ||
		!strings.Contains(result.Problems[0].Detail, when(setup)) {
		t.Fatalf("got %+v", result.Problems)
	}
	if result := BackupCheck(backupStatus(status, nil))(start.Add(-time.Minute)); keys(result) != "" {
		t.Fatalf("just under 14 days is fine: %q", keys(result))
	}
	status.Latest = start.Add(-time.Hour)
	if result := BackupCheck(backupStatus(status, nil))(start); keys(result) != "" {
		t.Fatalf("once the first backup has finished: %q", keys(result))
	}
	status.Latest, status.Setup = time.Time{}, time.Time{}
	if result := BackupCheck(backupStatus(status, nil))(start); keys(result) != "" {
		t.Fatalf("without a setup time nothing counts: %q", keys(result))
	}
}

func TestBackupCheckTellsDamageFromACheckThatCouldNotRun(t *testing.T) {
	status := system.BackupStatus{Configured: true, Latest: start, Check: &system.RepositoryCheck{At: start, OK: false}}
	result := BackupCheck(backupStatus(status, nil))(start)
	if keys(result) != "backup-check" || !strings.Contains(result.Problems[0].Detail, "damaged or missing data") {
		t.Fatalf("got %+v", result.Problems)
	}
	status.Check.OK = true
	if result := BackupCheck(backupStatus(status, nil))(start); keys(result) != "" {
		t.Fatalf("got %q", keys(result))
	}

	since := start.Add(-50 * time.Hour)
	status.CheckErrors = &system.BackupStreak{Since: since, Nights: 2}
	if result := BackupCheck(backupStatus(status, nil))(start); keys(result) != "" {
		t.Fatalf("two nights are not a problem yet: %q", keys(result))
	}
	status.CheckErrors.Nights = 3
	result = BackupCheck(backupStatus(status, nil))(start)
	if keys(result) != "backup-check-unrun" || result.Problems[0].Subject != "The monthly check of the offsite backup could not run" ||
		!strings.Contains(result.Problems[0].Detail, when(since)) || strings.Contains(result.Problems[0].Detail, "damage") {
		t.Fatalf("got %+v", result.Problems)
	}
}

func TestBackupCheckBeforeSetupAndWhenItCannotLook(t *testing.T) {
	unconfigured := system.BackupStatus{Run: &system.BackupRun{Started: start, Result: system.BackupFailed}, Latest: start.Add(-30 * 24 * time.Hour)}
	if result := BackupCheck(backupStatus(unconfigured, nil))(start); !result.Known || keys(result) != "" {
		t.Fatalf("a backup that is not set up is no problem: %q", keys(result))
	}
	if result := BackupCheck(backupStatus(system.BackupStatus{}, system.ErrNoBackupStatus))(start); !result.Known || keys(result) != "" {
		t.Fatal("no report yet is not a problem")
	}
	if result := BackupCheck(backupStatus(system.BackupStatus{}, errors.New("unreadable")))(start); result.Known || result.Name != "the offsite backup" {
		t.Fatalf("an unreadable status must hold the alerts: %+v", result)
	}
}

func TestCertCheckWarnsTwoWeeksAhead(t *testing.T) {
	cases := map[time.Duration]string{
		15 * 24 * time.Hour:           "",
		14 * 24 * time.Hour:           "",
		14*24*time.Hour - time.Hour:   "The portal certificate expires in 13 days",
		24*time.Hour + time.Hour:      "The portal certificate expires in 1 day",
		time.Hour:                     "The portal certificate expires within a day",
		-time.Hour:                    "The portal certificate has expired",
		-40 * 24 * time.Hour:          "The portal certificate has expired",
		14*24*time.Hour - time.Second: "The portal certificate expires in 13 days",
	}
	for left, want := range cases {
		expiry := start.Add(left)
		result := CertCheck(func() time.Time { return expiry })(start)
		got := ""
		if len(result.Problems) > 0 {
			got = result.Problems[0].Subject
		}
		if got != want {
			t.Errorf("%v left: got %q, want %q", left, got, want)
		}
	}
	if result := CertCheck(nil)(start); !result.Known || len(result.Problems) != 0 {
		t.Fatal("a box without the portal has no certificate to watch")
	}
}

func TestRestartCheckReportsAnUncleanRestartForADay(t *testing.T) {
	booted := start.Add(-time.Hour)
	boot := system.Boot{ID: "6a1b", Started: booted, CleanBefore: ptr(false)}
	read := func() (system.Boot, error) { return boot, nil }
	result := RestartCheck(read)(start)
	if keys(result) != "unclean-restart:6a1b" || !strings.Contains(result.Problems[0].Detail, when(booted)) {
		t.Fatalf("got %+v", result.Problems)
	}
	if result := RestartCheck(read)(booted.Add(restartWindow)); !result.Known || keys(result) != "" {
		t.Fatalf("a day after the start it is over: %q", keys(result))
	}
	boot.CleanBefore = ptr(true)
	if keys(RestartCheck(read)(start)) != "" {
		t.Fatal("a clean shutdown is no problem")
	}
	boot.CleanBefore = nil
	if keys(RestartCheck(read)(start)) != "" {
		t.Fatal("a journal that does not go back far enough is no problem")
	}
	failing := func() (system.Boot, error) { return system.Boot{}, errors.New("no /proc") }
	if RestartCheck(failing)(start).Known {
		t.Fatal("an unreadable boot must hold the alert")
	}
}

func TestNoTextUsesAnEmDash(t *testing.T) {
	disk := system.NewFakeDisk(system.DiskUsage{Total: 100, Used: 99, Free: 1})
	results := []Result{
		DiskCheck(disk)(start),
		SmartCheck(&system.FakeSmart{Value: []system.DiskHealth{{Device: "/dev/sda", Healthy: ptr(false)}}}, newCounters(t), newFailing(t, t.TempDir()))(start),
		MirrorCheck(func() (system.BtrfsHealth, error) { return system.BtrfsHealth{State: system.BtrfsUnmounted}, nil }, newCounters(t))(start),
		MirrorCheck(btrfsHealth([]system.BtrfsDevice{{ID: 2, Missing: true}}, &system.BtrfsScrub{Status: "finished", Errors: 1}), newCounters(t))(start),
		SnapshotCheck(snapshotStatus(system.SnapshotStatus{Enabled: true, Daily: start, LowSpace: &system.SnapshotLowSpace{At: start, Deleted: 1}}, nil))(start),
		CertCheck(func() time.Time { return start })(start),
		RestartCheck(func() (system.Boot, error) { return system.Boot{ID: "x", Started: start, CleanBefore: ptr(false)}, nil })(start),
		BackupCheck(backupStatus(system.BackupStatus{
			Configured: true, Latest: start.Add(-4 * 24 * time.Hour), Run: &system.BackupRun{Started: start, Result: system.BackupPrepareFailed},
			Check: &system.RepositoryCheck{At: start}, Unreadable: &system.BackupStreak{Since: start, Nights: 3},
			CheckErrors: &system.BackupStreak{Since: start, Nights: 3},
		}, nil))(start),
		BackupCheck(backupStatus(system.BackupStatus{Configured: true, Setup: start.Add(-firstBackupDue)}, nil))(start),
	}
	results = append(results, Result{Source: "unchecked-test", Name: "the test check"})
	_, state := Decide(State{}, results, start.Add(-UncheckedAfter))
	mails, state := Decide(state, results, start)
	state = deliverAll(state, mails, start)
	cleared, _ := gone(state, nil, start.Add(time.Hour))
	if len(mails) < 10 || len(cleared) != len(mails) {
		t.Fatalf("got %d started and %d cleared mails", len(mails), len(cleared))
	}
	for _, mail := range append(mails, cleared...) {
		if strings.ContainsRune(mail.Subject+mail.Text, 0x2014) || strings.Contains(mail.Text, " -- ") {
			t.Errorf("%s mail for %s uses an em-dash", mail.Kind, mail.Key)
		}
	}
}
