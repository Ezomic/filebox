package system

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func listKind(t *testing.T, files Files, kind string, pageSize int) []Photo {
	t.Helper()
	var all []Photo
	cursor := ""
	for range 1000 {
		page, err := files.ListPhotos(cursor, pageSize, kind)
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

func day(year int, month time.Month, d int) time.Time {
	return time.Date(year, month, d, 0, 0, 0, 0, time.UTC)
}

func TestListPhotosFiltersByKindBeforePaging(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		for i, name := range []string{"a.jpg", "b.mp4", "c.png", "d.mov", "e.heic", "f.webm"} {
			put(t, files, name, "x")
			setTime(t, files, name, day(2020, 1, 1+i))
		}

		if got := photoPaths(listKind(t, files, "", 500)); len(got) != 6 {
			t.Fatalf("no filter must list everything: %v", got)
		}
		if got := photoPaths(listKind(t, files, PhotoVideo, 500)); !slices.Equal(got, []string{"f.webm", "d.mov", "b.mp4"}) {
			t.Fatalf("videos: %v", got)
		}
		if got := photoPaths(listKind(t, files, PhotoImage, 500)); !slices.Equal(got, []string{"e.heic", "c.png", "a.jpg"}) {
			t.Fatalf("images: %v", got)
		}
		// One-item pages prove the cursor and the page size count filtered photos only.
		if got := photoPaths(listKind(t, files, PhotoVideo, 1)); !slices.Equal(got, []string{"f.webm", "d.mov", "b.mp4"}) {
			t.Fatalf("paged videos: %v", got)
		}
		page, err := files.ListPhotos("", 2, PhotoImage)
		if err != nil || len(page.Photos) != 2 || page.Next == "" {
			t.Fatalf("a full filtered page must carry a cursor: %+v %v", page, err)
		}
	})
}

func TestListPhotosRefusesAnUnknownKind(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.jpg", "x")
		for _, kind := range []string{"audio", "IMAGE", "photo", " image", "image,video", "*"} {
			if _, err := files.ListPhotos("", 10, kind); !errors.Is(err, ErrInvalidPath) {
				t.Errorf("kind %q: got %v", kind, err)
			}
		}
	})
}

func TestCustomDateMovesAPhotoAndMarksIt(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "old.jpg", "x")
		put(t, files, "new.jpg", "x")
		setTime(t, files, "old.jpg", day(2018, 1, 1))
		setTime(t, files, "new.jpg", day(2022, 1, 1))

		if err := files.SetPhotoDate("old.jpg", time.Date(2024, 5, 17, 13, 45, 0, 0, time.FixedZone("x", 7200))); err != nil {
			t.Fatal(err)
		}
		photos := listAll(t, files, 10)
		if got := photoPaths(photos); !slices.Equal(got, []string{"old.jpg", "new.jpg"}) {
			t.Fatalf("the custom date must decide the position: %v", got)
		}
		if !photos[0].Date.Equal(day(2024, 5, 17)) || !photos[0].Custom || photos[1].Custom {
			t.Fatalf("a day is stored as UTC midnight and only that photo is custom: %+v", photos)
		}

		if err := files.ResetPhotoDate("old.jpg"); err != nil {
			t.Fatal(err)
		}
		photos = listAll(t, files, 10)
		if got := photoPaths(photos); !slices.Equal(got, []string{"new.jpg", "old.jpg"}) || photos[1].Custom || !photos[1].Date.Equal(day(2018, 1, 1)) {
			t.Fatalf("reset must bring the original date back: %+v", photos)
		}
		if err := files.ResetPhotoDate("old.jpg"); err != nil {
			t.Fatalf("resetting twice is not an error: %v", err)
		}
	})
}

func TestPhotosOnTheSameCustomDayStayOrderedByName(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		for _, name := range []string{"c.jpg", "a.jpg", "b.jpg"} {
			put(t, files, name, "x")
			setTime(t, files, name, day(2019, 1, 1))
			if err := files.SetPhotoDate(name, day(2021, 3, 3)); err != nil {
				t.Fatal(err)
			}
		}
		if got := photoPaths(listAll(t, files, 1)); !slices.Equal(got, []string{"a.jpg", "b.jpg", "c.jpg"}) {
			t.Fatalf("got %v", got)
		}
	})
}

