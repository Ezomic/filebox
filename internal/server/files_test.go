package server

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const secretContent = "top secret outside the root"

type fileSession struct {
	*portalFilesHarness
	t       *testing.T
	cookie  *http.Cookie
	handler http.Handler
	root    string
	outside string
}

func newFileSession(t *testing.T) *fileSession {
	t.Helper()
	h := newPortalFilesHarness(t)
	outside := filepath.Join(h.base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secretContent), 0o644); err != nil {
		t.Fatal(err)
	}

	return &fileSession{portalFilesHarness: h, t: t, cookie: h.signedIn(t, "amy", amyPassword), handler: h.portal, root: filepath.Join(h.base, "amy", "files"), outside: outside}
}

func (s *fileSession) call(method, target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "192.0.2.10:5555"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.AddCookie(s.cookie)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)

	return rec
}

func (s *fileSession) get(target string) *httptest.ResponseRecorder {
	return s.call("GET", target, nil, "")
}

func (s *fileSession) put(target, content string) *httptest.ResponseRecorder {
	return s.call("PUT", target, strings.NewReader(content), "application/octet-stream")
}

func (s *fileSession) post(target string, fields map[string]any) *httptest.ResponseRecorder {
	return s.call("POST", target, strings.NewReader(jsonBody(s.t, fields)), "application/json")
}

func (s *fileSession) seed(path, content string) {
	s.t.Helper()
	full := filepath.Join(s.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *fileSession) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(path)))

	return err == nil
}

func (s *fileSession) rootNames() []string {
	s.t.Helper()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		s.t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

func (s *fileSession) assertOutsideUntouched() {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.outside, "secret.txt"))
	if err != nil || string(data) != secretContent {
		s.t.Fatalf("outside file damaged: %q %v", data, err)
	}
	entries, _ := os.ReadDir(s.outside)
	if len(entries) != 1 {
		s.t.Fatalf("outside directory was modified: %v", entries)
	}
}

func apiError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
		t.Fatalf("expected an {\"error\": ...} body, got %d %q", rec.Code, rec.Body.String())
	}

	return body["error"]
}

