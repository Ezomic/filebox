package system

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const goodBtrfsHealth = `filebox-disk-health 1
state on
device 1 ok 0
device 2 missing 1520
scrub finished 1793502001 4 1
something new 1 2
`

func TestBtrfsHealthIsReadLineByLine(t *testing.T) {
	health, err := ParseBtrfsHealth(strings.NewReader(goodBtrfsHealth))
	if err != nil {
		t.Fatal(err)
	}
	if health.State != BtrfsOn || len(health.Devices) != 2 {
		t.Fatalf("got %+v", health)
	}
	if health.Devices[0] != (BtrfsDevice{ID: 1}) || health.Devices[1] != (BtrfsDevice{ID: 2, Missing: true, Errors: 1520}) {
		t.Fatalf("devices %+v", health.Devices)
	}
	scrub := health.Scrub
	if scrub == nil || scrub.Status != "finished" || !scrub.Started.Equal(time.Unix(1793502001, 0)) || scrub.Errors != 4 || scrub.Uncorrectable != 1 {
		t.Fatalf("scrub %+v", scrub)
	}
}

func TestBtrfsHealthTellsUnknownApartFromNone(t *testing.T) {
	health, err := ParseBtrfsHealth(strings.NewReader("filebox-disk-health 1\nstate on\ndevices unknown\nscrub unknown\n"))
	if err != nil {
		t.Fatal(err)
	}
	if health.Devices != nil || health.Scrub != nil {
		t.Fatalf("got %+v", health)
	}
	health, err = ParseBtrfsHealth(strings.NewReader("filebox-disk-health 1\nstate on\ndevice 1 ok -\nscrub none - 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(health.Devices) != 1 || health.Scrub == nil || health.Scrub.Status != "none" || !health.Scrub.Started.IsZero() {
		t.Fatalf("got %+v", health)
	}
}

func TestBtrfsHealthOffAndUnmounted(t *testing.T) {
	off, err := ParseBtrfsHealth(strings.NewReader("filebox-disk-health 1\nstate off no_btrfs_progs\n"))
	if err != nil || off.State != BtrfsOff || off.Reason != "no_btrfs_progs" || off.Devices != nil {
		t.Fatalf("got %+v %v", off, err)
	}
	unmounted, err := ParseBtrfsHealth(strings.NewReader("filebox-disk-health 1\nstate unmounted\n"))
	if err != nil || unmounted.State != BtrfsUnmounted || unmounted.Devices != nil || unmounted.Scrub != nil {
		t.Fatalf("got %+v %v", unmounted, err)
	}
}

func TestBtrfsHealthRejectsWhatItDoesNotKnow(t *testing.T) {
	for _, report := range []string{
		"",
		"filebox-disk-health 2\nstate on\n",
		"filebox-disk-health 1\n",
		"filebox-disk-health 1\nstate maybe\n",
		"filebox-disk-health 1\nstate off because\n",
		"filebox-disk-health 1\nstate on\ndevice 1 gone 0\n",
		"filebox-disk-health 1\nstate on\ndevice 01 ok 0\n",
		"filebox-disk-health 1\nstate on\ndevice 1 ok -5\n",
		"filebox-disk-health 1\nstate on\ndevices maybe\n",
		"filebox-disk-health 1\nstate on\nscrub finished - 1\n",
		"filebox-disk-health 1\nstate on\nscrub paused - 0 0\n",
		"filebox-disk-health 1\nstate on\nscrub finished yesterday 0 0\n",
	} {
		if _, err := ParseBtrfsHealth(strings.NewReader(report)); !errors.Is(err, errBadBtrfsHealth) {
			t.Errorf("%q: got %v", report, err)
		}
	}
	many := "filebox-disk-health 1\nstate on\n" + strings.Repeat("device 1 ok 0\n", maxBtrfsDevices+1)
	if _, err := ParseBtrfsHealth(strings.NewReader(many)); err == nil {
		t.Error("an endless device list must be refused")
	}
}

func TestReadBtrfsHealthHelperReportsMissingCommand(t *testing.T) {
	if _, err := readBtrfsHealthHelper([]string{"this-command-does-not-exist"}); !errors.Is(err, ErrBtrfsHealthUnavailable) {
		t.Fatalf("got %v", err)
	}
}
