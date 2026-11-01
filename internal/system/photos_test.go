package system

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func setTime(t *testing.T, files Files, name string, when time.Time) {
	t.Helper()
	switch f := files.(type) {
	case *FakeFiles:
		f.SetModified(name, when)
	case *dirFiles:
		if err := os.Chtimes(filepath.Join(f.root.Name(), filepath.FromSlash(name)), when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func photoPaths(photos []Photo) []string {
	paths := make([]string, len(photos))
	for i, photo := range photos {
		paths[i] = photo.Path
	}

	return paths
}

func listAll(t *testing.T, files Files, pageSize int) []Photo {
	t.Helper()
	var all []Photo
	cursor := ""
	for range 1000 {
		page, err := files.ListPhotos(cursor, pageSize, "")
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Photos...)
		if page.Next == "" {
			return all
		}
		cursor = page.Next
	}
	t.Fatal("paging never ended")

	return nil
}

func TestListPhotosFindsMediaNewestFirst(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		for _, name := range []string{"docs", "trip", "trip/day1"} {
			if err := files.Mkdir(name); err != nil {
				t.Fatal(err)
			}
		}
		for _, name := range []string{"old.jpg", "trip/new.HEIC", "trip/day1/clip.mp4", "mid.png", "notes.txt", "trip/day1/readme.md", "docs/scan.PDF", "noext", ".jpg"} {
			put(t, files, name, "x")
		}
		put(t, files, "empty.jpg", "")
		setTime(t, files, "old.jpg", time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC))
		setTime(t, files, "mid.png", time.Date(2020, 6, 15, 0, 0, 0, 0, time.UTC))
		setTime(t, files, "trip/day1/clip.mp4", time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
		setTime(t, files, "trip/new.HEIC", time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC))

		photos := listAll(t, files, 500)
		if got := strings.Join(photoPaths(photos), ","); got != "trip/new.HEIC,trip/day1/clip.mp4,mid.png,old.jpg" {
			t.Fatalf("got %s", got)
		}
		kinds := map[string]string{}
		for _, photo := range photos {
			kinds[photo.Path] = photo.Kind
			if photo.Size != 1 || photo.Key == "" || photo.Date.IsZero() {
				t.Errorf("incomplete photo %+v", photo)
			}
		}
		if kinds["trip/day1/clip.mp4"] != PhotoVideo || kinds["mid.png"] != PhotoImage || kinds["trip/new.HEIC"] != PhotoImage {
			t.Fatalf("wrong kinds %v", kinds)
		}
	})
}

func TestListPhotosPagesWithoutGapsOrRepeats(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		base := time.Date(2023, 3, 1, 12, 0, 0, 0, time.UTC)
		var want []string
		for i := range 23 {
			name := fmt.Sprintf("p%02d.jpg", i)
			put(t, files, name, "x")
			setTime(t, files, name, base.Add(time.Duration(i/4)*time.Hour))
		}
		all := listAll(t, files, 500)
		want = photoPaths(all)
		if len(want) != 23 {
			t.Fatalf("got %d photos", len(want))
		}
		for _, size := range []int{1, 2, 5, 7, 22, 23, 24} {
			if got := photoPaths(listAll(t, files, size)); !slices.Equal(got, want) {
				t.Fatalf("page size %d changed the order or lost photos:\n%v\n%v", size, got, want)
			}
		}
		if !slices.IsSortedFunc(all, func(a, b Photo) int {
			if c := b.Date.Compare(a.Date); c != 0 {
				return c
			}

			return strings.Compare(a.Path, b.Path)
		}) {
			t.Fatal("not newest first with ties by path")
		}

		page, err := files.ListPhotos("", 10, "")
		if err != nil || len(page.Photos) != 10 || page.Next == "" {
			t.Fatalf("first page: %+v %v", page, err)
		}
		last, err := files.ListPhotos(page.Next, 500, "")
		if err != nil || len(last.Photos) != 13 || last.Next != "" {
			t.Fatalf("last page: %d photos next %q %v", len(last.Photos), last.Next, err)
		}
		if past, err := files.ListPhotos(fmt.Sprintf("%d|zzz", time.Unix(0, 0).Unix()), 10, ""); err != nil || len(past.Photos) != 0 {
			t.Fatalf("cursor past the end: %+v %v", past, err)
		}
		if capped, _ := files.ListPhotos("", 100000, ""); len(capped.Photos) != 23 {
			t.Fatalf("limit above the cap: %d", len(capped.Photos))
		}
	})
}

