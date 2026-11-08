package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	monday         = "2026-11-06T0030"
	tuesday        = "2026-11-07T0030"
	deletedAmy     = "amy.deleted-20261101000000"
	deletedSecret  = "previous-amy-contents"
	familyEarlier  = "family-earlier-contents"
	amyTuesdayText = "amy on tuesday"
)

type versionsHarness struct {
	*portalFilesHarness
	disk *system.FakeDisk
	amy  *http.Cookie
	bob  *http.Cookie
}

func newVersionsHarness(t *testing.T) *versionsHarness {
	t.Helper()
	base := t.TempDir()
	for _, dir := range []string{system.FamilyDir, "amy/files", "bob/files"} {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	snapshots := map[string]string{
		"amy/" + monday + "/report.txt":                  "amy on monday",
		"amy/" + tuesday + "/report.txt":                 amyTuesdayText,
		"amy/" + tuesday + "/docs/notes.txt":             "notes",
		"bob/" + tuesday + "/secret.txt":                 bobSecret,
		system.FamilyDir + "/" + tuesday + "/recipe.txt": familyEarlier,
		deletedAmy + "/2026-10-30T0030/old.txt":          deletedSecret,
	}
	for name, content := range snapshots {
		writeFile(t, filepath.Join(base, system.SnapshotsDir, filepath.FromSlash(name)), content)
	}

	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword}}
	users := system.NewFakeUsers("amy", "bob")
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := system.NewFakeDisk(diskAt(1000))
	portal := NewPortal(PortalDeps{
		Verifier:           verifier,
		Users:              users,
		Roles:              roles,
		OpenFiles:          func(user string) (system.Files, error) { return system.OpenUserFiles(base, user) },
		OpenFamily:         func() (system.Files, error) { return system.OpenFamilyFiles(base) },
		OpenVersions:       func(user string) (system.Versions, error) { return system.OpenUserVersions(base, user) },
		OpenFamilyVersions: func() (system.Versions, error) { return system.OpenFamilyVersions(base), nil },
		MaxUpload:          1 << 20,
		Disk:               disk,
	})
	h := &versionsHarness{
		portalFilesHarness: &portalFilesHarness{portalHarness: &portalHarness{portal: portal, verifier: verifier, roles: roles}, base: base, users: users},
		disk:               disk,
	}
	h.amy = h.signedIn(t, "amy", amyPassword)
	h.bob = h.signedIn(t, "bob", bobPassword)

	return h
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *versionsHarness) restore(cookie *http.Cookie, api, id, path string) *httptest.ResponseRecorder {
	return h.call("POST", api+"/versions/"+url.PathEscape(id)+"/restore", `{"path":`+quote(path)+`}`, cookie)
}

func (h *versionsHarness) liveFile(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.base, filepath.FromSlash(root), filepath.FromSlash(name)))
	if err != nil {
		return ""
	}

	return string(data)
}

