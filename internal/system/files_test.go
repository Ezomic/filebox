package system

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanPath(t *testing.T) {
	valid := map[string]string{
		"":                  "",
		"a":                 "a",
		"a/b/c.txt":         "a/b/c.txt",
		"café/ü ber":        "café/ü ber",
		"日本語/ファイル":          "日本語/ファイル",
		"-weird, name!.tar": "-weird, name!.tar",
		".hidden":           ".hidden",
	}
	for input, want := range valid {
		got, err := CleanPath(input)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", input, got, err, want)
		}
	}

	invalid := []string{
		"/", "/etc/passwd", "..", "../x", "a/../b", "a/..", "a/./b", ".", "a//b", "a/", "/a",
		`a\b`, `..\x`, "a\x00b", "\x00", "a\nb", "a\tb", "a\x7fb", "\xff\xfe",
		"trailing.", "trailing ", "dir/trailing.", "dir /file", " ", "  ", "a/ ",
		strings.Repeat("a", 256), strings.Repeat("a/", 3000), strings.Repeat("a", 4097),
		"%2e%2e", // stays literal here; decoding happens once in net/http
	}
	for _, input := range invalid[:len(invalid)-1] {
		if _, err := CleanPath(input); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("CleanPath(%q) err = %v; want ErrInvalidPath", input, err)
		}
	}
	if got, err := CleanPath("%2e%2e"); err != nil || got != "%2e%2e" {
		t.Errorf("literal percent name must be a plain file name, got %q, %v", got, err)
	}
	if got, err := CleanPath(strings.Repeat("a", 255)); err != nil || len(got) != 255 {
		t.Errorf("255 byte name must be accepted, got %v", err)
	}
}

type filesFactory struct {
	name string
	make func(t *testing.T) Files
}

func filesFactories() []filesFactory {
	return []filesFactory{
		{"dir", func(t *testing.T) Files {
			files, err := NewDirFiles(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = files.(io.Closer).Close() })

			return files
		}},
		{"memory", func(*testing.T) Files { return NewFakeFiles() }},
	}
}

