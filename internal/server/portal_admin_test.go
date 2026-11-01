package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/auth"
)

const newPassword = "a brand new password"

type adminRoute struct{ method, path, body string }

func adminRoutes() []adminRoute {
	return []adminRoute{
		{"GET", "/admin/users", ""},
		{"GET", "/api/admin/users", ""},
		{"POST", "/api/admin/users", `{"name":"dora","password":"` + newPassword + `"}`},
		{"PUT", "/api/admin/users/cleo/password", `{"password":"` + newPassword + `","confirm":"` + bobPassword + `"}`},
		{"PUT", "/api/admin/users/cleo/role", `{"role":"admin","confirm":"` + bobPassword + `"}`},
		{"DELETE", "/api/admin/users/cleo", `{"confirm":"` + bobPassword + `"}`},
	}
}

// The page pretends not to exist for plain users; the API says no with JSON.
func refusedStatus(route adminRoute) int {
	if route.path == "/admin/users" {
		return http.StatusNotFound
	}

	return http.StatusForbidden
}

func (h *portalHarness) call(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	return h.do(method, path, body, "192.0.2.10", cookie)
}

func (h *portalHarness) sessions(t *testing.T) (amy, bob, cleo *http.Cookie) {
	t.Helper()

	return h.signedIn(t, "amy", amyPassword), h.signedIn(t, "bob", bobPassword), h.signedIn(t, "cleo", cleoPassword)
}

func captureLog(t *testing.T) *strings.Builder {
	t.Helper()
	var logged strings.Builder
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	return &logged
}

func TestAdminRoutesRejectSignedOutRequestsWithoutTouchingAnything(t *testing.T) {
	h := newPortalHarness(t)
	for _, route := range adminRoutes() {
		rec := h.call(route.method, route.path, route.body, nil)
		want := http.StatusUnauthorized
		if route.method == "GET" && route.path == "/admin/users" {
			want = http.StatusSeeOther
		}
		if rec.Code != want {
			t.Errorf("%s %s: got %d, want %d", route.method, route.path, rec.Code, want)
		}
	}
	forged := &http.Cookie{Name: portalCookieName, Value: "forged"}
	for _, route := range adminRoutes()[1:] {
		if rec := h.call(route.method, route.path, route.body, forged); rec.Code != http.StatusUnauthorized {
			t.Errorf("forged cookie %s %s: got %d", route.method, route.path, rec.Code)
		}
	}
	if h.spy.mutations != 0 || h.verifier.Calls != 0 {
		t.Fatalf("a signed-out request reached the backend: %d mutations, %d checks", h.spy.mutations, h.verifier.Calls)
	}
}

func TestAdminRoutesRefusePlainUsersWithoutTouchingAnything(t *testing.T) {
	h := newPortalHarness(t)
	_, bob, _ := h.sessions(t)
	calls := h.verifier.Calls

	for _, route := range adminRoutes() {
		if rec := h.call(route.method, route.path, route.body, bob); rec.Code != refusedStatus(route) {
			t.Errorf("%s %s: got %d, want %d", route.method, route.path, rec.Code, refusedStatus(route))
		}
	}
	if h.spy.mutations != 0 || h.verifier.Calls != calls {
		t.Fatalf("a plain user reached the backend: %d mutations, %d checks", h.spy.mutations, h.verifier.Calls-calls)
	}
	if h.roles.IsAdmin("bob") || h.roles.IsAdmin("cleo") {
		t.Fatal("a plain user changed a role")
	}
}

func TestAdminPageIsServedToAdminsOnly(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	rec := h.call("GET", "/admin/users", "", amy)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/static/admin.js") {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.call("GET", "/static/admin.js", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("admin script got %d", rec.Code)
	}
}

func TestPlainUserSeesTheSameNotFoundPageAsAnyUnknownPath(t *testing.T) {
	h := newPortalHarness(t)
	_, bob, _ := h.sessions(t)

	page := h.call("GET", "/admin/users", "", bob)
	unknown := h.call("GET", "/no/such/page", "", bob)
	if page.Code != http.StatusNotFound || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("got %d %q", page.Code, page.Header().Get("Content-Type"))
	}
	if page.Body.String() != unknown.Body.String() || unknown.Code != http.StatusNotFound {
		t.Fatalf("the page must match any unknown path: %d %q vs %d %q", page.Code, page.Body, unknown.Code, unknown.Body)
	}
	body := strings.ToLower(page.Body.String())
	for _, word := range []string{"admin", "role", "forbidden", "permission"} {
		if strings.Contains(body, word) {
			t.Fatalf("the page must not hint at %q: %s", word, body)
		}
	}
	if !strings.Contains(body, "/static/style.css") || !strings.Contains(body, `href="/"`) || !strings.Contains(body, "viewport") {
		t.Fatalf("the page must use the shared look and link home: %s", body)
	}
}

