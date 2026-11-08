package system

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxBootJournalBytes = 64 << 10

var journalBootID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Boot is the current run of the machine.
type Boot struct {
	ID      string
	Started time.Time
	// CleanBefore tells whether the run before this one ended with a clean shutdown or reboot, nil when the journal
	// does not go back that far.
	CleanBefore *bool
}

// cachedBoot reads once and keeps the answer, because none of it changes until the next boot.
type cachedBoot struct {
	read func() (Boot, error)

	mu   sync.Mutex
	boot *Boot
}

func (c *cachedBoot) Boot() (Boot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.boot != nil {
		return *c.boot, nil
	}
	boot, err := c.read()
	if err != nil {
		return Boot{}, err
	}
	c.boot = &boot

	return boot, nil
}

func parseBootTime(stat string) (time.Time, error) {
	for _, line := range strings.Split(stat, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "btime" {
			return unixField(fields[1:], 1)
		}
	}

	return time.Time{}, errors.New("no btime in /proc/stat")
}

// A clean shutdown or reboot passes shutdown.target, and systemd logs reaching it. A run that ends in a power cut, a
// crash or the reset button never gets there. The earlier run is named by its boot ID rather than "-b -1", because
// without a persistent journal journalctl may ignore the boot offset and answer about the current run.
func previousRunClean(run journalRunner, currentID string) *bool {
	ctx, cancel := context.WithTimeout(context.Background(), journalTimeout)
	defer cancel()

	out, _, err := run(ctx, []string{"-b", "-1", "-n", "1", "-q", "--no-pager", "-o", "json", "--output-fields=_BOOT_ID"}, maxBootJournalBytes)
	if err != nil {
		log.Printf("no earlier run in the journal: %v", err)
		return nil
	}
	var entry struct {
		BootID string `json:"_BOOT_ID"`
	}
	if json.Unmarshal(out, &entry) != nil || !journalBootID.MatchString(entry.BootID) || entry.BootID == strings.ReplaceAll(currentID, "-", "") {
		return nil
	}
	out, _, err = run(ctx, []string{"-q", "--no-pager", "-o", "cat", "-n", "1", "_BOOT_ID=" + entry.BootID, "_PID=1", "UNIT=shutdown.target"}, maxBootJournalBytes)
	if err != nil {
		log.Printf("read the end of the earlier run from the journal: %v", err)
		return nil
	}
	clean := strings.TrimSpace(string(out)) != ""

	return &clean
}
