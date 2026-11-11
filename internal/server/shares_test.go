package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const visitorIP = "198.51.100.7"

// Windows cannot hold the names that matter most for HTML and header handling, so there they get tame stand-ins.
var htmlName, quotedName = "<b>bold<i>.txt", "q\"uote é.txt"

func init() {
	if runtime.GOOS == "windows" {
		htmlName, quotedName = "b-bold-b.txt", "quote é.txt"
	}
}

var shareEpochTime = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type shareHarness struct {
	*portalHarness
	base     string
	shares   *auth.Shares
	dataDir  string
	users    *system.FakeUsers
	amy, bob *http.Cookie

	clockMu sync.Mutex
	clock   time.Time
}

func newShareHarness(t *testing.T) *shareHarness {
	t.Helper()

	return newShareHarnessWith(t, func(*PortalDeps) {})
}

func newShareHarnessWith(t *testing.T, configure func(*PortalDeps)) *shareHarness {
	t.Helper()

	base := t.TempDir()
	for _, user := range []string{"amy", "bob", "cleo"} {
		if err := os.MkdirAll(filepath.Join(base, user, "files"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	shares, err := auth.OpenShares(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	h := &shareHarness{base: base, shares: shares, dataDir: dataDir, clock: shareEpochTime}
	h.users = system.NewFakeUsers("amy", "bob", "cleo")
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword, "cleo": cleoPassword}}
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.Promote("amy"); err != nil {
		t.Fatal(err)
	}
	deps := PortalDeps{
		Verifier:  verifier,
		Users:     h.users,
		Roles:     roles,
		OpenFiles: func(user string) (system.Files, error) { return system.OpenUserFiles(base, user) },
		MaxUpload: 1 << 20,
		Shares:    shares,
	}
	configure(&deps)
	portal := NewPortal(deps)
	portal.now = h.now
	h.portalHarness = &portalHarness{portal: portal, verifier: verifier, roles: roles}
	h.amy = h.signedIn(t, "amy", amyPassword)
	h.bob = h.signedIn(t, "bob", bobPassword)

	return h
}

func (h *shareHarness) now() time.Time {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()

	return h.clock
}

func (h *shareHarness) advance(d time.Duration) {
	h.clockMu.Lock()
	defer h.clockMu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *shareHarness) write(t *testing.T, user, name string, content []byte) {
	t.Helper()
	path := filepath.Join(h.base, user, "files", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *shareHarness) mkdir(t *testing.T, user, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(h.base, user, "files", filepath.FromSlash(name)), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *shareHarness) symlink(t *testing.T, target, user, name string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(h.base, user, "files", filepath.FromSlash(name))); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
}

func (h *shareHarness) seed(t *testing.T) {
	t.Helper()
	h.write(t, "amy", "notes.txt", []byte("hello notes"))
	h.write(t, "amy", "photo.jpg", []byte("\xff\xd8\xffjpeg-bytes"))
	h.write(t, "amy", "vector.svg", []byte("<svg onload=alert(1)></svg>"))
	h.write(t, "amy", "page.html", []byte("<script>alert(1)</script>"))
	h.write(t, "amy", "empty.bin", nil)
	h.write(t, "amy", "docs/a.txt", []byte("alpha"))
	h.write(t, "amy", "docs/sub/b.txt", []byte("bravo"))
	h.write(t, "amy", "docs/"+htmlName, []byte("literal"))
	h.write(t, "amy", "docs/"+quotedName, []byte("quoted"))
	h.mkdir(t, "amy", "docs/empty")
	h.write(t, "amy", "docs/.trash/old.txt", []byte("trashed"))
	h.write(t, "amy", "docs/.UPLOADS/part", []byte("partial"))
	h.write(t, "amy", "docs/.thumbs/t.jpg", []byte("thumb"))
	h.write(t, "bob", "secret.txt", []byte(bobSecret))
}

func (h *shareHarness) create(cookie *http.Cookie, name, path, expires string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"name": name, "path": path, "expires": expires})

	return h.do("POST", "/api/shares", string(body), "192.0.2.10", cookie)
}

func (h *shareHarness) mustCreate(t *testing.T, cookie *http.Cookie, name, path string) shareView {
	t.Helper()
	rec := h.create(cookie, name, path, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q %q got %d: %s", name, path, rec.Code, rec.Body)
	}
	var view shareView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	return view
}

func (h *shareHarness) visit(method, target string, headers ...string) *httptest.ResponseRecorder {
	return h.visitFrom(visitorIP, method, target, headers...)
}

func (h *shareHarness) visitFrom(ip, method, target string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	req.RemoteAddr = ip + ":4444"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	return rec
}

func query(path string) string {
	return "?path=" + url.QueryEscape(path)
}

func TestShareNamesAreCheckedAndNormalised(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)

	valid := map[string]string{
		"Chronos-App":           "chronos-app",
		"abc":                   "abc",
		"123":                   "123",
		"9lives":                "9lives",
		"a-b-c":                 "a-b-c",
		strings.Repeat("x", 40): strings.Repeat("x", 40),
	}
	for typed, stored := range valid {
		rec := h.create(h.amy, typed, "notes.txt", "")
		if rec.Code != http.StatusCreated {
			t.Errorf("%q should be accepted, got %d: %s", typed, rec.Code, rec.Body)
			continue
		}
		var view shareView
		_ = json.Unmarshal(rec.Body.Bytes(), &view)
		if view.Name != stored {
			t.Errorf("%q must be stored as %q, got %q", typed, stored, view.Name)
		}
	}

	invalid := []string{
		"ab", strings.Repeat("x", 41), "-abc", "abc-", "a--b", "a_b", "a b", " abc", "abc ", "abc.", "ab.c", "abc/", "/abc", "a/b",
		"<b>x</b>", "%61bc", "abc%2f", "..", "a\x00b", "a\nb", "caf\u00e9", "\u212aelvin", "\uff41bc", "\u0430bc", "\u0131stanbul",
	}
	for _, name := range invalid {
		rec := h.create(h.amy, name, "notes.txt", "")
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), codeShareNameInvalid) {
			t.Errorf("%q must be refused as invalid, got %d: %s", name, rec.Code, rec.Body)
		}
	}
	if got := len(h.shares.All()); got != len(valid) {
		t.Fatalf("only the valid names may be stored, have %d", got)
	}
}

