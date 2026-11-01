package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

type familyHarness struct {
	*portalFilesHarness
	disk *system.FakeDisk
	amy  *http.Cookie
	bob  *http.Cookie
}

func newFamilyHarness(t *testing.T) *familyHarness {
	t.Helper()

	base := t.TempDir()
	for _, dir := range []string{system.FamilyDir, "amy/files", "bob/files"} {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword}}
	users := system.NewFakeUsers("amy", "bob")
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.Promote("amy"); err != nil {
		t.Fatal(err)
	}
	disk := system.NewFakeDisk(diskAt(1000))
	portal := NewPortal(PortalDeps{
		Verifier:   verifier,
		Users:      users,
		Roles:      roles,
		OpenFiles:  func(user string) (system.Files, error) { return system.OpenUserFiles(base, user) },
		OpenFamily: func() (system.Files, error) { return system.OpenFamilyFiles(base) },
		MaxUpload:  1 << 20,
		Disk:       disk,
		Thumbs:     NewThumbs(system.NewFakeThumbnailer()),
	})
	h := &familyHarness{
		portalFilesHarness: &portalFilesHarness{portalHarness: &portalHarness{portal: portal, verifier: verifier, roles: roles}, base: base, users: users},
		disk:               disk,
	}
	h.amy = h.signedIn(t, "amy", amyPassword)
	h.bob = h.signedIn(t, "bob", bobPassword)

	return h
}

func (h *familyHarness) put(cookie *http.Cookie, path, content string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("PUT", "/api/family/files/content?path="+url.QueryEscape(path), strings.NewReader(content))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "application/octet-stream")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	return rec
}

func (h *familyHarness) call(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	return h.do(method, path, body, "192.0.2.10", cookie)
}

func (h *familyHarness) listing(t *testing.T, cookie *http.Cookie, route, dir string) []string {
	t.Helper()
	rec := h.call("GET", route+"?path="+url.QueryEscape(dir), "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s %q got %d %s", route, dir, rec.Code, rec.Body)
	}
	var body struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range body.Entries {
		names = append(names, entry.Name)
	}

	return names
}

func (h *familyHarness) trashID(t *testing.T, route, path string, cookie *http.Cookie) string {
	t.Helper()
	rec := h.call("DELETE", route+"?path="+url.QueryEscape(path), "", cookie)
	var body struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.ID == "" {
		t.Fatalf("trash %s got %d %s", path, rec.Code, rec.Body)
	}

	return body.ID
}

func (h *familyHarness) storage(t *testing.T, cookie *http.Cookie) storageView {
	t.Helper()
	rec := h.call("GET", "/api/family/storage", "", cookie)
	var view storageView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return view
}

func TestFamilyRoutesNeedASession(t *testing.T) {
	h := newFamilyHarness(t)
	api := []struct{ method, path string }{
		{"GET", "/api/family/files"}, {"DELETE", "/api/family/files?path=a"}, {"GET", "/api/family/files/download?path=a"},
		{"GET", "/api/family/files/view?path=a"}, {"GET", "/api/family/files/thumb?path=a"}, {"GET", "/api/family/files/preview?path=a"},
		{"PUT", "/api/family/files/content?path=a"}, {"POST", "/api/family/files/folder"}, {"POST", "/api/family/files/rename"},
		{"POST", "/api/family/files/exists"}, {"GET", "/api/family/files/zip?path=a"}, {"GET", "/api/family/trash"},
		{"DELETE", "/api/family/trash"}, {"POST", "/api/family/trash/x/restore"}, {"DELETE", "/api/family/trash/x"},
		{"POST", "/api/family/uploads"}, {"GET", "/api/family/uploads/x"}, {"PUT", "/api/family/uploads/x"},
		{"POST", "/api/family/uploads/x/finish"}, {"GET", "/api/family/storage"},
	}
	for _, c := range api {
		if rec := h.call(c.method, c.path, "", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session got %d", c.method, c.path, rec.Code)
		}
	}
	for _, page := range []string{"/family", "/family/trash"} {
		rec := h.call("GET", page, "", nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
			t.Errorf("%s without a session got %d to %q", page, rec.Code, rec.Header().Get("Location"))
		}
		if rec := h.call("GET", page, "", h.amy); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `data-scope="family"`) {
			t.Errorf("%s with a session got %d", page, rec.Code)
		}
	}
}

