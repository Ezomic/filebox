package system

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodHealth = `filebox-health 1
checked 1794000000
smart {"device":{"name":"/dev/sda"},"model_name":"WDC WD5000AAKX","serial_number":"WD-1","smart_status":{"passed":true},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":8}}]}}
smart {"device":{"name":"/dev/sdb"},"model_name":"ST1000DM003","serial_number":"Z1D2","power_mode":"STANDBY"}
smart {"not":"a disk"}
state on
device 1 ok 0
device 2 missing 1520
scrub finished 1793502001 4 1
something new 1 2
`

func TestHealthStatusIsReadLineByLine(t *testing.T) {
	status, err := ParseHealthStatus(strings.NewReader(goodHealth))
	if err != nil {
		t.Fatal(err)
	}
	if !status.Checked.Equal(time.Unix(1794000000, 0)) || len(status.Disks) != 2 {
		t.Fatalf("got %+v", status)
	}
	sda, sdb := status.Disks[0], status.Disks[1]
	if sda.Serial != "WD-1" || sda.Healthy == nil || !*sda.Healthy || sda.Reallocated == nil || *sda.Reallocated != 8 {
		t.Fatalf("sda %+v", sda)
	}
	if sdb.Healthy != nil || sdb.Reallocated != nil {
		t.Fatalf("a sleeping disk tells nothing: %+v", sdb)
	}
	btrfs := status.Btrfs
	if btrfs.State != BtrfsOn || len(btrfs.Devices) != 2 || btrfs.Devices[1] != (BtrfsDevice{ID: 2, Missing: true, Errors: 1520}) {
		t.Fatalf("btrfs %+v", btrfs)
	}
	if scrub := btrfs.Scrub; scrub == nil || scrub.Status != "finished" || !scrub.Started.Equal(time.Unix(1793502001, 0)) ||
		scrub.Errors != 4 || scrub.Uncorrectable != 1 {
		t.Fatalf("scrub %+v", btrfs.Scrub)
	}
}

func TestHealthStatusTellsUnknownApartFromNone(t *testing.T) {
	status, err := ParseHealthStatus(strings.NewReader("filebox-health 1\nchecked 1794000000\nsmart unavailable\nstate on\ndevices unknown\nscrub unknown\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.Disks != nil || status.Btrfs.Devices != nil || status.Btrfs.Scrub != nil {
		t.Fatalf("got %+v", status)
	}
	status, err = ParseHealthStatus(strings.NewReader("filebox-health 1\nchecked 1794000000\nstate on\ndevice 1 ok -\nscrub none - 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if status.Disks == nil || len(status.Disks) != 0 || len(status.Btrfs.Devices) != 1 || status.Btrfs.Scrub.Status != "none" {
		t.Fatalf("no disks found is not the same as smartctl missing: %+v", status)
	}
}

func TestHealthStatusOffAndUnmounted(t *testing.T) {
	off, err := ParseHealthStatus(strings.NewReader("filebox-health 1\nchecked 1794000000\nstate off no_btrfs_progs\n"))
	if err != nil || off.Btrfs.State != BtrfsOff || off.Btrfs.Reason != "no_btrfs_progs" || off.Btrfs.Devices != nil {
		t.Fatalf("got %+v %v", off, err)
	}
	unmounted, err := ParseHealthStatus(strings.NewReader("filebox-health 1\nchecked 1794000000\nstate unmounted\n"))
	if err != nil || unmounted.Btrfs.State != BtrfsUnmounted || unmounted.Btrfs.Devices != nil || unmounted.Btrfs.Scrub != nil {
		t.Fatalf("got %+v %v", unmounted, err)
	}
}

func TestHealthStatusRejectsWhatItDoesNotKnow(t *testing.T) {
	const ok = "filebox-health 1\nchecked 1794000000\n"
	for _, report := range []string{
		"",
		"filebox-health 2\nchecked 1794000000\nstate on\n",
		"filebox-health 1\nstate on\n",
		ok,
		ok + "checked soon\nstate on\n",
		ok + "state maybe\n",
		ok + "state off because\n",
		ok + "state on\ndevice 1 gone 0\n",
		ok + "state on\ndevice 01 ok 0\n",
		ok + "state on\ndevice 1 ok -5\n",
		ok + "state on\ndevices maybe\n",
		ok + "state on\nscrub finished - 1\n",
		ok + "state on\nscrub paused - 0 0\n",
		ok + "state on\nscrub finished yesterday 0 0\n",
	} {
		if _, err := ParseHealthStatus(strings.NewReader(report)); !errors.Is(err, errBadHealth) {
			t.Errorf("%q: got %v", report, err)
		}
	}
	many := ok + "state on\n" + strings.Repeat("device 1 ok 0\n", maxBtrfsDevices+1)
	if _, err := ParseHealthStatus(strings.NewReader(many)); err == nil {
		t.Error("an endless device list must be refused")
	}
}

func TestHealthCountsAnOldReportAsNone(t *testing.T) {
	checked := time.Unix(1794000000, 0)
	now := checked.Add(HealthMaxAge)
	health := &Health{now: func() time.Time { return now }, read: func() (HealthStatus, error) {
		return ParseHealthStatus(strings.NewReader(goodHealth))
	}}
	if disks, err := health.Disks(); err != nil || len(disks) != 2 {
		t.Fatalf("an hour old is still fine: %v %v", disks, err)
	}
	if btrfs, err := health.Btrfs(); err != nil || btrfs.State != BtrfsOn {
		t.Fatalf("got %+v %v", btrfs, err)
	}
	now = now.Add(time.Second)
	if _, err := health.Disks(); !errors.Is(err, ErrSmartUnavailable) {
		t.Fatalf("an old report is no report, got %v", err)
	}
	if _, err := health.Btrfs(); !errors.Is(err, ErrBtrfsHealthUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestHealthWithoutSmartctlHasNoDisks(t *testing.T) {
	health := &Health{now: time.Now, read: func() (HealthStatus, error) {
		return HealthStatus{Checked: time.Now(), Btrfs: BtrfsHealth{State: BtrfsOff, Reason: "not_btrfs"}}, nil
	}}
	if _, err := health.Disks(); !errors.Is(err, ErrSmartUnavailable) {
		t.Fatalf("got %v", err)
	}
	if btrfs, err := health.Btrfs(); err != nil || btrfs.State != BtrfsOff {
		t.Fatalf("the btrfs part still counts: %+v %v", btrfs, err)
	}
}

func TestReadHealthStatusFromTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status")
	if _, err := ReadHealthStatus(path); !errors.Is(err, ErrNoHealthStatus) {
		t.Fatalf("got %v", err)
	}
	if err := os.WriteFile(path, []byte(goodHealth), 0o640); err != nil {
		t.Fatal(err)
	}
	if status, err := ReadHealthStatus(path); err != nil || len(status.Disks) != 2 {
		t.Fatalf("got %+v %v", status, err)
	}
}