func TestShareNamesAreUniqueIgnoringCase(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.write(t, "bob", "notes.txt", []byte("bob notes"))
	h.mustCreate(t, h.amy, "Unique-Name", "notes.txt")

	for _, again := range []string{"unique-name", "UNIQUE-NAME", "Unique-name"} {
		for _, cookie := range []*http.Cookie{h.amy, h.bob} {
			rec := h.create(cookie, again, "notes.txt", "")
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), codeShareNameTaken) {
				t.Errorf("%q must be taken, got %d: %s", again, rec.Code, rec.Body)
			}
		}
	}
}

func TestShareNameMayEqualAUsername(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	view := h.mustCreate(t, h.amy, "cleo", "notes.txt")
	rec := h.visit("GET", "/"+view.Name+"/list")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "amy") {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
}

func TestShareReservedNamesAreRefused(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)

	names := slices.Clone(reservedShareNames)
	names = append(names, "API", "Login", "LOGIN", "Static", "Admin", "robots")
	for _, name := range names {
		normalized := normalizeShareName(name)
		if !shareNamePattern.MatchString(normalized) || len(normalized) < shareNameMin {
			continue
		}
		rec := h.create(h.amy, name, "notes.txt", "")
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), codeShareNameReserved) {
			if normalized == "robots" {
				continue
			}
			t.Errorf("%q must be reserved, got %d: %s", name, rec.Code, rec.Body)
		}
	}
	for _, name := range []string{"login", "api", "static", "family", "trash", "photos", "backup", "admin", "forgot", "reset", "sign-in", "confirm-email", "password", "shares"} {
		if rec := h.create(h.amy, name, "notes.txt", ""); !strings.Contains(rec.Body.String(), codeShareNameReserved) {
			t.Errorf("%q must be reserved: %s", name, rec.Body)
		}
	}
}

func fullyEnabledPortal(t *testing.T) *Portal {
	t.Helper()
	shares, err := auth.OpenShares(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return NewPortal(PortalDeps{
		Users:      system.NewFakeUsers("amy"),
		OpenFiles:  func(string) (system.Files, error) { return system.NewFakeFiles(), nil },
		OpenFamily: func() (system.Files, error) { return system.NewFakeFiles(), nil },
		TV:         &TV{},
		Shares:     shares,
	})
}

func TestEveryRegisteredRouteIsReservedFromShareNames(t *testing.T) {
	portal := fullyEnabledPortal(t)
	if len(portal.mux.first) < 10 {
		t.Fatalf("the router recorded only %v", portal.mux.first)
	}
	for segment := range portal.mux.first {
		if !slices.Contains(reservedShareNames, segment) {
			t.Errorf("the router serves /%s but reservedShareNames does not list it, so a portal without that route could hand the name out", segment)
		}
	}
}

func TestRouterRefusesAWildcardAsFirstSegment(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a route like /{name} would shadow every share link and must not register")
		}
	}()
	newRoutes().HandleFunc("GET /{name}", func(http.ResponseWriter, *http.Request) {})
}

func TestFirstSegmentOfPatterns(t *testing.T) {
	cases := map[string]string{
		"GET /static/style.css":     "static",
		"/":                         "",
		"GET /{$}":                  "",
		"POST /api/login":           "api",
		"GET /tv/s/{token}/epg.xml": "tv",
		"GET /manifest.webmanifest": "manifest.webmanifest",
		"DELETE /api/trash/{id}":    "api",
		"/api/tv/":                  "api",
	}
	for pattern, want := range cases {
		if got, wildcard := firstSegment(pattern); got != want || wildcard {
			t.Errorf("%q gave %q %v", pattern, got, wildcard)
		}
	}
}

func TestShareLinksDoNotShadowExistingRoutes(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "my-files", "notes.txt")

	cases := map[string]int{
		"/login": 200, "/static/style.css": 200, "/static/i18n.js": 200, "/manifest.webmanifest": 200, "/icon-192.png": 200,
		"/robots.txt": 200, "/api/me": 401, "/api/files": 401, "/api/shares": 401, "/trash": 303, "/": 303, "/password": 303, "/shares": 303,
		"/admin/users": 303, "/api/admin/shares": 401, "/forgot": 200,
	}
	for route, want := range cases {
		if rec := h.visit("GET", route); rec.Code != want {
			t.Errorf("%s got %d, want %d", route, rec.Code, want)
		}
	}
	if rec := h.visit("GET", "/api/nothing"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("unknown api paths keep their JSON answer: %d %s", rec.Code, rec.Body)
	}
}

func TestRobotsTxtDisallowsEverything(t *testing.T) {
	h := newShareHarness(t)
	rec := h.visit("GET", "/robots.txt")
	if rec.Code != 200 || rec.Body.String() != "User-agent: *\nDisallow: /\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
}

func TestShareFileLandingDownloadAndHardening(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	view := h.mustCreate(t, h.amy, "my-notes", "notes.txt")
	if view.Dir || view.Path != "notes.txt" || view.Expires != nil || view.Downloads != 0 {
		t.Fatalf("got %+v", view)
	}

	page := h.visit("GET", "/my-notes")
	if page.Code != 200 || !strings.Contains(page.Header().Get("Content-Type"), "text/html") || !strings.Contains(page.Body.String(), "share.js") {
		t.Fatalf("landing page got %d %q", page.Code, page.Header().Get("Content-Type"))
	}
	if upper := h.visit("GET", "/My-Notes/"); upper.Code != 200 {
		t.Fatalf("the name is case-insensitive and a trailing slash is fine, got %d", upper.Code)
	}
	if page.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("pages must say noindex: %v", page.Header())
	}

	info := h.visit("GET", "/my-notes/list")
	var listing struct {
		Kind, Name string
		Size       int64
		Preview    bool
	}
	_ = json.Unmarshal(info.Body.Bytes(), &listing)
	if info.Code != 200 || listing.Kind != "file" || listing.Name != "notes.txt" || listing.Size != 11 || listing.Preview {
		t.Fatalf("got %d %s", info.Code, info.Body)
	}
	for _, leak := range []string{"amy", h.base, "files/"} {
		if strings.Contains(info.Body.String(), leak) {
			t.Fatalf("the listing leaks %q: %s", leak, info.Body)
		}
	}

	dl := h.visit("GET", "/my-notes/download")
	if dl.Code != 200 || dl.Body.String() != "hello notes" {
		t.Fatalf("got %d %q", dl.Code, dl.Body)
	}
	header := dl.Header()
	if header.Get("Content-Type") != "application/octet-stream" || header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(header.Get("Content-Disposition"), "attachment") || header.Get("X-Robots-Tag") != "noindex, nofollow" ||
		!strings.Contains(header.Get("Content-Security-Policy"), "default-src 'none'") || header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("got %v", header)
	}
	if _, found := header["Set-Cookie"]; found {
		t.Fatal("a visitor must never get a cookie")
	}
}

