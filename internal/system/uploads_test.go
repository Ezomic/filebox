package system

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func eachFiles(t *testing.T, run func(t *testing.T, files Files)) {
	t.Helper()
	for _, factory := range filesFactories() {
		t.Run(factory.name, func(t *testing.T) { run(t, factory.make(t)) })
	}
}

func appendAll(t *testing.T, files Files, id string, offset int64, data string) UploadInfo {
	t.Helper()
	info, err := files.AppendUpload(id, offset, int64(len(data)), strings.NewReader(data))
	if err != nil {
		t.Fatalf("append at %d: %v", offset, err)
	}

	return info
}

func newUpload(t *testing.T, files Files, path string, size int64, overwrite bool) UploadInfo {
	t.Helper()
	info, err := files.CreateUpload(path, size, overwrite)
	if err != nil {
		t.Fatalf("create upload %q: %v", path, err)
	}

	return info
}

func TestUploadInChunksLandsAtTheTarget(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if err := files.Mkdir("photos"); err != nil {
			t.Fatal(err)
		}
		info := newUpload(t, files, "photos/a.bin", 9, false)
		if len(info.ID) != 32 || info.Offset != 0 || info.Size != 9 {
			t.Fatalf("unexpected create result: %+v", info)
		}
		if got := appendAll(t, files, info.ID, 0, "abc"); got.Offset != 3 {
			t.Fatalf("offset after first chunk: %+v", got)
		}
		appendAll(t, files, info.ID, 3, "def")
		status, err := files.UploadStatus(info.ID)
		if err != nil || status.Offset != 6 || status.Path != "photos/a.bin" || status.Size != 9 {
			t.Fatalf("status: %+v %v", status, err)
		}
		if _, _, err := files.Open("photos/a.bin"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("the target must not exist before finish: %v", err)
		}
		appendAll(t, files, info.ID, 6, "ghi")
		if err := files.FinishUpload(info.ID); err != nil {
			t.Fatal(err)
		}
		if got := read(t, files, "photos/a.bin"); got != "abcdefghi" {
			t.Fatalf("content %q", got)
		}
		if _, err := files.UploadStatus(info.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("a finished upload must be gone: %v", err)
		}
	})
}

func TestUploadOffsetMustMatchAndReportsTheCurrentOne(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 10, false)
		appendAll(t, files, info.ID, 0, "abcd")

		for _, offset := range []int64{0, 2, 5, 9, 100} {
			got, err := files.AppendUpload(info.ID, offset, 1, strings.NewReader("x"))
			if !errors.Is(err, ErrUploadOffset) || got.Offset != 4 {
				t.Fatalf("offset %d: got %+v %v", offset, got, err)
			}
		}
		if status, _ := files.UploadStatus(info.ID); status.Offset != 4 {
			t.Fatalf("a refused chunk must not change the partial: %+v", status)
		}
	})
}

func TestUploadChunkBeyondTheDeclaredSizeIsRefused(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 5, false)
		appendAll(t, files, info.ID, 0, "abc")

		if _, err := files.AppendUpload(info.ID, 3, 3, strings.NewReader("def")); !errors.Is(err, ErrUploadTooLarge) {
			t.Fatalf("declared length past the size: %v", err)
		}
		if status, _ := files.UploadStatus(info.ID); status.Offset != 3 {
			t.Fatalf("offset %d", status.Offset)
		}
	})
}

func TestUploadBodyLongerThanItsRangeIsRolledBack(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 10, false)
		appendAll(t, files, info.ID, 0, "abc")

		got, err := files.AppendUpload(info.ID, 3, 2, strings.NewReader("defghij"))
		if !errors.Is(err, ErrUploadTooLarge) || got.Offset != 3 {
			t.Fatalf("got %+v %v", got, err)
		}
		if status, _ := files.UploadStatus(info.ID); status.Offset != 3 {
			t.Fatalf("overlong body must be cut back, offset %d", status.Offset)
		}
	})
}

