package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var pathSeeds = []string{
	"", "a", "a/b/c.txt", "../x", "..", "/etc/passwd", "a/../b", "a//b", "link", "link/secret.txt", "filelink",
	"escape/secret.txt", `a\b`, "a\x00b", "%2e%2e/x", "....//x", "a/./b", strings.Repeat("a/", 50), "secret.txt", ".",
}

func FuzzCleanPath(f *testing.F) {
	for _, seed := range pathSeeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, path string) {
		clean, err := CleanPath(path)
		if err != nil || clean == "" {
			return
		}
		if clean != path {
			t.Fatalf("CleanPath must not rewrite paths: %q became %q", path, clean)
		}
		for _, segment := range strings.Split(clean, "/") {
			if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\\\x00") {
				t.Fatalf("accepted %q with bad segment %q", path, segment)
			}
		}
	})
}

// Whatever path an operation is given, nothing outside the root may be read, changed or created.
func FuzzDirFilesStaysInRoot(f *testing.F) {
	for _, seed := range pathSeeds {
		f.Add(uint8(0), seed, "x")
		f.Add(uint8(2), seed, "x")
		f.Add(uint8(4), "a", seed)
	}
	f.Fuzz(func(t *testing.T, op uint8, first, second string) {
		base := t.TempDir()
		root, outside := filepath.Join(base, "root"), filepath.Join(base, "outside")
		for _, dir := range []string{root, outside} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		secret := filepath.Join(outside, "secret.txt")
		if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "a"), []byte("inside"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Symlink(outside, filepath.Join(root, "link"))
		_ = os.Symlink(secret, filepath.Join(root, "filelink"))

		files, err := NewDirFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.(interface{ Close() error }).Close()

		switch op % 6 {
		case 0:
			_, _ = files.List(first)
		case 1:
			if file, _, err := files.Open(first); err == nil {
				buf := make([]byte, 64)
				n, _ := file.Read(buf)
				_ = file.Close()
				if strings.Contains(string(buf[:n]), "secret") {
					t.Fatalf("Open(%q) read the file outside the root", first)
				}
			}
		case 2:
			_ = files.Write(first, strings.NewReader("planted"), true)
		case 3:
			_ = files.Mkdir(first)
		case 4:
			_ = files.Rename(first, second)
		case 5:
			_ = files.Remove(first)
		}

		if data, err := os.ReadFile(secret); err != nil || string(data) != "secret" {
			t.Fatalf("op %d %q %q damaged the outside file: %q %v", op%6, first, second, data, err)
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 1 {
			t.Fatalf("op %d %q %q changed the outside folder: %v %v", op%6, first, second, entries, err)
		}
		if entries, err := os.ReadDir(base); err != nil || len(entries) != 2 {
			t.Fatalf("op %d %q %q created something next to the root: %v %v", op%6, first, second, entries, err)
		}
	})
}