func TestShareDownloadServesHostileFilesOnlyAsDownloads(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	for _, name := range []string{"vector.svg", "page.html"} {
		view := h.mustCreate(t, h.amy, "", name)
		rec := h.visit("GET", "/"+view.Name+"/download")
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/octet-stream" ||
			!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %v", name, rec.Header())
		}
		if preview := h.visit("GET", "/"+view.Name+"/preview"); preview.Code != 404 {
			t.Errorf("%s must never be previewed inline, got %d", name, preview.Code)
		}
	}
}

func TestShareImagePreviewIsHardenedAndOnlyForImages(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	view := h.mustCreate(t, h.amy, "my-photo", "photo.jpg")

	info := h.visit("GET", "/"+view.Name+"/list")
	if !strings.Contains(info.Body.String(), `"preview":true`) {
		t.Fatalf("got %s", info.Body)
	}
	rec := h.visit("GET", "/my-photo/preview")
	header := rec.Header()
	if rec.Code != 200 || header.Get("Content-Type") != "image/jpeg" || header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(header.Get("Content-Security-Policy"), "sandbox; default-src 'none'") || !strings.HasPrefix(header.Get("Content-Disposition"), "inline") {
		t.Fatalf("got %d %v", rec.Code, header)
	}

	text := h.mustCreate(t, h.amy, "plain-text", "notes.txt")
	if got := h.visit("GET", "/"+text.Name+"/preview"); got.Code != 404 {
		t.Fatalf("a text file has no preview, got %d", got.Code)
	}
}

func TestShareDownloadHonoursRangeAndHead(t *testing.T) {
	h := newShareHarness(t)
	content := make([]byte, 3<<20)
	for i := range content {
		content[i] = byte(i * 7)
	}
	h.write(t, "amy", "big.bin", content)
	h.mustCreate(t, h.amy, "big-file", "big.bin")

	full := h.visit("GET", "/big-file/download")
	if full.Code != 200 || !bytes.Equal(full.Body.Bytes(), content) {
		t.Fatalf("the whole file must stream intact, got %d with %d bytes", full.Code, full.Body.Len())
	}
	cases := map[string]struct {
		code       int
		start, end int
	}{
		"bytes=0-99":            {206, 0, 100},
		"bytes=1000-":           {206, 1000, len(content)},
		"bytes=-500":            {206, len(content) - 500, len(content)},
		"bytes=2097152-2097152": {206, 2097152, 2097153},
	}
	for spec, want := range cases {
		rec := h.visit("GET", "/big-file/download", "Range", spec)
		if rec.Code != want.code || !bytes.Equal(rec.Body.Bytes(), content[want.start:want.end]) {
			t.Errorf("%s got %d with %d bytes", spec, rec.Code, rec.Body.Len())
		}
	}
	for _, spec := range []string{"bytes=99999999-", "bytes=5-2", "bytes=abc"} {
		rec := h.visit("GET", "/big-file/download", "Range", spec)
		if rec.Code != http.StatusRequestedRangeNotSatisfiable && rec.Code != 200 {
			t.Errorf("%s got %d", spec, rec.Code)
		}
		if rec.Code == http.StatusRequestedRangeNotSatisfiable && strings.Contains(rec.Body.String(), "\x00") {
			t.Errorf("%s leaked file bytes into the error", spec)
		}
	}
	multi := h.visit("GET", "/big-file/download", "Range", "bytes=0-9,100-109")
	if multi.Code != 206 || !strings.HasPrefix(multi.Header().Get("Content-Type"), "multipart/byteranges") {
		t.Errorf("multiple ranges got %d %q", multi.Code, multi.Header().Get("Content-Type"))
	}
	head := h.visit("HEAD", "/big-file/download")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != fmt.Sprint(len(content)) {
		t.Errorf("HEAD got %d %q %d", head.Code, head.Header().Get("Content-Length"), head.Body.Len())
	}
}

func TestShareDownloadOfAnEmptyFile(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "empty-one", "empty.bin")
	rec := h.visit("GET", "/empty-one/download")
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatalf("got %d %d", rec.Code, rec.Body.Len())
	}
}

