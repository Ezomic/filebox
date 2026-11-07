//go:build !linux

package system

import "time"

func ReadBtrfsHealth() (BtrfsHealth, error) {
	return BtrfsHealth{
		State:   BtrfsOn,
		Devices: []BtrfsDevice{{ID: 1}, {ID: 2}},
		Scrub:   &BtrfsScrub{Status: "finished", Started: time.Now().Add(-6 * 24 * time.Hour)},
	}, nil
}
