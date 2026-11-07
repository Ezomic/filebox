package server

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
)

func TestHealthzSaysOnlyOkOrUnavailable(t *testing.T) {
	healthy := true
	portal := NewPortal(PortalDeps{Users: newPortalHarness(t).spy, Healthy: func() bool { return healthy }})
	get := func(ip string) (int, string) {
		h := &portalHarness{portal: portal}
		rec := h.do("GET", "/healthz", "", ip, nil)
		return rec.Code, rec.Body.String()
	}
	if code, body := get("192.0.2.1"); code != http.StatusOK || body != "ok" {
		t.Fatalf("got %d %q", code, body)
	}
	healthy = false
	if code, body := get("192.0.2.1"); code != http.StatusServiceUnavailable || body != "" {
		t.Fatalf("an open problem must give a bare 503, got %d %q", code, body)
	}

	for range healthzLimit {
		get("198.51.100.9")
	}
	if code, body := get("198.51.100.9"); code != http.StatusTooManyRequests || body != "" {
		t.Fatalf("got %d %q", code, body)
	}
	if code, _ := get("192.0.2.2"); code != http.StatusServiceUnavailable {
		t.Fatalf("the limit is per address, got %d", code)
	}

	open := NewPortal(PortalDeps{Users: newPortalHarness(t).spy})
	rec := (&portalHarness{portal: open}).do("GET", "/healthz", "", "192.0.2.1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("without alerts it is always ok, got %d", rec.Code)
	}
}

func TestHealthzWinsOverAShareOfTheSameName(t *testing.T) {
	shares, err := auth.OpenShares(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(auth.Share{Name: "healthz", Owner: "amy", Path: "notes.txt", Created: time.Now()}, sharesPerOwner); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	portal := NewPortal(PortalDeps{Users: newPortalHarness(t).spy, Shares: shares, Healthy: func() bool { return true }})
	if !strings.Contains(logged.String(), `share link "healthz" of "amy" no longer opens`) {
		t.Fatalf("the shadowed share must be logged at startup, got %q", logged.String())
	}
	rec := (&portalHarness{portal: portal}).do("GET", "/healthz", "", "192.0.2.1", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	h := newShareHarness(t)
	if rec := h.create(h.amy, "healthz", "notes.txt", ""); !strings.Contains(rec.Body.String(), codeShareNameReserved) {
		t.Fatalf("a new share cannot take the name: %s", rec.Body)
	}
}