func TestShareFolderListsOnlyWhatIsVisible(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.symlink(t, "../notes.txt", "amy", "docs/link.txt")
	h.symlink(t, filepath.Join(h.base, "bob", "files"), "amy", "docs/escape")
	h.mustCreate(t, h.amy, "my-docs", "docs")

	rec := h.visit("GET", "/my-docs/list")
	var listing struct {
		Kind, Name, Path string
		Entries          []struct {
			Name  string
			IsDir bool
			Size  int64
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil || rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	var names []string
	for _, entry := range listing.Entries {
		names = append(names, entry.Name)
	}
	want := []string{"empty", "sub", htmlName, "a.txt", quotedName}
	slices.Sort(names)
	slices.Sort(want)
	if listing.Kind != "folder" || listing.Name != "docs" || listing.Path != "" || !slices.Equal(names, want) {
		t.Fatalf("got %+v, want entries %v", listing, want)
	}
	for _, leak := range []string{".trash", ".UPLOADS", ".thumbs", "link.txt", "escape", "old.txt", "amy"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the listing must not show %q", leak)
		}
	}

	sub := h.visit("GET", "/my-docs/list"+query("sub"))
	if !strings.Contains(sub.Body.String(), "b.txt") || !strings.Contains(sub.Body.String(), `"path":"sub"`) {
		t.Fatalf("got %d %s", sub.Code, sub.Body)
	}
	file := h.visit("GET", "/my-docs/download"+query("sub/b.txt"))
	if file.Code != 200 || file.Body.String() != "bravo" {
		t.Fatalf("got %d %q", file.Code, file.Body)
	}
	if dir := h.visit("GET", "/my-docs/download"+query("sub")); dir.Code != 404 {
		t.Fatalf("a folder is not a download, got %d", dir.Code)
	}
}

func TestShareFolderZipHasEverythingVisibleAndNothingElse(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.symlink(t, "../notes.txt", "amy", "docs/link.txt")
	h.symlink(t, filepath.Join(h.base, "bob", "files"), "amy", "docs/escape")
	h.mustCreate(t, h.amy, "my-docs", "docs")

	rec := h.visit("GET", "/my-docs/zip")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
	archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range archive.File {
		names = append(names, file.Name)
	}
	slices.Sort(names)
	want := []string{"docs/", "docs/" + htmlName, "docs/a.txt", "docs/empty/", "docs/" + quotedName, "docs/sub/", "docs/sub/b.txt"}
	if !slices.Equal(names, want) {
		t.Fatalf("got %q, want %q", names, want)
	}

	subZip := h.visit("GET", "/my-docs/zip"+query("sub"))
	if subZip.Code != 200 || !strings.Contains(subZip.Header().Get("Content-Disposition"), "sub.zip") {
		t.Fatalf("got %d %v", subZip.Code, subZip.Header())
	}
	if head := h.visit("HEAD", "/my-docs/zip"); head.Code != 200 || head.Body.Len() != 0 {
		t.Fatalf("got %d %d", head.Code, head.Body.Len())
	}
	h.mustCreate(t, h.amy, "just-notes", "notes.txt")
	if got := h.visit("GET", "/just-notes/zip"); got.Code != 404 {
		t.Fatalf("a file link has no zip, got %d", got.Code)
	}
}

func TestShareContentDispositionSurvivesOddFileNames(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "my-docs", "docs")

	rec := h.visit("GET", "/my-docs/download"+query(quotedName))
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if strings.ContainsAny(disposition, "\r\n") {
		t.Fatalf("header injection: %q", disposition)
	}
	kind, params, err := mime.ParseMediaType(disposition)
	if err != nil || kind != "attachment" || params["filename"] != quotedName {
		t.Fatalf("got %q %v %v", kind, params, err)
	}
	if rec := h.visit("GET", "/my-docs/download"+query("a\r\nSet-Cookie: x=1")); rec.Code != 404 || rec.Header().Get("Set-Cookie") != "" {
		t.Fatalf("a path with CR LF must be a plain miss, got %d %v", rec.Code, rec.Header())
	}
}

func sameResponse(t *testing.T, label string, a, b *httptest.ResponseRecorder) {
	t.Helper()
	if a.Code != b.Code || a.Body.String() != b.Body.String() {
		t.Errorf("%s: status %d vs %d, bodies differ: %v", label, a.Code, b.Code, a.Body.String() != b.Body.String())
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Cache-Control", "Content-Security-Policy", "X-Robots-Tag", "Set-Cookie", "Content-Disposition"} {
		if a.Header().Get(name) != b.Header().Get(name) {
			t.Errorf("%s: header %s is %q vs %q", label, name, a.Header().Get(name), b.Header().Get(name))
		}
	}
	if len(a.Header()) != len(b.Header()) {
		t.Errorf("%s: header sets differ: %v vs %v", label, a.Header(), b.Header())
	}
}

func TestShareUnavailableLinksAllAnswerIdentically(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)

	h.mustCreate(t, h.amy, "will-expire", "notes.txt")
	if rec := h.create(h.amy, "short-lived", "notes.txt", "1d"); rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	h.mustCreate(t, h.amy, "will-revoke", "notes.txt")
	h.write(t, "amy", "gone.txt", []byte("soon gone"))
	h.write(t, "amy", "keep/ok.txt", []byte("ok"))
	h.mustCreate(t, h.amy, "will-vanish", "gone.txt")
	h.mustCreate(t, h.amy, "will-swap", "docs")
	h.mustCreate(t, h.amy, "live-folder", "keep")
	h.write(t, "cleo", "x.txt", []byte("cleo"))
	h.mustCreate(t, h.signedInAs(t, "cleo", cleoPassword), "cleos-link", "x.txt")

	h.advance(25 * time.Hour)
	if rec := h.do("DELETE", "/api/shares/will-revoke", "", "192.0.2.10", h.amy); rec.Code != 204 {
		t.Fatal(rec.Body)
	}
	if err := os.Remove(filepath.Join(h.base, "amy", "files", "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(h.base, "cleo")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(h.base, "amy", "files", "docs")); err != nil {
		t.Fatal(err)
	}
	h.write(t, "amy", "docs", []byte("now a file"))

	unknown := h.visit("GET", "/never-existed")
	for _, route := range []string{"/short-lived", "/will-revoke", "/will-vanish", "/will-swap", "/cleos-link", "/never-existed/list", "/short-lived/download", "/will-vanish/list", "/cleos-link/download",
		"/live-folder/list" + query("sub"), "/live-folder/list" + query("missing/dir"), "/live-folder/download" + query("nothing.txt"), "/live-folder/bogus", "/live-folder/list/extra", "/will-swap/list",
		"/not_a_name", "/UPPER_CASE.PHP", "/x"} {
		got := h.visit("GET", route)
		sameResponse(t, route, unknown, got)
		if got.Code != http.StatusNotFound || got.Header().Get("Content-Type") != "text/html; charset=utf-8" {
			t.Errorf("%s got %d %q", route, got.Code, got.Header().Get("Content-Type"))
		}
	}
	for _, route := range []string{"/live-folder/list" + query("sub"), "/live-folder/download" + query("nothing.txt")} {
		for _, method := range []string{"HEAD"} {
			if got := h.visit(method, route); got.Code != 404 {
				t.Errorf("%s %s got %d", method, route, got.Code)
			}
		}
	}
	if post := h.visitFrom("198.51.100.99", "POST", "/live-folder/list"); post.Code != 404 {
		t.Errorf("POST got %d", post.Code)
	}
}

func (h *shareHarness) signedInAs(t *testing.T, user, password string) *http.Cookie {
	t.Helper()

	return h.signedIn(t, user, password)
}