func TestUploadShortBodyKeepsWhatArrived(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 10, false)

		got, err := files.AppendUpload(info.ID, 0, 8, strings.NewReader("abc"))
		if !errors.Is(err, ErrUploadShort) || got.Offset != 3 {
			t.Fatalf("got %+v %v", got, err)
		}
		appendAll(t, files, info.ID, 3, "defghij")
		if err := files.FinishUpload(info.ID); err != nil {
			t.Fatal(err)
		}
		if got := read(t, files, "a.bin"); got != "abcdefghij" {
			t.Fatalf("content %q", got)
		}
	})
}

func TestUploadZeroLengthChunkIsRefused(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 4, false)
		if _, err := files.AppendUpload(info.ID, 0, 0, strings.NewReader("")); err == nil {
			t.Fatal("a chunk without bytes must be refused")
		}
	})
}

func TestFinishNeedsEveryByteAndOnlyWorksOnce(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 6, false)
		appendAll(t, files, info.ID, 0, "abc")

		if err := files.FinishUpload(info.ID); !errors.Is(err, ErrUploadIncomplete) {
			t.Fatalf("finish with size mismatch: %v", err)
		}
		if _, _, err := files.Open("a.bin"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("incomplete upload must not appear: %v", err)
		}
		appendAll(t, files, info.ID, 3, "def")
		if err := files.FinishUpload(info.ID); err != nil {
			t.Fatal(err)
		}
		if err := files.FinishUpload(info.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("second finish: %v", err)
		}
		if _, err := files.AppendUpload(info.ID, 6, 1, strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("chunk after finish: %v", err)
		}
		if got := read(t, files, "a.bin"); got != "abcdef" {
			t.Fatalf("content %q", got)
		}
	})
}

func TestZeroByteUpload(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "empty.txt", 0, false)
		if err := files.FinishUpload(info.ID); err != nil {
			t.Fatal(err)
		}
		if got := read(t, files, "empty.txt"); got != "" {
			t.Fatalf("content %q", got)
		}
	})
}

func TestFinishHonoursTheOverwriteChoiceMadeAtCreate(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		keep := newUpload(t, files, "a.txt", 3, false)
		appendAll(t, files, keep.ID, 0, "new")
		put(t, files, "a.txt", "old")
		if err := files.FinishUpload(keep.ID); !errors.Is(err, ErrExists) {
			t.Fatalf("finish onto a file that appeared meanwhile: %v", err)
		}
		if got := read(t, files, "a.txt"); got != "old" {
			t.Fatalf("existing file was touched: %q", got)
		}

		replace := newUpload(t, files, "a.txt", 3, true)
		appendAll(t, files, replace.ID, 0, "new")
		if err := files.FinishUpload(replace.ID); err != nil {
			t.Fatal(err)
		}
		if got := read(t, files, "a.txt"); got != "new" {
			t.Fatalf("content %q", got)
		}
	})
}

func TestCreateUploadChecksTheTargetUpFront(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "file.txt", "x")
		if err := files.Mkdir("dir"); err != nil {
			t.Fatal(err)
		}
		cases := []struct {
			path      string
			overwrite bool
			want      error
		}{
			{"file.txt", false, ErrExists},
			{"dir", true, ErrIsDir},
			{"file.txt/child", false, ErrNotDir},
			{"missing/child", false, ErrNotFound},
			{"", false, ErrInvalidPath},
			{"../x", false, ErrInvalidPath},
			{"/etc/passwd", false, ErrInvalidPath},
			{`a\b`, false, ErrInvalidPath},
			{"a\x00b", false, ErrInvalidPath},
		}
		for _, c := range cases {
			if _, err := files.CreateUpload(c.path, 1, c.overwrite); !errors.Is(err, c.want) {
				t.Errorf("CreateUpload(%q) = %v, want %v", c.path, err, c.want)
			}
		}
		if _, err := files.CreateUpload("ok.txt", -1, false); err == nil {
			t.Error("a negative size must be refused")
		}
	})
}

