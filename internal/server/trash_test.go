package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

type trashSurface struct {
	name string
	call func(method, target, body string) *httptest.ResponseRecorder
	seed func(t *testing.T, path, content string)
	root string
}

func (s trashSurface) exists(path string) bool {
	_, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(path)))

	return err == nil
}

type trashEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Folder  string `json:"folder"`
	Deleted string `json:"deleted"`
	Expires string `json:"expires"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"isDir"`
}

type trashListing struct {
	Items     []trashEntry `json:"items"`
	TotalSize int64        `json:"totalSize"`
}

func (s trashSurface) list(t *testing.T) trashListing {
	t.Helper()
	rec := s.call("GET", "/api/trash", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash got %d: %s", rec.Code, rec.Body)
	}
	var listing trashListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}

	return listing
}

func (s trashSurface) del(path string) *httptest.ResponseRecorder {
	return s.call("DELETE", "/api/files?path="+url.QueryEscape(path), "")
}

func (s trashSurface) trash(t *testing.T, path string) trashEntry {
	t.Helper()
	rec := s.del(path)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete %q got %d: %s", path, rec.Code, rec.Body)
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.ID == "" {
		t.Fatalf("delete must return the trash id: %q %v", rec.Body, err)
	}
	for _, item := range s.list(t).Items {
		if item.ID == body.ID {
			return item
		}
	}
	t.Fatalf("item %s missing from the trash list", body.ID)

	return trashEntry{}
}

func trashRequest(method, target, body string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5555"
	switch {
	case body == "":
	case strings.HasPrefix(target, "/api/files/content"):
		req.Header.Set("Content-Type", "application/octet-stream")
	default:
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(cookie)

	return req
}

func trashSurfaces(t *testing.T) []trashSurface {
	t.Helper()
	panel := newFileSession(t)
	portal := newPortalFilesHarness(t)
	amy := portal.signedIn(t, "amy", amyPassword)

	return []trashSurface{
		{
			name: "panel",
			call: func(method, target, body string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				panel.handler.ServeHTTP(rec, trashRequest(method, target, body, panel.cookie))

				return rec
			},
			seed: func(t *testing.T, path, content string) { t.Helper(); panel.seed(path, content) },
			root: panel.root,
		},
		{
			name: "portal",
			call: func(method, target, body string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				portal.portal.ServeHTTP(rec, trashRequest(method, target, body, amy))

				return rec
			},
			seed: func(t *testing.T, path, content string) { t.Helper(); portal.write(t, "amy", path, content) },
			root: filepath.Join(portal.base, "amy", "files"),
		},
	}
}

func eachTrashSurface(t *testing.T, run func(t *testing.T, s trashSurface)) {
	t.Helper()
	for _, s := range trashSurfaces(t) {
		t.Run(s.name, func(t *testing.T) { run(t, s) })
	}
}

func TestDeleteMovesToTrashAndRestoreBringsItBack(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "docs/a.txt", "hello")
		item := s.trash(t, "docs/a.txt")

		if s.exists("docs/a.txt") {
			t.Fatal("file still in place")
		}
		if item.Name != "a.txt" || item.Path != "docs/a.txt" || item.Folder != "docs" || item.Size != 5 || item.IsDir {
			t.Fatalf("item: %+v", item)
		}
		deleted, err := time.Parse(time.RFC3339, item.Deleted)
		expires, err2 := time.Parse(time.RFC3339, item.Expires)
		if err != nil || err2 != nil || expires.Sub(deleted) != system.TrashMaxAge {
			t.Fatalf("times: %q %q", item.Deleted, item.Expires)
		}
		if listing := s.list(t); listing.TotalSize != 5 || len(listing.Items) != 1 {
			t.Fatalf("listing: %+v", listing)
		}

		rec := s.call("POST", "/api/trash/"+item.ID+"/restore", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"docs/a.txt"`) {
			t.Fatalf("restore got %d: %s", rec.Code, rec.Body)
		}
		if data, err := os.ReadFile(filepath.Join(s.root, "docs", "a.txt")); err != nil || string(data) != "hello" {
			t.Fatalf("restored content: %q %v", data, err)
		}
		if rec := s.call("POST", "/api/trash/"+item.ID+"/restore", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("second restore got %d", rec.Code)
		}
		if len(s.list(t).Items) != 0 {
			t.Fatal("restored item still listed")
		}
	})
}

func TestDeletingAFolderTrashesItWholeEvenWhenItHasContent(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "trip/day1/photo.jpg", "12345")
		s.seed(t, "trip/notes.txt", "123")
		item := s.trash(t, "trip")

		if !item.IsDir || item.Size != 8 {
			t.Fatalf("item: %+v", item)
		}
		if len(s.list(t).Items) != 1 {
			t.Fatal("a folder must be one item")
		}
		if rec := s.call("POST", "/api/trash/"+item.ID+"/restore", ""); rec.Code != http.StatusOK {
			t.Fatalf("restore got %d: %s", rec.Code, rec.Body)
		}
		if !s.exists("trip/day1/photo.jpg") || !s.exists("trip/notes.txt") {
			t.Fatal("folder not restored whole")
		}
	})
}

func TestRestoreClashGetsARestoredNameAndNeverOverwrites(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "a.txt", "old")
		item := s.trash(t, "a.txt")
		s.seed(t, "a.txt", "new")

		rec := s.call("POST", "/api/trash/"+item.ID+"/restore", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"a (restored).txt"`) {
			t.Fatalf("restore got %d: %s", rec.Code, rec.Body)
		}
		if data, _ := os.ReadFile(filepath.Join(s.root, "a.txt")); string(data) != "new" {
			t.Fatalf("the existing file was overwritten: %q", data)
		}
		if data, _ := os.ReadFile(filepath.Join(s.root, "a (restored).txt")); string(data) != "old" {
			t.Fatalf("restored copy: %q", data)
		}
	})
}