func put(t *testing.T, files Files, path, content string) {
	t.Helper()
	if err := files.Write(path, strings.NewReader(content), false); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func read(t *testing.T, files Files, path string) string {
	t.Helper()
	file, _, err := files.Open(path)
	if err != nil {
		t.Fatalf("open %q: %v", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func names(t *testing.T, files Files, dir string) []string {
	t.Helper()
	entries, err := files.List(dir)
	if err != nil {
		t.Fatalf("list %q: %v", dir, err)
	}
	var out []string
	for _, entry := range entries {
		out = append(out, entry.Name)
	}

	return out
}

func TestFilesContract(t *testing.T) {
	for _, factory := range filesFactories() {
		t.Run(factory.name, func(t *testing.T) {
			t.Run("list orders folders first then names", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "b.txt", "bb")
				put(t, files, "A.txt", "a")
				if err := files.Mkdir("zdir"); err != nil {
					t.Fatal(err)
				}
				got := strings.Join(names(t, files, ""), ",")
				if got != "zdir,A.txt,b.txt" {
					t.Fatalf("got %s", got)
				}
				entries, _ := files.List("")
				if !entries[0].IsDir || entries[2].Size != 2 || entries[2].Modified.IsZero() {
					t.Fatalf("bad metadata: %+v", entries)
				}
			})

			t.Run("empty root lists as empty slice", func(t *testing.T) {
				entries, err := factory.make(t).List("")
				if err != nil || entries == nil || len(entries) != 0 {
					t.Fatalf("got %v, %v", entries, err)
				}
			})

			t.Run("list errors", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "f.txt", "x")
				if _, err := files.List("f.txt"); !errors.Is(err, ErrNotDir) {
					t.Fatalf("file as dir: %v", err)
				}
				if _, err := files.List("nope"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing: %v", err)
				}
				if _, err := files.List("../x"); !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("bad path: %v", err)
				}
			})

			t.Run("write and open round trip including zero bytes", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "empty", "")
				put(t, files, "data.bin", "hello")
				if read(t, files, "empty") != "" || read(t, files, "data.bin") != "hello" {
					t.Fatal("content mismatch")
				}
				_, info, _ := files.Open("data.bin")
				if info.Size != 5 || info.Name != "data.bin" {
					t.Fatalf("info %+v", info)
				}
			})

			t.Run("write refuses to overwrite unless asked", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "f", "one")
				if err := files.Write("f", strings.NewReader("two"), false); !errors.Is(err, ErrExists) {
					t.Fatalf("got %v", err)
				}
				if read(t, files, "f") != "one" {
					t.Fatal("existing file was touched")
				}
				if err := files.Write("f", strings.NewReader("two"), true); err != nil {
					t.Fatal(err)
				}
				if read(t, files, "f") != "two" {
					t.Fatal("overwrite did not apply")
				}
			})

			t.Run("write never replaces a folder", func(t *testing.T) {
				files := factory.make(t)
				_ = files.Mkdir("d")
				for _, overwrite := range []bool{false, true} {
					err := files.Write("d", strings.NewReader("x"), overwrite)
					if !errors.Is(err, ErrExists) && !errors.Is(err, ErrIsDir) {
						t.Fatalf("overwrite=%v: %v", overwrite, err)
					}
				}
			})

			t.Run("write needs an existing folder parent", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "f", "x")
				if err := files.Write("missing/x", strings.NewReader("x"), false); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing parent: %v", err)
				}
				if err := files.Write("f/x", strings.NewReader("x"), false); !errors.Is(err, ErrNotDir) {
					t.Fatalf("file parent: %v", err)
				}
				if err := files.Write("", strings.NewReader("x"), true); !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("root: %v", err)
				}
			})

			t.Run("failed write leaves no partial file", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "keep", "original")
				boom := errors.New("boom")
				reader := io.MultiReader(strings.NewReader("partial"), errReader{boom})
				if err := files.Write("keep", reader, true); !errors.Is(err, boom) {
					t.Fatalf("got %v", err)
				}
				if err := files.Write("fresh", io.MultiReader(strings.NewReader("p"), errReader{boom}), false); !errors.Is(err, boom) {
					t.Fatalf("got %v", err)
				}
				if got := strings.Join(names(t, files, ""), ","); got != "keep" {
					t.Fatalf("leftovers: %s", got)
				}
				if read(t, files, "keep") != "original" {
					t.Fatal("original was damaged by a failed overwrite")
				}
			})

			t.Run("open errors", func(t *testing.T) {
				files := factory.make(t)
				_ = files.Mkdir("d")
				if _, _, err := files.Open("d"); !errors.Is(err, ErrIsDir) {
					t.Fatalf("dir: %v", err)
				}
				if _, _, err := files.Open("nope"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing: %v", err)
				}
				if _, _, err := files.Open(""); !errors.Is(err, ErrIsDir) && !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("root: %v", err)
				}
			})

			t.Run("mkdir", func(t *testing.T) {
				files := factory.make(t)
				if err := files.Mkdir("d"); err != nil {
					t.Fatal(err)
				}
				if err := files.Mkdir("d"); !errors.Is(err, ErrExists) {
					t.Fatalf("duplicate: %v", err)
				}
				if err := files.Mkdir("a/b"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing parent: %v", err)
				}
				if err := files.Mkdir(""); !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("root: %v", err)
				}
				put(t, files, "f", "x")
				if err := files.Mkdir("f"); !errors.Is(err, ErrExists) {
					t.Fatalf("over file: %v", err)
				}
				if err := files.Mkdir("f/x"); !errors.Is(err, ErrNotDir) {
					t.Fatalf("under file: %v", err)
				}
			})

			t.Run("rename", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "a", "A")
				put(t, files, "b", "B")
				_ = files.Mkdir("d")
				_ = files.Mkdir("d/sub")
				put(t, files, "d/sub/f", "F")

				if err := files.Rename("a", "c"); err != nil {
					t.Fatal(err)
				}
				if read(t, files, "c") != "A" {
					t.Fatal("content lost")
				}
				if _, _, err := files.Open("a"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("old name still there: %v", err)
				}
				if err := files.Rename("c", "b"); !errors.Is(err, ErrExists) {
					t.Fatalf("onto existing: %v", err)
				}
				if err := files.Rename("b", "b"); !errors.Is(err, ErrExists) {
					t.Fatalf("onto itself: %v", err)
				}
				if err := files.Rename("nope", "z"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing: %v", err)
				}
				if err := files.Rename("b", "missing/z"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing target parent: %v", err)
				}
				if err := files.Rename("d", "d/sub/inside"); !errors.Is(err, ErrIntoSelf) {
					t.Fatalf("into itself: %v", err)
				}
				if err := files.Rename("d", "d/x"); !errors.Is(err, ErrIntoSelf) {
					t.Fatalf("into itself direct: %v", err)
				}
				if err := files.Rename("", "x"); !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("root source: %v", err)
				}
				if err := files.Rename("b", ""); !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("root target: %v", err)
				}
				if err := files.Rename("d", "e"); err != nil {
					t.Fatal(err)
				}
				if read(t, files, "e/sub/f") != "F" {
					t.Fatal("folder contents did not move")
				}
				if err := files.Rename("e", "ee"); err != nil {
					t.Fatal(err)
				}
				if err := files.Rename("ee", "e2"); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("rename prefix siblings are not nested", func(t *testing.T) {
				files := factory.make(t)
				_ = files.Mkdir("d")
				if err := files.Rename("d", "d2"); err != nil {
					t.Fatalf("d -> d2 is a sibling, got %v", err)
				}
			})

			t.Run("remove", func(t *testing.T) {
				files := factory.make(t)
				put(t, files, "f", "x")
				_ = files.Mkdir("empty")
				_ = files.Mkdir("full")
				put(t, files, "full/f", "x")

				if err := files.Remove("f"); err != nil {
					t.Fatal(err)
				}
				if err := files.Remove("empty"); err != nil {
					t.Fatal(err)
				}
				if err := files.Remove("full"); !errors.Is(err, ErrNotEmpty) {
					t.Fatalf("non-empty: %v", err)
				}
				if read(t, files, "full/f") != "x" {
					t.Fatal("non-empty folder lost data")
				}
				if err := files.Remove("nope"); !errors.Is(err, ErrNotFound) {
					t.Fatalf("missing: %v", err)
				}
				if err := files.Remove(""); !errors.Is(err, ErrInvalidPath) {
					t.Fatalf("root: %v", err)
				}
			})
		})
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestLeftoverUploadTempFilesAreListedAndDeletable(t *testing.T) {
	dir := t.TempDir()
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "d", ".upload-123.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(t, files, "d"), ","); got != ".upload-123.tmp" {
		t.Fatalf("a leftover temp file must be visible, got %q", got)
	}
	if err := files.Remove("d"); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("got %v", err)
	}
	if err := files.Remove("d/.upload-123.tmp"); err != nil {
		t.Fatal(err)
	}
	if err := files.Remove("d"); err != nil {
		t.Fatal(err)
	}
}

