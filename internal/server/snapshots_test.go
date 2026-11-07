package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

const panelSnapshotStatus = `filebox-snapshot 1
state on
checked 1794000000
daily 1793925000 failed
lowspace 1793990000 3 yes
scope _family yes 17 2026-10-04T0030 2026-11-07T0030 7340032 -
scope amy yes 23 2026-06-30T0030 2026-11-07T0030 1024 snapshot_failed
scope cleo.deleted-20261101093000 gone 4 2026-10-20T0030 2026-10-31T0030 - -
`

func newSnapshotPanel(t *testing.T, status func() (system.SnapshotStatus, error)) (*harness, *http.Cookie) {
	t.Helper()
	h := newHarness(t)
	h.handler = New(h.store, Deps{Host: h.host, Users: h.users, Logs: h.logs, Power: h.power, Smart: h.smart, Usage: h.usage, Disk: h.disk, Snapshots: status})

	return h, h.signedIn(t)
}

func parsedStatus(t *testing.T, text string) func() (system.SnapshotStatus, error) {
	t.Helper()
	status, err := system.ParseSnapshotStatus(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}

	return func() (system.SnapshotStatus, error) { return status, nil }
}

func panelSnapshots(t *testing.T, h *harness, cookie *http.Cookie) snapshotsView {
	t.Helper()
	rec := h.do("GET", "/api/snapshots", "", cookie)
	var view snapshotsView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return view
}

func TestPanelShowsEarlierVersionsPerFolder(t *testing.T) {
	h, cookie := newSnapshotPanel(t, parsedStatus(t, panelSnapshotStatus))
	view := panelSnapshots(t, h, cookie)
	if !view.Reported || !view.Enabled || view.DailyOK || view.Daily == nil || view.LowSpace == nil || view.LowSpace.Deleted != 3 || !view.LowSpace.StillFull {
		t.Fatalf("got %+v", view)
	}
	var got []string
	for _, folder := range view.Folders {
		got = append(got, folder.Kind+":"+folder.Name+":"+folder.Protection)
	}
	want := "family:_family:yes user:amy:yes user:bob:unchecked deleted:cleo.deleted-20261101093000:gone"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	amy := view.Folders[1]
	if amy.Count != 23 || amy.Error != "snapshot_failed" || amy.Held == nil || *amy.Held != 1024 || amy.Oldest == nil || amy.Newest == nil {
		t.Fatalf("amy %+v", amy)
	}
	if bob := view.Folders[2]; bob.Count != 0 || bob.Oldest != nil || bob.Held != nil {
		t.Fatalf("bob %+v", bob)
	}
}

func TestPanelOnlyListsFoldersMovedAsideOnADeleteAsDeleted(t *testing.T) {
	status := "filebox-snapshot 1\nstate on\n" +
		"scope amy unchecked 3 2026-11-05T0030 2026-11-07T0030 - -\n" +
		"scope bob gone 2 2026-11-06T0030 2026-11-07T0030 - -\n" +
		"scope zed gone 1 2026-11-07T0030 2026-11-07T0030 - -\n" +
		"scope cleo.deleted-20261101093000 gone 4 2026-10-20T0030 2026-10-31T0030 - -\n"
	h, cookie := newSnapshotPanel(t, parsedStatus(t, status))
	var got []string
	for _, folder := range panelSnapshots(t, h, cookie).Folders {
		got = append(got, folder.Kind+":"+folder.Name+":"+folder.Protection)
	}
	want := "family:_family:unchecked user:amy:unchecked user:bob:gone deleted:cleo.deleted-20261101093000:gone"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestPanelSaysWhyEarlierVersionsAreOff(t *testing.T) {
	h, cookie := newSnapshotPanel(t, parsedStatus(t, "filebox-snapshot 1\nstate off not_btrfs\nchecked 1794000000\n"))
	view := panelSnapshots(t, h, cookie)
	if !view.Reported || view.Enabled || view.Reason != "not_btrfs" || len(view.Folders) != 0 {
		t.Fatalf("got %+v", view)
	}
}

func TestPanelBeforeTheFirstSnapshotReport(t *testing.T) {
	for name, status := range map[string]func() (system.SnapshotStatus, error){
		"no status file": func() (system.SnapshotStatus, error) { return system.SnapshotStatus{}, system.ErrNoSnapshotStatus },
		"no helper":      nil,
	} {
		h, cookie := newSnapshotPanel(t, status)
		if view := panelSnapshots(t, h, cookie); view.Reported || view.Folders == nil {
			t.Errorf("%s: got %+v", name, view)
		}
	}
}

func TestPanelReportsAnUnreadableSnapshotStatus(t *testing.T) {
	h, cookie := newSnapshotPanel(t, func() (system.SnapshotStatus, error) { return system.SnapshotStatus{}, errors.New("bad line") })
	if rec := h.do("GET", "/api/snapshots", "", cookie); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "bad line") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPanelSnapshotStatusNeedsASession(t *testing.T) {
	h, _ := newSnapshotPanel(t, parsedStatus(t, panelSnapshotStatus))
	if rec := h.do("GET", "/api/snapshots", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}
