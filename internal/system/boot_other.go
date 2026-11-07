//go:build !linux

package system

import "time"

func NewBoot() func() (Boot, error) {
	clean, started := true, time.Now()

	return func() (Boot, error) {
		return Boot{ID: "00000000-0000-0000-0000-000000000000", Started: started, CleanBefore: &clean}, nil
	}
}
