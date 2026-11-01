package server

import (
	"encoding/json"
	"errors"
	"fmt"
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

type storageHarness struct {
	*portalFilesHarness
	disk *system.FakeDisk
	amy  *http.Cookie
	bob  *http.Cookie
}

func diskAt(used uint64) system.DiskUsage {
	return system.DiskUsage{Total: 10000, Used: used, Free: 10000 - used}
}

func newStorageHarness(t *testing.T, used uint64) *storageHarness {
	t.Helper()

	base := t.TempDir()
	for _, user := range []string{"amy", "bob"} {
		if err := os.MkdirAll(filepath.Join(base, user, "files"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword}}
	users := system.NewFakeUsers("amy", "bob")
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	disk := system.NewFakeDisk(diskAt(used))
	portal := NewPortal(PortalDeps{
		Verifier:  verifier,
		Users:     users,
		Roles:     roles,
		OpenFiles: func(user string) (system.Files, error) { return system.OpenUserFiles(base, user) },
		MaxUpload: 1 << 20,
		Disk:      disk,
	})
	h := &storageHarness{
		portalFilesHarness: &portalFilesHarness{portalHarness: &portalHarness{portal: portal, verifier: verifier}, base: base, users: users},
		disk:               disk,
	}
	h.amy = h.signedIn(t, "amy", amyPassword)
	h.bob = h.signedIn(t, "bob", bobPassword)

	return h
}

func (h *storageHarness) storage(t *testing.T, cookie *http.Cookie) storageView {
	t.Helper()
	rec := h.do("GET", "/api/storage", "", "192.0.2.10", cookie)
	var view storageView
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return view
}

func (h *storageHarness) setUsed(used uint64) {
	h.disk.Set(diskAt(used))
	h.portal.space.invalidate()
}

func (h *storageHarness) trash(t *testing.T, cookie *http.Cookie, path string) {
	t.Helper()
	if rec := h.do("DELETE", "/api/files?path="+path, "", "192.0.2.10", cookie); rec.Code != http.StatusOK {
		t.Fatalf("trash %s got %d %s", path, rec.Code, rec.Body)
	}
}

func (h *storageHarness) createUpload(cookie *http.Cookie, path string, size int) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"path":%q,"size":%d,"overwrite":false}`, path, size)

	return h.do("POST", "/api/uploads", body, "192.0.2.10", cookie)
}

func TestStorageReportsTheWholeDiskWhenTheUserHasNoQuota(t *testing.T) {
	h := newStorageHarness(t, 4000)

	view := h.storage(t, h.amy)
	if view.Used != 4000 || view.Total != 10000 || view.Free != 6000 || view.Quota != nil || view.QuotaUsed != nil {
		t.Fatalf("got %+v", view)
	}
	if view.Measure != "disk" || view.Level != system.LevelOK || view.Percent != 40 || view.TrashBytes != 0 {
		t.Fatalf("got %+v", view)
	}
}

func TestStorageReportsTheUsersOwnQuotaWhenOneApplies(t *testing.T) {
	h := newStorageHarness(t, 4000)
	h.users.SetUsed("amy", 9<<20)
	h.users.SetUsed("bob", 1<<20)
	if err := h.users.SetQuota("amy", 10<<20); err != nil {
		t.Fatal(err)
	}

	amy := h.storage(t, h.amy)
	if amy.Measure != "quota" || amy.Quota == nil || *amy.Quota != 10<<20 || amy.QuotaUsed == nil || *amy.QuotaUsed != 9<<20 {
		t.Fatalf("got %+v", amy)
	}
	if amy.Level != system.LevelWarning || amy.Percent != 90 {
		t.Fatalf("a user at 90 percent of the quota: %+v", amy)
	}
	bob := h.storage(t, h.bob)
	if bob.Measure != "disk" || bob.Quota != nil || bob.QuotaUsed != nil {
		t.Fatalf("bob has no quota and must not see the quota of amy: %+v", bob)
	}
}

func TestStorageQuotaLevelsUseTheQuotaNotTheDisk(t *testing.T) {
	h := newStorageHarness(t, 1000)
	if err := h.users.SetQuota("amy", 100<<20); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		used uint64
		want system.StorageLevel
	}{
		{0, system.LevelOK},
		{85<<20 - 1, system.LevelOK},
		{85 << 20, system.LevelWarning},
		{95<<20 - 1, system.LevelWarning},
		{95 << 20, system.LevelCritical},
		{100 << 20, system.LevelCritical},
		{150 << 20, system.LevelCritical},
	}
	for _, c := range cases {
		h.users.SetUsed("amy", c.used)
		h.portal.space.invalidate()
		if got := h.storage(t, h.amy); got.Level != c.want {
			t.Errorf("quota used %d: got %s, want %s", c.used, got.Level, c.want)
		}
	}
}

func TestStorageADiskAtTheFullLineOverridesAHealthyQuota(t *testing.T) {
	h := newStorageHarness(t, 9800)
	if err := h.users.SetQuota("amy", 100<<20); err != nil {
		t.Fatal(err)
	}
	h.users.SetUsed("amy", 1<<20)

	view := h.storage(t, h.amy)
	if view.Level != system.LevelFull || view.Measure != "quota" {
		t.Fatalf("got %+v", view)
	}
}

func TestStorageQuotaLargerThanTheDiskStillMeasuresAgainstTheQuota(t *testing.T) {
	h := newStorageHarness(t, 1000)
	if err := h.users.SetQuota("amy", 1<<40); err != nil {
		t.Fatal(err)
	}
	h.users.SetUsed("amy", 1<<30)

	view := h.storage(t, h.amy)
	if view.Level != system.LevelOK || view.Quota == nil || *view.Quota != 1<<40 {
		t.Fatalf("got %+v", view)
	}
}

func TestStorageQuotasOffFallBackToTheDisk(t *testing.T) {
	h := newStorageHarness(t, 9000)
	h.users.QuotasOff = true

	view := h.storage(t, h.amy)
	if view.Measure != "disk" || view.Quota != nil || view.Level != system.LevelWarning {
		t.Fatalf("got %+v", view)
	}
}

func TestStorageLevelsAtTheExactThresholds(t *testing.T) {
	h := newStorageHarness(t, 0)
	cases := []struct {
		used uint64
		want system.StorageLevel
	}{
		{8499, system.LevelOK},
		{8500, system.LevelWarning},
		{9499, system.LevelWarning},
		{9500, system.LevelCritical},
		{9799, system.LevelCritical},
		{9800, system.LevelFull},
	}
	for _, c := range cases {
		h.setUsed(c.used)
		if got := h.storage(t, h.amy).Level; got != c.want {
			t.Errorf("used %d of 10000: got %s, want %s", c.used, got, c.want)
		}
	}
}

func TestStorageOddDiskAnswersStaySane(t *testing.T) {
	h := newStorageHarness(t, 0)

	h.disk.Set(system.DiskUsage{})
	h.portal.space.invalidate()
	if view := h.storage(t, h.amy); view.Level != system.LevelOK || view.Percent != 0 || view.Total != 0 {
		t.Fatalf("zero total: %+v", view)
	}

	h.disk.Set(system.DiskUsage{Total: 100, Used: 10, Free: 5000})
	h.portal.space.invalidate()
	if view := h.storage(t, h.amy); view.Free != 90 || view.Used != 10 {
		t.Fatalf("free larger than total: %+v", view)
	}

	h.disk.Set(system.DiskUsage{Total: 100, Used: 500, Free: 0})
	h.portal.space.invalidate()
	if view := h.storage(t, h.amy); view.Used != 100 || view.Percent != 100 || view.Level != system.LevelFull {
		t.Fatalf("used larger than total: %+v", view)
	}
}

func TestStorageRequiresASession(t *testing.T) {
	h := newStorageHarness(t, 4000)
	for _, route := range []string{"/api/storage", "/api/storage?user=bob"} {
		if rec := h.do("GET", route, "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s got %d", route, rec.Code)
		}
	}
}

func TestStorageIgnoresAUserOverride(t *testing.T) {
	h := newStorageHarness(t, 4000)
	h.write(t, "bob", "big.bin", strings.Repeat("x", 100))
	h.trash(t, h.bob, "big.bin")
	if err := h.users.SetQuota("bob", 10<<20); err != nil {
		t.Fatal(err)
	}

	for _, route := range []string{"/api/storage?user=bob", "/api/storage?name=bob", "/api/storage?user=bob&path=../bob"} {
		rec := h.do("GET", route, "", "192.0.2.10", h.amy)
		var view storageView
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &view) != nil || view.TrashBytes != 0 || view.Quota != nil {
			t.Fatalf("%s leaked numbers of bob: %d %s", route, rec.Code, rec.Body)
		}
	}
}

func TestStorageCountsOnlyTheSignedInUsersTrash(t *testing.T) {
	h := newStorageHarness(t, 4000)
	h.write(t, "amy", "a.bin", strings.Repeat("a", 30))
	h.write(t, "amy", "dir/b.bin", strings.Repeat("b", 12))
	h.write(t, "bob", "c.bin", strings.Repeat("c", 700))
	h.trash(t, h.amy, "a.bin")
	h.trash(t, h.amy, "dir")
	h.trash(t, h.bob, "c.bin")

	if got := h.storage(t, h.amy).TrashBytes; got != 42 {
		t.Errorf("amy: got %d", got)
	}
	if got := h.storage(t, h.bob).TrashBytes; got != 700 {
		t.Errorf("bob: got %d", got)
	}
}

func TestStorageTrashSizeWithNoTrashFolderIsZero(t *testing.T) {
	h := newStorageHarness(t, 4000)
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", ".trash")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the test needs a missing trash folder")
	}
	if got := h.storage(t, h.amy).TrashBytes; got != 0 {
		t.Fatalf("got %d", got)
	}
}

func TestStorageTrashSizeSurvivesHugeAndBrokenEntries(t *testing.T) {
	h := newStorageHarness(t, 4000)
	trash := filepath.Join(h.base, "amy", "files", ".trash")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{
		"11111111111111111111111111111111": `{"path":"a","deleted":"2026-10-01T00:00:00Z","size":9223372036854775807}`,
		"22222222222222222222222222222222": `{"path":"b","deleted":"2026-10-01T00:00:00Z","size":9223372036854775807}`,
		"33333333333333333333333333333333": `{"path":"c","deleted":"2026-10-01T00:00:00Z","size":-50}`,
		"44444444444444444444444444444444": `not json`,
	}
	for id, content := range entries {
		if err := os.WriteFile(filepath.Join(trash, id+".json"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(trash, id), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const want = uint64(1<<63-1) * 2
	if got := h.storage(t, h.amy).TrashBytes; got != want {
		t.Fatalf("got %d, want %d", got, want)
	}
}

func TestStorageDiskUsageIsCachedBrieflyAndDroppedByWrites(t *testing.T) {
	h := newStorageHarness(t, 4000)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	h.portal.space.now = func() time.Time { return now }
	h.portal.space.invalidate()

	h.storage(t, h.amy)
	h.storage(t, h.bob)
	h.storage(t, h.amy)
	if calls := h.disk.Calls(); calls != 1 {
		t.Fatalf("many reads inside the cache window hit the disk %d times", calls)
	}

	now = now.Add(storageTTL + time.Millisecond)
	h.storage(t, h.amy)
	if calls := h.disk.Calls(); calls != 2 {
		t.Fatalf("an expired cache must read again, got %d calls", calls)
	}

	h.disk.Set(diskAt(9000))
	if h.storage(t, h.amy).Used != 4000 {
		t.Fatal("the cached value should still be served")
	}
	if rec := h.upload(h.amy, "small.txt", "hi"); rec.Code != http.StatusNoContent {
		t.Fatalf("upload got %d %s", rec.Code, rec.Body)
	}
	if got := h.storage(t, h.amy).Used; got != 9000 {
		t.Fatalf("a write must drop the cache, got %d", got)
	}
}

func TestStorageUnavailableDiskIsA503AndNeverBlocksUploads(t *testing.T) {
	h := newStorageHarness(t, 4000)
	h.disk.Fail(errors.New("statfs: i/o error"))
	h.portal.space.invalidate()

	rec := h.do("GET", "/api/storage", "", "192.0.2.10", h.amy)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "statfs") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.upload(h.amy, "ok.txt", "hi"); rec.Code != http.StatusNoContent {
		t.Fatalf("a broken statfs must not stop uploads: %d %s", rec.Code, rec.Body)
	}
}

func TestPortalWithoutADiskNeverBlocksUploads(t *testing.T) {
	h := newPortalFilesHarness(t)
	amy := h.signedIn(t, "amy", amyPassword)
	if rec := h.upload(amy, "ok.txt", "hi"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/api/storage", "", "192.0.2.10", amy); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestUploadsAreRefusedFromNinetyEightPercentOnBothPaths(t *testing.T) {
	cases := []struct {
		used    uint64
		refused bool
	}{{9700, false}, {9799, false}, {9800, true}, {9801, true}, {10000, true}}
	for _, c := range cases {
		h := newStorageHarness(t, c.used)

		single := h.upload(h.amy, "one.txt", "hi")
		chunked := h.createUpload(h.amy, "two.bin", 5)
		wantSingle, wantChunked := http.StatusNoContent, http.StatusCreated
		if c.refused {
			wantSingle, wantChunked = http.StatusInsufficientStorage, http.StatusInsufficientStorage
		}
		if single.Code != wantSingle || chunked.Code != wantChunked {
			t.Errorf("used %d: single %d, chunked %d", c.used, single.Code, chunked.Code)
		}
		if !c.refused {
			continue
		}
		for _, rec := range []*httptest.ResponseRecorder{single, chunked} {
			var body struct {
				Error    string `json:"error"`
				DiskFull bool   `json:"diskFull"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &body) != nil || body.Error != diskFullMessage || !body.DiskFull {
				t.Errorf("used %d: got %s", c.used, rec.Body)
			}
		}
		if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "one.txt")); err == nil {
			t.Errorf("used %d: a refused upload left a file behind", c.used)
		}
	}
}

