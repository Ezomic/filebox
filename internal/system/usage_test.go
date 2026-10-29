package system

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSized(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDirectorySizeAddsUpNestedFiles(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "alice", "files", "a.bin"), 100)
	writeSized(t, filepath.Join(root, "alice", "files", "deep", "b.bin"), 50)
	writeSized(t, filepath.Join(root, "bob", "files", "c.bin"), 7)
	writeSized(t, filepath.Join(root, "alice.deleted-20260101", "files", "old.bin"), 999)
	writeSized(t, filepath.Join(root, "alice", "outside-files.bin"), 999)

	sizes := (&dirUsage{root: root}).scan()
	if sizes["alice"] != 150 || sizes["bob"] != 7 || len(sizes) != 2 {
		t.Fatalf("got %v", sizes)
	}
}

func TestSizesScansInTheBackgroundAndCaches(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "alice", "files", "a.bin"), 10)
	usage := NewUsage(root)

	deadline := time.Now().Add(5 * time.Second)
	for {
		sizes, ready := usage.Sizes()
		if ready {
			if sizes["alice"] != 10 {
				t.Fatalf("got %v", sizes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scan never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}

	writeSized(t, filepath.Join(root, "alice", "files", "b.bin"), 10)
	if sizes, _ := usage.Sizes(); sizes["alice"] != 10 {
		t.Fatalf("a fresh result must be reused, got %v", sizes)
	}
}

func TestSizesRescansWhenStale(t *testing.T) {
	root := t.TempDir()
	writeSized(t, filepath.Join(root, "alice", "files", "a.bin"), 10)
	now := time.Now()
	u := &dirUsage{root: root, now: func() time.Time { return now }}
	u.refresh()

	writeSized(t, filepath.Join(root, "alice", "files", "b.bin"), 5)
	now = now.Add(usageTTL + time.Second)
	u.Sizes()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sizes, _ := u.Sizes(); sizes["alice"] == 15 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("stale result was never refreshed")
}

func TestScanMissingRootIsEmpty(t *testing.T) {
	if sizes := (&dirUsage{root: filepath.Join(t.TempDir(), "nope")}).scan(); len(sizes) != 0 {
		t.Fatalf("got %v", sizes)
	}
}
