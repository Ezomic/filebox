package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustTrash(t *testing.T, files Files, path string) TrashItem {
	t.Helper()
	item, err := files.Trash(path)
	if err != nil {
		t.Fatalf("trash %q: %v", path, err)
	}

	return item
}

func mustRestore(t *testing.T, files Files, id string) string {
	t.Helper()
	restored, err := files.RestoreTrash(id)
	if err != nil {
		t.Fatalf("restore %s: %v", id, err)
	}

	return restored
}

func TestTrashMovesAFileOutOfTheListingAndKeepsItsDetails(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "hello")
		before := time.Now()
		item := mustTrash(t, files, "a.txt")

		if !uploadIDPattern.MatchString(item.ID) || item.Path != "a.txt" || item.Size != 5 || item.IsDir || item.Name() != "a.txt" {
			t.Fatalf("item: %+v", item)
		}
		if item.DeletedAt.Before(before.Add(-time.Second)) || item.DeletedAt.After(time.Now().Add(time.Second)) {
			t.Fatalf("deleted time: %v", item.DeletedAt)
		}
		if got := names(t, files, ""); len(got) != 0 {
			t.Fatalf("listing still shows %v", got)
		}
		if _, err := files.Stat("a.txt"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("stat: %v", err)
		}
		items, err := files.ListTrash()
		if err != nil || len(items) != 1 || items[0].ID != item.ID || items[0].Size != 5 {
			t.Fatalf("trash list: %+v %v", items, err)
		}
	})
}

func TestTrashFolderIsOneItemAndRestoresWhole(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if err := files.Mkdir("d"); err != nil {
			t.Fatal(err)
		}
		if err := files.Mkdir("d/sub"); err != nil {
			t.Fatal(err)
		}
		put(t, files, "d/one.txt", "12345")
		put(t, files, "d/sub/two.txt", "123")

		item := mustTrash(t, files, "d")
		if !item.IsDir || item.Size != 8 || item.Path != "d" {
			t.Fatalf("item: %+v", item)
		}
		if items, _ := files.ListTrash(); len(items) != 1 {
			t.Fatalf("a folder must be a single item: %+v", items)
		}
		if got := mustRestore(t, files, item.ID); got != "d" {
			t.Fatalf("restored to %q", got)
		}
		if read(t, files, "d/one.txt") != "12345" || read(t, files, "d/sub/two.txt") != "123" {
			t.Fatal("contents changed")
		}
	})
}

func TestTrashRefusesRootMissingAndHiddenNames(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if _, err := files.Trash(""); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("root: %v", err)
		}
		if _, err := files.Trash("nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if _, err := files.Trash("a/b/c"); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNotDir) {
			t.Fatalf("missing parent: %v", err)
		}
		for _, bad := range []string{".trash", ".TRASH", ".Trash/x", "a/.trash", "a/.TrAsH/b", ".uploads", "../x"} {
			if _, err := files.Trash(bad); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Trash(%q): %v", bad, err)
			}
		}
	})
}

func TestTrashFolderIsHiddenFromEveryFileOperation(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "x")
		item := mustTrash(t, files, "a.txt")

		for _, name := range []string{".trash", ".TRASH", ".Trash", "x/.trash", ".trash/" + item.ID, ".TRASH/" + item.ID + ".json"} {
			if _, err := files.List(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("List(%q): %v", name, err)
			}
			if _, _, err := files.Open(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Open(%q): %v", name, err)
			}
			if _, err := files.Stat(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Stat(%q): %v", name, err)
			}
			if err := files.Write(name, strings.NewReader("x"), true); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Write(%q): %v", name, err)
			}
			if err := files.Mkdir(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Mkdir(%q): %v", name, err)
			}
			if err := files.Remove(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Remove(%q): %v", name, err)
			}
			if err := files.Rename(name, "elsewhere"); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Rename from %q: %v", name, err)
			}
			if err := files.Rename("a.txt", name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Rename to %q: %v", name, err)
			}
			if _, err := files.CreateUpload(name, 1, false); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("CreateUpload(%q): %v", name, err)
			}
		}
		if got := names(t, files, ""); len(got) != 0 {
			t.Fatalf("root listing shows %v", got)
		}
	})
}

func TestRestoreRecreatesMissingParents(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if err := files.Mkdir("a"); err != nil {
			t.Fatal(err)
		}
		if err := files.Mkdir("a/b"); err != nil {
			t.Fatal(err)
		}
		put(t, files, "a/b/f.txt", "deep")
		file := mustTrash(t, files, "a/b/f.txt")
		parent := mustTrash(t, files, "a")

		if got := mustRestore(t, files, file.ID); got != "a/b/f.txt" {
			t.Fatalf("restored to %q", got)
		}
		if read(t, files, "a/b/f.txt") != "deep" {
			t.Fatal("content lost")
		}
		if got := mustRestore(t, files, parent.ID); got != "a (restored)" {
			t.Fatalf("the folder now exists, so the old one must be renamed, got %q", got)
		}
	})
}