func TestShareExpiryBoundaries(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	rec := h.create(h.amy, "one-day", "notes.txt", "1d")
	if rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	var view shareView
	_ = json.Unmarshal(rec.Body.Bytes(), &view)
	if view.Expires == nil || !view.Expires.Equal(shareEpochTime.Add(24*time.Hour)) {
		t.Fatalf("got %+v", view)
	}

	h.advance(24*time.Hour - time.Second)
	if got := h.visit("GET", "/one-day/download"); got.Code != 200 {
		t.Fatalf("one second before expiry got %d", got.Code)
	}
	h.advance(time.Second)
	if got := h.visit("GET", "/one-day/download"); got.Code != 404 {
		t.Fatalf("exactly at expiry must already be gone, got %d", got.Code)
	}
	h.advance(time.Second)
	if got := h.visit("GET", "/one-day/download"); got.Code != 404 {
		t.Fatalf("one second after expiry got %d", got.Code)
	}

	listed := h.do("GET", "/api/shares", "", "192.0.2.10", h.amy)
	if !strings.Contains(listed.Body.String(), `"expired":true`) {
		t.Fatalf("the owner still sees an expired link: %s", listed.Body)
	}
	revive := h.do("PUT", "/api/shares/one-day", `{"expires":"7d"}`, "192.0.2.10", h.amy)
	if revive.Code != 200 || !strings.Contains(revive.Body.String(), `"expired":false`) {
		t.Fatalf("changing the expiry of an expired link revives it: %d %s", revive.Code, revive.Body)
	}
	if got := h.visit("GET", "/one-day/download"); got.Code != 200 {
		t.Fatalf("got %d", got.Code)
	}
	never := h.do("PUT", "/api/shares/one-day", `{"expires":"never"}`, "192.0.2.10", h.amy)
	if !strings.Contains(never.Body.String(), `"expires":null`) {
		t.Fatalf("got %s", never.Body)
	}
	h.advance(100 * 365 * 24 * time.Hour)
	if got := h.visit("GET", "/one-day/download"); got.Code != 200 {
		t.Fatalf("a link without expiry never expires, got %d", got.Code)
	}
	for _, bad := range []string{"2d", "forever", "0", "-1d", "1D"} {
		if rec := h.do("PUT", "/api/shares/one-day", `{"expires":"`+bad+`"}`, "192.0.2.10", h.amy); rec.Code != 422 || !strings.Contains(rec.Body.String(), codeShareExpiryInvalid) {
			t.Errorf("%q got %d %s", bad, rec.Code, rec.Body)
		}
	}
	if rec := h.create(h.amy, "bad-expiry", "notes.txt", "tomorrow"); rec.Code != 422 {
		t.Errorf("got %d", rec.Code)
	}
}

func TestSharePathAttacksStayInsideTheSharedFolder(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.write(t, "amy", "outside-share.txt", []byte("amy-outside-share"))
	h.symlink(t, filepath.Join(h.base, "bob", "files"), "amy", "docs/to-bob")
	h.symlink(t, filepath.Join(h.base, "bob", "files", "secret.txt"), "amy", "docs/secret-link.txt")
	h.symlink(t, "../outside-share.txt", "amy", "docs/up-link.txt")
	h.symlink(t, "sub", "amy", "docs/inner-alias")
	h.mustCreate(t, h.amy, "my-docs", "docs")

	attacks := []string{
		"..", "../outside-share.txt", "sub/../../outside-share.txt", "%2e%2e/outside-share.txt", "%252e%252e/outside-share.txt", "..%2foutside-share.txt",
		"..\\outside-share.txt", "sub\\..\\..\\outside-share.txt", "a\x00b", "sub/\x00", "/etc/passwd", "/outside-share.txt", "//outside-share.txt", "C:\\windows\\win.ini",
		strings.Repeat("a/", 3000), strings.Repeat("a", 5000), ".trash", ".TRASH/old.txt", ".Uploads/part", ".THUMBS/t.jpg", "sub/.trash", "sub/.thumbs/x", ".", "./a.txt", "sub/./b.txt", "sub//b.txt",
		"sub/", "a.txt/", "to-bob", "to-bob/secret.txt", "secret-link.txt", "up-link.txt", "inner-alias", "inner-alias/b.txt", "docs/a.txt", "../docs/a.txt",
	}
	for _, attack := range attacks {
		for _, action := range []string{"list", "download", "preview", "zip"} {
			rec := h.visit("GET", "/my-docs/"+action+query(attack))
			body := rec.Body.String()
			for _, secret := range []string{bobSecret, "amy-outside-share", "trashed", "partial", "thumb"} {
				if strings.Contains(body, secret) {
					t.Errorf("%s %q leaked %q", action, attack, secret)
				}
			}
			if rec.Code == 200 && attack != "" {
				t.Errorf("%s %q must not succeed, got 200", action, attack)
			}
			h.portal.shareMiss.reset(visitorIP)
			h.portal.shareFlood.reset(visitorIP)
		}
	}
	raw := httptest.NewRequest("GET", "/my-docs/download?path=%ZZ", nil)
	raw.RemoteAddr = visitorIP + ":1"
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, raw)
	if rec.Code != 404 {
		t.Errorf("a broken escape must be a miss, got %d", rec.Code)
	}
	if got := h.visit("GET", "/my-docs/download"+query("a.txt")); got.Code != 200 {
		t.Fatalf("the plain file still works: %d", got.Code)
	}
}

func TestSharePathsInTheURLItselfNeverReachAnything(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "my-docs", "docs")
	for _, route := range []string{"/my-docs/../login", "/my-docs/%2e%2e/login", "/my-docs/..%2flogin", "/my-docs/list/../../api/me", "/my-docs%2f..%2fadmin", "/my-docs/list%00", "/my-docs/a.txt", "/MY-DOCS/LIST/", "//my-docs/list", "/my-docs/list/a.txt"} {
		rec := h.visit("GET", route)
		if rec.Code == 200 && !strings.Contains(rec.Body.String(), `"kind"`) {
			t.Errorf("%s answered 200 with %q", route, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "amy") {
			t.Errorf("%s leaked %q", route, rec.Body)
		}
	}
}

