package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	amyPassword = "amy's long password"
	bobPassword = "bob's long password"
)

type portalHarness struct {
	portal   *Portal
	verifier *system.FakeVerifier
}

func newPortalHarness(t *testing.T) *portalHarness {
	t.Helper()
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword}}

	return &portalHarness{portal: NewPortal(PortalDeps{Verifier: verifier}), verifier: verifier}
}

func (h *portalHarness) do(method, path, body, ip string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = ip + ":5555"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	return rec
}

func (h *portalHarness) login(user, password, ip string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/login", `{"username":"`+user+`","password":"`+password+`"}`, ip, nil)
}

func (h *portalHarness) signedIn(t *testing.T, user, password string) *http.Cookie {
	t.Helper()
	rec := h.login(user, password, "192.0.2.10")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login got %d: %s", rec.Code, rec.Body)
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == portalCookieName {
			return cookie
		}
	}
	t.Fatal("no portal cookie")

	return nil
}

func TestPortalLoginStartsAUserSession(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.signedIn(t, "amy", amyPassword)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie must be HttpOnly, Secure and SameSite=Strict: %+v", cookie)
	}

	rec := h.do("GET", "/api/me", "", "192.0.2.10", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"amy"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPortalSessionsBelongToTheirOwnUser(t *testing.T) {
	h := newPortalHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)

	if body := h.do("GET", "/api/me", "", "192.0.2.10", amy).Body.String(); !strings.Contains(body, `"amy"`) {
		t.Fatalf("got %s", body)
	}
	if body := h.do("GET", "/api/me", "", "192.0.2.10", bob).Body.String(); !strings.Contains(body, `"bob"`) {
		t.Fatalf("got %s", body)
	}
}

func TestPortalRejectsWrongCredentialsWithTheSameMessage(t *testing.T) {
	h := newPortalHarness(t)
	cases := map[string][2]string{
		"wrong password":  {"amy", "not the password"},
		"unknown user":    {"nobody", amyPassword},
		"bad name format": {"Amy", amyPassword},
		"traversal name":  {"../amy", amyPassword},
		"empty":           {"", ""},
	}
	var messages []string
	for name, creds := range cases {
		rec := h.login(creds[0], creds[1], "192.0.2.10")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d", name, rec.Code)
		}
		messages = append(messages, rec.Body.String())
		h.portal.ipLimit.reset("192.0.2.10")
	}
	for _, message := range messages {
		if message != messages[0] {
			t.Fatalf("responses must not reveal which part was wrong: %q vs %q", message, messages[0])
		}
	}
}

func TestPortalNeverChecksPasswordsForInvalidUsernames(t *testing.T) {
	h := newPortalHarness(t)
	h.login("../etc/passwd", "whatever password", "192.0.2.10")
	h.login("a b", "whatever password", "192.0.2.10")
	if h.verifier.Calls != 0 {
		t.Fatalf("verifier called %d times for invalid names", h.verifier.Calls)
	}
}