func versionList(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Versions []struct {
			ID    string `json:"id"`
			Taken string `json:"taken"`
		} `json:"versions"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	ids := []string{}
	for _, version := range body.Versions {
		ids = append(ids, version.ID)
	}

	return ids
}

func TestEarlierVersionsAreListedPerFolderNewestFirst(t *testing.T) {
	h := newVersionsHarness(t)
	if got := versionList(t, h.call("GET", "/api/versions", "", h.amy)); !reflect.DeepEqual(got, []string{tuesday, monday}) {
		t.Fatalf("amy got %v", got)
	}
	if got := versionList(t, h.call("GET", "/api/versions", "", h.bob)); !reflect.DeepEqual(got, []string{tuesday}) {
		t.Fatalf("bob got %v", got)
	}
	if got := versionList(t, h.call("GET", "/api/family/versions", "", h.bob)); !reflect.DeepEqual(got, []string{tuesday}) {
		t.Fatalf("family got %v", got)
	}
}

func TestAnEarlierFolderCanBeBrowsedAndDownloaded(t *testing.T) {
	h := newVersionsHarness(t)
	rec := h.get(h.amy, "/api/versions/"+tuesday+"/files", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"docs"`) || !strings.Contains(rec.Body.String(), `"report.txt"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	rec = h.get(h.amy, "/api/versions/"+tuesday+"/files/download", "report.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != amyTuesdayText {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if rec := h.get(h.amy, "/api/versions/"+tuesday+"/files", "docs/nothing"); rec.Code != http.StatusNotFound {
		t.Fatalf("a folder that was not there got %d", rec.Code)
	}
}

func TestEarlierVersionsCannotBeChanged(t *testing.T) {
	h := newVersionsHarness(t)
	for _, route := range []struct{ method, path string }{
		{"PUT", "/api/versions/" + tuesday + "/files/content?path=x.txt"},
		{"DELETE", "/api/versions/" + tuesday + "/files?path=report.txt"},
		{"POST", "/api/versions/" + tuesday + "/files/rename"},
		{"POST", "/api/versions/" + tuesday + "/files/folder"},
		{"DELETE", "/api/versions/" + tuesday},
	} {
		if rec := h.call(route.method, route.path, `{"path":"x","from":"report.txt","to":"y.txt"}`, h.amy); rec.Code < 400 {
			t.Errorf("%s %s got %d", route.method, route.path, rec.Code)
		}
	}
	data, err := os.ReadFile(filepath.Join(h.base, system.SnapshotsDir, "amy", tuesday, "report.txt"))
	if err != nil || string(data) != amyTuesdayText {
		t.Fatalf("the snapshot changed: %q %v", data, err)
	}
}

func TestRestorePutsTheItemBackAndTrashesWhatIsThereNow(t *testing.T) {
	h := newVersionsHarness(t)
	h.write(t, "amy", "report.txt", "overwritten over sftp")

	rec := h.restore(h.amy, "/api", monday, "report.txt")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replaced":true`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := h.liveFile(t, "amy/files", "report.txt"); got != "amy on monday" {
		t.Fatalf("got %q", got)
	}
	trash := h.call("GET", "/api/trash", "", h.amy)
	if !strings.Contains(trash.Body.String(), `"path":"report.txt"`) {
		t.Fatalf("what was there must be in the trash: %s", trash.Body)
	}
}

func TestRestoreBringsBackAFolderDeletedOverSFTP(t *testing.T) {
	h := newVersionsHarness(t)
	rec := h.restore(h.amy, "/api", tuesday, "docs")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replaced":false`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := h.liveFile(t, "amy/files", "docs/notes.txt"); got != "notes" {
		t.Fatalf("got %q", got)
	}
}

func TestUsersNeverReachAnotherUsersOrADeletedAccountsVersions(t *testing.T) {
	h := newVersionsHarness(t)
	ids := []string{
		"../bob/" + tuesday, "..%2Fbob%2F" + tuesday, "../" + deletedAmy + "/2026-10-30T0030", "2026-10-30T0030",
		"../" + system.FamilyDir + "/" + tuesday, "..", ".", tuesday + "/../../bob/" + tuesday,
	}
	for _, id := range ids {
		for _, rec := range []*httptest.ResponseRecorder{
			h.get(h.amy, "/api/versions/"+id+"/files", ""),
			h.get(h.amy, "/api/versions/"+id+"/files/download", "secret.txt"),
			h.get(h.amy, "/api/versions/"+id+"/files/download", "old.txt"),
			h.restore(h.amy, "/api", id, "secret.txt"),
		} {
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), bobSecret) || strings.Contains(rec.Body.String(), deletedSecret) {
				t.Fatalf("id %q reached another folder: %d %s", id, rec.Code, rec.Body)
			}
		}
	}
	for _, path := range []string{"secret.txt", "../../bob/" + tuesday + "/secret.txt", "../../../bob/files/secret.txt"} {
		if rec := h.get(h.amy, "/api/versions/"+tuesday+"/files/download", path); rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), bobSecret) {
			t.Fatalf("path %q got %d %s", path, rec.Code, rec.Body)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, "amy", "files")); len(entries) != 0 {
		t.Fatalf("nothing may have been restored: %v", entries)
	}
}