func TestShareRefusesLinksAsTargetsAndNoticesThemLater(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.symlink(t, "notes.txt", "amy", "alias.txt")
	h.symlink(t, "docs", "amy", "alias-dir")
	h.symlink(t, filepath.Join(h.base, "bob", "files"), "amy", "to-bob")

	for _, target := range []string{"alias.txt", "alias-dir", "alias-dir/a.txt", "to-bob", "to-bob/secret.txt"} {
		if rec := h.create(h.amy, "", target, ""); rec.Code != 422 || !strings.Contains(rec.Body.String(), codeShareTargetInvalid) {
			t.Errorf("%q must be refused, got %d %s", target, rec.Code, rec.Body)
		}
	}

	h.mustCreate(t, h.amy, "my-file", "notes.txt")
	h.mustCreate(t, h.amy, "my-dir", "docs/sub")
	if err := os.Remove(filepath.Join(h.base, "amy", "files", "notes.txt")); err != nil {
		t.Fatal(err)
	}
	h.symlink(t, filepath.Join(h.base, "bob", "files", "secret.txt"), "amy", "notes.txt")
	got := h.visit("GET", "/my-file/download")
	if got.Code != 404 || strings.Contains(got.Body.String(), bobSecret) {
		t.Fatalf("a file replaced by a link must be unavailable, got %d", got.Code)
	}
	if err := os.RemoveAll(filepath.Join(h.base, "amy", "files", "docs", "sub")); err != nil {
		t.Fatal(err)
	}
	h.symlink(t, filepath.Join(h.base, "bob", "files"), "amy", "docs/sub")
	got = h.visit("GET", "/my-dir/list")
	if got.Code != 404 || strings.Contains(got.Body.String(), "secret") {
		t.Fatalf("a folder replaced by a link must be unavailable, got %d %s", got.Code, got.Body)
	}
}

func TestShareOwnersOnlyShareTheirOwnFiles(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)

	for _, target := range []string{"../bob/files", "../bob/files/secret.txt", "../../bob", "/bob/files/secret.txt", "secret.txt", "", "..", ".", "docs/..", ".trash", "docs/.trash", "missing.txt", "docs/a.txt/", "a\x00b", strings.Repeat("a", 3000)} {
		rec := h.create(h.amy, "", target, "")
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), codeShareTargetInvalid) {
			t.Errorf("amy sharing %q got %d %s", target, rec.Code, rec.Body)
		}
	}
	if rec := h.create(h.bob, "", "secret.txt", ""); rec.Code != 201 {
		t.Fatalf("bob can share his own file: %d %s", rec.Code, rec.Body)
	}
	if len(h.shares.ByOwner("amy")) != 0 {
		t.Fatal("amy must have no links")
	}
}

func TestShareManagementIsPerOwner(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "amys-link", "notes.txt")

	list := h.do("GET", "/api/shares", "", "192.0.2.10", h.bob)
	if list.Code != 200 || strings.Contains(list.Body.String(), "amys-link") {
		t.Fatalf("bob must not see amy's links: %s", list.Body)
	}
	if rec := h.do("PUT", "/api/shares/amys-link", `{"expires":"1d"}`, "192.0.2.10", h.bob); rec.Code != 404 || !strings.Contains(rec.Body.String(), codeShareNotFound) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("DELETE", "/api/shares/amys-link", "", "192.0.2.10", h.bob); rec.Code != 404 {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/shares/never-existed", "", "192.0.2.10", h.bob); rec.Code != 404 || !strings.Contains(rec.Body.String(), codeShareNotFound) {
		t.Fatalf("got %d", rec.Code)
	}
	if got := h.visit("GET", "/amys-link/download"); got.Code != 200 {
		t.Fatalf("the link must be untouched, got %d", got.Code)
	}
	for _, route := range []struct{ method, path, body string }{
		{"GET", "/api/shares", ""}, {"POST", "/api/shares", `{"path":"notes.txt"}`}, {"PUT", "/api/shares/amys-link", `{"expires":"1d"}`},
		{"DELETE", "/api/shares/amys-link", ""}, {"GET", "/api/admin/shares", ""}, {"DELETE", "/api/admin/shares/amys-link", ""},
	} {
		if rec := h.do(route.method, route.path, route.body, "192.0.2.10", nil); rec.Code != 401 {
			t.Errorf("%s %s without a session got %d", route.method, route.path, rec.Code)
		}
	}
	if rec := h.do("GET", "/shares", "", "192.0.2.10", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestShareManagementRefusesCrossOriginRequests(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "amys-link", "notes.txt")

	for _, route := range []struct{ method, path, body string }{
		{"POST", "/api/shares", `{"path":"notes.txt"}`}, {"PUT", "/api/shares/amys-link", `{"expires":"1d"}`}, {"DELETE", "/api/shares/amys-link", ""},
		{"DELETE", "/api/admin/shares/amys-link", ""},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		req.RemoteAddr = "192.0.2.10:5"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://evil.example")
		req.AddCookie(h.amy)
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s cross-origin got %d", route.method, route.path, rec.Code)
		}
	}
	if len(h.shares.All()) != 1 {
		t.Fatal("nothing may have changed")
	}
}

func TestAdminSeesAndRevokesEveryLinkAndOthersCannot(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.bob, "bobs-link", "secret.txt")
	h.mustCreate(t, h.amy, "amys-link", "notes.txt")

	list := h.do("GET", "/api/admin/shares", "", "192.0.2.10", h.amy)
	if list.Code != 200 || !strings.Contains(list.Body.String(), "bobs-link") || !strings.Contains(list.Body.String(), `"owner":"bob"`) {
		t.Fatalf("the admin sees all links: %d %s", list.Code, list.Body)
	}
	if rec := h.do("GET", "/api/admin/shares", "", "192.0.2.10", h.bob); rec.Code != 403 || !strings.Contains(rec.Body.String(), codeAdminsOnly) {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/admin/shares/amys-link", "", "192.0.2.10", h.bob); rec.Code != 403 {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/admin/shares/bobs-link", "", "192.0.2.10", h.amy); rec.Code != 204 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := h.visit("GET", "/bobs-link/download"); got.Code != 404 {
		t.Fatalf("a revoked link is gone at once, got %d", got.Code)
	}
	if rec := h.do("DELETE", "/api/admin/shares/bobs-link", "", "192.0.2.10", h.amy); rec.Code != 404 {
		t.Fatalf("got %d", rec.Code)
	}
	mine := h.do("GET", "/api/shares", "", "192.0.2.10", h.amy)
	if strings.Contains(mine.Body.String(), "owner") {
		t.Fatalf("the owner's own list names no owner: %s", mine.Body)
	}
}

func TestShareBlankNameGetsARandomOne(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	seen := map[string]bool{}
	random := regexp.MustCompile(`^[a-z0-9]{10}$`)
	for range 20 {
		view := h.mustCreate(t, h.amy, "", "notes.txt")
		if !random.MatchString(view.Name) || seen[view.Name] {
			t.Fatalf("got %q", view.Name)
		}
		seen[view.Name] = true
	}
	if got := h.visit("GET", "/"+slices.Collect(func(yield func(string) bool) {
		for n := range seen {
			if !yield(n) {
				return
			}
		}
	})[0]+"/download"); got.Code != 200 {
		t.Fatalf("got %d", got.Code)
	}
}

