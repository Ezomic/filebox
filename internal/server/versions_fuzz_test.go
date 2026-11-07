package server

import (
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

const outsideContents = "contents-at-the-data-root"

// FuzzVersionRoutesStayConfined sends any version id and path to every version route, for a home and for the family
// folder, and checks that nothing outside that folder's own earlier versions and live files is read or changed.
func FuzzVersionRoutesStayConfined(f *testing.F) {
	ids := []string{tuesday, monday, "", "..", "../bob/" + tuesday, "../" + deletedAmy + "/2026-10-30T0030", "2026-10-30T0030",
		"../" + system.FamilyDir + "/" + tuesday, tuesday + "/..", "2026-13-45T0030", tuesday + "%00"}
	paths := []string{"", "report.txt", "docs", "docs/notes.txt", "secret.txt", "../bob/files/secret.txt", "../../bob/" + tuesday + "/secret.txt",
		"recipe.txt", ".trash", "link", "link/secret.txt", "old.txt", "../../../outside.txt", "docs/../../x"}
	for op := range uint8(4) {
		for _, scope := range []uint8{0, fuzzFamily} {
			for _, id := range ids {
				f.Add(op|scope, id, "report.txt")
			}
			for _, path := range paths {
				f.Add(op|scope, tuesday, path)
			}
		}
	}
	f.Fuzz(func(t *testing.T, op uint8, id, path string) {
		h := newVersionsHarness(t)
		writeFile(t, filepath.Join(h.base, "outside.txt"), outsideContents)
		writeFile(t, filepath.Join(h.base, system.FamilyDir, "shared.txt"), "family-file")
		h.write(t, "bob", "secret.txt", bobSecret)
		h.write(t, "amy", "a.txt", "amy-file")
		_ = os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, system.SnapshotsDir, "amy", tuesday, "link"))
		_ = os.Symlink(filepath.Join(h.base, system.SnapshotsDir, "bob", tuesday), filepath.Join(h.base, system.SnapshotsDir, system.FamilyDir, tuesday, "link"))
		before := snapshotTree(t, h.base)

		api := "/api"
		if op&fuzzFamily != 0 {
			api = "/api/family"
		}
		escaped := url.PathEscape(id)
		var rec *httptest.ResponseRecorder
		switch op & 0x7f % 4 {
		case 0:
			rec = h.get(h.amy, api+"/versions/"+escaped+"/files", path)
		case 1:
			rec = h.get(h.amy, api+"/versions/"+escaped+"/files/download", path)
		case 2:
			rec = h.restore(h.amy, api, id, path)
		case 3:
			rec = h.call("GET", api+"/versions", "", h.amy)
		}

		body := rec.Body.String()
		if rec.Code >= 500 && runtime.GOOS != "windows" {
			t.Fatalf("op %#x %q %q gave a server error %d: %s", op, id, path, rec.Code, body)
		}
		for _, secret := range []string{bobSecret, deletedSecret, outsideContents} {
			if strings.Contains(body, secret) {
				t.Fatalf("op %#x %q %q leaked %q", op, id, path, secret)
			}
		}
		if op&fuzzFamily == 0 && (strings.Contains(body, familyEarlier) || strings.Contains(body, "family-file")) {
			t.Fatalf("op %#x %q %q reached the family folder from a home route", op, id, path)
		}
		if op&fuzzFamily != 0 && strings.Contains(body, amyTuesdayText) {
			t.Fatalf("op %#x %q %q reached amy's versions from the family view", op, id, path)
		}
		if after := snapshotTree(t, h.base); after != before {
			t.Fatalf("op %#x %q %q changed the earlier versions:\n%s\n%s", op, id, path, before, after)
		}
		if got := h.liveFile(t, "bob/files", "secret.txt"); got != bobSecret {
			t.Fatalf("op %#x %q %q changed bob's files", op, id, path)
		}
		if entries, _ := os.ReadDir(filepath.Join(h.base, "bob", "files")); len(entries) != 1 {
			t.Fatalf("op %#x %q %q wrote into bob's files: %v", op, id, path, entries)
		}
		if entries, _ := os.ReadDir(h.base); len(entries) != 5 {
			t.Fatalf("op %#x %q %q created something at the data root: %v", op, id, path, entries)
		}
		if op&fuzzFamily != 0 {
			if entries, _ := os.ReadDir(filepath.Join(h.base, "amy", "files")); len(entries) != 1 || h.liveFile(t, "amy/files", "a.txt") != "amy-file" {
				t.Fatalf("op %#x %q %q changed amy's files from the family view: %v", op, id, path, entries)
			}
		} else if h.liveFile(t, system.FamilyDir, "shared.txt") != "family-file" {
			t.Fatalf("op %#x %q %q changed the family folder from a home route", op, id, path)
		}
	})
}

// snapshotTree lists every path, type and size under the snapshots, and the contents of each file.
func snapshotTree(t *testing.T, base string) string {
	t.Helper()
	var tree strings.Builder
	root := filepath.Join(base, system.SnapshotsDir)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		tree.WriteString(path + " " + entry.Type().String())
		if entry.Type().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			tree.WriteString(" " + string(data))
		}
		tree.WriteString("\n")

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return tree.String()
}
