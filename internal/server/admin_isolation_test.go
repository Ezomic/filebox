package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

var allMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}

func TestAdminPanelHasNoRouteToUserFiles(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	paths := []string{
		"/files", "/files/", "/FILES", "/Files", "/files?path=amy", "/files/amy",
		"/photos", "/photos/", "/PHOTOS", "/photos?user=amy",
		"/trash", "/trash/", "/TRASH", "/trash?user=amy",
		"/api/files", "/api/files/", "/API/files", "/api/files?path=/&user=amy", "/api/files?user=amy",
		"/api/files/download?path=a.txt", "/api/files/view?path=a.txt",
		"/api/files/photos", "/api/files/photos?user=amy",
		"/api/files/thumb?path=a.jpg", "/api/files/preview?path=a.jpg",
		"/api/files/content?path=a.txt", "/api/files/folder", "/api/files/rename", "/api/files/exists",
		"/api/files/zip?path=/", "/api/files/zip?path=/&user=amy",
		"/api/files%2Fdownload?path=a.txt", "/api/files/download%2F..%2F..?path=a.txt",
		"/api/uploads", "/api/uploads/abc", "/api/uploads/abc/finish",
		"/api/trash", "/api/trash/", "/api/trash?user=amy", "/api/trash/abc", "/api/trash/abc/restore",
		"/api/trash%2Fabc",
	}

	for _, path := range paths {
		for _, method := range allMethods {
			for _, session := range []*http.Cookie{cookie, nil} {
				rec := h.do(method, path, "", session)
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s (session %v): got %d, want 404; body %q", method, path, session != nil, rec.Code, rec.Body.String())
					continue
				}
				if body := rec.Body.String(); method != "HEAD" && body != "404 page not found\n" {
					t.Errorf("%s %s: unexpected 404 body %q", method, path, body)
				}
			}
		}
	}
}

func TestAdminPanelTraversalIntoRemovedRoutesOnlyReachesAdminRoutes(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("GET", "/api/files/../users", "", cookie)
	if (rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusTemporaryRedirect) || rec.Header().Get("Location") != "/api/users" {
		t.Fatalf("got %d to %q, want the mux clean-path redirect to /api/users", rec.Code, rec.Header().Get("Location"))
	}

	for _, path := range []string{"/api/files/../files", "/api/trash/../files/download?path=a.txt", "/files/../photos"} {
		rec := h.do("GET", path, "", cookie)
		if rec.Code != http.StatusMovedPermanently && rec.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s: got %d", path, rec.Code)
			continue
		}
		if followed := h.do("GET", rec.Header().Get("Location"), "", cookie); followed.Code != http.StatusNotFound {
			t.Errorf("%s redirects to %s which answers %d", path, rec.Header().Get("Location"), followed.Code)
		}
	}
}

func TestAdminPanelNoLongerServesTheFileBrowserScripts(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	for _, name := range []string{"files.js", "trash.js", "photos.js", "viewer.js", "files.html", "trash.html", "photos.html"} {
		if rec := h.do("GET", "/static/"+name, "", cookie); rec.Code != http.StatusNotFound {
			t.Errorf("/static/%s: got %d, want 404", name, rec.Code)
		}
	}
	for _, name := range []string{"style.css", "dashboard.js", "users.js", "logs.js", "auth.js", "tv.js", "tvlink.js"} {
		if rec := h.do("GET", "/static/"+name, "", cookie); rec.Code != http.StatusOK {
			t.Errorf("/static/%s: got %d, want 200", name, rec.Code)
		}
	}
}

func TestAdminPagesNavigateToAdministrationOnly(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	pages := map[string]string{"/": "Dashboard", "/users": "Users", "/logs": "Logs"}
	for path, current := range pages {
		body := h.do("GET", path, "", cookie).Body.String()
		for _, gone := range []string{`href="/files"`, `href="/photos"`, `href="/trash"`, "files.js", "trash.js", "photos.js", "viewer.js"} {
			if strings.Contains(body, gone) {
				t.Errorf("%s still mentions %s", path, gone)
			}
		}
		if !strings.Contains(body, `aria-current="page">`+current+`</a>`) {
			t.Errorf("%s does not mark %s as the current page", path, current)
		}
		for _, kept := range []string{`href="/">Dashboard`, `href="/users">Users`, `href="/logs">Logs`, `id="logout"`} {
			if !strings.Contains(body, kept) && !strings.Contains(body, strings.Replace(kept, `">`, `" aria-current="page">`, 1)) {
				t.Errorf("%s lost nav item %s", path, kept)
			}
		}
	}
}

func TestUserListCarriesOnlyNameRoleAndByteUsage(t *testing.T) {
	h := newHarness(t)
	cookie := h.signedIn(t)

	rec := h.do("GET", "/api/users", "", cookie)
	var body struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Users) == 0 {
		t.Fatal("no users listed")
	}
	for _, user := range body.Users {
		var keys []string
		for key := range user {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"admin", "bytes", "name"}) {
			t.Errorf("user entry has fields %v", keys)
		}
	}
}