func TestRestoreNeverOverwritesAndNumbersClashes(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		var restored []string
		for i := range 3 {
			put(t, files, "a.txt", string(rune('A'+i)))
			item := mustTrash(t, files, "a.txt")
			put(t, files, "a.txt", "current")
			restored = append(restored, mustRestore(t, files, item.ID))
			if err := files.Remove("a.txt"); err != nil {
				t.Fatal(err)
			}
		}
		if restored[0] != "a (restored).txt" || restored[1] != "a (restored 2).txt" || restored[2] != "a (restored 3).txt" {
			t.Fatalf("names: %v", restored)
		}
		for i, name := range restored {
			if got := read(t, files, name); got != string(rune('A'+i)) {
				t.Fatalf("%s holds %q", name, got)
			}
		}
	})
}

func TestRestoreClashNamesForFoldersDotfilesAndMultipleExtensions(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		cases := map[string]string{
			"folder":         "folder (restored)",
			".bashrc":        ".bashrc (restored)",
			"archive.tar.gz": "archive.tar (restored).gz",
			"noext":          "noext (restored)",
		}
		for name, want := range cases {
			if name == "folder" {
				if err := files.Mkdir(name); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, files, name, "old")
			}
			item := mustTrash(t, files, name)
			if name == "folder" {
				if err := files.Mkdir(name); err != nil {
					t.Fatal(err)
				}
			} else {
				put(t, files, name, "new")
			}
			if got := mustRestore(t, files, item.ID); got != want {
				t.Errorf("%q restored as %q, want %q", name, got, want)
			}
		}
	})
}

func TestRestoreWithALongNameStillFitsTheLimit(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		for _, name := range []string{
			strings.Repeat("a", 255),
			strings.Repeat("a", 251) + ".txt",
			strings.Repeat("é", 126) + ".md",
			"x." + strings.Repeat("e", 252),
		} {
			put(t, files, name, "old")
			item := mustTrash(t, files, name)
			put(t, files, name, "new")
			got := mustRestore(t, files, item.ID)
			if len(got) > 255 || got == name || !strings.Contains(got, "(restored") {
				t.Errorf("%d byte name restored as %d bytes: %q", len(name), len(got), got)
			}
			if read(t, files, got) != "old" || read(t, files, name) != "new" {
				t.Errorf("%d byte name: contents mixed up", len(name))
			}
			_ = files.Remove(got)
			_ = files.Remove(name)
		}
	})
}

func TestRestoreHandlesAwkwardNames(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		for _, name := range []string{"my file (1).txt", "日本語.txt", ".hidden", "emoji 😀.png", "-dash"} {
			put(t, files, name, "x")
			item := mustTrash(t, files, name)
			if got := mustRestore(t, files, item.ID); got != name {
				t.Errorf("%q restored as %q", name, got)
			}
		}
	})
}

func TestRestoreFailsCleanlyWhenAParentBecameAFile(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if err := files.Mkdir("d"); err != nil {
			t.Fatal(err)
		}
		put(t, files, "d/f.txt", "x")
		item := mustTrash(t, files, "d/f.txt")
		if err := files.Remove("d"); err != nil {
			t.Fatal(err)
		}
		put(t, files, "d", "now a file")

		if _, err := files.RestoreTrash(item.ID); !errors.Is(err, ErrNotDir) {
			t.Fatalf("got %v", err)
		}
		if read(t, files, "d") != "now a file" {
			t.Fatal("the file in the way was touched")
		}
		if items, _ := files.ListTrash(); len(items) != 1 {
			t.Fatalf("the item must stay in the trash: %+v", items)
		}
	})
}

func TestRestoreTwiceAndUnknownIDs(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "x")
		item := mustTrash(t, files, "a.txt")
		mustRestore(t, files, item.ID)
		if _, err := files.RestoreTrash(item.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second restore: %v", err)
		}
		for _, id := range []string{"", "nope", strings.Repeat("0", 32), strings.ToUpper(item.ID), "../" + item.ID, item.ID + ".json", item.ID + "/x", item.ID + "\x00"} {
			if _, err := files.RestoreTrash(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("RestoreTrash(%q): %v", id, err)
			}
			if err := files.PurgeTrash(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("PurgeTrash(%q): %v", id, err)
			}
		}
	})
}