func TestPortalBlocksAnIPAfterRepeatedFailures(t *testing.T) {
	h := newPortalHarness(t)
	for range portalIPFails {
		h.login("amy", "wrong password here", "192.0.2.50")
	}
	if rec := h.login("amy", amyPassword, "192.0.2.50"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.login("amy", amyPassword, "192.0.2.51"); rec.Code != http.StatusNoContent {
		t.Fatalf("another IP must be unaffected, got %d", rec.Code)
	}
}

func TestPortalBlocksAUserAttackedFromManyIPs(t *testing.T) {
	h := newPortalHarness(t)
	for i := range portalUserFails {
		h.login("amy", "wrong password here", fmt.Sprintf("198.51.100.%d", i+1))
	}
	if rec := h.login("amy", amyPassword, "203.0.113.9"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.login("bob", bobPassword, "203.0.113.9"); rec.Code != http.StatusNoContent {
		t.Fatalf("other users must be unaffected, got %d", rec.Code)
	}
}

func TestPortalSuccessfulLoginDoesNotResetTheIPCounter(t *testing.T) {
	h := newPortalHarness(t)
	for range portalIPFails - 1 {
		h.login("amy", "wrong password here", "192.0.2.60")
	}
	if rec := h.login("bob", bobPassword, "192.0.2.60"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	h.login("amy", "wrong password here", "192.0.2.60")
	if rec := h.login("bob", bobPassword, "192.0.2.60"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("an own account must not buy more guesses at others, got %d", rec.Code)
	}
}

func TestPortalOnlyFailuresCountAgainstTheLimits(t *testing.T) {
	h := newPortalHarness(t)
	for range 3 * portalIPFails {
		if rec := h.login("amy", amyPassword, "192.0.2.61"); rec.Code != http.StatusNoContent {
			t.Fatalf("successful logins must never be limited, got %d", rec.Code)
		}
	}
}

func TestPortalSuccessResetsTheUserCounter(t *testing.T) {
	h := newPortalHarness(t)
	for i := range portalUserFails - 1 {
		h.login("amy", "wrong password here", fmt.Sprintf("198.51.100.%d", i+1))
	}
	h.login("amy", amyPassword, "203.0.113.5")
	for i := range portalUserFails - 1 {
		h.login("amy", "wrong password here", fmt.Sprintf("198.51.101.%d", i+1))
	}
	if rec := h.login("amy", amyPassword, "203.0.113.5"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPortalBadUsernamesCountAgainstTheIP(t *testing.T) {
	h := newPortalHarness(t)
	for range portalIPFails {
		h.login("../x", "whatever password", "192.0.2.62")
	}
	if rec := h.login("amy", amyPassword, "192.0.2.62"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPortalLimitsIPv6PerPrefix(t *testing.T) {
	h := newPortalHarness(t)
	for i := range portalIPFails {
		h.login("amy", "wrong password here", fmt.Sprintf("[2001:db8:1:2::%x]", i+1))
	}
	if rec := h.login("amy", amyPassword, "[2001:db8:1:2:ffff::1]"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the whole /64 shares one limit, got %d", rec.Code)
	}
	if rec := h.login("amy", amyPassword, "[2001:db8:1:3::1]"); rec.Code != http.StatusNoContent {
		t.Fatalf("another /64 is unaffected, got %d", rec.Code)
	}
}

func TestPortalSessionCannotStartIfTheAccountChangedMidLogin(t *testing.T) {
	racing := &racingVerifier{}
	h := &portalHarness{portal: NewPortal(PortalDeps{Verifier: racing})}
	racing.portal = h.portal

	if rec := h.login("amy", amyPassword, "192.0.2.10"); rec.Code != http.StatusConflict {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.login("amy", amyPassword, "192.0.2.10"); rec.Code != http.StatusNoContent {
		t.Fatalf("a retry after the change must work, got %d", rec.Code)
	}
}

type racingVerifier struct {
	portal *Portal
	calls  int
}

func (r *racingVerifier) Verify(string, string) (bool, error) {
	r.calls++
	if r.calls == 1 {
		r.portal.EndSessions("amy")
	}

	return true, nil
}

func TestPortalReportsAFailedPasswordCheckAsAServerError(t *testing.T) {
	h := newPortalHarness(t)
	h.verifier.Err = errors.New("helper missing")
	rec := h.login("amy", amyPassword, "192.0.2.10")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "helper missing") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

type blockingVerifier struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingVerifier) Verify(string, string) (bool, error) {
	b.started <- struct{}{}
	<-b.release

	return true, nil
}

func TestPortalLimitsConcurrentPasswordChecks(t *testing.T) {
	blocker := &blockingVerifier{started: make(chan struct{}, portalMaxVerifying), release: make(chan struct{})}
	h := &portalHarness{portal: NewPortal(PortalDeps{Verifier: blocker})}
	h.portal.verifyWait = 50 * time.Millisecond

	done := make(chan struct{})
	for i := range portalMaxVerifying {
		go func() {
			h.login("amy", amyPassword, fmt.Sprintf("192.0.2.%d", i+1))
			done <- struct{}{}
		}()
	}
	for range portalMaxVerifying {
		<-blocker.started
	}

	if rec := h.login("bob", bobPassword, "192.0.2.99"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
	close(blocker.release)
	for range portalMaxVerifying {
		<-done
	}
}

func TestPortalLogoutEndsTheSession(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.signedIn(t, "amy", amyPassword)
	if rec := h.do("POST", "/api/logout", "", "192.0.2.10", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPortalEndSessionsSignsAUserOutEverywhere(t *testing.T) {
	h := newPortalHarness(t)
	first := h.signedIn(t, "amy", amyPassword)
	second := h.signedIn(t, "amy", amyPassword)
	other := h.signedIn(t, "bob", bobPassword)

	h.portal.EndSessions("amy")

	for _, cookie := range []*http.Cookie{first, second} {
		if rec := h.do("GET", "/api/me", "", "192.0.2.10", cookie); rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d", rec.Code)
		}
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", other); rec.Code != http.StatusOK {
		t.Fatalf("other users stay signed in, got %d", rec.Code)
	}
}

func TestPortalRequiresASession(t *testing.T) {
	h := newPortalHarness(t)
	for _, path := range []string{"/api/me"} {
		if rec := h.do("GET", path, "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d", path, rec.Code)
		}
	}
	if rec := h.do("POST", "/api/logout", "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", &http.Cookie{Name: portalCookieName, Value: "forged"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPortalPagesFollowTheSession(t *testing.T) {
	h := newPortalHarness(t)
	if rec := h.do("GET", "/", "", "192.0.2.10", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := h.do("GET", "/login", "", "192.0.2.10", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	cookie := h.signedIn(t, "amy", amyPassword)
	if rec := h.do("GET", "/", "", "192.0.2.10", cookie); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/login", "", "192.0.2.10", cookie); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPortalHasNoAdminRoutes(t *testing.T) {
	h := newPortalHarness(t)
	cookie := h.signedIn(t, "amy", amyPassword)
	for _, path := range []string{
		"/users", "/files", "/logs", "/setup", "/api/users", "/api/files", "/api/metrics", "/api/disks",
		"/api/logs", "/api/setup", "/api/system/reboot", "/static/auth.js", "/static/dashboard.js", "/static/users.js",
	} {
		for _, method := range []string{"GET", "POST", "PUT", "DELETE"} {
			rec := h.do(method, path, "", "192.0.2.10", cookie)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: got %d", method, path, rec.Code)
			}
		}
	}
	if rec := h.do("GET", "/static/style.css", "", "192.0.2.10", nil); rec.Code != http.StatusOK {
		t.Fatalf("stylesheet got %d", rec.Code)
	}
}

func TestPortalSendsSecurityHeaders(t *testing.T) {
	h := newPortalHarness(t)
	header := h.do("GET", "/login", "", "192.0.2.10", nil).Header()
	if !strings.Contains(header.Get("Content-Security-Policy"), "default-src 'self'") ||
		header.Get("Strict-Transport-Security") == "" ||
		header.Get("X-Content-Type-Options") != "nosniff" ||
		header.Get("Cache-Control") != "no-store" {
		t.Fatalf("got %v", header)
	}
}

func TestPortalRequiresJSONForLogin(t *testing.T) {
	h := newPortalHarness(t)
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader("username=amy&password=x"))
	req.RemoteAddr = "192.0.2.10:1"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestAdminPasswordChangeAndDeleteEndPortalSessions(t *testing.T) {
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword}}
	portal := NewPortal(PortalDeps{Verifier: verifier})
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialPassword(store.SetupToken(), testPassword); err != nil {
		t.Fatal(err)
	}
	admin := New(store, Deps{Users: system.NewFakeUsers("amy"), OnUserChanged: portal.EndSessions})
	h := &portalHarness{portal: portal, verifier: verifier}

	adminDo := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "192.0.2.20:1"
		req.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		admin.ServeHTTP(rec, req)

		return rec
	}
	login := adminDo("POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	adminCookie := sessionCookie(t, login)

	session := h.signedIn(t, "amy", amyPassword)
	if rec := adminDo("PUT", "/api/users/amy/password", `{"password":"a brand new password"}`, adminCookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("password change must end portal sessions, got %d", rec.Code)
	}

	session = h.signedIn(t, "amy", amyPassword)
	if rec := adminDo("DELETE", "/api/users/amy", "", adminCookie); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("user deletion must end portal sessions, got %d", rec.Code)
	}
}

func TestUserSessionsExpireWhenIdleAndAtTheAbsoluteLimit(t *testing.T) {
	now := time.Now()
	sessions := newUserSessions(time.Hour, 12*time.Hour)
	sessions.now = func() time.Time { return now }

	token, err := sessions.create("amy", sessions.generation("amy"))
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Minute)
	if user, ok := sessions.lookup(token); !ok || user != "amy" {
		t.Fatal("session must still be valid before the idle limit")
	}
	now = now.Add(50 * time.Minute)
	if _, ok := sessions.lookup(token); !ok {
		t.Fatal("activity must extend the idle window")
	}

	now = now.Add(61 * time.Minute)
	if _, ok := sessions.lookup(token); ok {
		t.Fatal("idle session must expire")
	}

	token, _ = sessions.create("amy", sessions.generation("amy"))
	for range 13 {
		now = now.Add(55 * time.Minute)
		if _, ok := sessions.lookup(token); !ok {
			t.Fatal("an active session must stay valid before the absolute limit")
		}
	}
	now = now.Add(10 * time.Minute)
	if _, ok := sessions.lookup(token); ok {
		t.Fatal("session must expire at the absolute limit even when active")
	}
}

func TestUserSessionsKeepOnlyTheNewestPerUser(t *testing.T) {
	now := time.Now()
	sessions := newUserSessions(time.Hour, 12*time.Hour)
	sessions.now = func() time.Time { return now }

	var tokens []string
	for range maxSessionsPerUser + 3 {
		now = now.Add(time.Second)
		token, err := sessions.create("amy", sessions.generation("amy"))
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, token)
	}
	other, _ := sessions.create("bob", sessions.generation("bob"))

	valid := 0
	for _, token := range tokens {
		if _, ok := sessions.lookup(token); ok {
			valid++
		}
	}
	if valid != maxSessionsPerUser {
		t.Fatalf("%d sessions valid, want %d", valid, maxSessionsPerUser)
	}
	if _, ok := sessions.lookup(tokens[len(tokens)-1]); !ok {
		t.Fatal("the newest session must survive")
	}
	if _, ok := sessions.lookup(other); !ok {
		t.Fatal("other users must be unaffected")
	}
}

func TestLogNameIsShortAndQuoted(t *testing.T) {
	if got := logName(strings.Repeat("a", 500)); len(got) > maxLoggedName+2 {
		t.Fatalf("got %d bytes", len(got))
	}
	if got := logName("line" + string(rune(10)) + "break"); strings.ContainsRune(got, 10) {
		t.Fatalf("line breaks must be escaped, got %q", got)
	}
}