func TestUnknownPortalPathsAnswerJSONUnderAPIAndHTMLElsewhere(t *testing.T) {
	h := newPortalHarness(t)

	api := h.call("GET", "/api/nothing", "", nil)
	if api.Code != http.StatusNotFound || !strings.HasPrefix(api.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("got %d %q", api.Code, api.Header().Get("Content-Type"))
	}
	for _, path := range []string{"/nothing", "/admin/other", "/Admin/Users"} {
		rec := h.call("GET", path, "", nil)
		if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: got %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestMeReportsTheRole(t *testing.T) {
	h := newPortalHarness(t)
	amy, bob, _ := h.sessions(t)

	for cookie, want := range map[*http.Cookie]bool{amy: true, bob: false} {
		var body struct {
			User  string `json:"user"`
			Admin bool   `json:"admin"`
		}
		rec := h.call("GET", "/api/me", "", cookie)
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Admin != want {
			t.Fatalf("got %s, %v", rec.Body, err)
		}
	}
}

func TestAdminListsUsersWithRoles(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	var body struct {
		Users []struct {
			Name  string `json:"name"`
			Admin bool   `json:"admin"`
		} `json:"users"`
	}
	rec := h.call("GET", "/api/admin/users", "", amy)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if len(body.Users) != 3 || body.Users[0].Name != "amy" || !body.Users[0].Admin || body.Users[1].Admin || body.Users[2].Admin {
		t.Fatalf("got %+v", body.Users)
	}
}

func TestAdminListReportsBackendFailureWithoutDetail(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.spy.ListErr = errors.New("secret detail")

	rec := h.call("GET", "/api/admin/users", "", amy)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAdminCreatesAPlainUser(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	rec := h.call("POST", "/api/admin/users", jsonBody(t, map[string]any{"name": "dora", "password": newPassword}), amy)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.spy.Passwords["dora"] != newPassword || h.roles.IsAdmin("dora") {
		t.Fatal("the new account must exist and be a plain user")
	}
}

func TestAdminCreateClearsAStaleRoleForARecycledName(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	if err := h.roles.Promote("dora"); err != nil {
		t.Fatal(err)
	}

	if rec := h.call("POST", "/api/admin/users", jsonBody(t, map[string]any{"name": "dora", "password": newPassword}), amy); rec.Code != http.StatusCreated {
		t.Fatalf("got %d", rec.Code)
	}
	if h.roles.IsAdmin("dora") {
		t.Fatal("a recycled username must not inherit the old admin role")
	}
}

func TestAdminCreateRejectsBadInput(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	cases := map[string]struct {
		body string
		want int
	}{
		"empty name":         {jsonBody(t, map[string]any{"name": "", "password": newPassword}), 422},
		"whitespace name":    {jsonBody(t, map[string]any{"name": "   ", "password": newPassword}), 422},
		"padded name":        {jsonBody(t, map[string]any{"name": " dora ", "password": newPassword}), 422},
		"uppercase":          {jsonBody(t, map[string]any{"name": "Dora", "password": newPassword}), 422},
		"punctuation only":   {jsonBody(t, map[string]any{"name": "!!!!", "password": newPassword}), 422},
		"two characters":     {jsonBody(t, map[string]any{"name": "do", "password": newPassword}), 422},
		"33 characters":      {jsonBody(t, map[string]any{"name": "d" + strings.Repeat("o", 32), "password": newPassword}), 422},
		"cyrillic lookalike": {jsonBody(t, map[string]any{"name": "d\u043er\u0430", "password": newPassword}), 422},
		"fullwidth":          {jsonBody(t, map[string]any{"name": "ｄｏｒａ", "password": newPassword}), 422},
		"traversal":          {jsonBody(t, map[string]any{"name": "../dora", "password": newPassword}), 422},
		"newline in name":    {jsonBody(t, map[string]any{"name": "dora\n", "password": newPassword}), 422},
		"short password":     {jsonBody(t, map[string]any{"name": "dora", "password": "short"}), 422},
		"long password":      {jsonBody(t, map[string]any{"name": "dora", "password": strings.Repeat("x", 129)}), 422},
		"newline password":   {jsonBody(t, map[string]any{"name": "dora", "password": "twelve chars\nplus"}), 422},
		"missing password":   {`{"name":"dora"}`, 422},
		"missing name":       {`{"password":"` + newPassword + `"}`, 422},
		"name is a number":   {`{"name":5,"password":"` + newPassword + `"}`, 400},
		"password is a list": {`{"name":"dora","password":["x"]}`, 400},
		"not json":           {`{"name":`, 400},
		"huge body":          {`{"name":"dora","password":"` + strings.Repeat("x", 1<<20) + `"}`, 400},
		"existing name":      {jsonBody(t, map[string]any{"name": "bob", "password": newPassword}), 409},
	}
	for name, c := range cases {
		if rec := h.call("POST", "/api/admin/users", c.body, amy); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, c.want)
		}
	}
	if h.spy.mutations != 0 {
		t.Fatalf("bad input reached the backend %d times", h.spy.mutations)
	}
}

func TestAdminCreateWithAReservedNameThatTheHelperRefuses(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.spy.failWith = errors.New("name is reserved")

	for _, name := range []string{"admin", "root"} {
		rec := h.call("POST", "/api/admin/users", jsonBody(t, map[string]any{"name": name, "password": newPassword}), amy)
		if rec.Code != http.StatusInternalServerError || h.roles.IsAdmin(name) {
			t.Fatalf("%s: got %d", name, rec.Code)
		}
	}
}

func TestAdminResetsAPasswordAfterConfirmingTheirOwn(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, cleo := h.sessions(t)

	body := jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/cleo/password", body, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if h.spy.Passwords["cleo"] != newPassword {
		t.Fatal("password not changed")
	}
	if rec := h.call("GET", "/api/me", "", cleo); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the target's sessions must end, got %d", rec.Code)
	}
	if rec := h.call("GET", "/api/me", "", amy); rec.Code != http.StatusOK {
		t.Fatalf("the admin stays signed in, got %d", rec.Code)
	}
}

