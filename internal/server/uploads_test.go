package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type uploadReply struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Offset int64  `json:"offset"`
	Error  string `json:"error"`
}

func decodeReply(t *testing.T, rec *httptest.ResponseRecorder) uploadReply {
	t.Helper()
	var reply uploadReply
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("not JSON (%d): %q", rec.Code, rec.Body.String())
	}

	return reply
}

func applyHeaders(req *http.Request, headers map[string]string) {
	for key, value := range headers {
		if key == "Content-Length" {
			req.ContentLength, _ = strconv.ParseInt(value, 10, 64)
			continue
		}
		req.Header.Set(key, value)
	}
}

type requester func(method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder

func (s *fileSession) requester() requester {
	return func(method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, body)
		req.RemoteAddr = "192.0.2.10:5555"
		applyHeaders(req, headers)
		req.AddCookie(s.cookie)
		rec := httptest.NewRecorder()
		s.handler.ServeHTTP(rec, req)

		return rec
	}
}

func (h *portalFilesHarness) requester(cookie *http.Cookie) requester {
	return func(method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, body)
		req.RemoteAddr = "192.0.2.10:5555"
		applyHeaders(req, headers)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)

		return rec
	}
}

type uploadClient struct {
	t    *testing.T
	call requester
}

func (c uploadClient) create(path string, size int64, overwrite bool) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"path":%s,"size":%d,"overwrite":%t}`, quote(path), size, overwrite)

	return c.createRaw(body)
}

func (c uploadClient) createRaw(body string) *httptest.ResponseRecorder {
	return c.call("POST", "/api/uploads", strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
}

func (c uploadClient) mustCreate(path string, size int64, overwrite bool) string {
	c.t.Helper()
	rec := c.create(path, size, overwrite)
	if rec.Code != http.StatusCreated {
		c.t.Fatalf("create %q got %d: %s", path, rec.Code, rec.Body)
	}
	reply := decodeReply(c.t, rec)
	if len(reply.ID) != 32 || reply.Offset != 0 || reply.Size != size {
		c.t.Fatalf("create reply %+v", reply)
	}

	return reply.ID
}

func (c uploadClient) chunkRaw(id, contentRange, data string) *httptest.ResponseRecorder {
	headers := map[string]string{"Content-Type": "application/octet-stream"}
	if contentRange != "" {
		headers["Content-Range"] = contentRange
	}

	return c.call("PUT", "/api/uploads/"+id, strings.NewReader(data), headers)
}

func (c uploadClient) chunkWithLength(id, contentRange, data, contentLength string) *httptest.ResponseRecorder {
	headers := map[string]string{"Content-Type": "application/octet-stream", "Content-Range": contentRange, "Content-Length": contentLength}

	return c.call("PUT", "/api/uploads/"+id, strings.NewReader(data), headers)
}

func (c uploadClient) chunk(id string, start int64, total int64, data string) *httptest.ResponseRecorder {
	return c.chunkRaw(id, fmt.Sprintf("bytes %d-%d/%d", start, start+int64(len(data))-1, total), data)
}

func (c uploadClient) status(id string) *httptest.ResponseRecorder {
	return c.call("GET", "/api/uploads/"+id, nil, nil)
}

func (c uploadClient) finish(id string) *httptest.ResponseRecorder {
	return c.call("POST", "/api/uploads/"+id+"/finish", nil, nil)
}

func (c uploadClient) exists(paths ...string) *httptest.ResponseRecorder {
	list, _ := json.Marshal(map[string]any{"paths": paths})

	return c.call("POST", "/api/files/exists", bytes.NewReader(list), map[string]string{"Content-Type": "application/json"})
}

func (c uploadClient) mustFinish(id string) {
	c.t.Helper()
	if rec := c.finish(id); rec.Code != http.StatusNoContent {
		c.t.Fatalf("finish got %d: %s", rec.Code, rec.Body)
	}
}

func panelUploads(t *testing.T) (*fileSession, uploadClient) {
	s := newFileSession(t)

	return s, uploadClient{t: t, call: s.requester()}
}

func TestChunkedUploadLandsWithItsContent(t *testing.T) {
	s, c := panelUploads(t)
	s.seed("photos/keep.txt", "k")

	id := c.mustCreate("photos/trip.bin", 9, false)
	rec := c.chunk(id, 0, 9, "abc")
	if rec.Code != http.StatusOK || decodeReply(t, rec).Offset != 3 {
		t.Fatalf("first chunk: %d %s", rec.Code, rec.Body)
	}
	if got := decodeReply(t, c.status(id)); got.Offset != 3 || got.Size != 9 || got.Path != "photos/trip.bin" {
		t.Fatalf("status: %+v", got)
	}
	c.chunk(id, 3, 9, "def")
	if s.exists("photos/trip.bin") {
		t.Fatal("the file must not exist before finish")
	}
	if rec := c.chunk(id, 6, 9, "ghi"); rec.Code != http.StatusOK {
		t.Fatalf("last chunk: %d %s", rec.Code, rec.Body)
	}
	c.mustFinish(id)

	data, err := os.ReadFile(filepath.Join(s.root, "photos", "trip.bin"))
	if err != nil || string(data) != "abcdefghi" {
		t.Fatalf("content %q %v", data, err)
	}
	if rec := c.status(id); rec.Code != http.StatusNotFound {
		t.Fatalf("finished upload must be gone, got %d", rec.Code)
	}
	if entries, _ := os.ReadDir(filepath.Join(s.root, ".uploads")); len(entries) != 0 {
		t.Fatalf("partial files left behind: %v", entries)
	}
}

func TestCreateUploadRejectsBadInput(t *testing.T) {
	s, c := panelUploads(t)
	s.seed("file.txt", "x")
	s.seed("dir/placeholder", "x")

	bodies := map[string]int{
		`{"path":"a.bin","size":-1}`:                           http.StatusBadRequest,
		`{"path":"a.bin","size":"12"}`:                         http.StatusBadRequest,
		`{"path":"a.bin","size":"abc"}`:                        http.StatusBadRequest,
		`{"path":"a.bin","size":1.5}`:                          http.StatusBadRequest,
		`{"path":"a.bin","size":1e3}`:                          http.StatusBadRequest,
		`{"path":"a.bin","size":null}`:                         http.StatusBadRequest,
		`{"path":"a.bin"}`:                                     http.StatusBadRequest,
		`{"path":"a.bin","size":99999999999999999999}`:         http.StatusBadRequest,
		`{"path":"a.bin","size":9223372036854775807}`:          http.StatusRequestEntityTooLarge,
		`{"path":"a.bin","size":1025}`:                         http.StatusRequestEntityTooLarge,
		`{"path":"","size":1}`:                                 http.StatusBadRequest,
		`{"size":1}`:                                           http.StatusBadRequest,
		`{"path":"../x","size":1}`:                             http.StatusBadRequest,
		`{"path":"a/../../x","size":1}`:                        http.StatusBadRequest,
		`{"path":"/etc/passwd","size":1}`:                      http.StatusBadRequest,
		`{"path":"a\\b","size":1}`:                             http.StatusBadRequest,
		`{"path":"a\u0000b","size":1}`:                         http.StatusBadRequest,
		`{"path":".uploads/x","size":1}`:                       http.StatusBadRequest,
		`{"path":"dir/.UPLOADS/x","size":1}`:                   http.StatusBadRequest,
		`{"path":".Uploads","size":1}`:                         http.StatusBadRequest,
		`{"path":"file.txt","size":1}`:                         http.StatusConflict,
		`{"path":"dir","size":1,"overwrite":true}`:             http.StatusUnprocessableEntity,
		`{"path":"file.txt/child","size":1}`:                   http.StatusUnprocessableEntity,
		`{"path":"nowhere/child","size":1}`:                    http.StatusNotFound,
		`{"path":"` + strings.Repeat("n", 256) + `","size":1}`: http.StatusBadRequest,
		`not json`: http.StatusBadRequest,
		`[]`:       http.StatusBadRequest,
	}
	for body, want := range bodies {
		rec := c.createRaw(body)
		if rec.Code != want {
			t.Errorf("create %s got %d, want %d: %s", body, rec.Code, want, rec.Body)
		}
	}
	if rec := c.call("POST", "/api/uploads", strings.NewReader(`{"path":"a.bin","size":1}`), map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type got %d", rec.Code)
	}
	if s.exists(".uploads") {
		entries, _ := os.ReadDir(filepath.Join(s.root, ".uploads"))
		t.Errorf("refused creates must not leave partials: %v", entries)
	}
}

func TestCreateAcceptsAwkwardButLegalNames(t *testing.T) {
	s, c := panelUploads(t)
	names := []string{
		"日本語 ファイル.txt", "with space.txt", ".hidden", "-_-", "(((", "ünï cödé", ".htaccess", "a,b;c'd",
		strings.Repeat("l", 255),
	}
	for _, name := range names {
		id := c.mustCreate(name, 2, false)
		c.chunk(id, 0, 2, "hi")
		c.mustFinish(id)
		if !s.exists(name) {
			t.Errorf("%q did not land", name)
		}
	}
}

func TestChunkRangeRules(t *testing.T) {
	s, c := panelUploads(t)
	id := c.mustCreate("a.bin", 10, false)
	c.chunk(id, 0, 10, "abcd")

	conflicts := []struct{ name, header, body string }{
		{"gap", "bytes 6-7/10", "xx"},
		{"overlap", "bytes 2-5/10", "xxxx"},
		{"restart", "bytes 0-3/10", "xxxx"},
		{"far ahead", "bytes 9-9/10", "x"},
	}
	for _, c2 := range conflicts {
		rec := c.chunkRaw(id, c2.header, c2.body)
		if rec.Code != http.StatusConflict || decodeReply(t, rec).Offset != 4 {
			t.Errorf("%s: got %d %s", c2.name, rec.Code, rec.Body)
		}
	}

	malformed := []string{
		"", "bytes", "bytes 4-5", "bytes 4-5/", "bytes */10", "bytes 4-5/*", "bytes -4-5/10", "bytes 4--5/10",
		"bytes a-b/10", "bytes 5-4/10", "bytes 4-5/10 ", " bytes 4-5/10", "BYTES 4-5/10", "items 4-5/10",
		"bytes 4-5/10, 6-7/10", "bytes 4-99999999999999999999/10", "bytes 4-5/99999999999999999999", "bytes 0x4-5/10",
		"bytes 4-5/-10", "bytes +4-5/10",
	}
	for _, header := range malformed {
		if rec := c.chunkRaw(id, header, "xy"); rec.Code != http.StatusBadRequest {
			t.Errorf("Content-Range %q got %d, want 400", header, rec.Code)
		}
	}

	if rec := c.chunkRaw(id, "bytes 4-5/11", "xy"); rec.Code != http.StatusBadRequest {
		t.Errorf("a total that differs from the declared size got %d", rec.Code)
	}
	if rec := c.chunkRaw(id, "bytes 4-10/10", "xyzabcd"); rec.Code != http.StatusBadRequest {
		t.Errorf("a range past the declared size got %d", rec.Code)
	}
	if rec := c.chunkRaw(id, "bytes 4-5/10", "xyz"); rec.Code != http.StatusBadRequest {
		t.Errorf("a body longer than its Content-Length range got %d", rec.Code)
	}
	if rec := c.chunkWithLength(id, "bytes 4-5/10", "xyzabcdefgh", "-1"); rec.Code != http.StatusBadRequest {
		t.Errorf("an unsized body longer than its range got %d", rec.Code)
	}
	if got := decodeReply(t, c.status(id)).Offset; got != 4 {
		t.Fatalf("refused chunks changed the partial, offset %d", got)
	}
	if rec := c.chunkWithLength(id, "bytes 4-5/10", "x", "2"); rec.Code != http.StatusBadRequest {
		t.Errorf("a body shorter than its range got %d", rec.Code)
	} else if decodeReply(t, rec).Offset != 5 {
		t.Errorf("a short chunk keeps what arrived and reports it: %s", rec.Body)
	}
	if rec := c.call("PUT", "/api/uploads/"+id, strings.NewReader("y"), map[string]string{"Content-Range": "bytes 5-5/10"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a chunk without octet-stream got %d", rec.Code)
	}
	if rec := c.call("PUT", "/api/uploads/"+id, strings.NewReader("y"), map[string]string{"Content-Type": "application/json", "Content-Range": "bytes 5-5/10"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a JSON chunk got %d", rec.Code)
	}

	if rec := c.chunk(id, 5, 10, "efghi"); rec.Code != http.StatusOK {
		t.Fatalf("resync chunk: %d %s", rec.Code, rec.Body)
	}
	if rec := c.chunk(id, 10, 10, "z"); rec.Code != http.StatusBadRequest {
		t.Errorf("a chunk after the last byte got %d", rec.Code)
	}
	c.mustFinish(id)
	data, _ := os.ReadFile(filepath.Join(s.root, "a.bin"))
	if string(data) != "abcdxefghi" {
		t.Fatalf("content %q", data)
	}
}

func TestFinishRules(t *testing.T) {
	s, c := panelUploads(t)

	short := c.mustCreate("short.bin", 6, false)
	c.chunk(short, 0, 6, "abc")
	rec := c.finish(short)
	if rec.Code != http.StatusConflict || decodeReply(t, rec).Offset != 3 {
		t.Fatalf("finish with a size mismatch: %d %s", rec.Code, rec.Body)
	}
	if s.exists("short.bin") {
		t.Fatal("an incomplete upload must not appear")
	}

	done := c.mustCreate("done.bin", 3, false)
	c.chunk(done, 0, 3, "abc")
	c.mustFinish(done)
	if rec := c.finish(done); rec.Code != http.StatusNotFound {
		t.Fatalf("finish twice got %d", rec.Code)
	}
	if rec := c.chunk(done, 0, 3, "xyz"); rec.Code != http.StatusNotFound {
		t.Fatalf("chunk after finish got %d", rec.Code)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "done.bin")); string(data) != "abc" {
		t.Fatalf("content changed after finish: %q", data)
	}
}

func TestZeroByteUploadThroughTheChunkApi(t *testing.T) {
	s, c := panelUploads(t)
	id := c.mustCreate("empty.txt", 0, false)
	c.mustFinish(id)
	info, err := os.Stat(filepath.Join(s.root, "empty.txt"))
	if err != nil || info.Size() != 0 {
		t.Fatalf("%v %v", info, err)
	}
}

func TestFinishHonoursOverwriteChosenAtCreate(t *testing.T) {
	s, c := panelUploads(t)
	s.seed("doc.txt", "old")

	if rec := c.create("doc.txt", 3, false); rec.Code != http.StatusConflict {
		t.Fatalf("create onto an existing file without overwrite got %d", rec.Code)
	}

	keep := c.mustCreate("fresh.txt", 3, false)
	c.chunk(keep, 0, 3, "new")
	s.seed("fresh.txt", "appeared meanwhile")
	if rec := c.finish(keep); rec.Code != http.StatusConflict {
		t.Fatalf("finish onto a file that appeared got %d", rec.Code)
	}
	if data, _ := os.ReadFile(filepath.Join(s.root, "fresh.txt")); string(data) != "appeared meanwhile" {
		t.Fatalf("existing file damaged: %q", data)
	}

	replace := c.mustCreate("doc.txt", 3, true)
	c.chunk(replace, 0, 3, "new")
	c.mustFinish(replace)
	if data, _ := os.ReadFile(filepath.Join(s.root, "doc.txt")); string(data) != "new" {
		t.Fatalf("content %q", data)
	}
}

func TestUploadParentThatBecomesAFileFailsCleanly(t *testing.T) {
	s, c := panelUploads(t)
	s.seed("dir/placeholder", "x")
	id := c.mustCreate("dir/a.bin", 1, false)
	c.chunk(id, 0, 1, "x")
	if err := os.RemoveAll(filepath.Join(s.root, "dir")); err != nil {
		t.Fatal(err)
	}
	s.seed("dir", "now a file")
	if rec := c.finish(id); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
}

func TestConcurrentChunksToOneUploadAcceptExactlyOne(t *testing.T) {
	_, c := panelUploads(t)
	id := c.mustCreate("a.bin", 8, false)

	var ok, conflict atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch c.chunk(id, 0, 8, "abcd").Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusConflict:
				conflict.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 || conflict.Load() != 11 {
		t.Fatalf("ok %d conflict %d", ok.Load(), conflict.Load())
	}
	if got := decodeReply(t, c.status(id)).Offset; got != 4 {
		t.Fatalf("offset %d", got)
	}
}

func TestExistsCheck(t *testing.T) {
	s, c := panelUploads(t)
	s.seed("a.txt", "x")
	s.seed("sub/b.txt", "x")
	s.seed("sub/deeper/c.txt", "x")

	type existing struct {
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
	}
	read := func(rec *httptest.ResponseRecorder) []existing {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		var body struct {
			Existing []existing `json:"existing"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Existing == nil {
			t.Fatalf("body %q %v", rec.Body, err)
		}

		return body.Existing
	}

	got := read(c.exists("a.txt", "new.txt", "sub/b.txt", "sub", "a.txt", "sub/b.txt", "a.txt/child", "missing/x.txt"))
	slices.SortFunc(got, func(x, y existing) int { return strings.Compare(x.Path, y.Path) })
	want := []existing{{"a.txt", false}, {"sub", true}, {"sub/b.txt", false}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v (duplicates must collapse, missing and under-a-file paths are not conflicts)", got, want)
	}
	if got := read(c.exists()); len(got) != 0 {
		t.Fatalf("empty check: %v", got)
	}

	for _, bad := range []string{"", "..", "../a", "/etc/passwd", `a\b`, "a\x00b", ".uploads", "x/.uploads/y", ".UPLOADS"} {
		if rec := c.exists("a.txt", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("exists-check with %q got %d", bad, rec.Code)
		}
	}
	if rec := c.call("POST", "/api/files/exists", strings.NewReader(`{"paths":"a.txt"}`), map[string]string{"Content-Type": "application/json"}); rec.Code != http.StatusBadRequest {
		t.Errorf("paths that is not a list got %d", rec.Code)
	}
	if rec := c.call("POST", "/api/files/exists", strings.NewReader(`{"paths":["a.txt"]}`), map[string]string{"Content-Type": "text/plain"}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("wrong content type got %d", rec.Code)
	}

	many := make([]string, 0, 3000)
	for i := range 3000 {
		many = append(many, fmt.Sprintf("folder/with/a/fairly/long/path/name/file-%04d.jpg", i))
	}
	many = append(many, "a.txt")
	if got := read(c.exists(many...)); len(got) != 1 || got[0].Path != "a.txt" {
		t.Fatalf("a large batch must work: %v", got)
	}
	tooMany := make([]string, maxExistsPaths+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("f%d", i)
	}
	if rec := c.exists(tooMany...); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized batch got %d", rec.Code)
	}
}

