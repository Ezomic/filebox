package system

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

const snapshotID = "2026-11-07T0030"

func snapshotDir(t *testing.T, base, folder, id string) string {
	t.Helper()
	dir := filepath.Join(base, SnapshotsDir, folder, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	return dir
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func versionIDs(t *testing.T, versions Versions) []string {
	t.Helper()
	list, err := versions.List()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, version := range list {
		ids = append(ids, version.ID)
	}

	return ids
}

func TestVersionsListOnlySnapshotsNewestFirst(t *testing.T) {
	base := t.TempDir()
	for _, id := range []string{"2026-11-06T0030", snapshotID, "2026-10-31T0030", "junk", "2026-13-45T0030", "2026-11-07T2460"} {
		snapshotDir(t, base, "amy", id)
	}
	writeAt(t, filepath.Join(base, SnapshotsDir, "amy", "2026-11-08T0030"), "a file, not a snapshot")

	versions, err := OpenUserVersions(base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := versionIDs(t, versions), []string{snapshotID, "2026-11-06T0030", "2026-10-31T0030"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	list, _ := versions.List()
	if want := time.Date(2026, 11, 7, 0, 30, 0, 0, time.Local); !list[0].Taken.Equal(want) {
		t.Fatalf("taken %v, want %v", list[0].Taken, want)
	}
}

func TestVersionsOfAFolderWithoutSnapshotsAreAnEmptyList(t *testing.T) {
	versions, err := OpenUserVersions(t.TempDir(), "amy")
	if err != nil {
		t.Fatal(err)
	}
	if got := versionIDs(t, versions); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestVersionsOnlyOpenAUsersOwnFolder(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"amy.deleted-20261107120000", FamilyDir, "../amy", "amy/..", "", "Amy", ".snapshots"} {
		if _, err := OpenUserVersions(base, name); !errors.Is(err, ErrInvalidUsername) {
			t.Errorf("%q: got %v", name, err)
		}
	}
}

func TestVersionOpenRefusesAnythingButAListedSnapshot(t *testing.T) {
	base := t.TempDir()
	snapshotDir(t, base, "amy", snapshotID)
	writeAt(t, filepath.Join(snapshotDir(t, base, "bob", snapshotID), "secret.txt"), "bob")
	writeAt(t, filepath.Join(base, SnapshotsDir, "amy", "2026-11-08T0030"), "a file")
	versions, _ := OpenUserVersions(base, "amy")

	for _, id := range []string{"", ".", "..", "../bob/" + snapshotID, snapshotID + "/..", "2026-11-06T0030", "2026-11-08T0030", "2026-13-45T0030", snapshotID + "\x00"} {
		if files, err := versions.Open(id); !errors.Is(err, ErrNotFound) {
			closeFiles(files)
			t.Errorf("%q: got %v", id, err)
		}
	}
	files, err := versions.Open(snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	closeFiles(files)
}

func TestVersionFilesStayInsideTheirSnapshot(t *testing.T) {
	base := t.TempDir()
	dir := snapshotDir(t, base, "amy", snapshotID)
	writeAt(t, filepath.Join(snapshotDir(t, base, "bob", snapshotID), "secret.txt"), "bob")
	if err := os.Symlink(filepath.Join(base, SnapshotsDir, "bob", snapshotID), filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	versions, _ := OpenUserVersions(base, "amy")
	files, err := versions.Open(snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)

	if _, _, err := files.Open("link/secret.txt"); err == nil {
		t.Fatal("a link inside a snapshot reached another user's snapshot")
	}
	if _, err := VersionSize(files, "link"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a link is never restored, got %v", err)
	}
}

func TestFamilyVersionsLiveInTheirOwnFolder(t *testing.T) {
	base := t.TempDir()
	writeAt(t, filepath.Join(snapshotDir(t, base, FamilyDir, snapshotID), "recipe.txt"), "soup")
	snapshotDir(t, base, "amy", "2026-11-06T0030")

	versions := OpenFamilyVersions(base)
	if got := versionIDs(t, versions); !reflect.DeepEqual(got, []string{snapshotID}) {
		t.Fatalf("got %v", got)
	}
	files, err := versions.Open(snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)
	if got := read(t, files, "recipe.txt"); got != "soup" {
		t.Fatalf("got %q", got)
	}
}

func TestRestoreVersionPutsAFileBackAndTrashesWhatIsThere(t *testing.T) {
	for _, factory := range filesFactories() {
		t.Run(factory.name, func(t *testing.T) {
			live, earlier := factory.make(t), factory.make(t)
			for _, files := range []Files{live, earlier} {
				if err := files.Mkdir("docs"); err != nil {
					t.Fatal(err)
				}
			}
			put(t, live, "docs/report.txt", "overwritten")
			put(t, earlier, "docs/report.txt", "as it was")
			then := time.Date(2026, 11, 6, 15, 4, 5, 0, time.UTC)
			if err := earlier.SetModified("docs/report.txt", then); err != nil {
				t.Fatal(err)
			}

			restored, err := RestoreVersion(live, earlier, "docs/report.txt")
			if err != nil {
				t.Fatal(err)
			}
			if got := read(t, live, "docs/report.txt"); got != "as it was" {
				t.Fatalf("got %q", got)
			}
			if info, _ := live.Stat("docs/report.txt"); !info.Modified.Equal(then) {
				t.Fatalf("modified %v, want %v", info.Modified, then)
			}
			items, _ := live.ListTrash()
			if restored.Path != "docs/report.txt" || len(items) != 1 || items[0].ID != restored.TrashID || items[0].Path != "docs/report.txt" {
				t.Fatalf("restored %+v, trash %+v", restored, items)
			}
			if _, err := live.RestoreTrash(restored.TrashID); err != nil {
				t.Fatal(err)
			}
			if got := read(t, live, "docs/report (restored).txt"); got != "overwritten" {
				t.Fatalf("the replaced file must come back from the trash, got %q", got)
			}
		})
	}
}

func TestRestoreVersionBringsBackADeletedFolderAndItsParents(t *testing.T) {
	for _, factory := range filesFactories() {
		t.Run(factory.name, func(t *testing.T) {
			live, earlier := factory.make(t), factory.make(t)
			for _, dir := range []string{"a", "a/b", "a/b/c"} {
				if err := earlier.Mkdir(dir); err != nil {
					t.Fatal(err)
				}
			}
			put(t, earlier, "a/b/one.txt", "1")
			put(t, earlier, "a/b/c/two.txt", "22")

			restored, err := RestoreVersion(live, earlier, "a/b")
			if err != nil {
				t.Fatal(err)
			}
			if restored.TrashID != "" {
				t.Fatalf("nothing was in the way, got %+v", restored)
			}
			if read(t, live, "a/b/one.txt") != "1" || read(t, live, "a/b/c/two.txt") != "22" {
				t.Fatal("the folder did not come back whole")
			}
			if size, err := VersionSize(earlier, "a/b"); err != nil || size != 3 {
				t.Fatalf("size %d %v", size, err)
			}
		})
	}
}

func TestRestoreVersionReplacesAFolderWhole(t *testing.T) {
	for _, factory := range filesFactories() {
		t.Run(factory.name, func(t *testing.T) {
			live, earlier := factory.make(t), factory.make(t)
			for _, files := range []Files{live, earlier} {
				if err := files.Mkdir("photos"); err != nil {
					t.Fatal(err)
				}
			}
			put(t, live, "photos/x.jpg", "new x")
			put(t, live, "photos/y.jpg", "added later")
			put(t, earlier, "photos/x.jpg", "old x")
			put(t, earlier, "photos/z.jpg", "deleted since")

			restored, err := RestoreVersion(live, earlier, "photos")
			if err != nil {
				t.Fatal(err)
			}
			if got := names(t, live, "photos"); !reflect.DeepEqual(got, []string{"x.jpg", "z.jpg"}) {
				t.Fatalf("got %v", got)
			}
			if read(t, live, "photos/x.jpg") != "old x" {
				t.Fatal("x.jpg is not the earlier one")
			}
			items, _ := live.ListTrash()
			if len(items) != 1 || items[0].ID != restored.TrashID || !items[0].IsDir {
				t.Fatalf("the current folder must be in the trash as one item: %+v", items)
			}
		})
	}
}

func TestRestoreVersionRefusesTheRootHiddenFoldersAndMissingItems(t *testing.T) {
	live, earlier := NewFakeFiles(), NewFakeFiles()
	put(t, earlier, "a.txt", "x")
	for _, path := range []string{"", ".trash", ".uploads/x", ".thumbs", "../a.txt", "a.txt/.."} {
		if _, err := RestoreVersion(live, earlier, path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%q: got %v", path, err)
		}
	}
	if _, err := RestoreVersion(live, earlier, "b.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if entries, _ := live.List(""); len(entries) != 0 {
		t.Fatalf("a refused restore changed something: %v", entries)
	}
}

func TestRestoreVersionStopsWhereAParentIsAFile(t *testing.T) {
	live, earlier := NewFakeFiles(), NewFakeFiles()
	put(t, live, "docs", "a file now")
	if err := earlier.Mkdir("docs"); err != nil {
		t.Fatal(err)
	}
	put(t, earlier, "docs/a.txt", "x")
	if _, err := RestoreVersion(live, earlier, "docs/a.txt"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("got %v", err)
	}
	if items, _ := live.ListTrash(); len(items) != 0 || read(t, live, "docs") != "a file now" {
		t.Fatalf("nothing may move when the restore cannot happen: %+v", items)
	}
}

func TestAFailedRestoreLeavesEverythingAsItWas(t *testing.T) {
	live, earlier := NewFakeFiles(), NewFakeFiles()
	for _, files := range []Files{live, earlier} {
		if err := files.Mkdir("photos"); err != nil {
			t.Fatal(err)
		}
	}
	put(t, live, "photos/now.jpg", "12345")
	put(t, earlier, "photos/a.jpg", "0123456789")
	put(t, earlier, "photos/b.jpg", "0123456789")
	live.SetLimit(20)

	if _, err := RestoreVersion(live, earlier, "photos"); !errors.Is(err, ErrOverQuota) {
		t.Fatalf("got %v", err)
	}
	if got := names(t, live, "photos"); !reflect.DeepEqual(got, []string{"now.jpg"}) {
		t.Fatalf("got %v", got)
	}
	if items, _ := live.ListTrash(); len(items) != 0 {
		t.Fatalf("the trash must be as before: %+v", items)
	}
}

func TestRestoreVersionLeavesOutLinksAndNamesThePortalCannotUse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("names ending in a dot and symlinks need a Unix filesystem")
	}
	base := t.TempDir()
	dir := snapshotDir(t, base, "amy", snapshotID)
	writeAt(t, filepath.Join(dir, "docs", "keep.txt"), "123")
	writeAt(t, filepath.Join(dir, "docs", "odd."), "4567")
	writeAt(t, filepath.Join(dir, "docs", ".trash", "x"), "89")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "docs", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "amy", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	versions, _ := OpenUserVersions(base, "amy")
	earlier, err := versions.Open(snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(earlier)
	live, err := OpenUserFiles(base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(live)

	if size, err := VersionSize(earlier, "docs"); err != nil || size != 3 {
		t.Fatalf("size %d %v", size, err)
	}
	if _, err := RestoreVersion(live, earlier, "docs"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(base, "amy", "files", "docs"))
	if len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Fatalf("got %v", entries)
	}
}

func TestSweepsNeverEnterTheSnapshots(t *testing.T) {
	base := t.TempDir()
	dir := snapshotDir(t, base, "amy", snapshotID)
	writeAt(t, filepath.Join(dir, "beach.jpg"), "jpeg")
	old := time.Now().Add(-90 * 24 * time.Hour)
	writeAt(t, filepath.Join(dir, TrashDir, "0123456789abcdef0123456789abcdef"), "deleted long ago")
	writeAt(t, filepath.Join(dir, TrashDir, "0123456789abcdef0123456789abcdef.json"), `{"path":"x","deleted":"`+old.UTC().Format(time.RFC3339)+`"}`)
	writeAt(t, filepath.Join(dir, UploadsDir, "0123456789abcdef0123456789abcdef"), "partial")
	if err := os.Chtimes(filepath.Join(dir, UploadsDir, "0123456789abcdef0123456789abcdef"), old, old); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "beach.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	thumb := photoKey(SnapshotsDir+"/amy/"+snapshotID+"/beach.jpg", info.Size(), info.ModTime()) + ".t.jpg"
	writeAt(t, filepath.Join(base, ThumbsDir, thumb), "thumbnail")
	before := treeOf(t, filepath.Join(base, SnapshotsDir))

	now := time.Now()
	if _, err := SweepAllTrash(base, now, TrashMaxAge); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepAllUploads(base, now, UploadMaxAge); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepAllThumbs(base, now); err != nil {
		t.Fatal(err)
	}
	if after := treeOf(t, filepath.Join(base, SnapshotsDir)); !reflect.DeepEqual(after, before) {
		t.Fatalf("a sweep changed the snapshots:\n%v\n%v", before, after)
	}
	if _, err := os.Stat(filepath.Join(base, ThumbsDir, thumb)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the thumbnail sweep counted a photo inside the snapshots as live: %v", err)
	}
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		paths = append(paths, path)

		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	return paths
}