func TestPurgeAndEmpty(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if n, err := files.EmptyTrash(); err != nil || n != 0 {
			t.Fatalf("empty with nothing in it: %d %v", n, err)
		}
		if items, err := files.ListTrash(); err != nil || len(items) != 0 {
			t.Fatalf("list of nothing: %+v %v", items, err)
		}
		put(t, files, "a.txt", "x")
		put(t, files, "b.txt", "y")
		put(t, files, "c.txt", "z")
		a := mustTrash(t, files, "a.txt")
		mustTrash(t, files, "b.txt")
		mustTrash(t, files, "c.txt")

		if err := files.PurgeTrash(a.ID); err != nil {
			t.Fatal(err)
		}
		if err := files.PurgeTrash(a.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("purge twice: %v", err)
		}
		if n, err := files.EmptyTrash(); err != nil || n != 2 {
			t.Fatalf("empty: %d %v", n, err)
		}
		if items, _ := files.ListTrash(); len(items) != 0 {
			t.Fatalf("left over: %+v", items)
		}
	})
}

func TestTrashedNameCanBeReusedImmediately(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "old")
		mustTrash(t, files, "a.txt")
		put(t, files, "a.txt", "new")
		second := mustTrash(t, files, "a.txt")
		items, _ := files.ListTrash()
		if len(items) != 2 || items[0].ID == items[1].ID {
			t.Fatalf("items: %+v", items)
		}
		if got := mustRestore(t, files, second.ID); got != "a.txt" || read(t, files, "a.txt") != "new" {
			t.Fatalf("restored %q", got)
		}
	})
}

func TestSweepTrashKeepsFreshItemsAndRemovesOldOnes(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "x")
		item := mustTrash(t, files, "a.txt")

		if n, err := files.SweepTrash(time.Now().Add(-time.Hour)); err != nil || n != 0 {
			t.Fatalf("fresh item swept: %d %v", n, err)
		}
		if items, _ := files.ListTrash(); len(items) != 1 {
			t.Fatal("fresh item vanished")
		}
		if n, err := files.SweepTrash(time.Now().Add(time.Minute)); err != nil || n != 1 {
			t.Fatalf("old item kept: %d %v", n, err)
		}
		if _, err := files.RestoreTrash(item.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("restore after aging out: %v", err)
		}
		if n, err := files.SweepTrash(time.Now().Add(time.Minute)); err != nil || n != 0 {
			t.Fatalf("second sweep: %d %v", n, err)
		}
	})
}

func TestSweepTrashWithoutATrashFolderIsANoop(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if n, err := files.SweepTrash(time.Now().Add(time.Hour)); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
	})
}

func TestConcurrentRestoresOfOneItemSucceedOnce(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "x")
		item := mustTrash(t, files, "a.txt")

		var wg sync.WaitGroup
		var ok, gone atomic.Int32
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch _, err := files.RestoreTrash(item.ID); {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrNotFound):
					gone.Add(1)
				default:
					t.Errorf("restore: %v", err)
				}
			}()
		}
		wg.Wait()
		if ok.Load() != 1 || gone.Load() != 7 {
			t.Fatalf("%d restored, %d not found", ok.Load(), gone.Load())
		}
		if got := names(t, files, ""); !slices.Equal(got, []string{"a.txt"}) {
			t.Fatalf("listing: %v", got)
		}
	})
}

func TestConcurrentRestoreAndPurgeOfOneItemLeaveOneOutcome(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		for range 20 {
			put(t, files, "a.txt", "x")
			item := mustTrash(t, files, "a.txt")
			var wg sync.WaitGroup
			var restored, purged atomic.Int32
			wg.Add(2)
			go func() {
				defer wg.Done()
				if _, err := files.RestoreTrash(item.ID); err == nil {
					restored.Add(1)
				}
			}()
			go func() {
				defer wg.Done()
				if err := files.PurgeTrash(item.ID); err == nil {
					purged.Add(1)
				}
			}()
			wg.Wait()
			if restored.Load()+purged.Load() != 1 {
				t.Fatalf("restored %d purged %d", restored.Load(), purged.Load())
			}
			if items, _ := files.ListTrash(); len(items) != 0 {
				t.Fatalf("left over: %+v", items)
			}
			_ = files.Remove("a.txt")
		}
	})
}

func TestTrashingAHugeFolderIsOneAtomicMove(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if err := files.Mkdir("big"); err != nil {
			t.Fatal(err)
		}
		for i := range 30 {
			dir := "big/d" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			if err := files.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			for j := range 40 {
				put(t, files, dir+"/f"+string(rune('a'+j%26))+string(rune('a'+j/26)), "1234")
			}
		}
		item := mustTrash(t, files, "big")
		if item.Size != 30*40*4 {
			t.Fatalf("size %d", item.Size)
		}
		mustRestore(t, files, item.ID)
		if info, err := files.Stat("big/daa/faa"); err != nil || info.Size != 4 {
			t.Fatalf("%+v %v", info, err)
		}
	})
}

