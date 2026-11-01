package system

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func familyBase(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, dir := range []string{FamilyDir, "amy/files", "bob/files"} {
		if err := os.MkdirAll(filepath.Join(base, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	return base
}

func openFamily(t *testing.T, base string) Files {
	t.Helper()
	files, err := OpenFamilyFiles(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = files.(io.Closer).Close() })

	return files
}

func TestFamilyDirCanNeverBeAUsername(t *testing.T) {
	if ValidUsername(FamilyDir) {
		t.Fatalf("%q must not be a valid username", FamilyDir)
	}
	for _, name := range []string{"_family", "_Family", "_family/x", "..", "."} {
		if ValidUsername(name) {
			t.Errorf("%q must not be valid", name)
		}
	}
}

func TestOpenFamilyFilesLivesNextToTheHomesNotInThem(t *testing.T) {
	base := familyBase(t)
	family := openFamily(t, base)
	put(t, family, "photo.txt", "shared")

	if data, err := os.ReadFile(filepath.Join(base, FamilyDir, "photo.txt")); err != nil || string(data) != "shared" {
		t.Fatalf("family file is not at the data root: %q %v", data, err)
	}
	for _, user := range []string{"amy", "bob"} {
		if entries, _ := os.ReadDir(filepath.Join(base, user, "files")); len(entries) != 0 {
			t.Fatalf("%s home was touched: %v", user, entries)
		}
		home, err := OpenUserFiles(base, user)
		if err != nil {
			t.Fatal(err)
		}
		listing, err := home.List("")
		_ = home.(io.Closer).Close()
		if err != nil || len(listing) != 0 {
			t.Fatalf("%s sees %v %v", user, listing, err)
		}
	}
}

func TestOpenFamilyFilesNeedsTheFolderToExist(t *testing.T) {
	if _, err := OpenFamilyFiles(t.TempDir()); err == nil {
		t.Fatal("a missing family folder must not be created behind the owner's back")
	}
}

func TestFamilyRootCannotBeRemovedRenamedOrTrashed(t *testing.T) {
	family := openFamily(t, familyBase(t))
	for _, path := range []string{"", ".", "/", "./"} {
		if err := family.Remove(path); err == nil {
			t.Errorf("Remove(%q) worked", path)
		}
		if _, err := family.Trash(path); err == nil {
			t.Errorf("Trash(%q) worked", path)
		}
		if err := family.Rename(path, "x"); err == nil {
			t.Errorf("Rename(%q) worked", path)
		}
	}
}

func TestFamilyHiddenFoldersAreRefusedInAnyCaseAndPosition(t *testing.T) {
	family := openFamily(t, familyBase(t))
	for _, name := range []string{".trash", ".TRASH", ".Uploads", ".uploads", ".thumbs", ".ThUmBs", "a/.trash", "a/.uploads/x", ".thumbs/x"} {
		if err := family.Mkdir(name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Mkdir(%q) = %v", name, err)
		}
		if err := family.Write(name, strings.NewReader("x"), true); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Write(%q) = %v", name, err)
		}
		if _, err := family.List(name); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("List(%q) = %v", name, err)
		}
	}
}

func TestFamilyKeepsItsOwnTrashApartFromEveryHome(t *testing.T) {
	base := familyBase(t)
	family := openFamily(t, base)
	home, err := OpenUserFiles(base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer home.(io.Closer).Close()

	put(t, family, "shared.txt", "f")
	put(t, home, "mine.txt", "m")
	familyItem := mustTrash(t, family, "shared.txt")
	homeItem := mustTrash(t, home, "mine.txt")

	if items, _ := family.ListTrash(); len(items) != 1 || items[0].ID != familyItem.ID {
		t.Fatalf("family trash: %v", items)
	}
	if items, _ := home.ListTrash(); len(items) != 1 || items[0].ID != homeItem.ID {
		t.Fatalf("home trash: %v", items)
	}
	if _, err := family.RestoreTrash(homeItem.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("family restored a home item: %v", err)
	}
	if err := home.PurgeTrash(familyItem.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("home purged a family item: %v", err)
	}
	if path, err := family.RestoreTrash(familyItem.ID); err != nil || path != "shared.txt" {
		t.Fatalf("restore: %q %v", path, err)
	}
}

func TestSweepersAlsoCoverTheFamilyRoot(t *testing.T) {
	base := familyBase(t)
	family := openFamily(t, base)
	put(t, family, "old.txt", "x")
	mustTrash(t, family, "old.txt")
	partial := newUpload(t, family, "big.bin", 1, false)
	if err := family.WriteThumb(strings.Repeat("ab", 16)+".t.jpg", []byte("orphan")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	if n, err := SweepAllTrash(base, now.Add(29*24*time.Hour), TrashMaxAge); err != nil || n != 0 {
		t.Fatalf("29 day old family trash must survive: %d %v", n, err)
	}
	if n, err := SweepAllTrash(base, now.Add(31*24*time.Hour), TrashMaxAge); err != nil || n != 1 {
		t.Fatalf("31 day old family trash must go: %d %v", n, err)
	}
	if n, err := SweepAllUploads(base, now.Add(6*24*time.Hour), UploadMaxAge); err != nil || n != 0 {
		t.Fatalf("six day old family partial must survive: %d %v", n, err)
	}
	if n, err := SweepAllUploads(base, now.Add(8*24*time.Hour), UploadMaxAge); err != nil || n != 1 {
		t.Fatalf("eight day old family partial must go: %d %v", n, err)
	}
	if _, err := family.UploadStatus(partial.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("family partial survived: %v", err)
	}
	if n, err := SweepAllThumbs(base, now); err != nil || n != 1 {
		t.Fatalf("family orphan thumbnail must go: %d %v", n, err)
	}
}

func TestSweepersLeaveFamilyFilesAndOtherRootsAlone(t *testing.T) {
	base := familyBase(t)
	family := openFamily(t, base)
	home, err := OpenUserFiles(base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer home.(io.Closer).Close()
	put(t, family, "keep.txt", "f")
	put(t, home, "mine.txt", "m")
	late := time.Now().Add(400 * 24 * time.Hour)

	if _, err := SweepAllTrash(base, late, TrashMaxAge); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepAllUploads(base, late, UploadMaxAge); err != nil {
		t.Fatal(err)
	}
	if _, err := SweepAllThumbs(base, late); err != nil {
		t.Fatal(err)
	}
	for name, files := range map[string]Files{"keep.txt": family, "mine.txt": home} {
		if _, err := files.Stat(name); err != nil {
			t.Errorf("%s was swept: %v", name, err)
		}
	}
}

func TestSweepersSkipAFamilyFolderThatDoesNotExistYet(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "amy", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if n, err := SweepAllTrash(base, time.Now(), TrashMaxAge); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(base, FamilyDir)); !os.IsNotExist(err) {
		t.Fatalf("a sweep must not create the family folder: %v", err)
	}
}

func TestUsageNeverCountsTheFamilyFolder(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "alice", "files", "a.bin"), 10)
	before := (&dirUsage{root: root}).scan()

	writeSized(t, filepath.Join(root, FamilyDir, "big.bin"), 5000)
	writeSized(t, filepath.Join(root, FamilyDir, "files", "big.bin"), 5000)
	after := (&dirUsage{root: root}).scan()

	if len(after) != 1 || after["alice"] != 10 || before["alice"] != after["alice"] {
		t.Fatalf("before %v, after %v", before, after)
	}
	if _, ok := after[FamilyDir]; ok {
		t.Fatal("the family folder showed up as a user")
	}
}