func TestAFullDiskStillAllowsReadingDeletingRestoringAndEmptyingTheTrash(t *testing.T) {
	h := newStorageHarness(t, 10000)
	h.write(t, "amy", "keep.txt", "keep me")
	h.write(t, "amy", "drop.txt", "drop me")
	h.write(t, "amy", "gone.txt", "gone")

	check := func(name string, rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Errorf("%s on a full disk: got %d %s", name, rec.Code, rec.Body)
		}
	}
	call := func(method, route, body string) *httptest.ResponseRecorder {
		return h.do(method, route, body, "192.0.2.10", h.amy)
	}
	check("list", call("GET", "/api/files?path=", ""), http.StatusOK)
	download := call("GET", "/api/files/download?path=keep.txt", "")
	check("download", download, http.StatusOK)
	if download.Body.String() != "keep me" {
		t.Errorf("download body %q", download.Body)
	}
	check("view", call("GET", "/api/files/view?path=keep.txt", ""), http.StatusOK)
	check("zip", call("GET", "/api/files/zip?path=", ""), http.StatusOK)
	check("storage", call("GET", "/api/storage", ""), http.StatusOK)
	check("delete", call("DELETE", "/api/files?path=drop.txt", ""), http.StatusOK)
	check("delete 2", call("DELETE", "/api/files?path=gone.txt", ""), http.StatusOK)

	var listed struct {
		Items []trashJSON `json:"items"`
	}
	rec := call("GET", "/api/trash", "")
	check("list trash", rec, http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Items) != 2 {
		t.Fatalf("got %v %+v", err, listed)
	}
	check("restore", call("POST", "/api/trash/"+listed.Items[0].ID+"/restore", ""), http.StatusOK)
	check("purge", call("DELETE", "/api/trash/"+listed.Items[1].ID, ""), http.StatusNoContent)
	check("empty trash", call("DELETE", "/api/trash", ""), http.StatusOK)
}

