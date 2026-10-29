package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

const portalMaxUpload = 64

type portalFilesHarness struct {
	*portalHarness
	base  string
	users *system.FakeUsers
}

func newPortalFilesHarness(t *testing.T) *portalFilesHarness {
	t.Helper()

	base := t.TempDir()
	for _, user := range []string{"amy", "bob"} {
		if err := os.MkdirAll(filepath.Join(base, user, "files"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword}}
	users := system.NewFakeUsers("amy", "bob")
	portal := NewPortal(PortalDeps{
		Verifier:  verifier,
		Users:     users,
		OpenFiles: func(user string) (system.Files, error) { return system.OpenUserFiles(base, user) },
		MaxUpload: portalMaxUpload,
	})

	return &portalFilesHarness{portalHarness: &portalHarness{portal: portal, verifier: verifier}, base: base, users: users}
}

func (h *portalFilesHarness) write(t *testing.T, user, name, content string) {
	t.Helper()
	path := filepath.Join(h.base, user, "files", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *portalFilesHarness) upload(cookie *http.Cookie, path, content string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/files/content?path="+url.QueryEscape(path), strings.NewReader(content))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "application/octet-stream")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	return rec
}

func (h *portalFilesHarness) get(cookie *http.Cookie, route, path string) *httptest.ResponseRecorder {
	return h.do("GET", route+"?path="+url.QueryEscape(path), "", "192.0.2.10", cookie)
}

func TestPortalFileLifecycleStaysInTheUsersOwnFolder(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	if rec := h.upload(amy, "hello.txt", "hi there"); rec.Code != http.StatusNoContent {
		t.Fatalf("upload got %d: %s", rec.Code, rec.Body)
	}
	if data, err := os.ReadFile(filepath.Join(h.base, "amy", "files", "hello.txt")); err != nil || string(data) != "hi there" {
		t.Fatalf("file must land in amy's files folder: %q %v", data, err)
	}

	if rec := h.do("POST", "/api/files/folder", `{"path":"docs"}`, "192.0.2.10", amy); rec.Code != http.StatusCreated {
		t.Fatalf("mkdir got %d: %s", rec.Code, rec.Body)
	}
	if rec := h.do("POST", "/api/files/rename", `{"from":"hello.txt","to":"docs/hi.txt"}`, "192.0.2.10", amy); rec.Code != http.StatusNoContent {
		t.Fatalf("rename got %d: %s", rec.Code, rec.Body)
	}
	rec := h.get(amy, "/api/files", "docs")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hi.txt") {
		t.Fatalf("list got %d: %s", rec.Code, rec.Body)
	}
	rec = h.get(amy, "/api/files/download", "docs/hi.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != "hi there" {
		t.Fatalf("download got %d: %q", rec.Code, rec.Body)
	}
	if rec := h.do("DELETE", "/api/files?path=docs%2Fhi.txt", "", "192.0.2.10", amy); rec.Code != http.StatusNoContent {
		t.Fatalf("delete got %d: %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "docs", "hi.txt")); !os.IsNotExist(err) {
		t.Fatalf("file must be gone: %v", err)
	}
}

func TestPortalUsersNeverSeeEachOthersFiles(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "bob", "private.txt", "bob only")
	h.write(t, "amy", "mine.txt", "amy only")
	if err := os.WriteFile(filepath.Join(h.base, "amy", "outside.txt"), []byte("outside the files folder"), 0o644); err != nil {
		t.Fatal(err)
	}
	amy := h.signedIn(t, "amy", amyPassword)

	listing := h.get(amy, "/api/files", "")
	if !strings.Contains(listing.Body.String(), "mine.txt") || strings.Contains(listing.Body.String(), "private.txt") ||
		strings.Contains(listing.Body.String(), "outside.txt") || strings.Contains(listing.Body.String(), "bob") {
		t.Fatalf("listing leaks: %s", listing.Body)
	}

	escapes := []string{
		"../bob/files/private.txt", "../../bob/files/private.txt", "bob/files/private.txt", "/bob/files/private.txt",
		"/etc/passwd", "..", "../", "../outside.txt",
		"mine.txt/../../bob/files/private.txt", "..\\bob\\files\\private.txt", "./../bob/files/private.txt", "a\x00b",
		"....//bob/files/private.txt", "mine.txt/", "//bob/files/private.txt",
	}
	for _, path := range escapes {
		for _, route := range []string{"/api/files", "/api/files/download"} {
			rec := h.get(amy, route, path)
			if rec.Code == http.StatusOK && (strings.Contains(rec.Body.String(), "bob only") || strings.Contains(rec.Body.String(), "outside the files folder")) {
				t.Fatalf("%s %q escaped: %s", route, path, rec.Body)
			}
		}
		if rec := h.upload(amy, path, "planted"); rec.Code == http.StatusNoContent && path != "mine.txt/" {
			t.Fatalf("upload to %q must be refused", path)
		}
		h.do("DELETE", "/api/files?path="+url.QueryEscape(path), "", "192.0.2.10", amy)
		h.do("POST", "/api/files/rename", `{"from":"`+jsonSafe(path)+`","to":"stolen.txt"}`, "192.0.2.10", amy)
		h.do("POST", "/api/files/rename", `{"from":"mine.txt","to":"`+jsonSafe(path)+`"}`, "192.0.2.10", amy)
		h.do("POST", "/api/files/folder", `{"path":"`+jsonSafe(path)+`"}`, "192.0.2.10", amy)
	}

	for _, raw := range []string{
		"..%2Fbob%2Ffiles%2Fprivate.txt", "%2e%2e%2Fbob%2Ffiles%2Fprivate.txt", "%2e%2e/bob/files/private.txt",
		"..%5Cbob%5Cfiles%5Cprivate.txt", "%252e%252e%252Fbob%252Ffiles%252Fprivate.txt", "..%00/bob/files/private.txt",
	} {
		for _, route := range []string{"/api/files", "/api/files/download"} {
			rec := h.do("GET", route+"?path="+raw, "", "192.0.2.10", amy)
			if strings.Contains(rec.Body.String(), "bob only") || strings.Contains(rec.Body.String(), "outside the files folder") {
				t.Fatalf("%s raw %q escaped: %s", route, raw, rec.Body)
			}
		}
	}

	if data, err := os.ReadFile(filepath.Join(h.base, "bob", "files", "private.txt")); err != nil || string(data) != "bob only" {
		t.Fatalf("bob's file was touched: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "stolen.txt")); err == nil {
		t.Fatal("a file escaped into amy's home")
	}
	entries, err := os.ReadDir(filepath.Join(h.base, "bob", "files"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("bob's folder changed: %v %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(h.base, "planted")); err == nil {
		t.Fatal("an upload landed outside the user folders")
	}
}

func jsonSafe(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\x00", `\u0000`)

	return replacer.Replace(value)
}

func TestPortalFollowsNoSymlinkOutOfTheUsersFolder(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "bob", "private.txt", "bob only")
	if err := os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, "amy", "files", "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(h.base, "bob", "files", "private.txt"), filepath.Join(h.base, "amy", "files", "filelink")); err != nil {
		t.Fatal(err)
	}
	amy := h.signedIn(t, "amy", amyPassword)

	for _, path := range []string{"link", "link/private.txt", "filelink"} {
		for _, route := range []string{"/api/files", "/api/files/download"} {
			rec := h.get(amy, route, path)
			if strings.Contains(rec.Body.String(), "bob only") || strings.Contains(rec.Body.String(), "private.txt") {
				t.Fatalf("%s %q followed a symlink: %s", route, path, rec.Body)
			}
		}
		if rec := h.upload(amy, path+"/planted.txt", "x"); rec.Code == http.StatusNoContent {
			t.Fatalf("upload through %q must be refused", path)
		}
	}
	if _, err := os.Stat(filepath.Join(h.base, "bob", "files", "planted.txt")); err == nil {
		t.Fatal("an upload escaped through a symlink")
	}
}

func TestPortalFileEndpointsRequireASession(t *testing.T) {
	h := newPortalFilesHarness(t)
	opened := 0
	h.portal.openFiles = func(string) (system.Files, error) {
		opened++
		return nil, errors.New("must not be called")
	}
	requests := [][3]string{
		{"GET", "/api/files", ""}, {"DELETE", "/api/files?path=x", ""}, {"GET", "/api/files/download?path=x", ""},
		{"PUT", "/api/files/content?path=x", ""}, {"POST", "/api/files/folder", `{"path":"x"}`},
		{"POST", "/api/files/rename", `{"from":"a","to":"b"}`}, {"PUT", "/api/me/password", `{"current":"x","password":"a long enough one"}`},
	}
	for _, request := range requests {
		rec := h.do(request[0], request[1], request[2], "192.0.2.10", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: got %d", request[0], request[1], rec.Code)
		}
	}
	if opened != 0 {
		t.Fatalf("files were opened %d times without a session", opened)
	}
}

func TestPortalUploadLimit(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	if rec := h.upload(amy, "ok.bin", strings.Repeat("a", portalMaxUpload)); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.upload(amy, "big.bin", strings.Repeat("a", portalMaxUpload+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "big.bin")); err == nil {
		t.Fatal("an oversized upload must leave nothing behind")
	}
}

func TestPortalReportsAnUnopenableFolderAsAServerError(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.portal.openFiles = func(string) (system.Files, error) { return nil, errors.New("disk on fire") }
	amy := h.signedIn(t, "amy", amyPassword)

	rec := h.get(amy, "/api/files", "")
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk on fire") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestPortalServesTheFileBrowserScriptButNoOtherAdminScript(t *testing.T) {
	h := newPortalFilesHarness(t)
	if rec := h.do("GET", "/static/files.js", "", "192.0.2.10", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	for _, path := range []string{"/static/users.js", "/static/logs.js", "/static/dashboard.js", "/static/auth.js"} {
		if rec := h.do("GET", path, "", "192.0.2.10", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("%s got %d", path, rec.Code)
		}
	}
}

func TestPortalChangePassword(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	other := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)

	rec := h.do("PUT", "/api/me/password", `{"current":"`+amyPassword+`","password":"a brand new password"}`, "192.0.2.10", amy)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if h.users.Passwords["amy"] != "a brand new password" {
		t.Fatalf("password not set: %q", h.users.Passwords["amy"])
	}
	for _, cookie := range []*http.Cookie{amy, other} {
		if rec := h.do("GET", "/api/me", "", "192.0.2.10", cookie); rec.Code != http.StatusUnauthorized {
			t.Fatalf("every session of the user must end, got %d", rec.Code)
		}
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", bob); rec.Code != http.StatusOK {
		t.Fatalf("other users stay signed in, got %d", rec.Code)
	}
}

func TestPortalChangePasswordNeedsTheCurrentPassword(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	rec := h.do("PUT", "/api/me/password", `{"current":"not my password","password":"a brand new password"}`, "192.0.2.10", amy)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}
	if h.users.Passwords["amy"] != "" {
		t.Fatalf("password must not change, got %q", h.users.Passwords["amy"])
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", amy); rec.Code != http.StatusOK {
		t.Fatalf("a failed change must not end the session, got %d", rec.Code)
	}
}

func TestPortalChangePasswordValidatesTheNewPassword(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	for _, bad := range []string{"short", strings.Repeat("a", 129), "has a newline\\nin it"} {
		rec := h.do("PUT", "/api/me/password", `{"current":"`+amyPassword+`","password":"`+bad+`"}`, "192.0.2.10", amy)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%q: got %d", bad, rec.Code)
		}
	}
	if h.verifier.Calls != 1 {
		t.Fatalf("an invalid new password must be refused before checking the current one, calls=%d", h.verifier.Calls)
	}
}