func TestUploadIDsThatAreNotOurs(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		real := newUpload(t, files, "a.bin", 3, false)
		bad := []string{
			"", "x", "../" + real.ID, real.ID + "/..", strings.ToUpper(real.ID), real.ID[:31], real.ID + "0",
			strings.Repeat("0", 32), strings.Repeat("g", 32), real.ID + ".part", ".uploads/" + real.ID,
		}
		for _, id := range bad {
			if _, err := files.UploadStatus(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("UploadStatus(%q) = %v", id, err)
			}
			if _, err := files.AppendUpload(id, 0, 1, strings.NewReader("x")); !errors.Is(err, ErrNotFound) {
				t.Errorf("AppendUpload(%q) = %v", id, err)
			}
			if err := files.FinishUpload(id); !errors.Is(err, ErrNotFound) {
				t.Errorf("FinishUpload(%q) = %v", id, err)
			}
		}
	})
}

func TestConcurrentChunksAtTheSameOffsetAcceptExactlyOne(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 8, false)
		var accepted, refused atomic.Int32
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := files.AppendUpload(info.ID, 0, 4, strings.NewReader("abcd"))
				switch {
				case err == nil:
					accepted.Add(1)
				case errors.Is(err, ErrUploadOffset):
					refused.Add(1)
				default:
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()
		if accepted.Load() != 1 || refused.Load() != 15 {
			t.Fatalf("accepted %d refused %d", accepted.Load(), refused.Load())
		}
		if status, _ := files.UploadStatus(info.ID); status.Offset != 4 {
			t.Fatalf("offset %d", status.Offset)
		}
	})
}

func TestUploadsFolderIsInvisibleAndUnreachable(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		info := newUpload(t, files, "a.bin", 3, false)
		appendAll(t, files, info.ID, 0, "abc")
		put(t, files, "visible.txt", "x")

		if got := names(t, files, ""); len(got) != 1 || got[0] != "visible.txt" {
			t.Fatalf("root listing: %v", got)
		}
		for _, name := range []string{".uploads", ".UPLOADS", ".Uploads", ".uploads/" + info.ID + ".part", "a/.uploads/b", "a/.UpLoads"} {
			if _, err := CleanPath(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("CleanPath(%q) = %v", name, err)
			}
			if _, err := files.List(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("List(%q) = %v", name, err)
			}
			if _, _, err := files.Open(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Open(%q) = %v", name, err)
			}
			if err := files.Write(name, strings.NewReader("x"), true); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Write(%q) = %v", name, err)
			}
			if err := files.Mkdir(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Mkdir(%q) = %v", name, err)
			}
			if err := files.Remove(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Remove(%q) = %v", name, err)
			}
			if err := files.Rename(name, "elsewhere"); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Rename(from %q) = %v", name, err)
			}
			if err := files.Rename("visible.txt", name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Rename(to %q) = %v", name, err)
			}
			if _, err := files.Stat(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Stat(%q) = %v", name, err)
			}
			if _, err := files.CreateUpload(name, 1, true); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("CreateUpload(%q) = %v", name, err)
			}
		}
	})
}

func TestUploadsFolderIsHiddenAtAnyDepth(t *testing.T) {
	files, err := NewDirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	dir := files.(*dirFiles)
	if err := files.Mkdir("sub"); err != nil {
		t.Fatal(err)
	}
	if err := dir.root.Mkdir("sub/.uploads", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := dir.root.Mkdir(".Uploads", 0o755); err != nil {
		t.Fatal(err)
	}
	if got := names(t, files, ""); len(got) != 1 || got[0] != "sub" {
		t.Fatalf("root listing: %v", got)
	}
	if got := names(t, files, "sub"); len(got) != 0 {
		t.Fatalf("nested listing: %v", got)
	}
}

func TestSweepDeletesOnlyOldPartials(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		old := newUpload(t, files, "old.bin", 3, false)
		appendAll(t, files, old.ID, 0, "abc")
		before := time.Now().Add(-time.Minute)

		if n, err := files.SweepUploads(before); err != nil || n != 0 {
			t.Fatalf("fresh partial swept: %d %v", n, err)
		}
		if _, err := files.UploadStatus(old.ID); err != nil {
			t.Fatalf("fresh partial lost: %v", err)
		}

		if n, err := files.SweepUploads(time.Now().Add(time.Minute)); err != nil || n != 1 {
			t.Fatalf("old partial not swept: %d %v", n, err)
		}
		if _, err := files.UploadStatus(old.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("swept partial still there: %v", err)
		}
		if n, err := files.SweepUploads(time.Now().Add(time.Minute)); err != nil || n != 0 {
			t.Fatalf("sweeping nothing: %d %v", n, err)
		}
	})
}