func TestShareLimitPerOwner(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	for i := range sharesPerOwner {
		if rec := h.create(h.amy, fmt.Sprintf("link-%03d", i), "notes.txt", ""); rec.Code != 201 {
			t.Fatalf("link %d got %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := h.create(h.amy, "one-too-many", "notes.txt", "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), codeShareLimit) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.create(h.bob, "bobs-first", "secret.txt", ""); rec.Code != 201 {
		t.Fatalf("another owner has their own allowance: %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/shares/link-000", "", "192.0.2.10", h.amy); rec.Code != 204 {
		t.Fatal(rec.Code)
	}
	if rec := h.create(h.amy, "one-too-many", "notes.txt", ""); rec.Code != 201 {
		t.Fatalf("revoking frees a place: %d %s", rec.Code, rec.Body)
	}
}

func TestShareTwoCreationsOfTheSameNameAtOnceHaveOneWinner(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	var created, taken atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cookie := h.amy
			if i%2 == 1 {
				cookie = h.bob
			}
			target := "notes.txt"
			if i%2 == 1 {
				target = "secret.txt"
			}
			switch h.create(cookie, "Same-Name", target, "").Code {
			case 201:
				created.Add(1)
			case 409:
				taken.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || taken.Load() != 15 {
		t.Fatalf("created %d, taken %d", created.Load(), taken.Load())
	}
}

func TestShareDownloadCounterRule(t *testing.T) {
	h := newShareHarness(t)
	h.write(t, "amy", "big.bin", make([]byte, 4096))
	h.mkdir(t, "amy", "folder")
	h.write(t, "amy", "folder/f.txt", []byte("f"))
	h.mustCreate(t, h.amy, "counted", "big.bin")
	h.mustCreate(t, h.amy, "counted-dir", "folder")

	count := func(name string) (int64, string) {
		share, _ := h.shares.Get(name)
		return share.Downloads, share.LastUsed.Format(time.RFC3339)
	}
	h.visit("GET", "/counted")
	h.visit("GET", "/counted/list")
	h.visit("HEAD", "/counted/download")
	h.visit("GET", "/counted/preview")
	if n, _ := count("counted"); n != 0 {
		t.Fatalf("pages, listings and HEAD requests are not downloads, got %d", n)
	}
	h.advance(time.Hour)
	h.visit("GET", "/counted/download")
	n, last := count("counted")
	if n != 1 || last != shareEpochTime.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("got %d %s", n, last)
	}
	h.visit("GET", "/counted/download", "Range", "bytes=0-99")
	h.visit("GET", "/counted/download", "Range", "bytes=100-")
	h.visit("GET", "/counted/download", "Range", "bytes=-10")
	h.visit("GET", "/counted/download", "Range", "bytes=0-9,20-29")
	if n, _ := count("counted"); n != 3 {
		t.Fatalf("a download counts when it starts at the beginning, resumes do not, got %d", n)
	}
	h.visit("GET", "/counted-dir/zip")
	h.visit("GET", "/counted-dir/download"+query("f.txt"))
	h.visit("HEAD", "/counted-dir/zip")
	if n, _ := count("counted-dir"); n != 2 {
		t.Fatalf("got %d", n)
	}
	if mine := h.do("GET", "/api/shares", "", "192.0.2.10", h.amy); !strings.Contains(mine.Body.String(), `"downloads":3`) || !strings.Contains(mine.Body.String(), `"lastUsed":"`) {
		t.Fatalf("the owner sees the counters: %s", mine.Body)
	}
}

func TestShareConcurrentDownloadsAreAllCounted(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "busy-link", "notes.txt")
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.visitFrom(fmt.Sprintf("198.51.100.%d", i+1), "GET", "/busy-link/download")
		}()
	}
	wg.Wait()
	if share, _ := h.shares.Get("busy-link"); share.Downloads != 40 {
		t.Fatalf("got %d", share.Downloads)
	}
}

func TestShareMissesAreRateLimitedPerAddress(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "real-link", "notes.txt")

	for i := range shareMissLimit {
		if rec := h.visit("GET", fmt.Sprintf("/guess-%d", i)); rec.Code != 404 {
			t.Fatalf("guess %d got %d", i, rec.Code)
		}
	}
	blocked := h.visit("GET", "/guess-next")
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") == "" {
		t.Fatalf("the guessing must stop, got %d", blocked.Code)
	}
	if rec := h.visit("GET", "/real-link/download"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("a blocked address learns nothing about which names exist, got %d", rec.Code)
	}
	if rec := h.visitFrom("198.51.100.8", "GET", "/real-link/download"); rec.Code != 200 {
		t.Fatalf("other addresses are unaffected, got %d", rec.Code)
	}
	if rec := h.visit("GET", "/api/me"); rec.Code != 401 {
		t.Fatalf("the limit only covers share routes, got %d", rec.Code)
	}
	if rec := h.visit("GET", "/Not_A_Name.php"); rec.Code != 404 {
		t.Fatalf("names that cannot be links never reach the limiter, got %d", rec.Code)
	}
}

func TestShareIPv6AddressesAreLimitedAsOnePrefix(t *testing.T) {
	h := newShareHarness(t)
	for i := range shareMissLimit {
		h.visitFrom(fmt.Sprintf("2001:db8:1:2::%x", i+1), "GET", fmt.Sprintf("/guess-%d", i))
	}
	if rec := h.visitFrom("2001:db8:1:2:ffff::1", "GET", "/another"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("one /64 is one visitor, got %d", rec.Code)
	}
}

