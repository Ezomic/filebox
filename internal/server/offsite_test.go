package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

const panelOffsiteStatus = `filebox-backup 1
checked 1794000000
state on
destination sftp u123456.your-storagebox.de
setup 1791000000
run 1793926800 5400 incomplete 734003200
unreadable 1793840400 2
latest 1793926805
size 1793932300 412316860416
check 1791334800 ok
checkerror 1793926900 1
`

func newOffsitePanel(t *testing.T, status func() (system.BackupStatus, error)) (*harness, *http.Cookie) {
	t.Helper()
	h := newHarness(t)
	h.handler = New(h.store, Deps{Host: h.host, Users: h.users, Logs: h.logs, Power: h.power, Smart: h.smart, Usage: h.usage, Disk: h.disk, Offsite: status})

	return h, h.signedIn(t)
}

func panelOffsite(t *testing.T, h *harness, cookie *http.Cookie) (offsiteView, string) {
	t.Helper()
	rec := h.do("GET", "/api/offsite", "", cookie)
	var view offsiteView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return view, rec.Body.String()
}

func TestPanelShowsTheOffsiteBackup(t *testing.T) {
	status, err := system.ParseBackupStatus(strings.NewReader(panelOffsiteStatus))
	if err != nil {
		t.Fatal(err)
	}
	h, cookie := newOffsitePanel(t, func() (system.BackupStatus, error) { return status, nil })
	view, body := panelOffsite(t, h, cookie)
	if !view.Installed || !view.Reported || !view.Configured || view.Kind != "sftp" || view.Host != "u123456.your-storagebox.de" ||
		view.Latest == nil || view.Checked == nil {
		t.Fatalf("got %+v", view)
	}
	if run := view.Run; run == nil || run.Seconds != 5400 || run.Result != "incomplete" || run.Added == nil || *run.Added != 734003200 {
		t.Fatalf("run %+v", view.Run)
	}
	if view.Size == nil || view.Size.Bytes != 412316860416 || view.Check == nil || !view.Check.OK {
		t.Fatalf("size %+v check %+v", view.Size, view.Check)
	}
	if view.Setup == nil || view.UnreadableNights != 2 || view.CheckErrorNights != 1 {
		t.Fatalf("setup %v, unreadable %d, check errors %d", view.Setup, view.UnreadableNights, view.CheckErrorNights)
	}
	if strings.Contains(body, "u123456@") || strings.Contains(body, "password") {
		t.Fatalf("the panel must not see more than the host: %s", body)
	}
}

func TestPanelOffsiteBackupBeforeSetupAndWithoutTheAddOn(t *testing.T) {
	h, cookie := newOffsitePanel(t, func() (system.BackupStatus, error) { return system.BackupStatus{}, system.ErrNoBackupStatus })
	if view, _ := panelOffsite(t, h, cookie); !view.Installed || view.Reported || view.Run != nil {
		t.Fatalf("no report yet: %+v", view)
	}

	h, cookie = newOffsitePanel(t, nil)
	if view, body := panelOffsite(t, h, cookie); view.Installed || view.Reported || !strings.Contains(body, `"run":null`) {
		t.Fatalf("without the add-on: %s", body)
	}

	h, cookie = newOffsitePanel(t, func() (system.BackupStatus, error) { return system.BackupStatus{}, errors.New("unreadable") })
	if rec := h.do("GET", "/api/offsite", "", cookie); rec.Code != http.StatusInternalServerError {
		t.Fatalf("an unreadable status: got %d", rec.Code)
	}
}

func TestPanelOffsiteBackupNeedsASessionAndServesItsScript(t *testing.T) {
	h, _ := newOffsitePanel(t, nil)
	if rec := h.do("GET", "/api/offsite", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/static/offsite.js", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("offsite.js: got %d", rec.Code)
	}
}
