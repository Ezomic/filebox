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
)

// fakeJournal answers the two questions previousRunClean asks.
type fakeJournal struct {
	previous      string
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
		return []byte(`{"__CURSOR":"s=1","_BOOT_ID":"` + f.previous + `"}` + "\n"), false, nil
	}

	return []byte(f.shutdownLines), false, nil
}

func TestPreviousRunCleanLooksForShutdownTargetInThatRun(t *testing.T) {
	journal := &fakeJournal{previous: previousBoot, shutdownLines: "Reached target shutdown.target - System Shutdown.\n"}
	clean := previousRunClean(journal.run, currentBoot)
	if clean == nil || !*clean {
		t.Fatalf("got %v", clean)
	}
	asked := strings.Join(journal.asked[1], " ")
	if asked != "-q --no-pager -o cat -n 1 _BOOT_ID="+previousBoot+" _PID=1 UNIT=shutdown.target" {
		t.Fatalf("asked %q", asked)
	}

	journal.shutdownLines = ""
	if clean := previousRunClean(journal.run, currentBoot); clean == nil || *clean {
		t.Fatalf("a run that never reached shutdown.target ended uncleanly, got %v", clean)
	}
}

func TestPreviousRunCleanIsUnknownWithoutAnEarlierRun(t *testing.T) {
	cases := map[string]*fakeJournal{
		"no earlier run":               {previousErr: errors.New("Data from the specified boot (-1) is not available")},
		"the current run instead":      {previous: strings.ReplaceAll(currentBoot, "-", "")},
		"something that is not a boot": {previous: "nonsense"},
		"an empty answer":              {previous: ""},
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