func TestTrashMovesASymlinkAndNeverFollowsIt(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
	files, err := NewDirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()

	item := mustTrash(t, files, "link")
	if item.Size > 1024 {
		t.Fatalf("a link must not be sized by what it points at: %d", item.Size)
	}
	if data, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(data) != "secret" {
		t.Fatalf("the target was touched: %q %v", data, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link still there: %v", err)
	}
	mustRestore(t, files, item.ID)
	if target, err := os.Readlink(filepath.Join(root, "link")); err != nil || target != outside {
		t.Fatalf("link restored as %q %v", target, err)
	}
}

func TestDirFilesIgnoresTamperedTrashEntries(t *testing.T) {
	root := t.TempDir()
	files, err := NewDirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	put(t, files, "a.txt", "x")
	item := mustTrash(t, files, "a.txt")

	meta := filepath.Join(root, TrashDir, item.ID+".json")
	if err := os.WriteFile(meta, []byte(`{"path":"../escape.txt","deleted":"2020-01-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := files.RestoreTrash(item.ID); err == nil {
		t.Fatal("a tampered path must not restore")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); err == nil {
		t.Fatal("restore wrote outside the root")
	}
	if err := os.WriteFile(meta, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ListTrash(); err != nil {
		t.Fatalf("list must survive a broken entry: %v", err)
	}
	if err := files.PurgeTrash(item.ID); err != nil {
		t.Fatalf("a broken entry must still be deletable: %v", err)
	}
}

func TestSweepTrashRemovesBrokenEntriesByTheirAge(t *testing.T) {
	root := t.TempDir()
	files, err := NewDirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	put(t, files, "a.txt", "x")
	item := mustTrash(t, files, "a.txt")
	if err := os.WriteFile(filepath.Join(root, TrashDir, item.ID+".json"), []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	if n, err := files.SweepTrash(time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("fresh broken entry swept: %d %v", n, err)
	}
	if n, err := files.SweepTrash(time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("old broken entry kept: %d %v", n, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, TrashDir)); len(entries) != 0 {
		t.Fatalf("left over: %v", entries)
	}
}

func TestSweepAllTrashReachesEveryUserAndUsesTheGivenClock(t *testing.T) {
	base := t.TempDir()
	for _, user := range []string{"amy", "bob"} {
		if err := os.MkdirAll(filepath.Join(base, user, "files"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := NewDirFiles(base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.(interface{ Close() error }).Close()
	put(t, admin, "x.txt", "x")
	adminItem := mustTrash(t, admin, "x.txt")
	for _, user := range []string{"amy", "bob"} {
		files, err := OpenUserFiles(base, user)
		if err != nil {
			t.Fatal(err)
		}
		put(t, files, "a.txt", "x")
		mustTrash(t, files, "a.txt")
		_ = files.(interface{ Close() error }).Close()
	}

	now := time.Now()
	if n, err := SweepAllTrash(base, now.Add(29*24*time.Hour), TrashMaxAge); err != nil || n != 0 {
		t.Fatalf("29 days old must survive: %d %v", n, err)
	}
	if n, err := SweepAllTrash(base, now.Add(31*24*time.Hour), TrashMaxAge); err != nil || n != 3 {
		t.Fatalf("31 days old must go: %d %v", n, err)
	}
	if _, err := admin.RestoreTrash(adminItem.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin item survived: %v", err)
	}
}

func TestTrashSweeperRunsAtStartupAndOnEveryTick(t *testing.T) {
	base := t.TempDir()
	admin, err := NewDirFiles(base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.(interface{ Close() error }).Close()
	put(t, admin, "x.txt", "x")
	fresh := mustTrash(t, admin, "x.txt")

	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunTrashSweeper(ctx, base, 10*time.Millisecond, now)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	if items, _ := admin.ListTrash(); len(items) != 1 || items[0].ID != fresh.ID {
		t.Fatalf("a fresh item must survive the sweeps: %+v", items)
	}
	offset.Store(int64(31 * 24 * time.Hour))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if items, _ := admin.ListTrash(); len(items) == 0 {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("the old item was never swept")
}

func TestTrashCountsInTheSameRootSoFilesStayAtomic(t *testing.T) {
	root := t.TempDir()
	files, err := NewDirFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	put(t, files, "a.txt", "x")
	item := mustTrash(t, files, "a.txt")

	if _, err := os.Stat(filepath.Join(root, TrashDir, item.ID)); err != nil {
		t.Fatalf("item not stored as .trash/<id>: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, TrashDir, item.ID+".json")); err != nil {
		t.Fatalf("metadata missing: %v", err)
	}
	entries, err := files.List("")
	if err != nil || len(entries) != 0 {
		t.Fatalf("listing: %v %v", entries, err)
	}
}