func TestListFiles(t *testing.T) {
	s := newFileSession(t)
	s.seed("b.txt", "bb")
	s.seed("sub/inner.txt", "inner")
	s.seed("empty.bin", "")

	rec := s.get("/api/files")
	if rec.Code != http.StatusOK {
		t.Fatalf("root: %d %s", rec.Code, rec.Body)
	}
	var listing struct {
		Path    string `json:"path"`
		Entries []struct {
			Name     string    `json:"name"`
			IsDir    bool      `json:"isDir"`
			Size     int64     `json:"size"`
			Modified time.Time `json:"modified"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 3 || listing.Entries[0].Name != "sub" || !listing.Entries[0].IsDir ||
		listing.Entries[1].Name != "b.txt" || listing.Entries[1].Size != 2 || listing.Entries[1].Modified.IsZero() {
		t.Fatalf("unexpected listing: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), s.root) {
		t.Fatal("listing leaks the server path")
	}

	if rec := s.get("/api/files?path=sub"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "inner.txt") {
		t.Fatalf("subdir: %d %s", rec.Code, rec.Body)
	}
	if rec := s.get("/api/files?path="); rec.Code != http.StatusOK {
		t.Fatalf("empty path means root: %d", rec.Code)
	}
}

func TestEmptyFolderListsAsEmptyArray(t *testing.T) {
	s := newFileSession(t)
	if body := s.get("/api/files").Body.String(); !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("got %s", body)
	}
}

func TestListFileErrors(t *testing.T) {
	s := newFileSession(t)
	s.seed("f.txt", "x")

	cases := map[string]int{
		"/api/files?path=f.txt":   http.StatusUnprocessableEntity,
		"/api/files?path=nope":    http.StatusNotFound,
		"/api/files?path=nope/x":  http.StatusNotFound,
		"/api/files?path=f.txt/x": http.StatusUnprocessableEntity,
	}
	for target, want := range cases {
		rec := s.get(target)
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d (%s)", target, rec.Code, want, rec.Body)
		}
		apiError(t, rec)
	}
}

// Every payload is the raw query value as a client could send it.
var hostilePaths = map[string]string{
	"dotdot":                   "..",
	"dotdot dotdot":            "../..",
	"dotdot then secret":       "../outside/secret.txt",
	"encoded dotdot":           "%2e%2e",
	"encoded dotdot slash":     "%2e%2e%2f",
	"encoded dotdot secret":    "%2e%2e%2foutside%2fsecret.txt",
	"half encoded":             "..%2foutside%2fsecret.txt",
	"mixed":                    "a/..%2f..%2foutside%2fsecret.txt",
	"absolute":                 "/etc/passwd",
	"encoded absolute":         "%2fetc%2fpasswd",
	"absolute root":            "%2f",
	"backslash":                "..%5coutside%5csecret.txt",
	"backslash only":           "a%5cb",
	"windows drive":            "C:%5cWindows",
	"nul":                      "a%00b",
	"nul alone":                "%00",
	"newline":                  "a%0ab",
	"tab":                      "a%09b",
	"dot":                      ".",
	"dot segment":              "a/./b",
	"double slash":             "a//b",
	"trailing slash":           "a/",
	"trailing dot":             "a.",
	"trailing space":           "a%20",
	"trailing dot in dir":      "dir./f",
	"only spaces":              "%20%20",
	"unicode dotdot lookalike": "%ef%bc%8e%ef%bc%8e%2f",
	"invalid utf8":             "%ff%fe",
	"malformed escape":         "%zz",
	"very long":                strings.Repeat("a", 5000),
	"very long segments":       strings.Repeat("abcdefgh/", 600),
	"through symlink read":     "escape/secret.txt",
	"through symlink new file": "escape/new.txt",
	"through symlink folder":   "escape/dir",
}

func (s *fileSession) plantSymlinks() bool {
	s.t.Helper()
	if err := os.Symlink(s.outside, filepath.Join(s.root, "escape")); err != nil {
		s.t.Logf("SYMLINK CASES NOT RUN: this OS refused to create a symlink: %v", err)

		return false
	}
	if err := os.Symlink(filepath.Join(s.outside, "secret.txt"), filepath.Join(s.root, "link.txt")); err != nil {
		s.t.Logf("SYMLINK CASES NOT RUN: this OS refused to create a symlink: %v", err)

		return false
	}

	return true
}

func assertRejectedWithoutLeak(t *testing.T, s *fileSession, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("%s: got %d, want 400 or 404 (%.120s)", what, rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, secretContent) || strings.Contains(body, s.root) || strings.Contains(body, s.outside) {
		t.Errorf("%s leaked data or a server path: %.200s", what, body)
	}
	if rec.Code >= 400 {
		apiError(t, rec)
	}
}

func TestPathTraversalIsRefusedOnEveryRoute(t *testing.T) {
	s := newFileSession(t)
	s.plantSymlinks()
	s.seed("inside.txt", "inside")
	before := strings.Join(s.rootNames(), ",")

	for name, raw := range hostilePaths {
		decoded, err := url.QueryUnescape(raw)
		jsonValue := decoded

		assertRejectedWithoutLeak(t, s, "list "+name, s.get("/api/files?path="+raw))
		assertRejectedWithoutLeak(t, s, "download "+name, s.get("/api/files/download?path="+raw))
		assertRejectedWithoutLeak(t, s, "upload "+name, s.put("/api/files/content?path="+raw+"&overwrite=1", "pwned"))
		assertRejectedWithoutLeak(t, s, "delete "+name, s.call("DELETE", "/api/files?path="+raw, nil, ""))
		if err != nil || !utf8.ValidString(decoded) {
			continue
		}
		assertRejectedWithoutLeak(t, s, "folder "+name, s.post("/api/files/folder", map[string]any{"path": jsonValue}))
		assertRejectedWithoutLeak(t, s, "rename from "+name, s.post("/api/files/rename", map[string]any{"from": jsonValue, "to": "x"}))
		assertRejectedWithoutLeak(t, s, "rename to "+name, s.post("/api/files/rename", map[string]any{"from": "inside.txt", "to": jsonValue}))
	}

	for _, link := range []string{"escape", "link.txt"} {
		assertRejectedWithoutLeak(t, s, "list "+link, s.get("/api/files?path="+link))
		assertRejectedWithoutLeak(t, s, "download "+link, s.get("/api/files/download?path="+link))
		assertRejectedWithoutLeak(t, s, "overwrite "+link, s.put("/api/files/content?path="+link+"/x", "pwned"))
	}

	s.assertOutsideUntouched()
	if got := strings.Join(s.rootNames(), ","); got != before {
		t.Fatalf("root changed: before %q after %q", before, got)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "inside.txt")); string(data) != "inside" {
		t.Fatal("inside.txt was modified")
	}
}

func TestDotDotInTheURLPathDoesNotReachTheFilesystem(t *testing.T) {
	s := newFileSession(t)
	for _, target := range []string{"/api/files/../files?path=..", "/api/files/%2e%2e/files", "/files/../../etc/passwd", "/static/../index.html", "/static/%2e%2e/index.html"} {
		rec := s.get(target)
		if strings.Contains(rec.Body.String(), secretContent) {
			t.Errorf("%s leaked", target)
		}
	}
}

func TestDownload(t *testing.T) {
	s := newFileSession(t)
	s.seed("page.html", `<script>alert(1)</script>`)
	s.seed("docs/ünï cödé 日本.txt", "unicode name")
	s.seed("empty", "")

	rec := s.get("/api/files/download?path=page.html")
	if rec.Code != http.StatusOK || rec.Body.String() != `<script>alert(1)</script>` {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	header := rec.Header()
	if header.Get("Content-Type") != "application/octet-stream" ||
		header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(header.Get("Content-Disposition"), "page.html") {
		t.Fatalf("unsafe headers: %v", header)
	}
	if !strings.Contains(header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatal("CSP header missing on download")
	}

	rec = s.get("/api/files/download?path=docs%2F%C3%BCn%C3%AF%20c%C3%B6d%C3%A9%20%E6%97%A5%E6%9C%AC.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != "unicode name" {
		t.Fatalf("unicode download: %d", rec.Code)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(disposition, "attachment") || strings.ContainsAny(disposition, "\r\n") {
		t.Fatalf("disposition %q", disposition)
	}

	if rec := s.get("/api/files/download?path=empty"); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("zero byte download: %d", rec.Code)
	}
}

func TestDoubleEncodedTraversalIsJustALiteralName(t *testing.T) {
	s := newFileSession(t)
	literal := "%2e%2e%2foutside%2fsecret.txt"
	target := "path=%252e%252e%252foutside%252fsecret.txt"

	assertRejectedWithoutLeak(t, s, "list", s.get("/api/files?"+target))
	assertRejectedWithoutLeak(t, s, "download", s.get("/api/files/download?"+target))
	if rec := s.put("/api/files/content?"+target, "mine"); rec.Code != http.StatusNoContent {
		t.Fatalf("upload: %d", rec.Code)
	}
	if !s.exists(literal) {
		t.Fatalf("the name must be stored literally, root has %v", s.rootNames())
	}
	s.assertOutsideUntouched()
}

func TestDownloadHeadersCannotBeInjectedThroughTheFileName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot store a quote in a file name")
	}
	s := newFileSession(t)
	s.seed(`quote".txt`, "x")
	s.seed("semi;colon.txt", "x")
	rec := s.get("/api/files/download?path=quote%22.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if got := rec.Header().Values("Content-Disposition"); len(got) != 1 || !strings.HasPrefix(got[0], "attachment;") {
		t.Fatalf("got %v", got)
	}
}

func TestDownloadErrors(t *testing.T) {
	s := newFileSession(t)
	s.seed("dir/f", "x")
	cases := map[string]int{
		"/api/files/download?path=dir":     http.StatusUnprocessableEntity,
		"/api/files/download?path=":        http.StatusBadRequest,
		"/api/files/download":              http.StatusBadRequest,
		"/api/files/download?path=nope":    http.StatusNotFound,
		"/api/files/download?path=dir/f/x": http.StatusUnprocessableEntity,
	}
	for target, want := range cases {
		rec := s.get(target)
		if rec.Code != want {
			t.Errorf("%s: got %d want %d", target, rec.Code, want)
		}
		apiError(t, rec)
	}
}

func TestUpload(t *testing.T) {
	s := newFileSession(t)
	s.seed("sub/keep", "k")

	if rec := s.put("/api/files/content?path=new.txt", "hello"); rec.Code != http.StatusNoContent {
		t.Fatalf("new file: %d %s", rec.Code, rec.Body)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "new.txt")); string(data) != "hello" {
		t.Fatalf("content %q", data)
	}
	if rec := s.put("/api/files/content?path=sub/zero", ""); rec.Code != http.StatusNoContent || !s.exists("sub/zero") {
		t.Fatalf("zero byte upload: %d", rec.Code)
	}
	if rec := s.put("/api/files/content?path=%C3%BCn%C3%AF.txt", "u"); rec.Code != http.StatusNoContent || !s.exists("ünï.txt") {
		t.Fatalf("unicode upload: %d", rec.Code)
	}
	if rec := s.put("/api/files/content?path=%25%25%21%23%2C%3B.txt", "u"); rec.Code != http.StatusNoContent || !s.exists("%%!#,;.txt") {
		t.Fatalf("punctuation name upload: %d", rec.Code)
	}
}

func TestUploadOverwriteRules(t *testing.T) {
	s := newFileSession(t)
	s.seed("f.txt", "original")
	s.seed("dir/x", "x")

	for _, target := range []string{"/api/files/content?path=f.txt", "/api/files/content?path=f.txt&overwrite=0", "/api/files/content?path=f.txt&overwrite="} {
		if rec := s.put(target, "changed"); rec.Code != http.StatusConflict {
			t.Errorf("%s: got %d, want 409", target, rec.Code)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "f.txt")); string(data) != "original" {
		t.Fatalf("existing file changed without overwrite: %q", data)
	}

	for _, value := range []string{"yes", "true", "2", "01", "1%20", "1,1"} {
		if rec := s.put("/api/files/content?path=f.txt&overwrite="+value, "changed"); rec.Code != http.StatusBadRequest {
			t.Errorf("overwrite=%s: got %d, want 400", value, rec.Code)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "f.txt")); string(data) != "original" {
		t.Fatalf("file changed by an invalid overwrite flag: %q", data)
	}

	if rec := s.put("/api/files/content?path=f.txt&overwrite=1", "changed"); rec.Code != http.StatusNoContent {
		t.Fatalf("overwrite=1: %d", rec.Code)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "f.txt")); string(data) != "changed" {
		t.Fatalf("overwrite did not apply: %q", data)
	}

	for _, target := range []string{"/api/files/content?path=dir", "/api/files/content?path=dir&overwrite=1"} {
		if rec := s.put(target, "x"); rec.Code != http.StatusConflict {
			t.Errorf("%s over a folder: got %d, want 409", target, rec.Code)
		}
	}
	if !s.exists("dir/x") {
		t.Fatal("folder was replaced")
	}
}