func TestListPhotosRefusesBadCursors(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.jpg", "x")
		for _, cursor := range []string{"nonsense", "x|a.jpg", "|a.jpg", "99999999999999999999|a", "1.5|a"} {
			if _, err := files.ListPhotos(cursor, 10, ""); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("cursor %q: %v", cursor, err)
			}
		}
	})
}

func TestListPhotosPrefersExifDateOverModifiedTime(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "taken-in-2019.jpg", string(jpegWithExif(tiffWithDate(binary.LittleEndian, "2019:07:04 12:30:45"))))
		put(t, files, "heic-2021.heic", string(heicWithExif(tiffWithDate(binary.BigEndian, "2021:12:31 23:59:58"), 0)))
		put(t, files, "no-exif.jpg", "plain bytes")
		put(t, files, "broken-exif.jpg", string(jpegWithExif(tiffWithDate(binary.LittleEndian, "garbage garbage garbage"))))
		uploaded := time.Date(2024, 5, 5, 5, 5, 5, 0, time.UTC)
		for _, name := range []string{"taken-in-2019.jpg", "heic-2021.heic", "no-exif.jpg", "broken-exif.jpg"} {
			setTime(t, files, name, uploaded)
		}

		dates := map[string]time.Time{}
		for _, photo := range listAll(t, files, 500) {
			dates[photo.Path] = photo.Date
		}
		want := map[string]time.Time{
			"taken-in-2019.jpg": time.Date(2019, 7, 4, 12, 30, 45, 0, time.UTC),
			"heic-2021.heic":    time.Date(2021, 12, 31, 23, 59, 58, 0, time.UTC),
			"no-exif.jpg":       uploaded,
			"broken-exif.jpg":   uploaded,
		}
		for name, date := range want {
			if !dates[name].Equal(date) {
				t.Errorf("%s: got %v, want %v", name, dates[name], date)
			}
		}
	})
}

func TestListPhotosSkipsHiddenFoldersAtAnyDepthAndCase(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".thumbs", ".THUMBS", ".trash", ".uploads", "a/.Thumbs", "a/.trash/b", "visible"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name), "x.jpg"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()

	if got := photoPaths(listAll(t, files, 500)); !slices.Equal(got, []string{"visible/x.jpg"}) {
		t.Fatalf("got %v", got)
	}
}

func TestListPhotosSkipsSymlinksAndOddNames(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.jpg"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.jpg"), filepath.Join(dir, "photo.jpg")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real.jpg"), filepath.Join(dir, "inside-link.jpg")); err != nil {
		t.Fatal(err)
	}
	odd := []string{"-rf.jpg", "--version.heic", "with space.png", "ünï 日本.jpg", strings.Repeat("a", 251) + ".jpg"}
	if runtime.GOOS != "windows" {
		for _, name := range []string{"new\nline.jpg", "back\\slash.jpg", "trailing.jpg ", "tab\there.jpg"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range odd {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()

	got := photoPaths(listAll(t, files, 500))
	slices.Sort(got)
	want := append([]string{"real.jpg"}, odd...)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}

	for _, name := range []string{"photo.jpg", "inside-link.jpg", "linked/secret.jpg"} {
		if file, _, err := files.OpenPhoto(name); err == nil {
			_ = file.Close()
			t.Errorf("OpenPhoto(%q) must refuse a symlink", name)
		} else if !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenPhoto(%q): %v", name, err)
		}
	}
	for _, name := range odd {
		file, photo, err := files.OpenPhoto(name)
		if err != nil {
			t.Errorf("OpenPhoto(%q): %v", name, err)
			continue
		}
		_ = file.Close()
		if photo.Path != name {
			t.Errorf("path changed to %q", photo.Path)
		}
	}
}