func TestFamilyVersionsAreOnlyReachedThroughTheFamilyView(t *testing.T) {
	h := newVersionsHarness(t)
	if rec := h.get(h.amy, "/api/versions/"+tuesday+"/files/download", "recipe.txt"); rec.Code != http.StatusNotFound {
		t.Fatalf("a home route reached the family versions: %d %s", rec.Code, rec.Body)
	}
	rec := h.get(h.bob, "/api/family/versions/"+tuesday+"/files/download", "recipe.txt")
	if rec.Code != http.StatusOK || rec.Body.String() != familyEarlier {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if rec := h.get(h.bob, "/api/family/versions/"+tuesday+"/files/download", "report.txt"); rec.Code != http.StatusNotFound {
		t.Fatalf("the family view reached amy's versions: %d", rec.Code)
	}
	if rec := h.restore(h.bob, "/api/family", tuesday, "recipe.txt"); rec.Code != http.StatusOK {
		t.Fatalf("any family member can restore in Family, got %d %s", rec.Code, rec.Body)
	}
	if got := h.liveFile(t, system.FamilyDir, "recipe.txt"); got != familyEarlier {
		t.Fatalf("got %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, "bob", "files")); len(entries) != 0 {
		t.Fatalf("a family restore landed in bob's own files: %v", entries)
	}
}

func TestRestoreIsRefusedWhenItWouldGoOverTheQuota(t *testing.T) {
	h := newVersionsHarness(t)
	if err := h.users.SetQuota("amy", 1<<20); err != nil {
		t.Fatal(err)
	}
	h.users.SetUsed("amy", 1<<20-uint64(len("amy on monday"))+1)

	rec := h.restore(h.amy, "/api", monday, "report.txt")
	if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeRestoreOverQuota) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "report.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused restore wrote something: %v", err)
	}
	h.users.SetUsed("amy", 1<<20-uint64(len("amy on monday")))
	if rec := h.restore(h.amy, "/api", monday, "report.txt"); rec.Code != http.StatusOK {
		t.Fatalf("an exact fit must be allowed, got %d %s", rec.Code, rec.Body)
	}
}

func TestRestoreIsRefusedWhileTheDiskIsFull(t *testing.T) {
	h := newVersionsHarness(t)
	h.disk.Set(diskAt(9800))
	for _, rec := range []*httptest.ResponseRecorder{
		h.restore(h.amy, "/api", monday, "report.txt"),
		h.restore(h.bob, "/api/family", tuesday, "recipe.txt"),
	} {
		if rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeRestoreDiskFull) {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	}
	if h.liveFile(t, "amy/files", "report.txt") != "" || h.liveFile(t, system.FamilyDir, "recipe.txt") != "" {
		t.Fatal("a refused restore wrote something")
	}
	rec := h.get(h.amy, "/api/versions/"+tuesday+"/files/download", "report.txt")
	if rec.Code != http.StatusOK {
		t.Fatalf("looking at earlier versions must keep working on a full disk, got %d", rec.Code)
	}
}

func TestRestoreRefusesPathsItCannotRestore(t *testing.T) {
	h := newVersionsHarness(t)
	cases := map[string]int{
		"":                  http.StatusBadRequest,
		".trash":            http.StatusBadRequest,
		"docs/../..":        http.StatusBadRequest,
		"../bob/secret.txt": http.StatusBadRequest,
		"missing.txt":       http.StatusNotFound,
	}
	for path, want := range cases {
		if rec := h.restore(h.amy, "/api", tuesday, path); rec.Code != want {
			t.Errorf("%q: got %d, want %d", path, rec.Code, want)
		}
	}
	if rec := h.call("POST", "/api/versions/"+tuesday+"/restore", `not json`, h.amy); rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestVersionRoutesNeedASignedInUser(t *testing.T) {
	h := newVersionsHarness(t)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/versions"},
		{"GET", "/api/versions/" + tuesday + "/files"},
		{"GET", "/api/versions/" + tuesday + "/files/download?path=report.txt"},
		{"POST", "/api/versions/" + tuesday + "/restore"},
		{"GET", "/api/family/versions"},
		{"POST", "/api/family/versions/" + tuesday + "/restore"},
	} {
		if rec := h.call(route.method, route.path, `{"path":"report.txt"}`, nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s got %d", route.method, route.path, rec.Code)
		}
	}
}

func TestVersionRoutesAreOffWithoutSnapshots(t *testing.T) {
	h := newFamilyHarness(t)
	if rec := h.call("GET", "/api/versions", "", h.amy); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.call("GET", "/api/family/versions", "", h.amy); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}