func TestRestoreWhoseParentBecameAFileIsRefusedAndKeepsTheItem(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "d/f.txt", "x")
		item := s.trash(t, "d/f.txt")
		if rec := s.del("d"); rec.Code != http.StatusOK {
			t.Fatalf("delete empty folder got %d", rec.Code)
		}
		s.seed(t, "d", "a file now")

		rec := s.call("POST", "/api/trash/"+item.ID+"/restore", "")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		if len(s.list(t).Items) != 2 {
			t.Fatalf("the item must stay in the trash: %+v", s.list(t).Items)
		}
	})
}

func TestPurgeAndEmptyTrash(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		rec := s.call("DELETE", "/api/trash", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":0`) {
			t.Fatalf("empty with nothing in it got %d: %s", rec.Code, rec.Body)
		}
		s.seed(t, "a.txt", "1")
		s.seed(t, "b.txt", "2")
		s.seed(t, "c.txt", "3")
		a := s.trash(t, "a.txt")
		s.trash(t, "b.txt")
		s.trash(t, "c.txt")

		if rec := s.call("DELETE", "/api/trash/"+a.ID, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("purge got %d: %s", rec.Code, rec.Body)
		}
		if rec := s.call("DELETE", "/api/trash/"+a.ID, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("purge twice got %d", rec.Code)
		}
		rec = s.call("DELETE", "/api/trash", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":2`) {
			t.Fatalf("empty got %d: %s", rec.Code, rec.Body)
		}
		if listing := s.list(t); len(listing.Items) != 0 || listing.TotalSize != 0 {
			t.Fatalf("left over: %+v", listing)
		}
	})
}

