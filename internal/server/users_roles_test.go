package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestOwnerRoleRouteRequiresSession(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	if rec := h.do("PUT", "/api/users/amy/role", `{"role":"admin"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if h.store.IsAdmin("amy") {
		t.Fatal("an unauthenticated request changed a role")
	}
}

func TestOwnerPromotesAndDemotesAnyUser(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	logged := captureLog(t)

	if rec := h.do("PUT", "/api/users/amy/role", `{"role":"admin"}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if !h.store.IsAdmin("amy") || h.store.IsAdmin("bob") {
		t.Fatal("wrong role state")
	}
	if rec := h.do("PUT", "/api/users/amy/role", `{"role":"user"}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if h.store.IsAdmin("amy") {
		t.Fatal("demotion did not apply")
	}
	if !strings.Contains(logged.String(), `owner promote target="amy"`) || !strings.Contains(logged.String(), `owner demote target="amy"`) {
		t.Fatalf("role changes must be logged: %s", logged)
	}
}

func TestOwnerCanRemoveTheLastAdmin(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	_ = h.store.Promote("amy")

	if rec := h.do("PUT", "/api/users/amy/role", `{"role":"user"}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("the owner is the recovery path, got %d", rec.Code)
	}
	if h.store.IsAdmin("amy") {
		t.Fatal("the last admin should be removable by the owner")
	}
}

func TestOwnerRoleRejectsBadInput(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"unknown user":   {"/api/users/ghost/role", `{"role":"admin"}`, 404},
		"bad name":       {"/api/users/Amy/role", `{"role":"admin"}`, 422},
		"lookalike name": {"/api/users/%D0%B0my/role", `{"role":"admin"}`, 422},
		"empty role":     {"/api/users/amy/role", `{"role":""}`, 422},
		"missing role":   {"/api/users/amy/role", `{}`, 422},
		"other role":     {"/api/users/amy/role", `{"role":"owner"}`, 422},
		"uppercase role": {"/api/users/amy/role", `{"role":"Admin"}`, 422},
		"numeric role":   {"/api/users/amy/role", `{"role":1}`, 400},
		"not json":       {"/api/users/amy/role", `{"role":`, 400},
		"huge body":      {"/api/users/amy/role", `{"role":"` + strings.Repeat("a", 1<<20) + `"}`, 400},
		"json list body": {"/api/users/amy/role", `[]`, 400},
	} {
		if rec := h.do("PUT", c.path, c.body, cookie); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, c.want)
		}
	}
	if h.store.IsAdmin("amy") {
		t.Fatal("a rejected request changed a role")
	}
}

func TestPanelListShowsWhoIsAnAdmin(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	_ = h.store.Promote("bob")

	var body struct {
		Users []struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"users"`
	}
	if err := json.Unmarshal(h.do("GET", "/api/users", "", cookie).Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Users) != 2 || body.Users[0].Admin || !body.Users[1].Admin {
		t.Fatalf("got %+v", body.Users)
	}
}

func TestPanelDeleteDropsTheRoleSoARecycledNameStartsPlain(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	_ = h.store.Promote("bob")

	if rec := h.do("DELETE", "/api/users/bob", "", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if h.store.IsAdmin("bob") {
		t.Fatal("the role must go with the account")
	}
}

func TestPanelCreateClearsAStaleRole(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	_ = h.store.Promote("carl")

	body := jsonBody(t, map[string]any{"name": "carl", "password": goodPassword})
	if rec := h.do("POST", "/api/users", body, cookie); rec.Code != http.StatusCreated {
		t.Fatalf("got %d", rec.Code)
	}
	if h.store.IsAdmin("carl") {
		t.Fatal("a recycled username must not inherit the old admin role")
	}
}