func TestOpenPhoto(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.jpg", "hello")
		put(t, files, "notes.txt", "x")
		put(t, files, "zero.jpg", "")
		if err := files.Mkdir("folder.jpg"); err != nil {
			t.Fatal(err)
		}

		file, photo, err := files.OpenPhoto("a.jpg")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		_ = file.Close()
		if string(data) != "hello" || photo.Size != 5 || photo.Kind != PhotoImage || photo.Key == "" || photo.Scope == "" {
			t.Fatalf("got %q %+v", data, photo)
		}
		if file, _, err := files.OpenPhoto("zero.jpg"); err != nil {
			t.Fatalf("a zero byte file opens so the tool can refuse it: %v", err)
		} else {
			_ = file.Close()
		}

		cases := map[string]error{
			"notes.txt": ErrNotMedia, "folder.jpg": ErrNotMedia, "missing.jpg": ErrNotFound, "nodir/a.jpg": ErrNotFound,
			"a.jpg/x.jpg": ErrNotDir, "": ErrInvalidPath, "../a.jpg": ErrInvalidPath, "/a.jpg": ErrInvalidPath, "a\x00.jpg": ErrInvalidPath,
		}
		for name, want := range cases {
			if _, _, err := files.OpenPhoto(name); !errors.Is(err, want) && !(want == ErrNotDir && errors.Is(err, ErrNotFound)) {
				t.Errorf("OpenPhoto(%q): %v, want %v", name, err, want)
			}
		}
	})
}

func TestPhotoKeyChangesWhenTheFileChanges(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		key := func() string {
			file, photo, err := files.OpenPhoto("a.jpg")
			if err != nil {
				t.Fatal(err)
			}
			_ = file.Close()

			return photo.Key
		}
		put(t, files, "a.jpg", "one")
		setTime(t, files, "a.jpg", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
		first := key()
		if key() != first {
			t.Fatal("the key must be stable")
		}
		setTime(t, files, "a.jpg", time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC))
		second := key()
		if second == first {
			t.Fatal("a new modification time must give a new key")
		}
		if err := files.Write("a.jpg", strings.NewReader("longer content"), true); err != nil {
			t.Fatal(err)
		}
		if third := key(); third == second || third == first {
			t.Fatal("new content must give a new key")
		}
		if !thumbNamePattern.MatchString(Photo{Key: first}.ThumbName()) || !thumbNamePattern.MatchString(Photo{Key: first}.PreviewName()) {
			t.Fatal("thumbnail names must match the pattern ReadThumb accepts")
		}
	})
}

func TestThumbStorage(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		name := Photo{Key: strings.Repeat("ab", 16)}.ThumbName()
		if _, err := files.ReadThumb(name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing thumbnail: %v", err)
		}
		if err := files.WriteThumb(name, []byte("jpeg bytes")); err != nil {
			t.Fatal(err)
		}
		file, err := files.ReadThumb(name)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		_ = file.Close()
		if string(data) != "jpeg bytes" {
			t.Fatalf("got %q", data)
		}

		for _, bad := range []string{"", "x.jpg", "../" + name, name + "/..", ".thumbs/" + name, strings.ToUpper(name), "index.json", name + "\x00", "ab.t.jpg", strings.Repeat("g", 32) + ".t.jpg"} {
			if _, err := files.ReadThumb(bad); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("ReadThumb(%q): %v", bad, err)
			}
			if err := files.WriteThumb(bad, []byte("x")); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("WriteThumb(%q): %v", bad, err)
			}
		}
		if got := names(t, files, ""); len(got) != 0 {
			t.Fatalf("thumbnails must stay invisible, root shows %v", got)
		}
	})
}