func TestUploadRequiresOctetStream(t *testing.T) {
	s := newFileSession(t)
	for _, contentType := range []string{"", "text/plain", "text/html", "application/json", "multipart/form-data; boundary=x", "application/x-www-form-urlencoded", "application/octet-streamx"} {
		rec := s.call("PUT", "/api/files/content?path=x.bin", strings.NewReader("data"), contentType)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content type %q: got %d, want 415", contentType, rec.Code)
		}
	}
	if s.exists("x.bin") {
		t.Fatal("a rejected upload still created a file")
	}
	rec := s.call("PUT", "/api/files/content?path=ok.bin", strings.NewReader("data"), "application/octet-stream; charset=binary")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("octet-stream with parameters should be accepted, got %d", rec.Code)
	}
}

func TestUploadSizeCap(t *testing.T) {
	s := newFileSession(t)

	exact := strings.Repeat("x", portalMaxUpload)
	if rec := s.put("/api/files/content?path=exact.bin", exact); rec.Code != http.StatusNoContent {
		t.Fatalf("exactly at the cap: %d %s", rec.Code, rec.Body)
	}
	if info, err := os.Stat(filepath.Join(s.root, "exact.bin")); err != nil || info.Size() != portalMaxUpload {
		t.Fatalf("stored file: %v %v", info, err)
	}

	over := exact + "x"
	rec := s.put("/api/files/content?path=over.bin", over)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over (declared length): %d", rec.Code)
	}
	apiError(t, rec)

	unknownLength := io.MultiReader(strings.NewReader(over))
	rec = s.call("PUT", "/api/files/content?path=chunked.bin", unknownLength, "application/octet-stream")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("one byte over (unknown length): %d", rec.Code)
	}

	rec = s.call("PUT", "/api/files/content?path=exact.bin&overwrite=1", io.MultiReader(strings.NewReader(over)), "application/octet-stream")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("overwrite over the cap: %d", rec.Code)
	}
	if info, _ := os.Stat(filepath.Join(s.root, "exact.bin")); info == nil || info.Size() != portalMaxUpload {
		t.Fatal("a failed overwrite damaged the existing file")
	}

	if got := strings.Join(s.rootNames(), ","); got != "exact.bin" {
		t.Fatalf("partial or temp files left behind: %s", got)
	}
}