func TestTrashIDsAreValidatedBeforeTheyReachTheFilesystem(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "keep.txt", "x")
		item := s.trash(t, "keep.txt")
		for _, id := range []string{
			"nope", "..", "%2e%2e", "..%2f..%2fetc%2fpasswd", strings.ToUpper(item.ID), item.ID + ".json", item.ID + "%2f..",
			"0000", strings.Repeat("a", 300), "%00", url.PathEscape(item.ID + "\n"), url.PathEscape(" " + item.ID),
		} {
			for _, request := range [][2]string{{"POST", "/api/trash/" + id + "/restore"}, {"DELETE", "/api/trash/" + id}} {
				rec := s.call(request[0], request[1], "")
				if rec.Code != http.StatusNotFound && rec.Code != http.StatusBadRequest && rec.Code != http.StatusTemporaryRedirect {
					t.Errorf("%s %s got %d", request[0], request[1], rec.Code)
				}
			}
		}
		if len(s.list(t).Items) != 1 {
			t.Fatal("a bad id changed the trash")
		}
	})
}

func TestTrashFolderIsHiddenFromEveryFileRoute(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "visible.txt", "x")
		s.seed(t, "gone.txt", "SECRET")
		item := s.trash(t, "gone.txt")

		if rec := s.call("GET", "/api/files", ""); strings.Contains(strings.ToLower(rec.Body.String()), "trash") || strings.Contains(rec.Body.String(), item.ID) {
			t.Fatalf("listing leaks the trash folder: %s", rec.Body)
		}
		for _, name := range []string{
			".trash", ".TRASH", ".Trash", ".trash/" + item.ID, ".trash/" + item.ID + ".json", "sub/.trash", "a/.TrAsH/b", ".trash/../.trash", ".trash/",
		} {
			escaped := url.QueryEscape(name)
			checks := map[string]*httptest.ResponseRecorder{
				"list":        s.call("GET", "/api/files?path="+escaped, ""),
				"download":    s.call("GET", "/api/files/download?path="+escaped, ""),
				"view":        s.call("GET", "/api/files/view?path="+escaped, ""),
				"zip":         s.call("GET", "/api/files/zip?path="+escaped, ""),
				"delete":      s.del(name),
				"mkdir":       s.call("POST", "/api/files/folder", `{"path":`+quote(name)+`}`),
				"rename-from": s.call("POST", "/api/files/rename", `{"from":`+quote(name)+`,"to":"elsewhere"}`),
				"rename-to":   s.call("POST", "/api/files/rename", `{"from":"visible.txt","to":`+quote(name)+`}`),
				"exists":      s.call("POST", "/api/files/exists", `{"paths":[`+quote(name)+`]}`),
				"upload":      s.call("PUT", "/api/files/content?path="+escaped, "planted"),
				"create":      s.call("POST", "/api/uploads", `{"path":`+quote(name)+`,"size":1}`),
			}
			for action, rec := range checks {
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s %q got %d, want 400", action, name, rec.Code)
				}
				if strings.Contains(rec.Body.String(), "SECRET") {
					t.Errorf("%s %q leaked trash content", action, name)
				}
			}
		}
		if !s.exists("visible.txt") || len(s.list(t).Items) != 1 {
			t.Fatal("a refused request changed something")
		}
	})
}

func TestDeleteRefusesTheRootMissingPathsAndTraversal(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		s.seed(t, "later", "x")
		for _, target := range []string{"/api/files", "/api/files?path=", "/api/files?path=.", "/api/files?path=%2F", "/api/files?path=..", "/api/files?path=..%2Fbob", "/api/files?path=later/", "/api/files?path=%zz"} {
			if rec := s.call("DELETE", target, ""); rec.Code != http.StatusBadRequest {
				t.Errorf("%s got %d, want 400", target, rec.Code)
			}
		}
		if rec := s.del("nope"); rec.Code != http.StatusNotFound {
			t.Errorf("missing got %d", rec.Code)
		}
		if rec := s.del("later/child"); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusNotFound {
			t.Errorf("under a file got %d", rec.Code)
		}
		if !s.exists("later") || len(s.list(t).Items) != 0 {
			t.Fatal("a refused delete changed something")
		}
	})
}