func TestThumbsFolderIsHiddenFromEveryFileOperation(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.jpg", "x")
		if err := files.WriteThumb(Photo{Key: strings.Repeat("cd", 16)}.ThumbName(), []byte("jpeg")); err != nil {
			t.Fatal(err)
		}

		for _, name := range []string{".thumbs", ".THUMBS", ".Thumbs", "x/.thumbs", ".thumbs/" + strings.Repeat("cd", 16) + ".t.jpg", ".THUMBS/index.json", "a/.ThUmBs/b.jpg"} {
			if _, err := files.List(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("List(%q): %v", name, err)
			}
			if _, _, err := files.Open(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Open(%q): %v", name, err)
			}
			if _, _, err := files.OpenPhoto(name + "/x.jpg"); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("OpenPhoto(%q): %v", name+"/x.jpg", err)
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
			if err := files.Rename("a.jpg", name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Rename to %q: %v", name, err)
			}
			if _, err := files.Trash(name); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("Trash(%q): %v", name, err)
			}
			if _, err := files.CreateUpload(name, 1, false); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("CreateUpload(%q): %v", name, err)
			}
		}
		if got := names(t, files, ""); !slices.Equal(got, []string{"a.jpg"}) {
			t.Fatalf("root listing shows %v", got)
		}
	})
}

func TestSweepThumbsRemovesOnlyOrphans(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "keep.jpg", "keep")
		put(t, files, "gone.jpg", "gone")
		put(t, files, "changed.jpg", "before")
		thumbFor := func(name string) Photo {
			file, photo, err := files.OpenPhoto(name)
			if err != nil {
				t.Fatal(err)
			}
			_ = file.Close()

			return photo
		}
		old := map[string]Photo{}
		for _, name := range []string{"keep.jpg", "gone.jpg", "changed.jpg"} {
			old[name] = thumbFor(name)
			for _, thumb := range []string{old[name].ThumbName(), old[name].PreviewName()} {
				if err := files.WriteThumb(thumb, []byte("jpeg")); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := files.Trash("gone.jpg"); err != nil {
			t.Fatal(err)
		}
		if err := files.Write("changed.jpg", strings.NewReader("after, and longer"), true); err != nil {
			t.Fatal(err)
		}

		removed, err := files.SweepThumbs(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if removed != 4 {
			t.Fatalf("removed %d, want the 2 thumbnails of the trashed file and the 2 of the replaced one", removed)
		}
		exists := func(name string) bool {
			file, err := files.ReadThumb(name)
			if err == nil {
				_ = file.Close()
			}

			return err == nil
		}
		if !exists(old["keep.jpg"].ThumbName()) || !exists(old["keep.jpg"].PreviewName()) {
			t.Fatal("a thumbnail with a live source was removed")
		}
		for _, name := range []string{"gone.jpg", "changed.jpg"} {
			if exists(old[name].ThumbName()) || exists(old[name].PreviewName()) {
				t.Errorf("orphan thumbnail of %s survived", name)
			}
		}
		if again, err := files.SweepThumbs(time.Now()); err != nil || again != 0 {
			t.Fatalf("second sweep: %d, %v", again, err)
		}
	})
}

func TestSweepThumbsWithoutAThumbsFolder(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		if removed, err := files.SweepThumbs(time.Now()); err != nil || removed != 0 {
			t.Fatalf("got %d, %v", removed, err)
		}
	})
}