func TestUploadErrors(t *testing.T) {
	s := newFileSession(t)
	s.seed("f", "x")
	cases := map[string]int{
		"/api/files/content":                                  http.StatusBadRequest,
		"/api/files/content?path=":                            http.StatusBadRequest,
		"/api/files/content?path=%20":                         http.StatusBadRequest,
		"/api/files/content?path=...":                         http.StatusBadRequest,
		"/api/files/content?path=!!!":                         http.StatusNoContent,
		"/api/files/content?path=missing/x":                   http.StatusNotFound,
		"/api/files/content?path=f/x":                         http.StatusUnprocessableEntity,
		"/api/files/content?path=" + strings.Repeat("a", 256): http.StatusBadRequest,
	}
	for target, want := range cases {
		rec := s.put(target, "data")
		if rec.Code != want {
			t.Errorf("%.70s: got %d, want %d (%s)", target, rec.Code, want, rec.Body)
		}
	}
	if got := strings.Join(s.rootNames(), ","); got != "!!!,f" {
		t.Fatalf("unexpected files: %s", got)
	}
}

type brokenBody struct{ sent bool }

func (b *brokenBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial"), nil
	}

	return 0, errors.New("connection reset by peer")
}

func TestInterruptedUploadLeavesNothingBehind(t *testing.T) {
	s := newFileSession(t)
	s.seed("keep", "original")
	for _, target := range []string{"/api/files/content?path=new", "/api/files/content?path=keep&overwrite=1"} {
		rec := s.call("PUT", target, &brokenBody{}, "application/octet-stream")
		if rec.Code < 400 {
			t.Errorf("%s: got %d", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "reset by peer") {
			t.Error("raw error leaked")
		}
	}
	if got := strings.Join(s.rootNames(), ","); got != "keep" {
		t.Fatalf("left behind: %s", got)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "keep")); string(data) != "original" {
		t.Fatalf("original damaged: %q", data)
	}
}

