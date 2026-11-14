package system

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
)

func TestLoggedRestoreErrorsCarryNoFileName(t *testing.T) {
	pathErr := &fs.PathError{Op: "open", Path: "/srv/files/amy/diary.txt", Err: fs.ErrPermission}
	linkErr := &os.LinkError{Op: "rename", Old: "/srv/files/amy/a.txt", New: "/srv/files/amy/b.txt", Err: fs.ErrExist}
	cases := map[string]struct {
		err  error
		want error
	}{
		"path error":         {pathErr, fs.ErrPermission},
		"wrapped path error": {fmt.Errorf("restore: %w", pathErr), fs.ErrPermission},
		"link error":         {linkErr, fs.ErrExist},
		"wrapped link error": {fmt.Errorf("restore: %w", linkErr), fs.ErrExist},
	}
	for name, c := range cases {
		got := withoutPath(c.err)
		if !errors.Is(got, c.want) || strings.Contains(got.Error(), "amy") {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

func TestLoggedRestoreErrorsWithoutAPathAreKept(t *testing.T) {
	plain := errors.New("disk full")
	if got := withoutPath(plain); got != plain {
		t.Fatalf("got %v", got)
	}
}
