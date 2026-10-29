package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

type spyUsers struct {
	*system.FakeUsers
	mutations int
	failWith  error
}

func (s *spyUsers) Create(name, password string) error {
	s.mutations++
	if s.failWith != nil {
		return s.failWith
	}

	return s.FakeUsers.Create(name, password)
}

func (s *spyUsers) SetPassword(name, password string) error {
	s.mutations++
	if s.failWith != nil {
		return s.failWith
	}

	return s.FakeUsers.SetPassword(name, password)
}

func (s *spyUsers) Delete(name string) error {
	s.mutations++
	if s.failWith != nil {
		return s.failWith
	}

	return s.FakeUsers.Delete(name)
}

const goodPassword = "a perfectly fine password"

func jsonBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

func errorMessage(t *testing.T, body string) string {
	t.Helper()
	var decoded map[string]string
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("not an error body: %q", body)
	}

	return decoded["error"]
}

func TestUsersRoutesRequireSession(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	routes := []struct{ method, path, body string }{
		{"GET", "/api/users", ""},
		{"POST", "/api/users", `{"name":"carl","password":"` + goodPassword + `"}`},
		{"PUT", "/api/users/amy/password", `{"password":"` + goodPassword + `"}`},
		{"DELETE", "/api/users/amy", ""},
	}
	for _, route := range routes {
		if rec := h.do(route.method, route.path, route.body, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d", route.method, route.path, rec.Code)
		}
	}
	if h.users.mutations != 0 {
		t.Fatal("unauthenticated request reached Users")
	}
}