func TestAdminCannotResetAnAdminsPassword(t *testing.T) {
	h := newPortalHarness(t)
	amy, bob, _ := h.sessions(t)
	_ = h.roles.Promote("bob")
	body := jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword})
	bobBody := jsonBody(t, map[string]any{"password": newPassword, "confirm": bobPassword})

	for name, c := range map[string]struct {
		path, body string
		cookie     *http.Cookie
	}{
		"another admin": {"/api/admin/users/bob/password", body, amy},
		"themselves":    {"/api/admin/users/amy/password", body, amy},
		"the first":     {"/api/admin/users/amy/password", bobBody, bob},
	} {
		rec := h.call("PUT", c.path, c.body, c.cookie)
		if rec.Code != http.StatusForbidden || errorMessage(t, rec.Body.String()) != "admins cannot reset another admin's password" {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body)
		}
	}
	if h.spy.mutations != 0 {
		t.Fatal("an admin's password was changed")
	}
	for _, cookie := range []*http.Cookie{amy, bob} {
		if rec := h.call("GET", "/api/me", "", cookie); rec.Code != http.StatusOK {
			t.Fatalf("a refused reset must not end sessions, got %d", rec.Code)
		}
	}
}

func TestAnAdminDemotedToPlainCanThenBeReset(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	_ = h.roles.Promote("bob")
	body := jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/bob/password", body, amy); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}

	demote := jsonBody(t, map[string]any{"role": "user", "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/bob/role", demote, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.call("PUT", "/api/admin/users/bob/password", body, amy); rec.Code != http.StatusNoContent || h.spy.Passwords["bob"] != newPassword {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAdminActionsNeedTheActingAdminsOwnPassword(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	routes := []struct {
		method, path string
		body         func(confirm string) string
	}{
		{"PUT", "/api/admin/users/cleo/password", func(c string) string { return `{"password":"` + newPassword + `"` + c + `}` }},
		{"PUT", "/api/admin/users/cleo/role", func(c string) string { return `{"role":"admin"` + c + `}` }},
		{"DELETE", "/api/admin/users/cleo", func(c string) string { return `{"unused":1` + c + `}` }},
	}
	confirms := map[string]string{
		"missing":          "",
		"empty":            `,"confirm":""`,
		"wrong":            `,"confirm":"not my password"`,
		"the target's":     `,"confirm":"` + cleoPassword + `"`,
		"another admin's":  `,"confirm":"` + bobPassword + `"`,
		"wrong type":       `,"confirm":5`,
		"a very long one":  `,"confirm":"` + strings.Repeat("x", 200) + `"`,
		"whitespace added": `,"confirm":" ` + amyPassword + `"`,
	}
	ip := 0
	for _, route := range routes {
		h.portal.userLimit.reset("amy")
		for name, confirm := range confirms {
			ip++
			rec := h.do(route.method, route.path, route.body(confirm), fmt.Sprintf("198.51.100.%d", ip), amy)
			want := http.StatusUnauthorized
			if name == "wrong type" {
				want = http.StatusBadRequest
			}
			if rec.Code != want {
				t.Errorf("%s %s with %s confirm: got %d, want %d", route.method, route.path, name, rec.Code, want)
			}
		}
	}
	if h.spy.mutations != 0 || h.roles.IsAdmin("cleo") {
		t.Fatal("an unconfirmed action went through")
	}
}

func TestAdminConfirmationFailuresShareTheLoginRateLimit(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	for range portalIPFails {
		h.call("PUT", "/api/admin/users/cleo/role", `{"role":"admin","confirm":"guess guess guess"}`, amy)
	}
	rec := h.call("PUT", "/api/admin/users/cleo/role", `{"role":"admin","confirm":"`+amyPassword+`"}`, amy)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	if h.roles.IsAdmin("cleo") {
		t.Fatal("a rate limited request must not act")
	}
}

func TestAdminConfirmationReportsAFailedCheckAsAServerError(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.verifier.Err = errors.New("helper missing")

	rec := h.call("DELETE", "/api/admin/users/cleo", `{"confirm":"`+amyPassword+`"}`, amy)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "helper missing") || h.spy.mutations != 0 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestAdminActionsRejectBadTargetsAndBodies(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	ok := `"confirm":"` + amyPassword + `"`

	for name, c := range map[string]struct {
		method, path, body string
		want               int
	}{
		"unknown user password":   {"PUT", "/api/admin/users/ghost/password", `{"password":"` + newPassword + `",` + ok + `}`, 404},
		"unknown user role":       {"PUT", "/api/admin/users/ghost/role", `{"role":"admin",` + ok + `}`, 404},
		"unknown user delete":     {"DELETE", "/api/admin/users/ghost", `{` + ok + `}`, 404},
		"uppercase target":        {"DELETE", "/api/admin/users/Cleo", `{` + ok + `}`, 422},
		"lookalike target":        {"DELETE", "/api/admin/users/cl%D0%B5o", `{` + ok + `}`, 422},
		"too short target":        {"DELETE", "/api/admin/users/ab", `{` + ok + `}`, 422},
		"reserved target":         {"DELETE", "/api/admin/users/root", `{` + ok + `}`, 404},
		"short new password":      {"PUT", "/api/admin/users/cleo/password", `{"password":"short",` + ok + `}`, 422},
		"long new password":       {"PUT", "/api/admin/users/cleo/password", `{"password":"` + strings.Repeat("x", 129) + `",` + ok + `}`, 422},
		"missing new password":    {"PUT", "/api/admin/users/cleo/password", `{` + ok + `}`, 422},
		"role empty":              {"PUT", "/api/admin/users/cleo/role", `{"role":"",` + ok + `}`, 422},
		"role missing":            {"PUT", "/api/admin/users/cleo/role", `{` + ok + `}`, 422},
		"role owner":              {"PUT", "/api/admin/users/cleo/role", `{"role":"owner",` + ok + `}`, 422},
		"role uppercase":          {"PUT", "/api/admin/users/cleo/role", `{"role":"ADMIN",` + ok + `}`, 422},
		"role padded":             {"PUT", "/api/admin/users/cleo/role", `{"role":" admin",` + ok + `}`, 422},
		"role number":             {"PUT", "/api/admin/users/cleo/role", `{"role":1,` + ok + `}`, 400},
		"role boolean":            {"PUT", "/api/admin/users/cleo/role", `{"role":true,` + ok + `}`, 400},
		"role null is missing":    {"PUT", "/api/admin/users/cleo/role", `{"role":null,` + ok + `}`, 422},
		"huge role body":          {"PUT", "/api/admin/users/cleo/role", `{"role":"` + strings.Repeat("a", 1<<20) + `"}`, 400},
		"delete without a body":   {"DELETE", "/api/admin/users/cleo", ``, 415},
		"delete with a list body": {"DELETE", "/api/admin/users/cleo", `[]`, 400},
	} {
		if rec := h.call(c.method, c.path, c.body, amy); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", name, rec.Code, c.want, rec.Body)
		}
	}
	if h.spy.mutations != 0 || h.roles.IsAdmin("cleo") {
		t.Fatal("a rejected request changed something")
	}
}

func TestAdminPromotionAppliesToOpenSessionsImmediately(t *testing.T) {
	h := newPortalHarness(t)
	amy, bob, _ := h.sessions(t)
	if rec := h.call("GET", "/api/admin/users", "", bob); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}

	body := jsonBody(t, map[string]any{"role": "admin", "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/bob/role", body, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.call("GET", "/api/admin/users", "", bob); rec.Code != http.StatusOK {
		t.Fatalf("an open session must gain access at once, got %d", rec.Code)
	}
	if rec := h.call("PUT", "/api/admin/users/bob/role", body, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("promoting an admin again must be harmless, got %d", rec.Code)
	}
}

func TestAdminDemotionAppliesToOpenSessionsImmediately(t *testing.T) {
	h := newPortalHarness(t)
	amy, bob, _ := h.sessions(t)
	_ = h.roles.Promote("bob")
	if rec := h.call("GET", "/api/admin/users", "", bob); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	body := jsonBody(t, map[string]any{"role": "user", "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/bob/role", body, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	for _, route := range adminRoutes() {
		if rec := h.call(route.method, route.path, route.body, bob); rec.Code != refusedStatus(route) {
			t.Errorf("%s %s after demotion: got %d", route.method, route.path, rec.Code)
		}
	}
	if rec := h.call("GET", "/api/me", "", bob); rec.Code != http.StatusOK {
		t.Fatalf("a demoted user keeps their own files session, got %d", rec.Code)
	}
}

func TestAdminCannotDemoteOrDeleteThemselves(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	_ = h.roles.Promote("bob")

	demote := jsonBody(t, map[string]any{"role": "user", "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/amy/role", demote, amy); rec.Code != http.StatusConflict {
		t.Fatalf("self demotion got %d", rec.Code)
	}
	if rec := h.call("DELETE", "/api/admin/users/amy", `{"confirm":"`+amyPassword+`"}`, amy); rec.Code != http.StatusConflict {
		t.Fatalf("self deletion got %d", rec.Code)
	}
	if !h.roles.IsAdmin("amy") || h.spy.mutations != 0 {
		t.Fatal("an admin removed themselves")
	}
	promote := jsonBody(t, map[string]any{"role": "admin", "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/amy/role", promote, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("keeping their own role must be harmless, got %d", rec.Code)
	}
}

type staleAdminRoles struct {
	*auth.Store
	stale string
}

func (s staleAdminRoles) IsAdmin(name string) bool { return name == s.stale || s.Store.IsAdmin(name) }

func TestTheLastAdminSurvivesARaceBetweenTwoAdmins(t *testing.T) {
	h := newPortalHarness(t)
	_, bob, _ := h.sessions(t)
	h.portal.roles = staleAdminRoles{Store: h.roles, stale: "bob"}

	demote := jsonBody(t, map[string]any{"role": "user", "confirm": bobPassword})
	if rec := h.call("PUT", "/api/admin/users/amy/role", demote, bob); rec.Code != http.StatusConflict {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.call("DELETE", "/api/admin/users/amy", `{"confirm":"`+bobPassword+`"}`, bob); rec.Code != http.StatusConflict {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if _, exists := h.spy.Passwords["amy"]; !h.roles.IsAdmin("amy") || h.spy.mutations != 0 || !exists {
		t.Fatal("the last admin was removed")
	}
}

func TestAdminDeletesAnotherAdminWhenOneRemains(t *testing.T) {
	h := newPortalHarness(t)
	amy, bob, _ := h.sessions(t)
	_ = h.roles.Promote("bob")

	if rec := h.call("DELETE", "/api/admin/users/bob", `{"confirm":"`+amyPassword+`"}`, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if _, exists := h.spy.Passwords["bob"]; exists || h.roles.IsAdmin("bob") {
		t.Fatal("account or role still present")
	}
	if rec := h.call("GET", "/api/me", "", bob); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the deleted user's sessions must end, got %d", rec.Code)
	}
}

func TestAdminDeleteOfAPlainUser(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, cleo := h.sessions(t)

	if rec := h.call("DELETE", "/api/admin/users/cleo", `{"confirm":"`+amyPassword+`"}`, amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if _, exists := h.spy.Passwords["cleo"]; exists {
		t.Fatal("account still present")
	}
	if rec := h.call("GET", "/api/me", "", cleo); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestFailedDeleteKeepsTheAdminRole(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	_ = h.roles.Promote("bob")
	h.spy.failWith = errors.New("boom")

	if rec := h.call("DELETE", "/api/admin/users/bob", `{"confirm":"`+amyPassword+`"}`, amy); rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", rec.Code)
	}
	if !h.roles.IsAdmin("bob") {
		t.Fatal("an account that was not deleted lost its role")
	}
}

func TestFailedPasswordResetKeepsTheTargetsSessions(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, cleo := h.sessions(t)
	h.spy.failWith = errors.New("boom")

	body := jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword})
	if rec := h.call("PUT", "/api/admin/users/cleo/password", body, amy); rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.call("GET", "/api/me", "", cleo); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAdminActionsAreLoggedWithoutPasswords(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	logged := captureLog(t)

	h.call("POST", "/api/admin/users", jsonBody(t, map[string]any{"name": "dora", "password": newPassword}), amy)
	h.call("PUT", "/api/admin/users/cleo/role", jsonBody(t, map[string]any{"role": "admin", "confirm": amyPassword}), amy)
	h.call("PUT", "/api/admin/users/bob/password", jsonBody(t, map[string]any{"password": newPassword, "confirm": amyPassword}), amy)
	h.call("DELETE", "/api/admin/users/dora", `{"confirm":"`+amyPassword+`"}`, amy)
	h.call("PUT", "/api/admin/users/bob/role", jsonBody(t, map[string]any{"role": "admin", "confirm": "wrong wrong wrong"}), amy)

	for _, want := range []string{
		`create by="amy" target="dora"`, `promote by="amy" target="cleo"`,
		`reset-password by="amy" target="bob"`, `delete by="amy" target="dora"`,
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logged)
		}
	}
	for _, secret := range []string{newPassword, amyPassword, bobPassword, cleoPassword, "wrong wrong wrong"} {
		if strings.Contains(logged.String(), secret) {
			t.Fatalf("a password reached the log: %s", logged)
		}
	}
	if strings.Contains(logged.String(), `promote by="amy" target="bob"`) {
		t.Fatal("a refused action must not be logged as done")
	}
}

func TestOwnerOnlyRoutesDoNotExistOnThePortalEvenForAdmins(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	for _, path := range []string{
		"/api/users", "/api/users/cleo", "/api/users/cleo/role", "/api/users/cleo/password", "/api/metrics", "/api/disks", "/api/logs",
		"/api/system/reboot", "/api/system/shutdown", "/api/system/restart-sshd", "/logs", "/users", "/files", "/setup",
	} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
			if rec := h.call(method, path, "", amy); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got %d", method, path, rec.Code)
			}
		}
	}
}

func TestAdminWritesAreRefusedCrossOrigin(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)

	req := httptest.NewRequest("DELETE", "https://example.com/api/admin/users/cleo", strings.NewReader(`{"confirm":"`+amyPassword+`"}`))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(amy)
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || h.spy.mutations != 0 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAdminConfirmationUsesTheLimitedVerifierGate(t *testing.T) {
	h := newPortalHarness(t)
	amy, _, _ := h.sessions(t)
	h.portal.verifyWait = 0
	for range portalMaxVerifying {
		h.portal.verifying <- struct{}{}
	}

	rec := h.call("DELETE", "/api/admin/users/cleo", `{"confirm":"`+amyPassword+`"}`, amy)
	if rec.Code != http.StatusServiceUnavailable || h.spy.mutations != 0 {
		t.Fatalf("got %d", rec.Code)
	}
}