func TestDeleteAndRestoreAwkwardNames(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		names := []string{
			"two words.txt", "日本語.txt", ".hidden", "emoji 😀.png", strings.Repeat("n", 255), "x'onerror=alert(1) y.txt", "a&b=c#d%e.txt",
		}
		for _, name := range names {
			s.seed(t, name, "x")
			item := s.trash(t, name)
			if item.Name != name {
				t.Errorf("name %q came back as %q", name, item.Name)
			}
			rec := s.call("POST", "/api/trash/"+item.ID+"/restore", "")
			if rec.Code != http.StatusOK || !s.exists(name) {
				t.Errorf("%q: restore got %d: %.100s", name, rec.Code, rec.Body)
			}
		}
	})
}

func TestRestoreOfALongNameThatClashesStillFits(t *testing.T) {
	eachTrashSurface(t, func(t *testing.T, s trashSurface) {
		name := strings.Repeat("a", 251) + ".txt"
		s.seed(t, name, "old")
		item := s.trash(t, name)
		s.seed(t, name, "new")

		rec := s.call("POST", "/api/trash/"+item.ID+"/restore", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		var body struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Path) > 255 || !s.exists(body.Path) {
			t.Fatalf("restored as %q (%d bytes) %v", body.Path, len(body.Path), err)
		}
	})
}

func TestDeletedSymlinkIsMovedNotFollowed(t *testing.T) {
	s := newFileSession(t)
	if !s.plantSymlinks() {
		t.Skip("this OS refused to create a symlink")
	}
	for _, link := range []string{"link.txt", "escape"} {
		if rec := s.call("DELETE", "/api/files?path="+link, nil, ""); rec.Code != http.StatusOK {
			t.Fatalf("delete %s got %d %s", link, rec.Code, rec.Body)
		}
	}
	if s.exists("link.txt") || s.exists("escape") {
		t.Fatal("links still there")
	}
	s.assertOutsideUntouched()
}

func TestTrashRoutesNeedASessionAndOnlyAcceptTheirOwnMethods(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)
	id := strings.Repeat("a", 32)
	for _, route := range []struct{ method, target string }{
		{"GET", "/api/trash"}, {"DELETE", "/api/trash"}, {"POST", "/api/trash/" + id + "/restore"}, {"DELETE", "/api/trash/" + id},
		{"GET", "/api/trash?user=amy"}, {"GET", "/trash"},
	} {
		rec := h.do(route.method, route.target, "", nil)
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusSeeOther {
			t.Errorf("%s %s without a session: %d", route.method, route.target, rec.Code)
		}
	}
	s := &fileSession{harness: h, t: t, cookie: cookie}
	for _, route := range []struct{ method, target string }{
		{"POST", "/api/trash"}, {"PUT", "/api/trash"}, {"GET", "/api/trash/" + id + "/restore"}, {"PUT", "/api/trash/" + id},
	} {
		if rec := s.call(route.method, route.target, nil, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: got %d", route.method, route.target, rec.Code)
		}
	}
}

func TestPortalTrashRoutesNeedASessionAndSameOrigin(t *testing.T) {
	h := newPortalFilesHarness(t)
	opened := 0
	real := h.portal.openFiles
	h.portal.openFiles = func(user string) (system.Files, error) { opened++; return real(user) }
	id := strings.Repeat("a", 32)
	routes := [][2]string{{"GET", "/api/trash"}, {"DELETE", "/api/trash"}, {"POST", "/api/trash/" + id + "/restore"}, {"DELETE", "/api/trash/" + id}}
	for _, route := range routes {
		if rec := h.do(route[0], route[1], "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session: %d", route[0], route[1], rec.Code)
		}
	}
	if opened != 0 {
		t.Fatalf("files were opened %d times without a session", opened)
	}

	amy := h.signedIn(t, "amy", amyPassword)
	h.write(t, "amy", "a.txt", "x")
	trashed := h.do("DELETE", "/api/files?path=a.txt", "", "192.0.2.10", amy)
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(trashed.Body.Bytes(), &body); err != nil || body.ID == "" {
		t.Fatalf("delete got %d: %s", trashed.Code, trashed.Body)
	}
	for _, route := range [][2]string{{"POST", "/api/trash/" + body.ID + "/restore"}, {"DELETE", "/api/trash/" + body.ID}, {"DELETE", "/api/trash"}, {"DELETE", "/api/files?path=a.txt"}} {
		req := httptest.NewRequest(route[0], route[1], nil)
		req.RemoteAddr = "192.0.2.10:5555"
		req.Header.Set("Origin", "https://evil.example")
		req.AddCookie(amy)
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin %s %s got %d", route[0], route[1], rec.Code)
		}
	}
	if items, _ := listPortalTrash(t, h, amy); len(items) != 1 {
		t.Fatalf("a cross-origin request changed the trash: %+v", items)
	}
}