type trickleBody struct {
	chunks int
	pause  time.Duration
}

func (b *trickleBody) Read(p []byte) (int, error) {
	if b.chunks == 0 {
		return 0, io.EOF
	}
	b.chunks--
	time.Sleep(b.pause)

	return copy(p, "0123456789"), nil
}

func TestSlowUploadOutlivesTheServerReadTimeout(t *testing.T) {
	s := newFileSession(t)
	srv := httptest.NewUnstartedServer(s.handler)
	srv.Config.ReadTimeout = 250 * time.Millisecond
	srv.Config.WriteTimeout = 250 * time.Millisecond
	srv.Start()
	defer srv.Close()

	req, err := http.NewRequest("PUT", srv.URL+"/api/files/content?path=slow.bin", &trickleBody{chunks: 6, pause: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.AddCookie(s.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upload slower than ReadTimeout was cut off: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if info, err := os.Stat(filepath.Join(s.root, "slow.bin")); err != nil || info.Size() != 60 {
		t.Fatalf("stored: %v %v", info, err)
	}
}

func TestCreateFolder(t *testing.T) {
	s := newFileSession(t)

	if rec := s.post("/api/files/folder", map[string]any{"path": "photos"}); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := s.post("/api/files/folder", map[string]any{"path": "photos/2026"}); rec.Code != http.StatusCreated {
		t.Fatalf("nested: %d", rec.Code)
	}
	if rec := s.post("/api/files/folder", map[string]any{"path": "photos"}); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", rec.Code)
	}
	s.seed("file", "x")
	if rec := s.post("/api/files/folder", map[string]any{"path": "file"}); rec.Code != http.StatusConflict {
		t.Fatalf("over a file: %d", rec.Code)
	}
	if rec := s.post("/api/files/folder", map[string]any{"path": "nope/x"}); rec.Code != http.StatusNotFound {
		t.Fatalf("missing parent: %d", rec.Code)
	}
	if rec := s.post("/api/files/folder", map[string]any{"path": "file/x"}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("parent is a file: %d", rec.Code)
	}
	if rec := s.post("/api/files/folder", map[string]any{"path": "日本語 フォルダ"}); rec.Code != http.StatusCreated {
		t.Fatalf("unicode: %d", rec.Code)
	}
	if rec := s.post("/api/files/folder", map[string]any{"path": "!!!"}); rec.Code != http.StatusCreated {
		t.Fatalf("punctuation only is a legal name: %d", rec.Code)
	}
}

func TestCreateFolderBadInput(t *testing.T) {
	s := newFileSession(t)
	for _, path := range []string{"", " ", "   ", ".", "..", "/", "a b ", strings.Repeat("a", 256)} {
		if rec := s.post("/api/files/folder", map[string]any{"path": path}); rec.Code != http.StatusBadRequest {
			t.Errorf("path %q: got %d, want 400", path, rec.Code)
		}
	}
	bodies := map[string]string{
		"null":        "null",
		"empty":       "",
		"array":       "[]",
		"wrong type":  `{"path": 5}`,
		"object path": `{"path": {"a": 1}}`,
		"not json":    "path=photos",
		"truncated":   `{"path": "pho`,
		"missing key": `{}`,
		"huge":        `{"path": "` + strings.Repeat("a", 10000) + `"}`,
	}
	for name, body := range bodies {
		rec := s.call("POST", "/api/files/folder", strings.NewReader(body), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
	rec := s.call("POST", "/api/files/folder", strings.NewReader(`{"path":"x"}`), "text/plain")
	if rec.Code != http.StatusUnsupportedMediaType || s.exists("x") {
		t.Fatalf("wrong content type: %d", rec.Code)
	}
	if got := s.rootNames(); len(got) != 0 {
		t.Fatalf("bad input created folders: %v", got)
	}
}

func TestRename(t *testing.T) {
	s := newFileSession(t)
	s.seed("a.txt", "A")
	s.seed("b.txt", "B")
	s.seed("dir/sub/f", "F")

	rename := func(from, to string) *httptest.ResponseRecorder {
		return s.post("/api/files/rename", map[string]any{"from": from, "to": to})
	}

	if rec := rename("a.txt", "c.txt"); rec.Code != http.StatusNoContent || s.exists("a.txt") || !s.exists("c.txt") {
		t.Fatalf("simple rename: %d", rec.Code)
	}
	if rec := rename("c.txt", "b.txt"); rec.Code != http.StatusConflict {
		t.Fatalf("onto existing: %d", rec.Code)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "b.txt")); string(data) != "B" {
		t.Fatal("rename onto existing destroyed the target")
	}
	if rec := rename("b.txt", "b.txt"); rec.Code != http.StatusConflict {
		t.Fatalf("onto itself: %d", rec.Code)
	}
	if !s.exists("b.txt") {
		t.Fatal("rename onto itself removed the file")
	}
	if rec := rename("nope", "z"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing source: %d", rec.Code)
	}
	if rec := rename("b.txt", "nope/z"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing target folder: %d", rec.Code)
	}
	if rec := rename("dir", "dir/sub/inside"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("folder into itself: %d", rec.Code)
	}
	if rec := rename("dir", "dir/x"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("folder into itself (direct): %d", rec.Code)
	}
	if !s.exists("dir/sub/f") {
		t.Fatal("folder was damaged")
	}
	if rec := rename("", "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("rename root: %d", rec.Code)
	}
	if rec := rename("b.txt", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("rename to root: %d", rec.Code)
	}
	if rec := rename("  ", "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("whitespace from: %d", rec.Code)
	}
	if rec := rename("b.txt", "sub dir/"); rec.Code != http.StatusBadRequest {
		t.Fatalf("trailing slash: %d", rec.Code)
	}
	if rec := rename("dir", "moved"); rec.Code != http.StatusNoContent || !s.exists("moved/sub/f") {
		t.Fatalf("folder rename: %d", rec.Code)
	}
	if rec := rename("moved", "d"); rec.Code != http.StatusNoContent {
		t.Fatalf("sibling with shared prefix: %d", rec.Code)
	}
	if rec := s.call("POST", "/api/files/rename", strings.NewReader(`{"from":"b.txt"}`), "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing to: %d", rec.Code)
	}
	if rec := s.call("POST", "/api/files/rename", strings.NewReader(`{"from":1,"to":2}`), "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong types: %d", rec.Code)
	}
}

func TestDelete(t *testing.T) {
	s := newFileSession(t)
	s.seed("f.txt", "x")
	s.seed("full/inner", "x")
	s.seed("later", "x")
	if err := os.Mkdir(filepath.Join(s.root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	del := func(target string) *httptest.ResponseRecorder { return s.call("DELETE", target, nil, "") }

	if rec := del("/api/files?path=f.txt"); rec.Code != http.StatusOK || s.exists("f.txt") {
		t.Fatalf("file: %d", rec.Code)
	}
	if rec := del("/api/files?path=empty"); rec.Code != http.StatusOK || s.exists("empty") {
		t.Fatalf("empty folder: %d", rec.Code)
	}
	if rec := del("/api/files?path=full"); rec.Code != http.StatusOK || s.exists("full") {
		t.Fatalf("non-empty folder: %d %s", rec.Code, rec.Body)
	}
	if rec := del("/api/files?path=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing: %d", rec.Code)
	}
	for _, target := range []string{"/api/files", "/api/files?path=", "/api/files?path=.", "/api/files?path=%2F", "/api/files?path=%20", "/api/files?path=..", "/api/files?path=later/"} {
		if rec := del(target); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", target, rec.Code)
		}
	}
	if !s.exists("later") {
		t.Fatal("something was deleted by a refused request")
	}
	if _, err := os.Stat(s.root); err != nil {
		t.Fatalf("the root itself must survive: %v", err)
	}
}

func TestDeleteDoesNotFollowSymlinks(t *testing.T) {
	s := newFileSession(t)
	if !s.plantSymlinks() {
		t.Skip("this OS refused to create a symlink")
	}
	if rec := s.call("DELETE", "/api/files?path=link.txt", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("removing the link itself: %d %s", rec.Code, rec.Body)
	}
	if s.exists("link.txt") {
		t.Fatal("link still there")
	}
	s.assertOutsideUntouched()
}

type failingFiles struct {
	system.Files
	err error
}

func (f failingFiles) List(string) ([]system.FileInfo, error) { return nil, f.err }
func (f failingFiles) Open(string) (system.File, system.FileInfo, error) {
	return nil, system.FileInfo{}, f.err
}
func (f failingFiles) Write(string, io.Reader, bool) error    { return f.err }
func (f failingFiles) Rename(string, string) error            { return f.err }
func (f failingFiles) Remove(string) error                    { return f.err }
func (f failingFiles) Trash(string) (system.TrashItem, error) { return system.TrashItem{}, f.err }
func (f failingFiles) Mkdir(string) error                     { return f.err }

func (f failingFiles) Close() error { return f.Files.(io.Closer).Close() }

func failingFileSession(t *testing.T, raw error) *fileSession {
	t.Helper()
	s := newFileSession(t)
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	failing := NewPortal(PortalDeps{
		Verifier: s.verifier,
		Users:    s.users,
		Roles:    roles,
		OpenFiles: func(user string) (system.Files, error) {
			files, err := system.OpenUserFiles(s.base, user)
			if err != nil {
				return nil, err
			}

			return failingFiles{Files: files, err: raw}, nil
		},
		MaxUpload: portalMaxUpload,
		Thumbs:    s.thumbs,
	})
	s.handler = failing
	s.portal = failing
	s.cookie = s.signedIn(t, "amy", amyPassword)

	return s
}

func TestFileHandlersNeverLogFileNamesOrPaths(t *testing.T) {
	raw := &fs.PathError{Op: "open", Path: "/srv/sftp/amy/files/private-name.txt", Err: errors.New("permission denied")}
	s := failingFileSession(t, raw)
	var logged strings.Builder
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)

	s.get("/api/files?path=private-name.txt")
	s.get("/api/files/download?path=private-name.txt")
	s.get("/api/files/zip?path=private-name.txt")
	s.put("/api/files/content?path=private-name.txt", "x")
	s.call("DELETE", "/api/files?path=private-name.txt", nil, "")
	s.get("/api/files/thumb?path=private-name.jpg")

	if logged.Len() == 0 {
		t.Fatal("nothing was logged, the failures were not exercised")
	}
	for _, leak := range []string{"private-name", "/srv/sftp"} {
		if strings.Contains(logged.String(), leak) {
			t.Errorf("the journal is readable from the admin panel but the log has %q: %s", leak, logged.String())
		}
	}
}

func TestUnexpectedFilesystemErrorsAreNotLeaked(t *testing.T) {
	raw := errors.New("open /srv/sftp/amy/files/secret: permission denied")
	s := failingFileSession(t, raw)

	responses := map[string]*httptest.ResponseRecorder{
		"list":     s.get("/api/files"),
		"download": s.get("/api/files/download?path=a"),
		"upload":   s.put("/api/files/content?path=a", "x"),
		"folder":   s.post("/api/files/folder", map[string]any{"path": "a"}),
		"rename":   s.post("/api/files/rename", map[string]any{"from": "a", "to": "b"}),
		"delete":   s.call("DELETE", "/api/files?path=a", nil, ""),
	}
	for name, rec := range responses {
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: got %d, want 500", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "/srv") || strings.Contains(rec.Body.String(), "permission") {
			t.Errorf("%s leaked the raw error: %s", name, rec.Body)
		}
		apiError(t, rec)
	}
}