func TestFamilyPhotoRoutesDoNotExist(t *testing.T) {
	h := newFamilyHarness(t)
	for _, path := range []string{"/api/family/files/photos", "/api/family/files/photo-date?path=a.jpg"} {
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			if rec := h.call(method, path, "", h.amy); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s got %d", method, path, rec.Code)
			}
		}
	}
}

func TestFamilyIsOffWithoutAFamilyOpener(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	for _, path := range []string{"/family", "/family/trash", "/api/family/files", "/api/family/storage", "/api/family/trash"} {
		if rec := h.do("GET", path, "", "192.0.2.10", amy); rec.Code != http.StatusNotFound {
			t.Errorf("%s got %d", path, rec.Code)
		}
	}
}

func TestEveryoneSharesTheFamilyFolder(t *testing.T) {
	h := newFamilyHarness(t)

	if rec := h.call("POST", "/api/family/files/folder", `{"path":"Holiday"}`, h.amy); rec.Code != http.StatusCreated {
		t.Fatalf("mkdir got %d %s", rec.Code, rec.Body)
	}
	if rec := h.put(h.amy, "Holiday/beach.txt", "sand"); rec.Code != http.StatusNoContent {
		t.Fatalf("upload got %d %s", rec.Code, rec.Body)
	}

	if got := h.listing(t, h.bob, "/api/family/files", "Holiday"); len(got) != 1 || got[0] != "beach.txt" {
		t.Fatalf("bob sees %v", got)
	}
	rec := h.call("GET", "/api/family/files/download?path=Holiday/beach.txt", "", h.bob)
	if rec.Code != http.StatusOK || rec.Body.String() != "sand" {
		t.Fatalf("bob download got %d %q", rec.Code, rec.Body)
	}

	if rec := h.call("POST", "/api/family/files/rename", `{"from":"Holiday/beach.txt","to":"Holiday/sea.txt"}`, h.bob); rec.Code != http.StatusNoContent {
		t.Fatalf("bob rename got %d %s", rec.Code, rec.Body)
	}
	if got := h.listing(t, h.amy, "/api/family/files", "Holiday"); len(got) != 1 || got[0] != "sea.txt" {
		t.Fatalf("amy sees %v", got)
	}

	id := h.trashID(t, "/api/family/files", "Holiday/sea.txt", h.bob)
	if rec := h.call("GET", "/api/family/trash", "", h.amy); !strings.Contains(rec.Body.String(), id) {
		t.Fatalf("amy does not see the family trash item: %s", rec.Body)
	}
	if rec := h.call("POST", "/api/family/trash/"+id+"/restore", "", h.amy); rec.Code != http.StatusOK {
		t.Fatalf("amy restore got %d %s", rec.Code, rec.Body)
	}
	if got := h.listing(t, h.bob, "/api/family/files", "Holiday"); len(got) != 1 || got[0] != "sea.txt" {
		t.Fatalf("after undo bob sees %v", got)
	}
}

