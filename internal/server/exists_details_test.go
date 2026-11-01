package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type existsAnswer struct {
	Path     string     `json:"path"`
	IsDir    bool       `json:"isDir"`
	Size     *int64     `json:"size"`
	Modified *time.Time `json:"modified"`
}

func decodeExists(t *testing.T, body string) []existsAnswer {
	t.Helper()
	var parsed struct {
		Existing []existsAnswer `json:"existing"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Existing == nil {
		t.Fatalf("body %q: %v", body, err)
	}

	return parsed.Existing
}

func TestExistsCheckReportsSizeAndModifiedForFilesOnly(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "Photo backup/2024/03/IMG_0001.jpg", "twelve bytes")
	h.write(t, "amy", "empty.bin", "")
	when := time.Date(2020, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(h.base, "amy", "files", "empty.bin"), when, when); err != nil {
		t.Fatal(err)
	}
	amy := h.signedIn(t, "amy", amyPassword)

	rec := h.do("POST", "/api/files/exists", `{"paths":["Photo backup/2024/03/IMG_0001.jpg","empty.bin","Photo backup","Photo backup/2024/03/missing.jpg"]}`, "192.0.2.10", amy)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	got := map[string]existsAnswer{}
	for _, entry := range decodeExists(t, rec.Body.String()) {
		got[entry.Path] = entry
	}
	if len(got) != 3 {
		t.Fatalf("want the two files and the folder, got %v", got)
	}
	photo := got["Photo backup/2024/03/IMG_0001.jpg"]
	if photo.IsDir || photo.Size == nil || *photo.Size != 12 || photo.Modified == nil {
		t.Errorf("file answer is wrong: %+v", photo)
	}
	empty := got["empty.bin"]
	if empty.Size == nil || *empty.Size != 0 || empty.Modified == nil || !empty.Modified.Equal(when) {
		t.Errorf("an empty file must report a size of 0 and its modified time: %+v", empty)
	}
	folder := got["Photo backup"]
	if !folder.IsDir || folder.Size != nil || folder.Modified != nil {
		t.Errorf("a folder must not report a size or a modified time: %+v", folder)
	}
}

func TestExistsCheckNeverRevealsAnotherUsersFiles(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "bob", "secret.jpg", "bob has this photo")
	h.write(t, "amy", "mine.jpg", "x")
	amy := h.signedIn(t, "amy", amyPassword)

	for _, path := range []string{"secret.jpg", "../bob/files/secret.jpg", "bob/files/secret.jpg", "/bob/files/secret.jpg", `..\bob\files\secret.jpg`} {
		rec := h.do("POST", "/api/files/exists", `{"paths":["`+jsonSafe(path)+`"]}`, "192.0.2.10", amy)
		if strings.Contains(rec.Body.String(), "secret.jpg") && rec.Code == http.StatusOK {
			t.Errorf("%q leaked: %s", path, rec.Body)
		}
		if rec.Code == http.StatusOK && len(decodeExists(t, rec.Body.String())) != 0 {
			t.Errorf("%q must not exist for amy: %s", path, rec.Body)
		}
	}
	if rec := h.do("POST", "/api/files/exists", `{"paths":["mine.jpg"]}`, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("signed out got %d", rec.Code)
	}
}

func TestExistsCheckKeepsHiddenFoldersAndTraversalOut(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", ".thumbs/a.jpg", "thumb")
	h.write(t, "amy", ".trash/b.jpg", "trash")
	h.write(t, "amy", ".uploads/c.part", "part")
	amy := h.signedIn(t, "amy", amyPassword)

	for _, path := range []string{".thumbs/a.jpg", ".trash/b.jpg", ".uploads/c.part", ".trash", "Photo backup/../.trash/b.jpg", "../a.jpg", ".."} {
		rec := h.do("POST", "/api/files/exists", `{"paths":["`+jsonSafe(path)+`"]}`, "192.0.2.10", amy)
		if rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), `"size"`) {
			t.Errorf("%q got %d: %s", path, rec.Code, rec.Body)
		}
	}
}

func TestExistsCheckFollowsNoSymlinkOutOfTheUsersFolder(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "bob", "private.jpg", "bob only")
	outside := filepath.Join(h.base, "bob", "files", "private.jpg")
	if err := os.Symlink(outside, filepath.Join(h.base, "amy", "files", "link.jpg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	amy := h.signedIn(t, "amy", amyPassword)

	rec := h.do("POST", "/api/files/exists", `{"paths":["link.jpg"]}`, "192.0.2.10", amy)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"size"`) {
		t.Errorf("a symlink out of the folder leaked a size: %s", rec.Body)
	}
}

func TestExistsCheckBatchLimitsStayTheSame(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "a.jpg", "abc")
	amy := h.signedIn(t, "amy", amyPassword)

	paths := make([]string, 0, maxExistsPaths+1)
	for range maxExistsPaths + 1 {
		paths = append(paths, `"a.jpg"`)
	}
	rec := h.do("POST", "/api/files/exists", `{"paths":[`+strings.Join(paths, ",")+`]}`, "192.0.2.10", amy)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized batch got %d", rec.Code)
	}
	rec = h.do("POST", "/api/files/exists", `{"paths":["a.jpg","a.jpg","a.jpg"]}`, "192.0.2.10", amy)
	if got := decodeExists(t, rec.Body.String()); len(got) != 1 || got[0].Size == nil || *got[0].Size != 3 {
		t.Errorf("duplicates must collapse to one answer: %s", rec.Body)
	}
}
