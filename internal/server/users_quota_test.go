package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

type quotaUserView struct {
	Name       string  `json:"name"`
	Bytes      *uint64 `json:"bytes"`
	Quota      *uint64 `json:"quota"`
	QuotaReady bool    `json:"quotaReady"`
}

func listQuotaUsers(t *testing.T, h *harness, cookie *http.Cookie) (users []quotaUserView, quotas bool) {
	t.Helper()
	rec := h.do("GET", "/api/users", "", cookie)
	var body struct {
		Users  []quotaUserView `json:"users"`
		Quotas bool            `json:"quotas"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return body.Users, body.Quotas
}

func TestQuotaRoutesRequireSession(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	for _, route := range []struct{ method, path, body string }{
		{"PUT", "/api/users/amy/quota", `{"bytes":10485760}`},
		{"DELETE", "/api/users/amy/quota", ""},
	} {
		if rec := h.do(route.method, route.path, route.body, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d", route.method, route.path, rec.Code)
		}
	}
	if h.users.mutations != 0 {
		t.Fatal("unauthenticated request reached Users")
	}
}

func TestOwnerSetsShowsAndClearsAQuota(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	h.users.SetUsed("amy", 3<<20)

	users, quotas := listQuotaUsers(t, h, cookie)
	if !quotas || users[0].Quota != nil || !users[0].QuotaReady || users[0].Bytes == nil || *users[0].Bytes != 3<<20 {
		t.Fatalf("a new user has no quota and the usage comes from the filesystem: %+v quotas=%v", users[0], quotas)
	}

	if rec := h.do("PUT", "/api/users/amy/quota", `{"bytes":10485760}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	users, _ = listQuotaUsers(t, h, cookie)
	if users[0].Quota == nil || *users[0].Quota != 10<<20 || users[1].Quota != nil {
		t.Fatalf("got %+v", users)
	}

	if rec := h.do("DELETE", "/api/users/amy/quota", "", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if users, _ = listQuotaUsers(t, h, cookie); users[0].Quota != nil {
		t.Fatalf("got %+v", users)
	}
}

func TestPanelRefusesBadQuotaValues(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	bodies := map[string]int{
		`{}`:                            http.StatusUnprocessableEntity,
		`{"bytes":null}`:                http.StatusUnprocessableEntity,
		`{"bytes":0}`:                   http.StatusUnprocessableEntity,
		`{"bytes":-1}`:                  http.StatusUnprocessableEntity,
		`{"bytes":1048575}`:             http.StatusUnprocessableEntity,
		`{"bytes":1125899906842625}`:    http.StatusUnprocessableEntity,
		`{"bytes":9223372036854775808}`: http.StatusBadRequest,
		`{"bytes":1e9}`:                 http.StatusBadRequest,
		`{"bytes":"10GB"}`:              http.StatusBadRequest,
		`{"bytes":"10485760"}`:          http.StatusBadRequest,
		`{"bytes":10485760.5}`:          http.StatusBadRequest,
		`{"bytes":010485760}`:           http.StatusBadRequest,
		`{"bytes":١٠٤٨٥٧٦٠}`:            http.StatusBadRequest,
	}
	for body, want := range bodies {
		if rec := h.do("PUT", "/api/users/amy/quota", body, cookie); rec.Code != want {
			t.Errorf("%s: got %d, want %d", body, rec.Code, want)
		}
	}
	if rec := h.do("PUT", "/api/users/amy/quota", "", cookie); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no body: got %d", rec.Code)
	}
	if h.users.mutations != 0 {
		t.Fatal("a refused value reached the helper")
	}
}

func TestPanelQuotaNeedsAnExistingValidUser(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	for _, name := range []string{"ghost", "root", "..%2Fetc", "A", "-rf"} {
		rec := h.do("PUT", "/api/users/"+name+"/quota", `{"bytes":10485760}`, cookie)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d", name, rec.Code)
		}
		rec = h.do("DELETE", "/api/users/"+name+"/quota", "", cookie)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	if h.users.mutations != 0 {
		t.Fatal("an unknown user reached the helper")
	}
}

func TestPanelQuotaWhenTheBoxHasNoQuotas(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	h.users.QuotasOff = true

	users, quotas := listQuotaUsers(t, h, cookie)
	if quotas || len(users) != 2 || users[0].QuotaReady || users[0].Quota != nil {
		t.Fatalf("the page must still list users: %+v quotas=%v", users, quotas)
	}
	rec := h.do("PUT", "/api/users/amy/quota", `{"bytes":10485760}`, cookie)
	if rec.Code != http.StatusConflict || !strings.Contains(errorMessage(t, rec.Body.String()), "quotas") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPanelQuotaHelperFailureHidesDetail(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	h.users.failWith = errors.New("sudo: secret detail")

	rec := h.do("PUT", "/api/users/amy/quota", `{"bytes":10485760}`, cookie)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPanelLogsQuotaChanges(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	logged := captureLog(t)

	h.do("PUT", "/api/users/amy/quota", `{"bytes":10485760}`, cookie)
	h.do("DELETE", "/api/users/amy/quota", "", cookie)

	for _, want := range []string{"panel: owner set-quota target=\"amy\" bytes=10485760", "panel: owner clear-quota target=\"amy\""} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("missing %q in %q", want, logged)
		}
	}
}
