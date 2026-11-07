//go:build linux

package system

import (
	"os"
	"strings"
)

func NewBoot() func() (Boot, error) {
	return (&cachedBoot{read: func() (Boot, error) { return readBoot(runJournalctl) }}).Boot
}

func readBoot(run journalRunner) (Boot, error) {
	id, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return Boot{}, err
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return Boot{}, err
	}
	started, err := parseBootTime(string(stat))
	if err != nil {
		return Boot{}, err
	}
	boot := Boot{ID: strings.TrimSpace(string(id)), Started: started}
	boot.CleanBefore = previousRunClean(run, boot.ID)

	return boot, nil
}