func TestFamilyFilesLiveOutsideEveryHome(t *testing.T) {
	h := newFamilyHarness(t)
	if rec := h.put(h.amy, "shared.txt", "family"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.upload(h.amy, "mine.txt", "private"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}

	if data, err := os.ReadFile(filepath.Join(h.base, system.FamilyDir, "shared.txt")); err != nil || string(data) != "family" {
		t.Fatalf("not at the data root: %q %v", data, err)
	}
	for _, cookie := range []*http.Cookie{h.amy, h.bob} {
		for _, name := range h.listing(t, cookie, "/api/files", "") {
			if name == "shared.txt" || name == system.FamilyDir {
				t.Fatalf("the family folder shows in Files: %s", name)
			}
		}
	}
	if got := h.listing(t, h.bob, "/api/family/files", ""); len(got) != 1 || got[0] != "shared.txt" {
		t.Fatalf("bob sees %v in the family folder", got)
	}
}

func TestFamilyAndHomeEndpointsCannotReachEachOther(t *testing.T) {
	h := newFamilyHarness(t)
	h.write(t, "bob", "secret.txt", bobSecret)
	if rec := h.put(h.amy, "shared.txt", "family-file"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}

	for _, path := range []string{"../_family/shared.txt", "../../_family/shared.txt", "/_family/shared.txt", `..\_family\shared.txt`, "%2e%2e/_family/shared.txt", "_family/shared.txt"} {
		if rec := h.get(h.amy, "/api/files/download", path); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "family-file") {
			t.Errorf("a home endpoint reached the family folder with %q: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"../bob/files/secret.txt", "../../bob/files/secret.txt", "../amy/files", "/bob/files/secret.txt", `..\bob\files\secret.txt`, "bob/files/secret.txt"} {
		for _, route := range []string{"/api/family/files/download", "/api/family/files", "/api/family/files/zip"} {
			if rec := h.get(h.amy, route, path); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), bobSecret) {
				t.Errorf("%s with %q reached a home: %d", route, path, rec.Code)
			}
		}
	}
	for _, query := range []string{"&user=bob", "&scope=family", "&family=1", "&user=_family"} {
		rec := h.call("GET", "/api/files?path="+query, "", h.amy)
		if strings.Contains(rec.Body.String(), "shared.txt") {
			t.Errorf("a parameter %q switched the home endpoint to the family folder", query)
		}
		rec = h.call("GET", "/api/family/files?path="+query, "", h.amy)
		if strings.Contains(rec.Body.String(), "secret.txt") {
			t.Errorf("a parameter %q switched the family endpoint to a home", query)
		}
	}
}

