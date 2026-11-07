//go:build !linux

package system

import "time"

func NewHealth(string) *Health {
	return &Health{now: time.Now, read: func() (HealthStatus, error) {
		return HealthStatus{
			Checked: time.Now(),
			Disks:   NewFakeSmart().Value,
			Btrfs: BtrfsHealth{
				State:   BtrfsOn,
				Devices: []BtrfsDevice{{ID: 1}, {ID: 2}},
				Scrub:   &BtrfsScrub{Status: "finished", Started: time.Now().Add(-6 * 24 * time.Hour)},
			},
		}, nil
	}}
}