func TestUploadsFolderIsUnreachableThroughTheFileApis(t *testing.T) {
	s, c := panelUploads(t)
	s.seed("visible.txt", "v")
	id := c.mustCreate("a.bin", 6, false)
	c.chunk(id, 0, 6, "SECRET")

	if !s.exists(".uploads/" + id + ".part") {
		t.Fatal("the partial should be on disk in .uploads")
	}
	if rec := s.get("/api/files?path="); strings.Contains(rec.Body.String(), "uploads") || strings.Contains(strings.ToLower(rec.Body.String()), "uploads") {
		t.Fatalf("listing leaks the uploads folder: %s", rec.Body)
	}

	for _, name := range []string{".uploads", ".UPLOADS", ".Uploads", ".uploads/" + id + ".part", ".uploads/" + id + ".json", "sub/.uploads", ".uploads/../.uploads"} {
		escaped := url.QueryEscape(name)
		checks := map[string]*httptest.ResponseRecorder{
			"list":        s.get("/api/files?path=" + escaped),
			"download":    s.get("/api/files/download?path=" + escaped),
			"zip":         s.get("/api/files/zip?path=" + escaped),
			"delete":      s.call("DELETE", "/api/files?path="+escaped, nil, ""),
			"mkdir":       s.post("/api/files/folder", map[string]any{"path": name}),
			"rename-from": s.post("/api/files/rename", map[string]any{"from": name, "to": "elsewhere"}),
			"rename-to":   s.post("/api/files/rename", map[string]any{"from": "visible.txt", "to": name}),
			"upload":      s.put("/api/files/content?path="+escaped, "planted"),
		}
		for action, rec := range checks {
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s %q got %d, want 400", action, name, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "SECRET") {
				t.Errorf("%s %q leaked the partial", action, name)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(s.root, ".uploads", id+".part")); err != nil || string(data) != "SECRET" {
		t.Fatalf("partial damaged: %q %v", data, err)
	}
	if !s.exists("visible.txt") {
		t.Fatal("visible.txt was moved")
	}

	zipBody := s.get("/api/files/zip?path=").Body.Bytes()
	reader, err := zip.NewReader(bytes.NewReader(zipBody), int64(len(zipBody)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range reader.File {
		if strings.Contains(strings.ToLower(f.Name), "uploads") {
			t.Errorf("root zip contains %q", f.Name)
		}
	}
}

func TestZipDownload(t *testing.T) {
	s, _ := panelUploads(t)
	s.seed("photos/2024/a.jpg", "AAAA")
	s.seed("photos/2024/日本語 b.jpg", "BB")
	s.seed("photos/top.txt", "top")
	s.seed("photos/.hidden", "h")
	s.seed("photos/empty/.keep", "")
	if err := os.Remove(filepath.Join(s.root, "photos", "empty", ".keep")); err != nil {
		t.Fatal(err)
	}
	s.seed("other.txt", "not in the zip")
	rec := s.get("/api/files/zip?path=photos")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("content type %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "photos.zip") {
		t.Errorf("disposition %q", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}

	entries := zipEntries(t, rec.Body.Bytes())
	want := map[string]string{
		"photos/":               "",
		"photos/2024/":          "",
		"photos/2024/a.jpg":     "AAAA",
		"photos/2024/日本語 b.jpg": "BB",
		"photos/top.txt":        "top",
		"photos/.hidden":        "h",
		"photos/empty/":         "",
	}
	if len(entries) != len(want) {
		t.Fatalf("entries %v, want %v", keys(entries), keys(want))
	}
	for name, content := range want {
		if got, ok := entries[name]; !ok || got != content {
			t.Errorf("entry %q = %q (present %v), want %q", name, got, ok, content)
		}
	}
	for name := range entries {
		if strings.Contains(name, `\`) || strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, s.root) {
			t.Errorf("unclean entry name %q", name)
		}
	}
}

func zipEntries(t *testing.T, data []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range reader.File {
		if f.Method != zip.Store {
			t.Errorf("%q uses method %d, want Store", f.Name, f.Method)
		}
		if f.Modified.Year() < 2020 {
			t.Errorf("%q has no modification time: %v", f.Name, f.Modified)
		}
		handle, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(handle)
		_ = handle.Close()
		if err != nil {
			t.Fatalf("%q: %v", f.Name, err)
		}
		out[f.Name] = string(content)
	}

	return out
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)

	return out
}

func TestZipOfASingleFileAndOfTheRoot(t *testing.T) {
	s, _ := panelUploads(t)
	s.seed("docs/note.txt", "hello")
	s.seed("top.txt", "top")

	single := s.get("/api/files/zip?path=" + url.QueryEscape("docs/note.txt"))
	if single.Code != http.StatusOK {
		t.Fatalf("got %d", single.Code)
	}
	if got := zipEntries(t, single.Body.Bytes()); len(got) != 1 || got["note.txt"] != "hello" {
		t.Fatalf("single file zip: %v", got)
	}
	if got := single.Header().Get("Content-Disposition"); !strings.Contains(got, "note.txt.zip") {
		t.Errorf("disposition %q", got)
	}

	root := s.get("/api/files/zip?path=")
	if root.Code != http.StatusOK {
		t.Fatalf("got %d", root.Code)
	}
	got := zipEntries(t, root.Body.Bytes())
	if got["files/top.txt"] != "top" || got["files/docs/note.txt"] != "hello" || got["files/docs/"] != "" || got["files/"] != "" {
		t.Fatalf("root zip: %v", keys(got))
	}
}

func TestZipOfAnEmptyFolderHasItsDirectoryEntry(t *testing.T) {
	s, _ := panelUploads(t)
	if err := os.Mkdir(filepath.Join(s.root, "nothing"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := s.get("/api/files/zip?path=nothing")
	if got := zipEntries(t, rec.Body.Bytes()); len(got) != 1 || got["nothing/"] != "" {
		t.Fatalf("%v", keys(got))
	}
}

func TestZipErrors(t *testing.T) {
	s, _ := panelUploads(t)
	if rec := s.get("/api/files/zip?path=missing"); rec.Code != http.StatusNotFound {
		t.Errorf("missing got %d", rec.Code)
	}
	for _, bad := range []string{"..", "../x", "/etc", `a\b`, "a%00b"} {
		if rec := s.get("/api/files/zip?path=" + bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%q got %d", bad, rec.Code)
		}
	}
	if rec := s.get("/api/files/zip?path=%zz"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed query got %d", rec.Code)
	}
	if rec := s.call("POST", "/api/files/zip?path=", nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST got %d", rec.Code)
	}
}

func TestZipSkipsSymlinks(t *testing.T) {
	s, _ := panelUploads(t)
	s.seed("pics/real.txt", "real")
	if err := os.Symlink(filepath.Join(s.outside, "secret.txt"), filepath.Join(s.root, "pics", "link.txt")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
	if err := os.Symlink(s.outside, filepath.Join(s.root, "pics", "linkdir")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(s.root, "pics"), filepath.Join(s.root, "pics", "loop")); err != nil {
		t.Skipf("this OS refused to create a symlink: %v", err)
	}

	got := zipEntries(t, s.get("/api/files/zip?path=pics").Body.Bytes())
	for name, content := range got {
		if strings.Contains(content, secretContent) || strings.Contains(name, "link") || strings.Contains(name, "loop") {
			t.Errorf("symlink leaked into the zip: %q", name)
		}
	}
	if got["pics/real.txt"] != "real" || len(got) != 2 {
		t.Fatalf("%v", keys(got))
	}
}

func TestNewRoutesRequireASession(t *testing.T) {
	h := newHarness(t)
	h.signedIn(t)
	id := strings.Repeat("a", 32)
	routes := []struct{ method, target, body string }{
		{"POST", "/api/uploads", `{"path":"a","size":1}`},
		{"GET", "/api/uploads/" + id, ""},
		{"PUT", "/api/uploads/" + id, "x"},
		{"POST", "/api/uploads/" + id + "/finish", ""},
		{"POST", "/api/files/exists", `{"paths":["a"]}`},
		{"GET", "/api/files/zip?path=", ""},
	}
	for _, route := range routes {
		rec := h.do(route.method, route.target, route.body, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session got %d", route.method, route.target, rec.Code)
		}
	}
}

func TestNewRoutesRefuseWrongMethods(t *testing.T) {
	s, _ := panelUploads(t)
	id := strings.Repeat("a", 32)
	for _, route := range []struct{ method, target string }{
		{"GET", "/api/uploads"},
		{"DELETE", "/api/uploads/" + id},
		{"GET", "/api/uploads/" + id + "/finish"},
		{"PUT", "/api/uploads/" + id + "/finish"},
		{"GET", "/api/files/exists"},
		{"PUT", "/api/files/zip"},
	} {
		if rec := s.call(route.method, route.target, nil, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s got %d", route.method, route.target, rec.Code)
		}
	}
}

func TestSlowChunkOutlivesTheServerReadTimeout(t *testing.T) {
	_, c := panelUploads(t)
	id := c.mustCreate("a.bin", 4, false)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- c.chunk(id, 0, 4, "abcd") }()
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chunk did not finish")
	}
}

func portalUploads(t *testing.T) (*portalFilesHarness, uploadClient, uploadClient) {
	h := newPortalFilesHarness(t)
	amy := uploadClient{t: t, call: h.requester(h.signedIn(t, "amy", amyPassword))}
	bob := uploadClient{t: t, call: h.requester(h.signedIn(t, "bob", bobPassword))}

	return h, amy, bob
}

func TestPortalUploadLandsInTheUsersOwnFolder(t *testing.T) {
	h, amy, _ := portalUploads(t)
	amyID := amy.mustCreate("a.bin", 6, false)
	amy.chunk(amyID, 0, 6, "abc")
	amy.chunk(amyID, 3, 6, "def")
	amy.mustFinish(amyID)
	if data, err := os.ReadFile(filepath.Join(h.base, "amy", "files", "a.bin")); err != nil || string(data) != "abcdef" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", ".uploads")); err != nil {
		t.Fatalf("partials must live in the user's own files folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.base, "bob", "files", ".uploads")); err == nil {
		t.Fatal("bob must not get an uploads folder from amy's upload")
	}
}

func TestAnotherUsersUploadIDIsNotFound(t *testing.T) {
	h, amy, bob := portalUploads(t)
	id := amy.mustCreate("secret.bin", 6, false)
	amy.chunk(id, 0, 6, "SECRET")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"status": bob.status(id),
		"chunk":  bob.chunk(id, 6, 12, "BOBBOB"),
		"finish": bob.finish(id),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("bob %s with amy's id got %d: %s", name, rec.Code, rec.Body)
		}
	}
	if got := decodeReply(t, amy.status(id)); got.Offset != 6 {
		t.Fatalf("amy's upload was changed: %+v", got)
	}
	amy.mustFinish(id)
	if _, err := os.Stat(filepath.Join(h.base, "bob", "files", "secret.bin")); err == nil {
		t.Fatal("bob received amy's file")
	}
	if data, _ := os.ReadFile(filepath.Join(h.base, "amy", "files", "secret.bin")); string(data) != "SECRET" {
		t.Fatalf("%q", data)
	}
}

func TestPortalUploadIDGuessesAreNotFound(t *testing.T) {
	_, amy, _ := portalUploads(t)
	for _, id := range []string{"x", strings.Repeat("0", 32), "%2e%2e", strings.Repeat("a", 33), strings.Repeat("A", 32)} {
		if rec := amy.status(id); rec.Code != http.StatusNotFound {
			t.Errorf("status %q got %d", id, rec.Code)
		}
		if rec := amy.finish(id); rec.Code != http.StatusNotFound {
			t.Errorf("finish %q got %d", id, rec.Code)
		}
	}
}

func TestPortalEnforcesTheUploadLimitAtCreateAndPerChunk(t *testing.T) {
	_, amy, _ := portalUploads(t)
	if rec := amy.create("big.bin", portalMaxUpload+1, false); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the limit got %d", rec.Code)
	}
	id := amy.mustCreate("ok.bin", portalMaxUpload, false)
	if rec := amy.chunkRaw(id, fmt.Sprintf("bytes 0-%d/%d", portalMaxUpload-1, portalMaxUpload), strings.Repeat("x", portalMaxUpload+30)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a body past the declared range got %d", rec.Code)
	}
	if got := decodeReply(t, amy.status(id)).Offset; got != 0 {
		t.Fatalf("offset %d", got)
	}
	if rec := amy.chunk(id, 0, portalMaxUpload, strings.Repeat("x", portalMaxUpload)); rec.Code != http.StatusOK {
		t.Fatalf("exactly the limit got %d: %s", rec.Code, rec.Body)
	}
	amy.mustFinish(id)
}

func TestPortalZipIsConfinedToTheUser(t *testing.T) {
	h, amy, _ := portalUploads(t)
	h.write(t, "amy", "mine/a.txt", "amy-data")
	h.write(t, "bob", "theirs/b.txt", bobSecret)
	h.write(t, "amy", "root.txt", "r")
	id := amy.mustCreate("pending.bin", 3, false)
	amy.chunk(id, 0, 3, "abc")

	cookie := h.signedIn(t, "amy", amyPassword)
	for _, path := range []string{"", "mine"} {
		rec := h.get(cookie, "/api/files/zip", path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%q got %d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), bobSecret) {
			t.Fatalf("zip of %q leaked bob's file", path)
		}
		for name := range zipEntries(t, rec.Body.Bytes()) {
			if strings.Contains(name, "theirs") || strings.Contains(name, "uploads") || strings.Contains(name, "pending") {
				t.Errorf("unexpected entry %q in zip of %q", name, path)
			}
		}
	}
	if rec := h.get(cookie, "/api/files/zip", "../bob/files/theirs"); rec.Code != http.StatusBadRequest {
		t.Errorf("traversal got %d", rec.Code)
	}
	if rec := h.get(cookie, "/api/files/zip", "theirs"); rec.Code != http.StatusNotFound {
		t.Errorf("bob's folder name inside amy's view got %d", rec.Code)
	}
}

func TestPortalNewRoutesNeedASessionAndSameOrigin(t *testing.T) {
	h, _, _ := portalUploads(t)
	for _, route := range []struct{ method, target, body string }{
		{"POST", "/api/uploads", `{"path":"a","size":1}`},
		{"GET", "/api/uploads/" + strings.Repeat("a", 32), ""},
		{"PUT", "/api/uploads/" + strings.Repeat("a", 32), "x"},
		{"POST", "/api/uploads/" + strings.Repeat("a", 32) + "/finish", ""},
		{"POST", "/api/files/exists", `{"paths":["a"]}`},
		{"GET", "/api/files/zip?path=", ""},
	} {
		if rec := h.do(route.method, route.target, route.body, "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a session got %d", route.method, route.target, rec.Code)
		}
	}

	cookie := h.signedIn(t, "amy", amyPassword)
	for _, route := range []struct{ method, target, body string }{
		{"POST", "/api/uploads", `{"path":"a","size":1}`},
		{"PUT", "/api/uploads/" + strings.Repeat("a", 32), "x"},
		{"POST", "/api/uploads/" + strings.Repeat("a", 32) + "/finish", ""},
		{"POST", "/api/files/exists", `{"paths":["a"]}`},
	} {
		req := httptest.NewRequest(route.method, route.target, strings.NewReader(route.body))
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin %s %s got %d", route.method, route.target, rec.Code)
		}
	}
}