func TestUsersPageRedirects(t *testing.T) {
	h := newHarness(t)
	if rec := h.do("GET", "/users", "", nil); rec.Header().Get("Location") != "/setup" {
		t.Fatalf("before setup: got %d %q", rec.Code, rec.Header().Get("Location"))
	}

	cookie := h.signedIn(t)
	if rec := h.do("GET", "/users", "", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("signed out: got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := h.do("GET", "/users", "", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/static/users.js") {
		t.Fatalf("signed in: got %d", rec.Code)
	}
}

func TestListUsers(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("GET", "/api/users", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var body struct {
		Users []struct {
			Name string `json:"name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Users) != 2 || body.Users[0].Name != "amy" || body.Users[1].Name != "bob" {
		t.Fatalf("got %+v", body)
	}
}

func TestListUsersWhenEmptyIsAnArray(t *testing.T) {
	h := newHarness(t)
	h.users.FakeUsers = system.NewFakeUsers()
	cookie := h.signedIn(t)

	if body := h.do("GET", "/api/users", "", cookie).Body.String(); !strings.Contains(body, `"users":[]`) {
		t.Fatalf("got %s", body)
	}
}

func TestListUsersReportsHelperFailureWithoutDetail(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	h.users.ListErr = errors.New("sudo: secret detail")

	rec := h.do("GET", "/api/users", "", cookie)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestCreateUser(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": "carl", "password": goodPassword}), cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), goodPassword) {
		t.Fatal("password echoed")
	}
	if h.users.Passwords["carl"] != goodPassword {
		t.Fatal("password not passed to Users")
	}
}

func TestCreateUserConflictAndExtraFields(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": "amy", "password": goodPassword}), cookie)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d", rec.Code)
	}
	if h.users.mutations != 0 {
		t.Fatal("duplicate reached Users")
	}

	rec = h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": "dave", "password": goodPassword, "admin": true, "uid": 0}), cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("extra fields: got %d", rec.Code)
	}
}

func TestCreateUserRejectsBadNames(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	names := []string{
		"", " ", "   ", "---", "!!!", "a", "ab", strings.Repeat("a", 33),
		"Alice", "AMY", " amy", "amy ", "amy\n", "1abc", "-abc", "_abc",
		"../etc", "..", "a/b", "ab\x00c", "alicé", "аlice", "ａｌｉｃｅ", "root;id", "a$b", "a'b",
	}
	for _, name := range names {
		rec := h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": name, "password": goodPassword}), cookie)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("name %q: got %d", name, rec.Code)
		}
	}
	if h.users.mutations != 0 {
		t.Fatal("invalid name reached Users")
	}
}

func TestCreateUserAcceptsBoundaryNames(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	for _, name := range []string{"abc", strings.Repeat("z", 32), "a-_9"} {
		rec := h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": name, "password": goodPassword}), cookie)
		if rec.Code != http.StatusCreated {
			t.Errorf("name %q: got %d", name, rec.Code)
		}
	}
}

func TestPasswordBoundaries(t *testing.T) {
	cases := []struct {
		label    string
		password string
		valid    bool
	}{
		{"empty", "", false},
		{"11", strings.Repeat("a", 11), false},
		{"12", strings.Repeat("a", 12), true},
		{"128", strings.Repeat("a", 128), true},
		{"129", strings.Repeat("a", 129), false},
		{"12 multibyte runes", strings.Repeat("é", 12), true},
		{"11 multibyte runes", strings.Repeat("é", 11), false},
		{"nul", "abcdefghijk\x00l", false},
		{"newline", "abcdefghijkl\nmore", false},
		{"carriage return", "abcdefghijkl\rmore", false},
		{"quotes and dollar", `it's "$HOME" ` + "`id`;", true},
		{"whitespace only", strings.Repeat(" ", 12), true},
	}
	for i, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			h := newHarness(t)
			cookie := h.signedIn(t)
			name := fmt.Sprintf("user%d", i)
			wantCreate, wantChange := http.StatusCreated, http.StatusNoContent
			if !tc.valid {
				wantCreate, wantChange = http.StatusUnprocessableEntity, http.StatusUnprocessableEntity
			}

			rec := h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": name, "password": tc.password}), cookie)
			if rec.Code != wantCreate {
				t.Fatalf("create: got %d: %s", rec.Code, rec.Body)
			}
			if tc.valid && h.users.Passwords[name] != tc.password {
				t.Fatal("password altered on the way")
			}

			rec = h.do("PUT", "/api/users/amy/password", jsonBody(t, map[string]any{"password": tc.password}), cookie)
			if rec.Code != wantChange {
				t.Fatalf("change: got %d: %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestUserBodiesRejectMalformedJSON(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	bodies := []string{
		``, `null`, `[]`, `"x"`, `{`, `not json`,
		`{"name":"carl","password":12345678901234}`,
		`{"name":"carl","password":["x"]}`,
		`{"name":"carl","password":null}`,
		`{"password":{"a":1}}`,
		`{"name":"carl"}`,
		`{}`,
		`{"password":["x"]}`,
		`{"password":"` + strings.Repeat("a", 5000) + `"}`,
	}
	for _, body := range bodies {
		for _, path := range []string{"/api/users", "/api/users/amy/password"} {
			method := "POST"
			if strings.HasSuffix(path, "/password") {
				method = "PUT"
			}
			if rec := h.do(method, path, body, cookie); rec.Code < 400 || rec.Code >= 500 {
				t.Errorf("%s %.40q: got %d", method, body, rec.Code)
			}
		}
	}
	if h.users.mutations != 0 {
		t.Fatal("malformed input reached Users")
	}
}

func TestCreateUserRejectsWrongNameType(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	for _, name := range []string{`5`, `["carl"]`, `null`, `{"a":1}`, `true`} {
		rec := h.do("POST", "/api/users", `{"name":`+name+`,"password":"`+goodPassword+`"}`, cookie)
		if rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("name %s: got %d", name, rec.Code)
		}
	}
	if h.users.mutations != 0 {
		t.Fatal("malformed input reached Users")
	}
}

func TestUserBodiesRequireJSONContentType(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	req := httptest.NewRequest("POST", "/api/users", strings.NewReader("name=carl&password="+goodPassword))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType || h.users.mutations != 0 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestChangePassword(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("PUT", "/api/users/amy/password", jsonBody(t, map[string]any{"password": goodPassword}), cookie)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if h.users.Passwords["amy"] != goodPassword {
		t.Fatal("password not passed to Users")
	}

	rec = h.do("PUT", "/api/users/ghost/password", jsonBody(t, map[string]any{"password": goodPassword}), cookie)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user: got %d", rec.Code)
	}
}

func TestDeleteUser(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	if rec := h.do("DELETE", "/api/users/amy", "", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	names, _ := h.users.List()
	if !slices.Equal(names, []string{"bob"}) {
		t.Fatalf("got %v", names)
	}
	if rec := h.do("DELETE", "/api/users/amy", "", cookie); rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: got %d", rec.Code)
	}
}

func TestPathNamesAreValidatedBeforeUsers(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	paths := []string{
		"/api/users/..", "/api/users/%2e%2e", "/api/users/%2E%2E%2Fetc", "/api/users/a%2Fb", "/api/users/..%2f..%2fetc",
		"/api/users/AMY", "/api/users/Amy", "/api/users/1abc", "/api/users/-rf", "/api/users/a", "/api/users/%20",
		"/api/users/am%00y", "/api/users/am%0ay", "/api/users/" + strings.Repeat("a", 33), "/api/users/al%C3%A9", "/api/users/amy%20",
	}
	for _, path := range paths {
		for _, method := range []string{"DELETE", "PUT"} {
			target, body := path, ""
			if method == "PUT" {
				target += "/password"
				body = jsonBody(t, map[string]any{"password": goodPassword})
			}
			if rec := h.do(method, target, body, cookie); rec.Code < 300 || rec.Code == http.StatusNoContent {
				t.Errorf("%s %s: got %d", method, target, rec.Code)
			}
		}
	}
	if h.users.mutations != 0 {
		t.Fatalf("%d invalid paths reached Users", h.users.mutations)
	}
	names, _ := h.users.List()
	if !slices.Equal(names, []string{"amy", "bob"}) {
		t.Fatalf("users changed: %v", names)
	}
}

func TestInvalidPathNameIsUnprocessable(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	if rec := h.do("DELETE", "/api/users/AMY", "", cookie); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestHelperFailuresAreMappedToSafeMessages(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	h.users.failWith = errors.New("useradd: secret internal detail")

	requests := []struct{ method, path, body string }{
		{"POST", "/api/users", jsonBody(t, map[string]any{"name": "carl", "password": goodPassword})},
		{"PUT", "/api/users/amy/password", jsonBody(t, map[string]any{"password": goodPassword})},
		{"DELETE", "/api/users/amy", ""},
	}
	for _, request := range requests {
		rec := h.do(request.method, request.path, request.body, cookie)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: got %d", request.method, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), goodPassword) {
			t.Errorf("%s leaked detail: %s", request.method, rec.Body)
		}
		if errorMessage(t, rec.Body.String()) == "" {
			t.Errorf("%s: no error message", request.method)
		}
	}
}

func TestPasswordIsNeverLogged(t *testing.T) {
	var logged strings.Builder
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)

	h := newHarness(t)
	cookie := h.signedIn(t)
	h.users.failWith = errors.New("boom")
	h.do("POST", "/api/users", jsonBody(t, map[string]any{"name": "carl", "password": goodPassword}), cookie)

	if logged.Len() == 0 {
		t.Fatal("expected the failure to be logged")
	}
	if strings.Contains(logged.String(), goodPassword) {
		t.Fatalf("password in log: %s", logged.String())
	}
}
