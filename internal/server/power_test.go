package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

func waitForActions(t *testing.T, power *system.FakePower, want int) []system.PowerAction {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := power.Actions(); len(got) >= want {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}

	return power.Actions()
}

func TestRestartSSHD(t *testing.T) {
	s := newFileSession(t)
	rec := s.post("/api/system/restart-sshd", map[string]any{"confirm": true})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := s.power.Actions(); len(got) != 1 || got[0] != system.RestartSSHD {
		t.Fatalf("got %v", got)
	}
}

func TestRestartSSHDFailureHidesDetail(t *testing.T) {
	s := newFileSession(t)
	s.power.Err = errors.New("sudo: a password is required")
	rec := s.post("/api/system/restart-sshd", map[string]any{"confirm": true})
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "sudo") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestShutdownAndRebootRunAfterResponse(t *testing.T) {
	for path, want := range map[string]system.PowerAction{
		"/api/system/shutdown": system.Shutdown,
		"/api/system/reboot":   system.Reboot,
	} {
		s := newFileSession(t)
		rec := s.post(path, map[string]any{"confirm": true})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s: got %d %s", path, rec.Code, rec.Body)
		}
		if got := waitForActions(t, s.power, 1); len(got) != 1 || got[0] != want {
			t.Fatalf("%s: got %v", path, got)
		}
	}
}

func TestPowerRequiresConfirmation(t *testing.T) {
	for _, path := range []string{"/api/system/restart-sshd", "/api/system/shutdown", "/api/system/reboot"} {
		s := newFileSession(t)
		for _, fields := range []map[string]any{{}, {"confirm": false}, {"confirm": "yes"}} {
			if rec := s.post(path, fields); rec.Code == http.StatusAccepted || rec.Code == http.StatusNoContent {
				t.Fatalf("%s %v: got %d", path, fields, rec.Code)
			}
		}
		time.Sleep(20 * time.Millisecond)
		if got := s.power.Actions(); len(got) != 0 {
			t.Fatalf("%s ran without confirmation: %v", path, got)
		}
	}
}

func TestPowerRequiresSessionAndJSON(t *testing.T) {
	s := newFileSession(t)
	rec := s.do("POST", "/api/system/reboot", `{"confirm":true}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	rec = s.call("POST", "/api/system/reboot", strings.NewReader("confirm=true"), "application/x-www-form-urlencoded")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", rec.Code)
	}
	if got := s.power.Actions(); len(got) != 0 {
		t.Fatalf("ran: %v", got)
	}
}

func TestPowerRejectsWrongMethod(t *testing.T) {
	s := newFileSession(t)
	if rec := s.get("/api/system/reboot"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d", rec.Code)
	}
}
