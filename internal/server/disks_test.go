package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
)

func TestDisksReportsHealth(t *testing.T) {
	s := newFileSession(t)
	rec := s.get("/api/disks")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var body struct {
		Available bool `json:"available"`
		Disks     []struct {
			Device  string `json:"device"`
			Healthy *bool  `json:"healthy"`
		} `json:"disks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Available || len(body.Disks) != 2 || body.Disks[1].Healthy == nil || *body.Disks[1].Healthy {
		t.Fatalf("got %s", rec.Body)
	}
}

func TestDisksDegradesWhenSmartIsMissing(t *testing.T) {
	s := newFileSession(t)
	s.smart.Err = errors.New("smartctl is not installed")
	rec := s.get("/api/disks")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var body struct {
		Available bool  `json:"available"`
		Disks     []any `json:"disks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Available || body.Disks == nil {
		t.Fatalf("got %s %v", rec.Body, err)
	}
}

func TestDisksRequireSession(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("GET", "/api/disks", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestUsersIncludeDiskUsage(t *testing.T) {
	s := newFileSession(t)
	var body struct {
		Ready bool `json:"usageReady"`
		Users []struct {
			Name  string  `json:"name"`
			Bytes *uint64 `json:"bytes"`
		} `json:"users"`
	}
	if err := json.Unmarshal(s.get("/api/users").Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Ready || len(body.Users) != 2 || body.Users[0].Bytes == nil || *body.Users[0].Bytes != 2048 || body.Users[1].Bytes != nil {
		t.Fatalf("got %+v", body)
	}

	s.usage.Ready = false
	if err := json.Unmarshal(s.get("/api/users").Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Ready {
		t.Fatal("usageReady must be false before the first scan")
	}
}
