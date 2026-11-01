package system

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var pathSeeds = []string{
	"", "a", "a/b/c.txt", "../x", "..", "/etc/passwd", "a/../b", "a//b", "link", "link/secret.txt", "filelink",
	"escape/secret.txt", `a\b`, "a\x00b", "%2e%2e/x", "....//x", "a/./b", strings.Repeat("a/", 50), "secret.txt", ".",
	".thumbs", ".THUMBS/x.t.jpg", "a/.Thumbs/b.jpg", "escape/secret.jpg", "photo.jpg", "link/photo.jpg", "filelink.jpg",
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
		f.Add(uint8(6), seed, "x")
		f.Add(uint8(4), "a", seed)
		f.Add(uint8(7), seed, "x")
		f.Add(uint8(8), seed, seed)
		f.Add(uint8(9), seed, "x")
		f.Add(uint8(10), seed, "x")
		f.Add(uint8(11), seed, "x")
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
		_ = os.Symlink(secret, filepath.Join(root, "filelink.jpg"))
		_ = os.WriteFile(filepath.Join(outside, "secret.jpg"), []byte("secret"), 0o644)
		_ = os.Symlink(filepath.Join(outside, "secret.jpg"), filepath.Join(root, "photo.jpg"))

		files, err := NewDirFiles(root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.(interface{ Close() error }).Close()

		switch op % 12 {
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
		case 6:
			if info, err := files.CreateUpload(first, 1, true); err == nil {
				_, _ = files.AppendUpload(info.ID, 0, 1, strings.NewReader("planted"))
				_ = files.FinishUpload(info.ID)
			}
		case 7:
			if item, err := files.Trash(first); err == nil {
				_, _ = files.RestoreTrash(item.ID)
			}
		case 8:
			if _, err := files.Trash("a"); err == nil {
				_, _ = files.RestoreTrash(first)
				_ = files.PurgeTrash(first)
				_, _ = files.EmptyTrash()
			}
			_, _ = files.ListTrash()
		case 9:
			if file, _, err := files.OpenPhoto(first); err == nil {
				buf := make([]byte, 64)
				n, _ := file.Read(buf)
				_ = file.Close()
				if strings.Contains(string(buf[:n]), "secret") {
					t.Fatalf("OpenPhoto(%q) read the file outside the root", first)
				}
			}
		case 10:
			if page, err := files.ListPhotos(second, 10, ""); err == nil {
				for _, photo := range page.Photos {
					if strings.Contains(photo.Path, "secret") || photo.Path == "photo.jpg" || photo.Path == "filelink.jpg" {
						t.Fatalf("ListPhotos returned %q", photo.Path)
					}
				}
			}
		case 11:
			_ = files.WriteThumb(first, []byte("planted"))
			if file, err := files.ReadThumb(first); err == nil {
				_ = file.Close()
			}
			_, _ = files.SweepThumbs(time.Now())
		}

		if data, err := os.ReadFile(secret); err != nil || string(data) != "secret" {
			t.Fatalf("op %d %q %q damaged the outside file: %q %v", op%12, first, second, data, err)
		}
		if entries, err := os.ReadDir(outside); err != nil || len(entries) != 2 {
			t.Fatalf("op %d %q %q changed the outside folder: %v %v", op%12, first, second, entries, err)
		}
		if entries, err := os.ReadDir(base); err != nil || len(entries) != 2 {
			t.Fatalf("op %d %q %q created something next to the root: %v %v", op%12, first, second, entries, err)
		}
	})
}
