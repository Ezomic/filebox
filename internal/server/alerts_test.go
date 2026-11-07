package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/alerts"
	"github.com/Ezomic/filebox/internal/mail"
)

type alertHarness struct {
	*harness
	monitor *alerts.Monitor
	sender  *mail.Fake
	found   []alerts.Problem
	blind   bool
}

func newAlertHarness(t *testing.T, sender *mail.Fake) *alertHarness {
	t.Helper()
	h := &alertHarness{sender: sender}
	var s mail.Sender
	if sender != nil {
		s = sender
	}
	monitor, err := alerts.Open(t.TempDir(), s, []alerts.Check{func(time.Time) alerts.Result {
		return alerts.Result{Source: "test", Name: "the test disk", Known: !h.blind, Problems: h.found}
	}})
	if err != nil {
		t.Fatal(err)
	}
	h.monitor = monitor
	base := newHarness(t)
	base.handler = New(base.store, Deps{Host: base.host, Users: base.users, Logs: base.logs, Power: base.power, Smart: base.smart,
		Usage: base.usage, Disk: base.disk, Alerts: monitor})
	h.harness = base

	return h
}

func decodeAlerts(t *testing.T, body *bytes.Buffer) alertsView {
	t.Helper()
	var view alertsView
	if err := json.Unmarshal(body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	return view
}

func TestAlertsListTheOpenProblems(t *testing.T) {
	h := newAlertHarness(t, &mail.Fake{})
	cookie := h.signedIn(t)

	view := decodeAlerts(t, h.do("GET", "/api/alerts", "", cookie).Body)
	if view.Checked != nil || len(view.Problems) != 0 || !view.Mail || view.Address != "" {
		t.Fatalf("before the first check: %+v", view)
	}

	h.found = []alerts.Problem{{Key: "disk-full", Subject: "The data disk is 96% full", Detail: "Delete files."}}
	h.monitor.Tick(context.Background())
	view = decodeAlerts(t, h.do("GET", "/api/alerts", "", cookie).Body)
	if view.Checked == nil || len(view.Problems) != 1 || view.Problems[0].Subject != "The data disk is 96% full" || view.Problems[0].Detail != "Delete files." {
		t.Fatalf("got %+v", view)
	}
	if strings.Contains(h.do("GET", "/api/alerts", "", cookie).Body.String(), "disk-full") {
		t.Fatal("internal keys stay inside the panel process")
	}
	if view.Problems[0].Gone != nil || view.Unchecked == nil || len(view.Unchecked) != 0 {
		t.Fatalf("got %+v", view)
	}
}

func TestAlertsListWhatCouldNotBeChecked(t *testing.T) {
	h := newAlertHarness(t, &mail.Fake{})
	cookie := h.signedIn(t)
	h.found = []alerts.Problem{{Key: "disk-full", Subject: "The data disk is 96% full"}}
	h.monitor.Tick(context.Background())

	h.found = nil
	h.monitor.Tick(context.Background())
	view := decodeAlerts(t, h.do("GET", "/api/alerts", "", cookie).Body)
	if len(view.Problems) != 1 || view.Problems[0].Gone == nil {
		t.Fatalf("a problem that went away shows since when, until its hour is up: %+v", view.Problems)
	}

	h.blind = true
	h.monitor.Tick(context.Background())
	view = decodeAlerts(t, h.do("GET", "/api/alerts", "", cookie).Body)
	if len(view.Unchecked) != 1 || view.Unchecked[0].Name != "the test disk" || view.Unchecked[0].Since.IsZero() {
		t.Fatalf("got %+v", view.Unchecked)
	}
}

func TestAlertAddressIsCheckedAndSaved(t *testing.T) {
	h := newAlertHarness(t, &mail.Fake{})
	cookie := h.signedIn(t)

	for _, bad := range []string{"nobody", "two@example.com, three@example.com", "a@b@example.com", strings.Repeat("a", 250) + "@example.com"} {
		body, _ := json.Marshal(alertAddressRequest{Address: bad})
		if rec := h.do("PUT", "/api/alerts/address", string(body), cookie); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%q: got %d", bad, rec.Code)
		}
	}
	if rec := h.do("PUT", "/api/alerts/address", `{"address":"  admin@example.com "}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if view := decodeAlerts(t, h.do("GET", "/api/alerts", "", cookie).Body); view.Address != "admin@example.com" {
		t.Fatalf("got %+v", view)
	}
	if rec := h.do("PUT", "/api/alerts/address", `{"address":""}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("an empty address turns the emails off, got %d", rec.Code)
	}
	if h.monitor.Status().Address != "" {
		t.Fatal("the address was not cleared")
	}
}

func TestAlertTestEmail(t *testing.T) {
	h := newAlertHarness(t, &mail.Fake{})
	cookie := h.signedIn(t)

	if rec := h.do("POST", "/api/alerts/test", "", cookie); rec.Code != http.StatusConflict {
		t.Fatalf("without an address: got %d", rec.Code)
	}
	h.do("PUT", "/api/alerts/address", `{"address":"admin@example.com"}`, cookie)
	if rec := h.do("POST", "/api/alerts/test", "", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if sent := h.sender.Sent(); len(sent) != 1 || sent[0].To != "admin@example.com" {
		t.Fatalf("got %+v", sent)
	}
	h.sender.Err = mail.ErrUnavailable
	if rec := h.do("POST", "/api/alerts/test", "", cookie); rec.Code != http.StatusBadGateway {
		t.Fatalf("a refused email: got %d", rec.Code)
	}

	off := newAlertHarness(t, nil)
	offCookie := off.signedIn(t)
	off.do("PUT", "/api/alerts/address", `{"address":"admin@example.com"}`, offCookie)
	rec := off.do("POST", "/api/alerts/test", "", offCookie)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "mail.json") {
		t.Fatalf("without mail.json: got %d %s", rec.Code, rec.Body)
	}
	if view := decodeAlerts(t, off.do("GET", "/api/alerts", "", offCookie).Body); view.Mail {
		t.Fatal("the dashboard must say email is off")
	}
}

func TestAlertRoutesNeedASession(t *testing.T) {
	h := newAlertHarness(t, &mail.Fake{})
	h.signedIn(t)
	for _, route := range [][2]string{{"GET", "/api/alerts"}, {"PUT", "/api/alerts/address"}, {"POST", "/api/alerts/test"}} {
		if rec := h.do(route[0], route[1], `{"address":"x@example.com"}`, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d", route[0], route[1], rec.Code)
		}
	}
	if rec := h.do("GET", "/settings", "", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("settings page without a session: got %d", rec.Code)
	}
}

func TestSettingsPageAndScriptsAreServed(t *testing.T) {
	h := newAlertHarness(t, nil)
	cookie := h.signedIn(t)
	if rec := h.do("GET", "/settings", "", cookie); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Send test email") {
		t.Fatalf("got %d", rec.Code)
	}
	for _, script := range []string{"/static/settings.js", "/static/problems.js"} {
		if rec := h.do("GET", script, "", nil); rec.Code != http.StatusOK {
			t.Errorf("%s: got %d", script, rec.Code)
		}
	}
}

func TestAlertsWithoutAMonitor(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	if view := decodeAlerts(t, h.do("GET", "/api/alerts", "", cookie).Body); len(view.Problems) != 0 || view.Problems == nil {
		t.Fatalf("got %+v", view)
	}
	if rec := h.do("POST", "/api/alerts/test", "", cookie); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}