func TestPortalChangePasswordIsRateLimited(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	for range portalIPFails {
		h.do("PUT", "/api/me/password", `{"current":"wrong guess here","password":"a brand new password"}`, "192.0.2.10", amy)
	}
	rec := h.do("PUT", "/api/me/password", `{"current":"`+amyPassword+`","password":"a brand new password"}`, "192.0.2.10", amy)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestPortalPasswordChangeReportsAHelperFailure(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	failing := &failingUsers{FakeUsers: h.users}
	h.portal.users = failing

	rec := h.do("PUT", "/api/me/password", `{"current":"`+amyPassword+`","password":"a brand new password"}`, "192.0.2.10", amy)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "useradd") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("GET", "/api/me", "", "192.0.2.10", amy); rec.Code != http.StatusOK {
		t.Fatalf("a failed change must keep the session, got %d", rec.Code)
	}
}

type failingUsers struct{ *system.FakeUsers }

func (f *failingUsers) SetPassword(string, string) error { return errors.New("useradd: secret detail") }

func TestPortalPasswordChangeFailuresCountWithLoginFailures(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)

	for range portalIPFails - 1 {
		h.login("bob", "wrong password here", "192.0.2.10")
	}
	h.do("PUT", "/api/me/password", `{"current":"wrong guess here","password":"a brand new password"}`, "192.0.2.10", amy)

	rec := h.do("PUT", "/api/me/password", `{"current":"`+amyPassword+`","password":"a brand new password"}`, "192.0.2.10", amy)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("one shared failure budget per IP, got %d", rec.Code)
	}
}

func TestPortalPasswordChangeSuccessDoesNotResetTheIPCounter(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)

	for range portalIPFails - 1 {
		h.do("PUT", "/api/me/password", `{"current":"wrong guess here","password":"a brand new password"}`, "192.0.2.10", bob)
	}
	if rec := h.do("PUT", "/api/me/password", `{"current":"`+amyPassword+`","password":"a brand new password"}`, "192.0.2.10", amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	h.do("PUT", "/api/me/password", `{"current":"wrong guess here","password":"a brand new password"}`, "192.0.2.10", bob)
	if rec := h.do("PUT", "/api/me/password", `{"current":"`+bobPassword+`","password":"a brand new password"}`, "192.0.2.10", bob); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("an own successful change must not reset the guess budget, got %d", rec.Code)
	}
}