func TestDirFilesRefusesSymlinkEscape(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("this OS refused to create a symlink, the escape cases cannot run: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()

	if _, err := files.List("escape"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("list through escaping symlink: %v", err)
	}
	if _, _, err := files.Open("escape/secret.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open through escaping symlink: %v", err)
	}
	if _, _, err := files.Open("link.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open escaping file symlink: %v", err)
	}
	if err := files.Write("escape/new.txt", bytes.NewReader([]byte("x")), false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write through escaping symlink: %v", err)
	}
	if err := files.Mkdir("escape/d"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mkdir through escaping symlink: %v", err)
	}
	if err := files.Rename("link.txt", "escape/moved.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename through escaping symlink: %v", err)
	}
	if err := files.Remove("escape/secret.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove through escaping symlink: %v", err)
	}
	for _, name := range []string{"secret.txt"} {
		if _, err := os.Stat(filepath.Join(outside, name)); err != nil {
			t.Fatalf("outside file was damaged: %v", err)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 1 {
		t.Fatalf("outside directory was modified: %v", entries)
	}
}

func TestDirFilesFollowsSymlinksInsideRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "f"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
	files, err := NewDirFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer files.(io.Closer).Close()
	if read(t, files, "alias/f") != "ok" {
		t.Fatal("inside symlink should work")
	}
}

func TestNewDirFilesNeedsExistingDirectory(t *testing.T) {
	if _, err := NewDirFiles(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing root")
	}
}