func listPortalTrash(t *testing.T, h *portalFilesHarness, cookie *http.Cookie) ([]trashEntry, int64) {
	t.Helper()
	rec := h.do("GET", "/api/trash", "", "192.0.2.10", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list trash got %d: %s", rec.Code, rec.Body)
	}
	var listing trashListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}

	return listing.Items, listing.TotalSize
}

func TestPortalUsersNeverSeeOrTouchEachOthersTrash(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "amy-secret.txt", "AMY ONLY")
	h.write(t, "bob", "bob-file.txt", "bob file")
	amy := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)

	var amyItem struct {
		ID string `json:"id"`
	}
	rec := h.do("DELETE", "/api/files?path=amy-secret.txt", "", "192.0.2.10", amy)
	if err := json.Unmarshal(rec.Body.Bytes(), &amyItem); err != nil || amyItem.ID == "" {
		t.Fatalf("delete got %d: %s", rec.Code, rec.Body)
	}
	h.do("DELETE", "/api/files?path=bob-file.txt", "", "192.0.2.10", bob)

	bobItems, _ := listPortalTrash(t, h, bob)
	if len(bobItems) != 1 || bobItems[0].Name != "bob-file.txt" {
		t.Fatalf("bob sees %+v", bobItems)
	}
	for _, request := range [][2]string{
		{"POST", "/api/trash/" + amyItem.ID + "/restore"}, {"DELETE", "/api/trash/" + amyItem.ID},
	} {
		rec := h.do(request[0], request[1], "", "192.0.2.10", bob)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "AMY") {
			t.Errorf("bob %s %s got %d: %s", request[0], request[1], rec.Code, rec.Body)
		}
	}
	for _, path := range []string{"../amy/files/.trash", "../../amy/files/.trash/" + amyItem.ID, "amy/files/.trash"} {
		for _, route := range []string{"/api/files", "/api/files/download", "/api/files/view", "/api/files/zip"} {
			if rec := h.get(bob, route, path); rec.Code == http.StatusOK {
				t.Errorf("bob %s %q got 200: %s", route, path, rec.Body)
			}
		}
	}
	h.do("DELETE", "/api/trash/"+amyItem.ID+"?user=amy", "", "192.0.2.10", bob)
	h.do("DELETE", "/api/trash?user=amy", "", "192.0.2.10", bob)

	amyItems, _ := listPortalTrash(t, h, amy)
	if len(amyItems) != 1 || amyItems[0].ID != amyItem.ID {
		t.Fatalf("amy's trash was touched by bob: %+v", amyItems)
	}
	if bobAfter, _ := listPortalTrash(t, h, bob); len(bobAfter) != 0 {
		t.Fatalf("?user= must be ignored, so the empty call hits bob's own trash only: %+v", bobAfter)
	}
	if rec := h.do("POST", "/api/trash/"+amyItem.ID+"/restore", "", "192.0.2.10", amy); rec.Code != http.StatusOK {
		t.Fatalf("amy restore got %d", rec.Code)
	}
	if data, err := os.ReadFile(filepath.Join(h.base, "amy", "files", "amy-secret.txt")); err != nil || string(data) != "AMY ONLY" {
		t.Fatalf("amy's file: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.base, "bob", "files", "amy-secret.txt")); err == nil {
		t.Fatal("amy's file landed in bob's folder")
	}
}

