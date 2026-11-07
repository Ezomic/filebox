package system

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseBootTime(t *testing.T) {
	started, err := parseBootTime("cpu  1 2 3\nintr 5\nbtime 1794000000\nprocesses 12\n")
	if err != nil || !started.Equal(time.Unix(1794000000, 0)) {
		t.Fatalf("got %v %v", started, err)
	}
	if _, err := parseBootTime("cpu 1 2 3\n"); err == nil {
		t.Fatal("no btime must be an error")
	}
}

const (
	currentBoot  = "6a1bc0de-4f2a-4c3b-9d8e-0123456789ab"
	previousBoot = "5f0e9d8c7b6a59483726150403020100"

	// What journalctl -b -1 -n 1 -o json --output-fields=_BOOT_ID prints: systemd 252 (Debian 12) without the
	// sequence numbers, 257 (Debian 13) with them.
	lastEntryDebian12 = `{"__CURSOR":"s=0b5ee1fd8b0a4d29a1f5c4e8d3b2a190;i=2a5f;b=5f0e9d8c7b6a59483726150403020100;m=1b1d3c7e2;t=6239a2c4e5f60;x=9c1d2e3f4a5b6c7d","__REALTIME_TIMESTAMP":"1730899012345678","__MONOTONIC_TIMESTAMP":"7277365218","_BOOT_ID":"5f0e9d8c7b6a59483726150403020100"}` + "\n"
	lastEntryDebian13 = `{"__CURSOR":"s=0b5ee1fd8b0a4d29a1f5c4e8d3b2a190;i=2a5f;b=5f0e9d8c7b6a59483726150403020100;m=1b1d3c7e2;t=6239a2c4e5f60;x=9c1d2e3f4a5b6c7d","__REALTIME_TIMESTAMP":"1730899012345678","__MONOTONIC_TIMESTAMP":"7277365218","_BOOT_ID":"5f0e9d8c7b6a59483726150403020100","__SEQNUM":"10847","__SEQNUM_ID":"0b5ee1fd8b0a4d29a1f5c4e8d3b2a190"}` + "\n"
	// What journalctl -o cat -n 1 _PID=1 UNIT=shutdown.target prints for a run that was shut down or rebooted cleanly,
	// on Debian 12 and 13 alike.
	reachedShutdown = "Reached target shutdown.target - System Shutdown.\n"
)

// fakeJournal answers the two questions previousRunClean asks with what journalctl prints.
type fakeJournal struct {
	lastEntry     string
	previousErr   error
	shutdownLines string
	asked         [][]string
}

func (f *fakeJournal) run(_ context.Context, args []string, _ int64) ([]byte, bool, error) {
	f.asked = append(f.asked, args)
	if args[0] == "-b" {
		if f.previousErr != nil {
			return nil, false, f.previousErr
		}
		return []byte(f.lastEntry), false, nil
	}

	return []byte(f.shutdownLines), false, nil
}

func TestPreviousRunCleanLooksForShutdownTargetInThatRun(t *testing.T) {
	for name, lastEntry := range map[string]string{"Debian 12": lastEntryDebian12, "Debian 13": lastEntryDebian13} {
		journal := &fakeJournal{lastEntry: lastEntry, shutdownLines: reachedShutdown}
		clean := previousRunClean(journal.run, currentBoot)
		if clean == nil || !*clean {
			t.Fatalf("%s: got %v", name, clean)
		}
		asked := strings.Join(journal.asked[1], " ")
		if asked != "-q --no-pager -o cat -n 1 _BOOT_ID="+previousBoot+" _PID=1 UNIT=shutdown.target" {
			t.Fatalf("%s: asked %q", name, asked)
		}

		journal.shutdownLines = ""
		if clean := previousRunClean(journal.run, currentBoot); clean == nil || *clean {
			t.Fatalf("%s: a run that never reached shutdown.target ended uncleanly, got %v", name, clean)
		}
	}
}

func TestPreviousRunCleanIsUnknownWithoutAnEarlierRun(t *testing.T) {
	current := strings.ReplaceAll(lastEntryDebian13, previousBoot, strings.ReplaceAll(currentBoot, "-", ""))
	cases := map[string]*fakeJournal{
		"no earlier run on Debian 12":  {previousErr: errors.New("Data from the specified boot (-1) is not available: No such boot ID in journal")},
		"no earlier run on Debian 13":  {previousErr: errors.New("No journal boot entry found from the specified boot offset (-1).")},
		"the current run instead":      {lastEntry: current},
		"something that is not a boot": {lastEntry: `{"_BOOT_ID":"nonsense"}`},
		"no entry at all":              {lastEntry: ""},
	}
	for name, journal := range cases {
		if clean := previousRunClean(journal.run, currentBoot); clean != nil {
			t.Errorf("%s: got %v", name, *clean)
		}
		if len(journal.asked) != 1 {
			t.Errorf("%s: asked about shutdown.target anyway", name)
		}
	}
}

func TestCachedBootReadsOnceItWorked(t *testing.T) {
	reads := 0
	failing := true
	cache := &cachedBoot{read: func() (Boot, error) {
		reads++
		if failing {
			return Boot{}, errors.New("not yet")
		}
		return Boot{ID: "x"}, nil
	}}
	if _, err := cache.Boot(); err == nil {
		t.Fatal("want the error")
	}
	failing = false
	for range 3 {
		if boot, err := cache.Boot(); err != nil || boot.ID != "x" {
			t.Fatalf("got %+v %v", boot, err)
		}
	}
	if reads != 2 {
		t.Fatalf("reads=%d", reads)
	}
}