func TestCustomDateWorksWithKindFilterAndPaging(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "clip.mp4", "x")
		put(t, files, "pic.jpg", "x")
		setTime(t, files, "clip.mp4", day(2020, 1, 1))
		setTime(t, files, "pic.jpg", day(2021, 1, 1))
		if err := files.SetPhotoDate("clip.mp4", day(2023, 1, 1)); err != nil {
			t.Fatal(err)
		}
		if got := photoPaths(listKind(t, files, "", 1)); !slices.Equal(got, []string{"clip.mp4", "pic.jpg"}) {
			t.Fatalf("got %v", got)
		}
		if got := listKind(t, files, PhotoVideo, 1); len(got) != 1 || !got[0].Custom {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestSetPhotoDateRefusesWhatIsNotAPhoto(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "notes.txt", "x")
		put(t, files, "a.jpg", "x")
		if err := files.Mkdir("folder.jpg"); err != nil {
			t.Fatal(err)
		}
		cases := map[string]error{
			"notes.txt":     ErrNotMedia,
			"folder.jpg":    ErrNotMedia,
			"missing.jpg":   ErrNotFound,
			"../a.jpg":      ErrInvalidPath,
			"/a.jpg":        ErrInvalidPath,
			"a/../a.jpg":    ErrInvalidPath,
			".thumbs/a.jpg": ErrInvalidPath,
			"":              ErrInvalidPath,
		}
		for name, want := range cases {
			if err := files.SetPhotoDate(name, day(2020, 1, 1)); !errors.Is(err, want) {
				t.Errorf("set %q: got %v, want %v", name, err, want)
			}
			if err := files.ResetPhotoDate(name); !errors.Is(err, want) {
				t.Errorf("reset %q: got %v, want %v", name, err, want)
			}
		}
	})
}

func TestSetPhotoDateRefusesDatesThatCannotBeStored(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.jpg", "x")
		for _, when := range []time.Time{{}, day(1899, 12, 31), day(10000, 1, 1), time.Now().AddDate(0, 0, 3), time.Now().AddDate(50, 0, 0)} {
			if err := files.SetPhotoDate("a.jpg", when); !errors.Is(err, ErrInvalidDate) {
				t.Errorf("%v: got %v", when, err)
			}
		}
		if err := files.SetPhotoDate("a.jpg", day(1900, 1, 1)); err != nil {
			t.Fatalf("1900-01-01 is the earliest allowed day: %v", err)
		}
		if err := files.SetPhotoDate("a.jpg", day(1969, 12, 31)); err != nil {
			t.Fatalf("a date before 1970 is allowed: %v", err)
		}
		if got := listAll(t, files, 10); !got[0].Date.Equal(day(1969, 12, 31)) {
			t.Fatalf("got %v", got[0].Date)
		}
	})
}

func TestRenamingMovingOrReplacingAPhotoDropsItsCustomDate(t *testing.T) {
	eachFiles(t, func(t *testing.T, files Files) {
		put(t, files, "a.jpg", "x")
		setTime(t, files, "a.jpg", day(2018, 1, 1))
		if err := files.SetPhotoDate("a.jpg", day(2024, 1, 1)); err != nil {
			t.Fatal(err)
		}

		if err := files.Rename("a.jpg", "b.jpg"); err != nil {
			t.Fatal(err)
		}
		if got := listAll(t, files, 10); got[0].Custom || got[0].Date.Year() == 2024 {
			t.Fatalf("a renamed photo keeps no custom date: %+v", got[0])
		}

		if err := files.SetPhotoDate("b.jpg", day(2024, 1, 1)); err != nil {
			t.Fatal(err)
		}
		if err := files.Mkdir("sub"); err != nil {
			t.Fatal(err)
		}
		if err := files.Rename("b.jpg", "sub/b.jpg"); err != nil {
			t.Fatal(err)
		}
		if got := listAll(t, files, 10); got[0].Custom {
			t.Fatalf("a moved photo keeps no custom date: %+v", got[0])
		}

		if err := files.SetPhotoDate("sub/b.jpg", day(2024, 1, 1)); err != nil {
			t.Fatal(err)
		}
		if err := files.Write("sub/b.jpg", strings.NewReader("different bytes"), true); err != nil {
			t.Fatal(err)
		}
		if got := listAll(t, files, 10); got[0].Custom {
			t.Fatalf("a replaced photo keeps no custom date: %+v", got[0])
		}
	})
}