func TestShareForwardedHeadersDoNotChooseTheLimitedAddress(t *testing.T) {
	h := newShareHarness(t)
	for i := range shareMissLimit {
		h.visit("GET", fmt.Sprintf("/guess-%d", i), "X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i), "X-Real-IP", "203.0.113.1", "Forwarded", "for=203.0.113.9")
	}
	if rec := h.visit("GET", "/guess-last", "X-Forwarded-For", "203.0.113.200"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed headers must not buy a fresh allowance, got %d", rec.Code)
	}
}

func TestShareResumedDownloadsAreNotBlockedByTheLimiter(t *testing.T) {
	h := newShareHarness(t)
	h.write(t, "amy", "big.bin", make([]byte, 1<<20))
	h.mustCreate(t, h.amy, "resume-me", "big.bin")
	for i := range 100 {
		start := i * 1000
		if rec := h.visit("GET", "/resume-me/download", "Range", fmt.Sprintf("bytes=%d-", start)); rec.Code != 206 {
			t.Fatalf("resume %d got %d", i, rec.Code)
		}
	}
	if rec := h.visit("GET", "/resume-me/download"); rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestShareFloodOfValidRequestsIsLimited(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "popular", "notes.txt")
	var last int
	for range shareFloodLimit + 5 {
		last = h.visit("GET", "/popular/list").Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("got %d", last)
	}
}

func TestShareStoreSurvivesARestart(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "survivor", "docs")
	h.visit("GET", "/survivor/zip")

	again, err := auth.OpenShares(h.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewPortal(PortalDeps{
		Users:     h.users,
		OpenFiles: func(user string) (system.Files, error) { return system.OpenUserFiles(h.base, user) },
		Shares:    again,
	})
	req := httptest.NewRequest("GET", "/survivor/list", nil)
	req.RemoteAddr = visitorIP + ":1"
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	if share, _ := again.Get("survivor"); share.Downloads != 1 {
		t.Fatalf("the counter must survive too, got %d", share.Downloads)
	}
}

func TestDeletingAUserRemovesTheirLinksAndANewUserInheritsNone(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.bob, "bobs-link", "secret.txt")
	h.mustCreate(t, h.amy, "amys-link", "notes.txt")

	rec := h.do("DELETE", "/api/admin/users/bob", `{"confirm":"`+amyPassword+`"}`, "192.0.2.10", h.amy)
	if rec.Code != 204 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := h.visit("GET", "/bobs-link/download"); got.Code != 404 {
		t.Fatalf("got %d", got.Code)
	}
	if len(h.shares.ByOwner("bob")) != 0 {
		t.Fatal("the links must be deleted, not only hidden")
	}
	if len(h.shares.ByOwner("amy")) != 1 {
		t.Fatal("other users keep theirs")
	}
	if err := h.users.Create("bob", "a brand new password"); err != nil {
		t.Fatal(err)
	}
	h.write(t, "bob", "secret.txt", []byte(bobSecret))
	h.portal.EndSessions("bob")
	if got := h.visit("GET", "/bobs-link/download"); got.Code != 404 {
		t.Fatalf("a new bob must not inherit the old link, got %d", got.Code)
	}
}

func TestPanelDeletionAndPasswordChangesGoThroughEndSessions(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "amys-link", "notes.txt")

	h.portal.EndSessions("amy")
	if len(h.shares.ByOwner("amy")) != 1 {
		t.Fatal("a password change ends sessions but the account still exists, so its links stay")
	}
	if err := h.users.Delete("amy"); err != nil {
		t.Fatal(err)
	}
	h.portal.EndSessions("amy")
	if len(h.shares.ByOwner("amy")) != 0 {
		t.Fatal("the links of a deleted account must go")
	}
}

func TestStartupForgetsLinksOfAccountsRemovedWhileTheyWereAway(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "amys-link", "notes.txt")
	h.mustCreate(t, h.bob, "bobs-link", "secret.txt")

	NewPortal(PortalDeps{Users: system.NewFakeUsers("amy"), Shares: h.shares})
	if len(h.shares.ByOwner("bob")) != 0 || len(h.shares.ByOwner("amy")) != 1 {
		t.Fatalf("got %+v", h.shares.All())
	}
	NewPortal(PortalDeps{Users: system.NewFakeUsers(), Shares: h.shares})
	if len(h.shares.ByOwner("amy")) != 1 {
		t.Fatal("an empty user list is more likely a fault than a fact, so nothing is wiped")
	}
}

func TestShareVisitorResponsesCarryNoSessionOrOwnerDetails(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "my-docs", "docs")
	for _, route := range []string{"/my-docs", "/my-docs/list", "/my-docs/list" + query("sub"), "/my-docs/download" + query("a.txt"), "/my-docs/zip", "/nope-nope"} {
		rec := h.visit("GET", route, "Cookie", portalCookieName+"="+h.amy.Value)
		if _, found := rec.Header()["Set-Cookie"]; found {
			t.Errorf("%s sets a cookie", route)
		}
		if route != "/my-docs/zip" && route != "/my-docs/download"+query("a.txt") {
			for _, leak := range []string{"amy", h.base} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("%s leaks %q", route, leak)
				}
			}
		}
		if rec.Header().Get("Referrer-Policy") != "no-referrer" || rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
			t.Errorf("%s lacks hardening headers: %v", route, rec.Header())
		}
	}
	other := h.visit("GET", "/api/files", "Cookie", portalCookieName+"="+h.amy.Value)
	if other.Code != 200 {
		t.Fatalf("sanity: the owner's session still works on session routes, got %d", other.Code)
	}
	if rec := h.visit("GET", "/api/files"); rec.Code != 401 {
		t.Fatalf("a visitor has no session routes, got %d", rec.Code)
	}
}

func TestSharePagesRenderNamesAsText(t *testing.T) {
	for _, name := range []string{"web/portal/share.js", "web/portal/shares.js", "web/portal/shareui.js", "web/portal/admin.js", "web/files.js", "web/portal/receive.js"} {
		bytes, err := fs.ReadFile(webFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		raw := string(bytes)
		for _, banned := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
			if strings.Contains(raw, banned) {
				t.Errorf("%s uses %s", name, banned)
			}
		}
	}
	raw, _ := fs.ReadFile(webFiles, "web/portal/shareui.js")
	if !strings.Contains(string(raw), "window.location.origin") {
		t.Error("the copied address must be built from the page's own origin")
	}
}

func TestMissesOnRoutePathsNeverCountAgainstTheVisitor(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mustCreate(t, h.amy, "real-link", "notes.txt")
	for i := range shareMissLimit + 10 {
		if rec := h.visit("GET", fmt.Sprintf("/static/nope-%d.png", i)); rec.Code != http.StatusNotFound {
			t.Fatalf("got %d", rec.Code)
		}
		if rec := h.visit("GET", "/api/nothing"); rec.Code != http.StatusNotFound {
			t.Fatalf("got %d", rec.Code)
		}
	}
	if rec := h.visit("GET", "/real-link/download"); rec.Code != 200 {
		t.Fatalf("a visitor with broken asset requests must still reach a real link, got %d", rec.Code)
	}
}