func TestFamilyPathsStayInsideTheFamilyFolder(t *testing.T) {
	h := newFamilyHarness(t)
	h.write(t, "bob", "secret.txt", bobSecret)
	if err := os.WriteFile(filepath.Join(h.base, "outside.txt"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	bad := []string{"..", "../outside.txt", "a/../../outside.txt", "/outside.txt", "/etc/passwd", `a\..\..\outside.txt`, "a\x00b", "%2e%2e/outside.txt", "....//outside.txt", "./../outside.txt", "a//b/../../../outside.txt"}
	for _, path := range bad {
		for _, route := range []string{"/api/family/files", "/api/family/files/download", "/api/family/files/view", "/api/family/files/zip", "/api/family/files/thumb", "/api/family/files/preview"} {
			rec := h.get(h.amy, route, path)
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "outside-secret") {
				t.Errorf("GET %s %q got %d", route, path, rec.Code)
			}
		}
		if rec := h.put(h.amy, path, "planted"); rec.Code == http.StatusNoContent {
			t.Errorf("upload to %q worked", path)
		}
		if rec := h.call("POST", "/api/family/files/folder", `{"path":`+quote(path)+`}`, h.amy); rec.Code == http.StatusCreated {
			t.Errorf("mkdir %q worked", path)
		}
		if rec := h.call("POST", "/api/family/files/rename", `{"from":"x","to":`+quote(path)+`}`, h.amy); rec.Code == http.StatusNoContent {
			t.Errorf("rename to %q worked", path)
		}
		if rec := h.call("POST", "/api/family/uploads", `{"path":`+quote(path)+`,"size":1}`, h.amy); rec.Code == http.StatusCreated {
			t.Errorf("upload session at %q worked", path)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(h.base, "outside.txt")); string(data) != "outside-secret" {
		t.Fatalf("a file outside the family folder changed: %q", data)
	}
	if entries, _ := os.ReadDir(h.base); len(entries) != 4 {
		t.Fatalf("something was created at the data root: %v", entries)
	}
}

func TestFamilySymlinksCannotPointOutOfTheFolder(t *testing.T) {
	h := newFamilyHarness(t)
	h.write(t, "bob", "secret.txt", bobSecret)
	family := filepath.Join(h.base, system.FamilyDir)
	if err := os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(family, "home")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if err := os.Symlink(filepath.Join(h.base, "bob", "files", "secret.txt"), filepath.Join(family, "secret.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.base, filepath.Join(family, "up")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"home", "home/secret.txt", "secret.txt", "up", "up/bob/files/secret.txt"} {
		for _, route := range []string{"/api/family/files", "/api/family/files/download", "/api/family/files/view", "/api/family/files/zip"} {
			if rec := h.get(h.amy, route, path); strings.Contains(rec.Body.String(), bobSecret) {
				t.Errorf("GET %s %q leaked bob's file", route, path)
			}
		}
		if rec := h.put(h.amy, path+"/planted.txt", "planted"); rec.Code == http.StatusNoContent {
			t.Errorf("upload through %q worked", path)
		}
		if rec := h.call("DELETE", "/api/family/files?path="+url.QueryEscape(path+"/secret.txt"), "", h.amy); rec.Code == http.StatusOK {
			t.Errorf("trash through %q worked", path)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.base, "bob", "files", "secret.txt")); err != nil || string(data) != bobSecret {
		t.Fatalf("bob's file was damaged: %q %v", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, "bob", "files")); len(entries) != 1 {
		t.Fatalf("bob's folder changed: %v", entries)
	}
}

func TestFamilyHiddenFoldersAreRefusedInAnyCaseAndPosition(t *testing.T) {
	h := newFamilyHarness(t)
	for _, name := range []string{".trash", ".TRASH", ".Uploads", ".thumbs", ".ThUmBs", "a/.trash", ".uploads/x", ".thumbs/x"} {
		for _, call := range []string{
			"GET /api/family/files?path=" + url.QueryEscape(name),
			"GET /api/family/files/download?path=" + url.QueryEscape(name),
			"GET /api/family/files/zip?path=" + url.QueryEscape(name),
			"DELETE /api/family/files?path=" + url.QueryEscape(name),
		} {
			method, path, _ := strings.Cut(call, " ")
			if rec := h.call(method, path, "", h.amy); rec.Code != http.StatusBadRequest {
				t.Errorf("%s got %d, want 400", call, rec.Code)
			}
		}
		if rec := h.put(h.amy, name, "x"); rec.Code != http.StatusBadRequest {
			t.Errorf("upload %q got %d", name, rec.Code)
		}
		if rec := h.call("POST", "/api/family/files/folder", `{"path":`+quote(name)+`}`, h.amy); rec.Code != http.StatusBadRequest {
			t.Errorf("mkdir %q got %d", name, rec.Code)
		}
	}
	h.put(h.amy, "a.txt", "x")
	h.trashID(t, "/api/family/files", "a.txt", h.amy)
	for _, name := range h.listing(t, h.amy, "/api/family/files", "") {
		if strings.HasPrefix(name, ".") {
			t.Fatalf("a hidden folder is listed: %s", name)
		}
	}
}

func TestFamilyRootCannotBeDeletedRenamedOrReplaced(t *testing.T) {
	h := newFamilyHarness(t)
	h.put(h.amy, "keep.txt", "x")

	for _, path := range []string{"", ".", "/", "./", ".."} {
		if rec := h.call("DELETE", "/api/family/files?path="+url.QueryEscape(path), "", h.amy); rec.Code != http.StatusBadRequest {
			t.Errorf("delete %q got %d", path, rec.Code)
		}
		if rec := h.call("POST", "/api/family/files/rename", `{"from":`+quote(path)+`,"to":"gone"}`, h.amy); rec.Code != http.StatusBadRequest {
			t.Errorf("rename %q got %d", path, rec.Code)
		}
		if rec := h.call("POST", "/api/family/files/rename", `{"from":"keep.txt","to":`+quote(path)+`}`, h.amy); rec.Code == http.StatusNoContent {
			t.Errorf("rename onto %q worked", path)
		}
		if rec := h.put(h.amy, path, "x"); rec.Code == http.StatusNoContent {
			t.Errorf("upload onto %q worked", path)
		}
	}
	if _, err := os.Stat(filepath.Join(h.base, system.FamilyDir, "keep.txt")); err != nil {
		t.Fatalf("the family folder was damaged: %v", err)
	}
}

func TestFamilyTrashRestoreAndPurgeNeverCrossIntoHomes(t *testing.T) {
	h := newFamilyHarness(t)
	h.upload(h.amy, "mine.txt", "m")
	h.put(h.amy, "shared.txt", "s")
	mine := h.trashID(t, "/api/files", "mine.txt", h.amy)
	shared := h.trashID(t, "/api/family/files", "shared.txt", h.amy)

	if rec := h.call("POST", "/api/family/trash/"+mine+"/restore", "", h.amy); rec.Code != http.StatusNotFound {
		t.Errorf("family restored a home item: %d", rec.Code)
	}
	if rec := h.call("DELETE", "/api/family/trash/"+mine, "", h.amy); rec.Code != http.StatusNotFound {
		t.Errorf("family purged a home item: %d", rec.Code)
	}
	if rec := h.call("POST", "/api/trash/"+shared+"/restore", "", h.amy); rec.Code != http.StatusNotFound {
		t.Errorf("a home restored a family item: %d", rec.Code)
	}
	if rec := h.call("DELETE", "/api/trash/"+shared, "", h.amy); rec.Code != http.StatusNotFound {
		t.Errorf("a home purged a family item: %d", rec.Code)
	}

	if rec := h.call("DELETE", "/api/family/trash", "", h.bob); rec.Code != http.StatusOK {
		t.Fatalf("empty family trash got %d", rec.Code)
	}
	if rec := h.call("GET", "/api/trash", "", h.amy); !strings.Contains(rec.Body.String(), mine) {
		t.Fatalf("emptying the family trash touched a home trash: %s", rec.Body)
	}
	if rec := h.call("GET", "/api/family/trash", "", h.amy); strings.Contains(rec.Body.String(), shared) {
		t.Fatalf("the family trash was not emptied: %s", rec.Body)
	}
}

func TestFamilyResumableUploadAndConflictCheck(t *testing.T) {
	h := newFamilyHarness(t)
	h.put(h.bob, "dup.txt", "old")

	rec := h.call("POST", "/api/family/files/exists", `{"paths":["dup.txt","new.txt"]}`, h.amy)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "dup.txt") || strings.Contains(rec.Body.String(), "new.txt") {
		t.Fatalf("conflict check got %d %s", rec.Code, rec.Body)
	}

	rec = h.call("POST", "/api/family/uploads", `{"path":"big.bin","size":6}`, h.amy)
	var session struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &session) != nil {
		t.Fatalf("create got %d %s", rec.Code, rec.Body)
	}
	for _, part := range []struct {
		cookie *http.Cookie
		from   int
		data   string
	}{{h.amy, 0, "abc"}, {h.bob, 3, "def"}} {
		req := httptest.NewRequest("PUT", "/api/family/uploads/"+session.ID, strings.NewReader(part.data))
		req.RemoteAddr = "192.0.2.10:5555"
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/6", part.from, part.from+2))
		req.AddCookie(part.cookie)
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("append got %d %s", rec.Code, rec.Body)
		}
	}
	if rec := h.call("POST", "/api/family/uploads/"+session.ID+"/finish", "", h.amy); rec.Code >= 300 {
		t.Fatalf("finish got %d %s", rec.Code, rec.Body)
	}
	if data, err := os.ReadFile(filepath.Join(h.base, system.FamilyDir, "big.bin")); err != nil || string(data) != "abcdef" {
		t.Fatalf("got %q %v", data, err)
	}
	if rec := h.call("GET", "/api/uploads/"+session.ID, "", h.amy); rec.Code != http.StatusNotFound {
		t.Fatalf("a home can see a family upload session: %d", rec.Code)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, "amy", "files")); len(entries) != 0 {
		t.Fatalf("the upload left something in a home: %v", entries)
	}
}

