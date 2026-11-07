package system

import (
	"errors"
	"time"
)

const maxBtrfsDevices = 64

var ErrBtrfsHealthUnavailable = errors.New("data disk health unavailable")

const (
	BtrfsOn        = "on"
	BtrfsUnmounted = "unmounted"
	BtrfsOff       = "off"
)

// BtrfsHealth is what filebox-health reports about the btrfs data disk: disk IDs, counts and dates, never a file.
type BtrfsHealth struct {
	// State is BtrfsOn, BtrfsUnmounted (set up in /etc/fstab but not mounted), or BtrfsOff with Reason "not_btrfs" or
	// "no_btrfs_progs".
	State  string
	Reason string
	// Devices is nil when they could not be read.
	Devices []BtrfsDevice
	// Scrub is nil when its status could not be read.
	Scrub *BtrfsScrub
}

type BtrfsDevice struct {
	ID      uint64
	Missing bool
	// Errors is how many read, write and flush errors btrfs counted on the disk since it was added or the counts were
	// reset.
	Errors uint64
}

type BtrfsScrub struct {
	// Status is "none" before the first scrub, else "running", "finished", "aborted" or "interrupted".
	Status string
	// Started is zero when it is not known.
	Started       time.Time
	Errors        uint64
	Uncorrectable uint64
}

func (h *BtrfsHealth) parseState(fields []string) error {
	switch {
	case len(fields) == 1 && (fields[0] == BtrfsOn || fields[0] == BtrfsUnmounted):
		h.State, h.Reason = fields[0], ""
	case len(fields) == 2 && fields[0] == BtrfsOff && (fields[1] == "not_btrfs" || fields[1] == "no_btrfs_progs"):
		h.State, h.Reason = BtrfsOff, fields[1]
	default:
		return errors.New("state")
	}
	if h.State == BtrfsOn && h.Devices == nil {
		h.Devices = []BtrfsDevice{}
	}

	return nil
}

// device <id> <ok|missing> <errors|->
func (h *BtrfsHealth) parseDevice(fields []string) error {
	if len(fields) != 3 || (fields[1] != "ok" && fields[1] != "missing") || len(h.Devices) >= maxBtrfsDevices {
		return errors.New("device")
	}
	id, ok := parseCanonical(fields[0])
	if !ok {
		return errors.New("device id")
	}
	device := BtrfsDevice{ID: id, Missing: fields[1] == "missing"}
	if fields[2] != "-" {
		if device.Errors, ok = parseCanonical(fields[2]); !ok {
			return errors.New("device errors")
		}
	}
	h.Devices = append(h.Devices, device)

	return nil
}

// scrub unknown | scrub <status> <started|-> <errors> <uncorrectable>
func parseScrub(fields []string) (*BtrfsScrub, error) {
	if len(fields) == 1 && fields[0] == "unknown" {
		return nil, nil
	}
	if len(fields) != 4 {
		return nil, errors.New("scrub")
	}
	scrub := &BtrfsScrub{Status: fields[0]}
	switch scrub.Status {
	case "none", "running", "finished", "aborted", "interrupted":
	default:
		return nil, errors.New("scrub status")
	}
	if fields[1] != "-" {
		started, err := unixField(fields[1:2], 1)
		if err != nil {
			return nil, err
		}
		scrub.Started = started
	}
	var ok bool
	if scrub.Errors, ok = parseCanonical(fields[2]); !ok {
		return nil, errors.New("scrub errors")
	}
	if scrub.Uncorrectable, ok = parseCanonical(fields[3]); !ok {
		return nil, errors.New("scrub uncorrectable")
	}

	return scrub, nil
}