func TestEmptyingOneUsersTrashLeavesTheOthersAlone(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "a.txt", "x")
	h.write(t, "bob", "b.txt", "x")
	amy := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)
	h.do("DELETE", "/api/files?path=a.txt", "", "192.0.2.10", amy)
	h.do("DELETE", "/api/files?path=b.txt", "", "192.0.2.10", bob)

	if rec := h.do("DELETE", "/api/trash", "", "192.0.2.10", amy); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if items, _ := listPortalTrash(t, h, bob); len(items) != 1 {
		t.Fatalf("bob's trash was emptied: %+v", items)
	}
	if items, _ := listPortalTrash(t, h, amy); len(items) != 0 {
		t.Fatalf("amy's trash was not emptied: %+v", items)
	}
}

func TestPanelTrashCanReachAUsersTrash(t *testing.T) {
	s := newFileSession(t)
	userFiles, err := system.OpenUserFiles(s.usersBase, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer userFiles.(interface{ Close() error }).Close()
	if err := userFiles.Write("mine.txt", strings.NewReader("12345"), false); err != nil {
		t.Fatal(err)
	}
	item, err := userFiles.Trash("mine.txt")
	if err != nil {
		t.Fatal(err)
	}

	own := s.get("/api/trash")
	if own.Code != http.StatusOK || strings.Contains(own.Body.String(), item.ID) {
		t.Fatalf("the panel's own trash must not show a user's items: %s", own.Body)
	}
	rec := s.get("/api/trash?user=amy")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), item.ID) || !strings.Contains(rec.Body.String(), `"totalSize":5`) {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if rec := s.get("/api/trash?user=bob"); strings.Contains(rec.Body.String(), item.ID) {
		t.Fatalf("bob's trash shows amy's item: %s", rec.Body)
	}
	for _, user := range []string{"..", "../amy", "AMY ", "a/b", "nobody-here", "%00", "-x"} {
		rec := s.get("/api/trash?user=" + url.QueryEscape(user))
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("user %q got %d: %s", user, rec.Code, rec.Body)
		}
	}
	if rec := s.call("DELETE", "/api/trash/"+item.ID+"?user=amy", nil, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("purge got %d: %s", rec.Code, rec.Body)
	}
	if rec := s.call("DELETE", "/api/trash?user=amy", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("empty got %d: %s", rec.Code, rec.Body)
	}
}

func TestPanelDeleteInsideAUsersFolderLandsInThePanelTrashAndRestoresThere(t *testing.T) {
	s := newFileSession(t)
	s.seed("amy/files/pic.jpg", "123")
	rec := s.call("DELETE", "/api/files?path=amy%2Ffiles%2Fpic.jpg", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if rec := s.call("POST", "/api/trash/"+item.ID+"/restore", nil, ""); rec.Code != http.StatusOK || !s.exists("amy/files/pic.jpg") {
		t.Fatalf("restore got %d: %s", rec.Code, rec.Body)
	}
}

func TestPortalTrashSweepAfterAgeMakesRestoreFail(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "a.txt", "x")
	amy := h.signedIn(t, "amy", amyPassword)
	rec := h.do("DELETE", "/api/files?path=a.txt", "", "192.0.2.10", amy)
	var item struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}

	if n, err := system.SweepAllTrash(h.base, time.Now().Add(29*24*time.Hour), system.TrashMaxAge); err != nil || n != 0 {
		t.Fatalf("29 days: %d %v", n, err)
	}
	if n, err := system.SweepAllTrash(h.base, time.Now().Add(31*24*time.Hour), system.TrashMaxAge); err != nil || n != 1 {
		t.Fatalf("31 days: %d %v", n, err)
	}
	if rec := h.do("POST", "/api/trash/"+item.ID+"/restore", "", "192.0.2.10", amy); rec.Code != http.StatusNotFound {
		t.Fatalf("restore after aging out got %d", rec.Code)
	}
}