func TestTwoPeopleUploadingTheSameFamilyNameLeaveOneWholeFile(t *testing.T) {
	h := newFamilyHarness(t)

	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cookie := h.amy
			if i%2 == 1 {
				cookie = h.bob
			}
			codes[i] = h.put(cookie, "same.txt", strings.Repeat(fmt.Sprint(i), 512)).Code
		}()
	}
	wg.Wait()

	for _, code := range codes {
		if code != http.StatusNoContent && code != http.StatusConflict {
			t.Errorf("unexpected status %d in %v", code, codes)
		}
	}
	data, err := os.ReadFile(filepath.Join(h.base, system.FamilyDir, "same.txt"))
	if err != nil || len(data) != 512 || strings.Trim(string(data), string(data[:1])) != "" {
		t.Fatalf("the file is not one writer's whole content: %d bytes %v", len(data), err)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, system.FamilyDir)); len(entries) != 1 {
		t.Fatalf("temporary files were left behind: %v", entries)
	}
}

func TestDeletingWhileSomeoneElseDownloadsLikeASingleUser(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows cannot move a file that is open")
	}
	h := newFamilyHarness(t)
	h.put(h.amy, "movie.txt", strings.Repeat("x", 4096))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := h.call("GET", "/api/family/files/download?path=movie.txt", "", h.bob)
			if rec.Code == http.StatusOK && rec.Body.Len() != 4096 {
				t.Errorf("a download came back cut short: %d bytes", rec.Body.Len())
			}
			if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
				t.Errorf("download got %d", rec.Code)
			}
		}()
		go func() {
			defer wg.Done()
			if rec := h.call("DELETE", "/api/family/files?path=movie.txt", "", h.amy); rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
				t.Errorf("delete got %d", rec.Code)
			}
		}()
	}
	wg.Wait()
}

