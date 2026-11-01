package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

func TestPanelStorageRequiresASession(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("GET", "/api/storage", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPanelStorageReportsTheWholeDiskAtEveryLevel(t *testing.T) {
	cases := []struct {
		used uint64
		want system.StorageLevel
	}{
		{8499, system.LevelOK},
		{8500, system.LevelWarning},
		{9499, system.LevelWarning},
		{9500, system.LevelCritical},
		{9799, system.LevelCritical},
		{9800, system.LevelFull},
	}
	for _, c := range cases {
		s := newAdminSession(t)
		s.disk.Set(diskAt(c.used))
		var body diskView
		rec := s.get("/api/storage")
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
		if body.Level != c.want || body.Used != c.used || body.Total != 10000 || body.Free != 10000-c.used {
			t.Errorf("used %d: got %+v, want level %s", c.used, body, c.want)
		}
	}
}

func TestPanelStorageHidesAFailingStatfs(t *testing.T) {
	s := newAdminSession(t)
	s.disk.Fail(errors.New("statfs /srv: secret detail"))
	rec := s.get("/api/storage")
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