func TestAFullDiskDoesNotStopAnUploadThatAlreadyStarted(t *testing.T) {
	h := newStorageHarness(t, 0)
	rec := h.createUpload(h.amy, "big.bin", 4)
	var info struct {
		ID string `json:"id"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &info) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	h.setUsed(10000)
	req := httptest.NewRequest("PUT", "/api/uploads/"+info.ID, strings.NewReader("abcd"))
	req.RemoteAddr = "192.0.2.10:5555"
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Range", "bytes 0-3/4")
	req.AddCookie(h.amy)
	chunk := httptest.NewRecorder()
	h.portal.ServeHTTP(chunk, req)
	if chunk.Code != http.StatusOK {
		t.Fatalf("a chunk of an upload that began earlier got %d %s", chunk.Code, chunk.Body)
	}
}

func TestStorageResponsesAreNotCachedAndOnlyAnswerGet(t *testing.T) {
	h := newStorageHarness(t, 4000)
	rec := h.do("GET", "/api/storage", "", "192.0.2.10", h.amy)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %q", rec.Header().Get("Cache-Control"))
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if rec := h.do(method, "/api/storage", "", "192.0.2.10", h.amy); rec.Code == http.StatusOK || rec.Code == http.StatusNoContent {
			t.Errorf("%s got %d", method, rec.Code)
		}
	}
}

func TestStorageScriptIsServedToThePortalButTheAdminBannerScriptIsNot(t *testing.T) {
	h := newStorageHarness(t, 4000)
	if rec := h.do("GET", "/static/storage.js", "", "192.0.2.10", nil); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.do("GET", "/static/diskalert.js", "", "192.0.2.10", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the admin banner script must stay off the portal: %d", rec.Code)
	}
}

func TestStorageSeesAQuotaTheAdminJustSet(t *testing.T) {
	h := newStorageHarness(t, 1000)
	if h.storage(t, h.bob).Quota != nil {
		t.Fatal("bob starts without a quota")
	}
	h.portal.roles.Promote("amy")
	body := jsonBody(t, map[string]any{"bytes": 10 << 20, "confirm": amyPassword})
	if rec := h.do("PUT", "/api/admin/users/bob/quota", body, "192.0.2.10", h.amy); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if view := h.storage(t, h.bob); view.Quota == nil || *view.Quota != 10<<20 {
		t.Fatalf("got %+v", view)
	}
}