func TestFamilyStorageMeasuresTheWholeDiskAndNeverAQuota(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.users.SetQuota("amy", 10<<20); err != nil {
		t.Fatal(err)
	}
	h.users.SetUsed("amy", 9<<20)

	view := h.storage(t, h.amy)
	if view.Measure != "disk" || view.Quota != nil || view.QuotaUsed != nil || view.Used != 1000 || view.Total != 10000 {
		t.Fatalf("got %+v", view)
	}
	if own := h.call("GET", "/api/storage", "", h.amy); !strings.Contains(own.Body.String(), `"measure":"quota"`) {
		t.Fatalf("amy's own bar lost her quota: %s", own.Body)
	}
}

func TestFamilyStorageReportsTheFamilyTrash(t *testing.T) {
	h := newFamilyHarness(t)
	h.put(h.amy, "a.txt", "12345")
	h.upload(h.amy, "b.txt", "123")
	h.trashID(t, "/api/family/files", "a.txt", h.amy)
	h.trashID(t, "/api/files", "b.txt", h.amy)

	if got := h.storage(t, h.bob).TrashBytes; got != 5 {
		t.Fatalf("family trash holds %d bytes, want 5", got)
	}
}

func TestFamilyUploadsAreRefusedWhenTheDiskIsFullButReadsAndDeletesWork(t *testing.T) {
	h := newFamilyHarness(t)
	h.put(h.amy, "old.txt", "old")
	h.disk.Set(diskAt(9800))
	h.portal.space.invalidate()

	if rec := h.put(h.bob, "new.txt", "x"); rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeDiskFull) {
		t.Fatalf("upload got %d %s", rec.Code, rec.Body)
	}
	if rec := h.call("POST", "/api/family/uploads", `{"path":"new.bin","size":1}`, h.bob); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload session got %d", rec.Code)
	}
	if view := h.storage(t, h.bob); view.Level != system.LevelFull {
		t.Fatalf("got %+v", view)
	}
	if rec := h.call("GET", "/api/family/files/download?path=old.txt", "", h.bob); rec.Code != http.StatusOK {
		t.Fatalf("download got %d", rec.Code)
	}
	if rec := h.call("DELETE", "/api/family/files?path=old.txt", "", h.bob); rec.Code != http.StatusOK {
		t.Fatalf("delete got %d", rec.Code)
	}
}