func TestSweepThumbsRemovesOnlyStaleTempFilesAndKeepsStrangers(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ThumbsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) {
		path := filepath.Join(dir, ThumbsDir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	write(".tmp-stale", 2*time.Hour)
	write(".tmp-fresh", time.Minute)
	write("index.json", 48*time.Hour)
	write("notes.txt", 48*time.Hour)
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()

	removed, err := files.SweepThumbs(time.Now())
	if err != nil || removed != 1 {
		t.Fatalf("got %d, %v", removed, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ThumbsDir))
	var left []string
	for _, entry := range entries {
		left = append(left, entry.Name())
	}
	slices.Sort(left)
	if !slices.Equal(left, []string{".tmp-fresh", "index.json", "notes.txt"}) {
		t.Fatalf("left %v", left)
	}

	if removed, err := files.SweepThumbs(time.Now().Add(3 * time.Hour)); err != nil || removed != 1 {
		t.Fatalf("with a later clock the fresh temp file is stale too: %d, %v", removed, err)
	}
}

func TestSweepAllThumbsCoversTheAdminRootAndEveryUser(t *testing.T) {
	base := t.TempDir()
	for _, root := range []string{base, filepath.Join(base, "amy", "files"), filepath.Join(base, "bob", "files")} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		files, err := NewDirFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := files.WriteThumb(strings.Repeat("ef", 16)+".t.jpg", []byte("orphan")); err != nil {
			t.Fatal(err)
		}
		_ = files.(io.Closer).Close()
	}

	removed, err := SweepAllThumbs(base, time.Now())
	if err != nil || removed != 3 {
		t.Fatalf("got %d, %v", removed, err)
	}
}

func TestDateIndexIsWrittenOnceAndReused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), jpegWithExif(tiffWithDate(binary.LittleEndian, "2019:07:04 12:30:45")), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()

	photos := listAll(t, files, 10)
	indexPath := filepath.Join(dir, ThumbsDir, thumbIndexName)
	data, err := os.ReadFile(indexPath)
	if err != nil || !strings.Contains(string(data), photos[0].Key) {
		t.Fatalf("index not written: %q %v", data, err)
	}

	forged := strings.Replace(string(data), fmt.Sprint(photos[0].Date.Unix()), "1000000000", 1)
	if err := os.WriteFile(indexPath, []byte(forged), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listAll(t, files, 10); !got[0].Date.Equal(time.Unix(1000000000, 0)) {
		t.Fatalf("the cached date was not used: %v", got[0].Date)
	}

	if err := os.WriteFile(indexPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listAll(t, files, 10); !got[0].Date.Equal(photos[0].Date) {
		t.Fatalf("a corrupt index must be rebuilt: %v", got[0].Date)
	}
}

func TestDateBudgetLeavesUnreadFilesOnModifiedTime(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), jpegWithExif(tiffWithDate(binary.LittleEndian, "2019:07:04 12:30:45")), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()

	budget := photoDateBudget
	photoDateBudget = -time.Second
	defer func() { photoDateBudget = budget }()
	first := listAll(t, files, 10)
	if first[0].Date.Year() == 2019 {
		t.Fatal("no budget means no exif read")
	}
	photoDateBudget = budget
	if second := listAll(t, files, 10); second[0].Date.Year() != 2019 {
		t.Fatalf("the next listing must finish the job: %v", second[0].Date)
	}
}

func TestCursorSurvivesAnyPathCharacters(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		names := []string{"a|b.jpg", "c|d|e.jpg", "plain.jpg"}
		if _, onDisk := files.(*dirFiles); onDisk && runtime.GOOS == "windows" {
			names = []string{"plain.jpg", "plain2.jpg", "plain3.jpg"}
		}
		for _, name := range names {
			put(t, files, name, "x")
			setTime(t, files, name, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
		}
		if got := photoPaths(listAll(t, files, 1)); !slices.Equal(got, names) {
			t.Fatalf("got %v", got)
		}
	})
}

func TestListPhotosPagesThroughOneSecondOfSubsecondTimes(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		base := time.Date(2023, 3, 1, 12, 0, 0, 0, time.UTC)
		for i := range 9 {
			name := fmt.Sprintf("p%d.jpg", 9-i)
			put(t, files, name, "x")
			setTime(t, files, name, base.Add(time.Duration(i)*100*time.Millisecond))
		}
		want := photoPaths(listAll(t, files, 500))
		for _, size := range []int{1, 2, 4} {
			if got := photoPaths(listAll(t, files, size)); !slices.Equal(got, want) || len(got) != 9 {
				t.Fatalf("page size %d lost or repeated photos: %v", size, got)
			}
		}
	})
}