func TestTheOriginalFileIsNeverTouchedByACustomDate(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "a.jpg")
	if err := os.WriteFile(original, []byte("pixels"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := day(2018, 1, 1)
	if err := os.Chtimes(original, when, when); err != nil {
		t.Fatal(err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)

	if err := files.SetPhotoDate("a.jpg", day(2024, 1, 1)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(original)
	data, _ := os.ReadFile(original)
	if err != nil || string(data) != "pixels" || !info.ModTime().Equal(when) {
		t.Fatalf("the file changed: %q %v %v", data, info.ModTime(), err)
	}
}

func TestCustomDatesLiveInTheUsersOwnJSONFileKeyedByPhotoKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)

	if err := files.SetPhotoDate("a.jpg", day(2024, 2, 29)); err != nil {
		t.Fatal(err)
	}
	photos := listAll(t, files, 10)
	data, err := os.ReadFile(filepath.Join(dir, ThumbsDir, customDatesName))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"` + photos[0].Key + `":"2024-02-29"`; !strings.Contains(string(data), want) {
		t.Fatalf("want %s in %s", want, data)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ThumbsDir))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), thumbTempPrefix) {
			t.Errorf("temp file left behind: %s", entry.Name())
		}
	}
}

func TestACorruptOrHostileOverrideFileFallsBackToOriginalDates(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := day(2018, 1, 1)
	_ = os.Chtimes(filepath.Join(dir, "a.jpg"), when, when)
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)
	key := listAll(t, files, 10)[0].Key
	path := filepath.Join(dir, ThumbsDir, customDatesName)

	hostile := map[string]string{
		"corrupt":       "{not json",
		"wrong type":    `{"dates":[1,2]}`,
		"wrong value":   `{"dates":{"` + key + `":5}}`,
		"bad day":       `{"dates":{"` + key + `":"2026-13-45"}}`,
		"unicode digit": `{"dates":{"` + key + `":"２０２４-０１-０１"}}`,
		"far future":    `{"dates":{"` + key + `":"9999-12-31"}}`,
		"year 10000":    `{"dates":{"` + key + `":"10000-01-01"}}`,
		"too early":     `{"dates":{"` + key + `":"1800-01-01"}}`,
		"empty":         ``,
		"huge":          `{"dates":{"x":"` + strings.Repeat("a", maxCustomDatesFile+1) + `"}}`,
	}
	for name, content := range hostile {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		photos := listAll(t, files, 10)
		if len(photos) != 1 || photos[0].Custom || !photos[0].Date.Equal(when) {
			t.Errorf("%s: must fall back to the original date: %+v", name, photos)
		}
	}

	if err := os.WriteFile(path, []byte(`{"dates":{"`+key+`":"2020-01-01","`+key+`":"2021-02-03"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := listAll(t, files, 10)[0]; !got.Custom || !got.Date.Equal(day(2021, 2, 3)) {
		t.Fatalf("with duplicate keys the last one wins: %+v", got)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := files.SetPhotoDate("a.jpg", day(2024, 1, 1)); err != nil {
		t.Fatalf("setting a date over a corrupt file must repair it: %v", err)
	}
	if got := listAll(t, files, 10)[0]; !got.Custom {
		t.Fatalf("got %+v", got)
	}
}

func TestSweepThumbsForgetsCustomDatesOfPhotosThatAreGone(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"keep.jpg", "gone.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)
	for _, name := range []string{"keep.jpg", "gone.jpg"} {
		if err := files.SetPhotoDate(name, day(2024, 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(dir, "gone.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := files.SweepThumbs(time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ThumbsDir, customDatesName))
	if err != nil || strings.Count(string(data), "2024-01-01") != 1 {
		t.Fatalf("only the live photo may keep a date: %s %v", data, err)
	}
}

func TestCustomDatesOfOneRootNeverReachAnother(t *testing.T) {
	first, second := NewFakeFiles(), NewFakeFiles()
	for _, files := range []*FakeFiles{first, second} {
		put(t, files, "same.jpg", "x")
		files.SetModified("same.jpg", day(2018, 1, 1))
	}
	if err := first.SetPhotoDate("same.jpg", day(2024, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if got := listAll(t, second, 10)[0]; got.Custom {
		t.Fatalf("got %+v", got)
	}
}

func closeFiles(files Files) {
	if closer, ok := files.(io.Closer); ok {
		_ = closer.Close()
	}
}

func TestTooManyCustomDatesAreRefusedInsteadOfGrowingPastTheReadLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= maxCustomDates; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%05d.jpg", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)

	full := map[string]string{}
	for _, photo := range listAll(t, files, MaxPhotoPage) {
		full[photo.Key] = "2020-01-01"
	}
	data, _ := encodeCustomDates(full)
	if len(data) >= maxCustomDatesFile {
		t.Fatalf("a full file must stay readable: %d bytes", len(data))
	}
	if err := os.WriteFile(filepath.Join(dir, ThumbsDir, customDatesName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := files.SetPhotoDate("p00000.jpg", day(2024, 1, 1)); !errors.Is(err, ErrTooManyDates) {
		t.Fatalf("got %v", err)
	}
}

func TestSettingADateForgetsDatesOfPhotosThatAreGoneOnceTheFileIsBig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(files)
	stale := map[string]string{}
	for i := range maxCustomDates {
		stale[fmt.Sprintf("%032x", i)] = "2020-01-01"
	}
	data, _ := encodeCustomDates(stale)
	if err := os.MkdirAll(filepath.Join(dir, ThumbsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ThumbsDir, customDatesName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := files.SetPhotoDate("a.jpg", day(2024, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ThumbsDir, customDatesName)); strings.Count(string(got), "2020-01-01") != 0 {
		t.Fatalf("stale dates must be pruned: %d bytes", len(got))
	}
}