func TestFamilyWritesNeedTheSameOriginAsEverythingElse(t *testing.T) {
	h := newFamilyHarness(t)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/family/files/folder", `{"path":"x"}`},
		{"DELETE", "/api/family/files?path=x", ""},
		{"POST", "/api/family/files/rename", `{"from":"a","to":"b"}`},
		{"PUT", "/api/family/files/content?path=x", "x"},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.RemoteAddr = "192.0.2.10:5555"
		req.Header.Set("Origin", "https://evil.example")
		req.AddCookie(h.amy)
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s from another origin got %d", c.method, c.path, rec.Code)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, system.FamilyDir)); len(entries) != 0 {
		t.Fatalf("a cross-origin request wrote something: %v", entries)
	}
}

func TestAMissingFamilyFolderFailsCleanlyAndIsNotCreated(t *testing.T) {
	h := newFamilyHarness(t)
	if err := os.Remove(filepath.Join(h.base, system.FamilyDir)); err != nil {
		t.Fatal(err)
	}

	rec := h.call("GET", "/api/family/files", "", h.amy)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), codeFamilyUnavailable) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.put(h.amy, "x.txt", "x"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("upload got %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(h.base, system.FamilyDir)); !os.IsNotExist(err) {
		t.Fatalf("the portal must not create the family folder: %v", err)
	}
	if rec := h.call("GET", "/api/files", "", h.amy); rec.Code != http.StatusOK {
		t.Fatalf("a missing family folder broke Files: %d", rec.Code)
	}
}

func TestNoAccountCanBeNamedLikeTheFamilyFolder(t *testing.T) {
	h := newFamilyHarness(t)
	for _, name := range []string{system.FamilyDir, "_Family", "_family/", "../_family"} {
		rec := h.call("POST", "/api/admin/users", jsonBodyOf(map[string]any{"name": name, "password": newPassword}), h.amy)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("admin create %q got %d", name, rec.Code)
		}
		h.verifier.Passwords[name] = "any password at all"
		if rec := h.login(name, "any password at all", "192.0.2.77"); rec.Code != http.StatusUnauthorized {
			t.Errorf("login as %q got %d", name, rec.Code)
		}
	}
	names, _ := h.users.List()
	for _, name := range names {
		if strings.Contains(name, "family") {
			t.Fatalf("an account was created: %v", names)
		}
	}
}

func jsonBodyOf(value map[string]any) string {
	raw, _ := json.Marshal(value)

	return string(raw)
}

func TestFamilyFilesDoNotChangeAnyoneQuotaNumbers(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.users.SetQuota("amy", 10<<20); err != nil {
		t.Fatal(err)
	}
	h.users.SetUsed("amy", 3<<20)
	before := h.call("GET", "/api/storage", "", h.amy).Body.String()

	h.put(h.amy, "big.bin", strings.Repeat("x", 4096))
	h.portal.space.invalidate()

	if after := h.call("GET", "/api/storage", "", h.amy).Body.String(); after != before {
		t.Fatalf("amy's bar changed after a family upload:\nbefore %s\nafter  %s", before, after)
	}
	if list := h.call("GET", "/api/admin/users", "", h.amy).Body.String(); strings.Contains(list, system.FamilyDir) {
		t.Fatalf("the admin user list mentions the family folder: %s", list)
	}
}