func TestSweepKeepsAnActiveUploadWhoseMetadataIsOld(t *testing.T) {
	files, err := NewDirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	dir := files.(*dirFiles)
	info := newUpload(t, files, "a.bin", 6, false)
	appendAll(t, files, info.ID, 0, "abc")
	ancient := time.Now().Add(-30 * 24 * time.Hour)
	if err := dir.root.Chtimes(".uploads/"+info.ID+".json", ancient, ancient); err != nil {
		t.Fatal(err)
	}
	if n, err := files.SweepUploads(time.Now().Add(-7 * 24 * time.Hour)); err != nil || n != 0 {
		t.Fatalf("active upload swept: %d %v", n, err)
	}
	if _, err := files.UploadStatus(info.ID); err != nil {
		t.Fatalf("active upload lost: %v", err)
	}

	if err := dir.root.Chtimes(".uploads/"+info.ID+".part", ancient, ancient); err != nil {
		t.Fatal(err)
	}
	if n, err := files.SweepUploads(time.Now().Add(-7 * 24 * time.Hour)); err != nil || n != 1 {
		t.Fatalf("stale upload kept: %d %v", n, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir.root.Name(), ".uploads")); len(entries) != 0 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestSweepLeavesUnknownFilesAlone(t *testing.T) {
	files, err := NewDirFiles(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	dir := files.(*dirFiles)
	newUpload(t, files, "a.bin", 1, false)
	stray := filepath.Join(dir.root.Name(), ".uploads", "notes.txt")
	if err := os.WriteFile(stray, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := files.SweepUploads(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("sweep deleted a file it did not create: %v", err)
	}
}

func TestSweepWithoutAnUploadsFolderIsANoop(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if n, err := files.SweepUploads(time.Now().Add(time.Hour)); err != nil || n != 0 {
			t.Fatalf("%d %v", n, err)
		}
	})
}

func TestSweepAllUploadsReachesEveryUserAndUsesTheGivenClock(t *testing.T) {
	base := t.TempDir()
	for _, user := range []string{"amy", "bob"} {
		if err := os.MkdirAll(filepath.Join(base, user, "files"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "not a user!", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	admin, err := NewDirFiles(base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.(interface{ Close() error }).Close()
	ids := map[string]UploadInfo{"admin": newUpload(t, admin, "a.bin", 1, false)}
	for _, user := range []string{"amy", "bob"} {
		files, err := OpenUserFiles(base, user)
		if err != nil {
			t.Fatal(err)
		}
		ids[user] = newUpload(t, files, "a.bin", 1, false)
		_ = files.(interface{ Close() error }).Close()
	}

	now := time.Now()
	if n, err := SweepAllUploads(base, now.Add(6*24*time.Hour), UploadMaxAge); err != nil || n != 0 {
		t.Fatalf("six days old must survive: %d %v", n, err)
	}
	if n, err := SweepAllUploads(base, now.Add(8*24*time.Hour), UploadMaxAge); err != nil || n != 3 {
		t.Fatalf("eight days old must go: %d %v", n, err)
	}
	if _, err := admin.UploadStatus(ids["admin"].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("admin partial survived: %v", err)
	}
}

func TestStat(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.txt", "four")
		if err := files.Mkdir("d"); err != nil {
			t.Fatal(err)
		}
		if info, err := files.Stat("a.txt"); err != nil || info.IsDir || info.Size != 4 {
			t.Fatalf("file: %+v %v", info, err)
		}
		if info, err := files.Stat("d"); err != nil || !info.IsDir {
			t.Fatalf("dir: %+v %v", info, err)
		}
		if _, err := files.Stat("nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if _, err := files.Stat("a.txt/child"); !errors.Is(err, ErrNotDir) && !errors.Is(err, ErrNotFound) {
			t.Fatalf("under a file: %v", err)
		}
		if _, err := files.Stat("../x"); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("traversal: %v", err)
		}
	})
}

func TestListFlagsSymlinksSoTheyCanBeSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	entries, err := files.List("")
	if err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, entry := range entries {
		flags[entry.Name] = entry.Symlink
	}
	if flags["link.txt"] != true || flags["real.txt"] != false {
		t.Fatalf("symlink flags: %v", flags)
	}
}

func TestUploadSweeperRunsAtStartupAndOnEveryTick(t *testing.T) {
	base := t.TempDir()
	admin, err := NewDirFiles(base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.(interface{ Close() error }).Close()
	fresh := newUpload(t, admin, "fresh.bin", 1, false)

	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunUploadSweeper(ctx, base, 10*time.Millisecond, now)
		close(done)
	}()

	time.Sleep(60 * time.Millisecond)
	if _, err := admin.UploadStatus(fresh.ID); err != nil {
		t.Fatalf("a fresh partial must survive the sweeps: %v", err)
	}
	offset.Store(int64(8 * 24 * time.Hour))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := admin.UploadStatus(fresh.ID); errors.Is(err, ErrNotFound) {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("the stale partial was never swept")
}

func TestVisitorUploadsNeverOverwriteAndAreSweptOnTheirOwn(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "taken.txt", "mine")
		if _, err := files.CreateVisitorUpload("taken.txt", 3); !errors.Is(err, ErrExists) {
			t.Fatalf("a visitor upload must not start over an existing file: %v", err)
		}
		late, err := files.CreateVisitorUpload("late.txt", 3)
		if err != nil || !late.Visitor || late.Overwrite {
			t.Fatalf("got %+v %v", late, err)
		}
		appendAll(t, files, late.ID, 0, "new")
		put(t, files, "late.txt", "owner")
		if err := files.FinishUpload(late.ID); !errors.Is(err, ErrExists) {
			t.Fatalf("a visitor upload must not finish over a file that appeared meanwhile: %v", err)
		}
		if info, err := files.Stat("late.txt"); err != nil || info.Size != int64(len("owner")) {
			t.Fatalf("the owner's file changed: %+v %v", info, err)
		}

		visitor, err := files.CreateVisitorUpload("guest.bin", 3)
		if err != nil {
			t.Fatal(err)
		}
		if status, err := files.UploadStatus(visitor.ID); err != nil || !status.Visitor {
			t.Fatalf("got %+v %v", status, err)
		}
		owner := newUpload(t, files, "own.bin", 3, false)
		if status, _ := files.UploadStatus(owner.ID); status.Visitor {
			t.Fatal("the owner's own upload is no visitor upload")
		}

		if n, err := files.SweepVisitorUploads(time.Now().Add(-time.Minute)); err != nil || n != 0 {
			t.Fatalf("fresh visitor uploads swept: %d %v", n, err)
		}
		if n, err := files.SweepVisitorUploads(time.Now().Add(time.Minute)); err != nil || n != 2 {
			t.Fatalf("idle visitor uploads not swept: %d %v", n, err)
		}
		if _, err := files.UploadStatus(visitor.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("visitor upload survived: %v", err)
		}
		if _, err := files.UploadStatus(owner.ID); err != nil {
			t.Fatalf("the visitor sweep must leave the owner's own upload: %v", err)
		}
	})
}

func TestSweepAllVisitorUploadsWaitsADay(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "amy", "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := OpenUserFiles(base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer files.(interface{ Close() error }).Close()
	visitor, err := files.CreateVisitorUpload("guest.bin", 1)
	if err != nil {
		t.Fatal(err)
	}
	owner := newUpload(t, files, "own.bin", 1, false)

	now := time.Now()
	if n, err := SweepAllVisitorUploads(base, now.Add(23*time.Hour)); err != nil || n != 0 {
		t.Fatalf("23 hours idle must survive: %d %v", n, err)
	}
	if n, err := SweepAllVisitorUploads(base, now.Add(25*time.Hour)); err != nil || n != 1 {
		t.Fatalf("25 hours idle must go: %d %v", n, err)
	}
	if _, err := files.UploadStatus(visitor.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("visitor upload survived: %v", err)
	}
	if _, err := files.UploadStatus(owner.ID); err != nil {
		t.Fatalf("the owner's upload keeps its week: %v", err)
	}
}
