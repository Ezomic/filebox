package server

import (
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func FuzzUploadLinkRoutesStayConfined(f *testing.F) {
	for _, seed := range []string{
		"a.txt", "", ".", "..", "../outside.txt", "../../bob/files/secret.txt", "..\\outside.txt", "/etc/passwd", "a\x00b", ".trash", ".uploads/x",
		"to-bob", "to-bob/secret.txt", "up-link.txt", "sub", "sub/b.txt", "a.txt/", "notes.txt", strings.Repeat("a/", 300), strings.Repeat("é", 300),
		"uploads", "uploads/" + strings.Repeat("0", 32), "uploads/" + strings.Repeat("0", 32) + "/finish", "info", "list", "zip", "download?path=a.txt",
	} {
		for op := range uint8(4) {
			f.Add(op, seed, "Anna", "")
			f.Add(op, "photo.jpg", seed, seed)
		}
	}
	f.Fuzz(func(t *testing.T, op uint8, text, sender, visit string) {
		h := newShareHarness(t)
		h.seed(t)
		h.write(t, "amy", "outside.txt", []byte("amy-outside-share"))
		_ = os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, "amy", "files", "docs", "to-bob"))
		_ = os.Symlink("../outside.txt", filepath.Join(h.base, "amy", "files", "docs", "up-link.txt"))
		h.smallLink(t, "amy", "drop-box", "docs", 1<<20)
		before := snapshot(t, h.base)

		var recs []*httptest.ResponseRecorder
		switch op % 4 {
		case 0:
			recs = h.fuzzSend(text, sender, visit)
		case 1:
			recs = h.fuzzSend("photo.jpg", text, visit)
		case 2:
			req := httptest.NewRequest("POST", "/x", strings.NewReader(startBody(sender, 4, visit, "")))
			req.URL.Path, req.URL.RawQuery = "/drop-box/"+text, ""
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = visitorIP + ":1"
			rec := httptest.NewRecorder()
			h.portal.ServeHTTP(rec, req)
			recs = append(recs, rec)
		default:
			for _, method := range []string{"GET", "PUT", "DELETE"} {
				req := httptest.NewRequest(method, "/x", strings.NewReader("data"))
				req.URL.Path, req.URL.RawQuery = "/"+text, ""
				req.Header.Set("Content-Range", "bytes 0-3/4")
				req.Header.Set("Content-Type", "application/octet-stream")
				req.RemoteAddr = visitorIP + ":1"
				rec := httptest.NewRecorder()
				h.portal.ServeHTTP(rec, req)
				recs = append(recs, rec)
			}
		}

		for _, rec := range recs {
			if rec.Code >= 500 && runtime.GOOS != "windows" {
				t.Fatalf("op %d %q %q %q gave a server error %d: %s", op, text, sender, visit, rec.Code, rec.Body)
			}
			for _, secret := range []string{bobSecret, "amy-outside-share", "trashed", "partial", "hello notes"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatalf("op %d %q %q leaked %q", op, text, sender, secret)
				}
			}
			if _, found := rec.Header()["Set-Cookie"]; found {
				t.Fatalf("a visitor got a cookie from %q", text)
			}
		}
		after := snapshot(t, h.base)
		for path, content := range before {
			if after[path] != content {
				t.Fatalf("op %d %q %q %q changed or removed %s", op, text, sender, visit, path)
			}
		}
		for path := range after {
			partial := path == "amy/files/.uploads" || strings.HasPrefix(path, "amy/files/.uploads/")
			if _, existed := before[path]; !existed && !partial && !strings.HasPrefix(path, "amy/files/docs/") {
				t.Fatalf("op %d %q %q %q wrote outside the link's folder: %s", op, text, sender, visit, path)
			}
		}
	})
}

func (h *shareHarness) fuzzSend(name, sender, visit string) []*httptest.ResponseRecorder {
	start := h.guestStart(visitorIP, "drop-box", startBody(name, 4, sender, visit))
	var started guestUpload
	if start.Code != 201 || json.Unmarshal(start.Body.Bytes(), &started) != nil {
		return []*httptest.ResponseRecorder{start}
	}

	return []*httptest.ResponseRecorder{start, h.guestChunk(visitorIP, "drop-box", started.ID, 0, 4, "data"), h.guestFinish(visitorIP, "drop-box", started.ID)}
}

// snapshot maps every entry under base to what it holds: a file's content, or a marker for a folder or a link.
func snapshot(t *testing.T, base string) map[string]string {
	t.Helper()
	entries := map[string]string{}
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, path)
		rel = filepath.ToSlash(rel)
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			entries[rel] = "link:" + target
		case entry.IsDir():
			entries[rel] = "folder"
		default:
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entries[rel] = string(data)
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return entries
}
