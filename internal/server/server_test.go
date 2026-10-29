package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const testPassword = "correct horse battery"

type harness struct {
	handler http.Handler
	store   *auth.Store
	host    *system.Fake
	users   *spyUsers
	files   system.Files
	logs    *spyLogs
	power   *system.FakePower
	smart   *system.FakeSmart
	usage   *system.FakeUsage
	root    string
	outside string
}

const testMaxUpload = 1024

func newHarness(t *testing.T) *harness {
	t.Helper()

	return newHarnessWithFiles(t, func(files system.Files) system.Files { return files })
}

func newHarnessWithFiles(t *testing.T, wrap func(system.Files) system.Files) *harness {
	t.Helper()

	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := &system.Fake{Value: system.Metrics{CPUPercent: 12.5, MemTotal: 100, MemUsed: 40}}

	users := &spyUsers{FakeUsers: system.NewFakeUsers("amy", "bob")}

	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files, err := system.NewDirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.(io.Closer).Close() })
	logs := &spyLogs{FakeLogs: system.NewFakeLogs()}
	power := &system.FakePower{}
	smart := system.NewFakeSmart()
	usage := &system.FakeUsage{Bytes: map[string]uint64{"amy": 2048}, Ready: true}

	handler := New(store, Deps{Host: host, Users: users, Files: wrap(files), Logs: logs, Power: power, Smart: smart, Usage: usage, MaxUpload: testMaxUpload})

	return &harness{handler: handler, store: store, host: host, users: users, files: files, logs: logs, power: power, smart: smart, usage: usage, root: root, outside: outside}
}

func (h *harness) do(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5555"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == cookieName {
			return cookie
		}
	}
	t.Fatal("no session cookie in response")

	return nil
}

func (h *harness) setup(token, password string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/setup", `{"token":"`+token+`","password":"`+password+`"}`, nil)
}

func (h *harness) signedIn(t *testing.T) *http.Cookie {
	t.Helper()
	if err := h.store.SetInitialPassword(h.store.SetupToken(), testPassword); err != nil {
		t.Fatal(err)
	}

	return sessionCookie(t, h.do("POST", "/api/login", `{"password":"`+testPassword+`"}`, nil))
}

func TestPagesRedirectToSetupBeforeConfigured(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/", "/login"} {
		rec := h.do("GET", path, "", nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
			t.Fatalf("%s: got %d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
	if rec := h.do("GET", "/setup", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("setup page: got %d", rec.Code)
	}
}

func TestSetupCreatesPasswordAndSessionOnce(t *testing.T) {
	h := newHarness(t)

	rec := h.setup(h.store.SetupToken(), testPassword)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	cookie := sessionCookie(t, rec)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("weak cookie flags: %+v", cookie)
	}
	if rec := h.do("GET", "/api/metrics", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("metrics after setup: %d", rec.Code)
	}

	second := h.setup("", "another long password")
	if second.Code != http.StatusForbidden {
		t.Fatalf("second setup: got %d", second.Code)
	}
	if !h.store.Verify(testPassword) {
		t.Fatal("second setup must not change the password")
	}
}

func TestSetupRejectsShortPassword(t *testing.T) {
	h := newHarness(t)
	if rec := h.setup(h.store.SetupToken(), "short"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d", rec.Code)
	}
	if h.store.Configured() {
		t.Fatal("must stay unconfigured")
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	rec := h.do("POST", "/api/login", `{"password":"wrong password here"}`, nil)
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("got %d with %d cookies", rec.Code, len(rec.Result().Cookies()))
	}
}

func TestLoginRequiresJSON(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	req := httptest.NewRequest("POST", "/api/login", strings.NewReader("password="+testPassword))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestLoginLocksOutAfterRepeatedFailures(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	for range maxLoginFails {
		h.do("POST", "/api/login", `{"password":"wrong password here"}`, nil)
	}

	rec := h.do("POST", "/api/login", `{"password":"`+testPassword+`"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestMetricsRequireSession(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	if rec := h.do("GET", "/api/metrics", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/api/metrics", "", &http.Cookie{Name: cookieName, Value: "forged"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie: got %d", rec.Code)
	}
}

func TestMetricsReturnsHostValues(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("GET", "/api/metrics", "", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cpuPercent":12.5`) {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
}

func TestMetricsHostFailureIsReported(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	h.host.Err = errors.New("disk unreadable")

	rec := h.do("GET", "/api/metrics", "", cookie)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "unreadable") {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	if rec := h.do("POST", "/api/logout", "", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("logout: got %d", rec.Code)
	}
	if rec := h.do("GET", "/api/metrics", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: got %d", rec.Code)
	}
}

func TestDashboardPageNeedsSession(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	if rec := h.do("GET", "/", "", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("anonymous: got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec := h.do("GET", "/", "", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Filebox") {
		t.Fatalf("signed in: got %d", rec.Code)
	}
}

func TestStaticServesOnlyAssets(t *testing.T) {
	h := newHarness(t)
	_ = h.store.SetInitialPassword(h.store.SetupToken(), testPassword)

	if rec := h.do("GET", "/static/style.css", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("css: got %d", rec.Code)
	}
	for _, path := range []string{"/static/index.html", "/static/", "/static/login.html"} {
		if rec := h.do("GET", path, "", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d", path, rec.Code)
		}
	}
}

func TestSecurityHeadersPresent(t *testing.T) {
	h := newHarness(t)
	rec := h.do("GET", "/setup", "", nil)

	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing headers: %v", rec.Header())
	}
}

func TestSetupRequiresTheToken(t *testing.T) {
	h := newHarness(t)
	token := h.store.SetupToken()
	if token == "" {
		t.Fatal("a fresh store must have a setup token")
	}

	missing := h.do("POST", "/api/setup", `{"password":"`+testPassword+`"}`, nil)
	wrong := h.setup("00000000000000000000000000000000", testPassword)
	for name, rec := range map[string]*httptest.ResponseRecorder{"missing": missing, "wrong": wrong} {
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s token: got %d", name, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s token must not start a session", name)
		}
	}
	if h.store.Configured() {
		t.Fatal("a bad token must not configure the panel")
	}

	if rec := h.setup(token, testPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("right token: got %d %s", rec.Code, rec.Body)
	}
}

func TestSetupTokenCannotBeReused(t *testing.T) {
	h := newHarness(t)
	token := h.store.SetupToken()
	if rec := h.setup(token, testPassword); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}

	if rec := h.setup(token, "a different long password"); rec.Code != http.StatusForbidden {
		t.Fatalf("reuse: got %d", rec.Code)
	}
	if !h.store.Verify(testPassword) || h.store.SetupToken() != "" {
		t.Fatal("the token must be gone and the password unchanged")
	}
}

func TestSetupPageAsksForTheToken(t *testing.T) {
	h := newHarness(t)
	if body := h.do("GET", "/setup", "", nil).Body.String(); !strings.Contains(body, `id="token"`) {
		t.Fatal("setup page has no token field")
	}
}

func TestPortalStatusNeedsSessionAndReportsExpiry(t *testing.T) {
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetInitialPassword(store.SetupToken(), testPassword); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(10*24*time.Hour + time.Hour)
	handler := New(store, Deps{Host: &system.Fake{}, PortalCertExpiry: func() time.Time { return expiry }})

	do := func(cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/portal", nil)
		req.RemoteAddr = "192.0.2.10:5555"
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		return rec
	}
	if rec := do(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous got %d", rec.Code)
	}

	login := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	login.RemoteAddr = "192.0.2.10:5555"
	login.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, login)
	rec = do(sessionCookie(t, rec))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"certDaysLeft":10`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
