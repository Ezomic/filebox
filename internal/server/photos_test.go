package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

type photoSurface struct {
	name   string
	call   func(method, target, body string) *httptest.ResponseRecorder
	seed   func(t *testing.T, path, content string)
	root   string
	tool   *system.FakeThumbnailer
	thumbs *Thumbs
	cookie *http.Cookie
	serve  func(http.ResponseWriter, *http.Request)
}

func (s photoSurface) get(target string) *httptest.ResponseRecorder {
	return s.call("GET", target, "")
}

func (s photoSurface) path(parts ...string) string {
	return filepath.Join(append([]string{s.root}, parts...)...)
}

func (s photoSurface) thumbFiles() []string {
	entries, _ := os.ReadDir(s.path(system.ThumbsDir))
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jpg") {
			names = append(names, entry.Name())
		}
	}

	return names
}

func photoSurfaces(t *testing.T) []photoSurface {
	t.Helper()
	panel := newFileSession(t)
	portal := newPortalFilesHarness(t)
	amy := portal.signedIn(t, "amy", amyPassword)

	return []photoSurface{
		{
			name: "panel",
			call: func(method, target, body string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				panel.handler.ServeHTTP(rec, trashRequest(method, target, body, panel.cookie))

				return rec
			},
			seed:   func(t *testing.T, path, content string) { t.Helper(); panel.seed(path, content) },
			root:   panel.root,
			tool:   panel.tool,
			thumbs: panel.thumbs,
			cookie: panel.cookie,
			serve:  panel.handler.ServeHTTP,
		},
		{
			name: "portal",
			call: func(method, target, body string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				portal.portal.ServeHTTP(rec, trashRequest(method, target, body, amy))

				return rec
			},
			seed:   func(t *testing.T, path, content string) { t.Helper(); portal.write(t, "amy", path, content) },
			root:   filepath.Join(portal.base, "amy", "files"),
			tool:   portal.tool,
			thumbs: portal.thumbs,
			cookie: amy,
			serve:  portal.portal.ServeHTTP,
		},
	}
}

func eachPhotoSurface(t *testing.T, run func(t *testing.T, s photoSurface)) {
	t.Helper()
	for _, s := range photoSurfaces(t) {
		t.Run(s.name, func(t *testing.T) { run(t, s) })
	}
}

type photoListing struct {
	Photos []struct {
		Path string    `json:"path"`
		Kind string    `json:"kind"`
		Size int64     `json:"size"`
		Date time.Time `json:"date"`
	} `json:"photos"`
	Next string `json:"next"`
}

func (s photoSurface) list(t *testing.T, query string) photoListing {
	t.Helper()
	rec := s.get("/api/files/photos" + query)
	if rec.Code != http.StatusOK {
		t.Fatalf("list photos got %d: %s", rec.Code, rec.Body)
	}
	var listing photoListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}

	return listing
}

func (l photoListing) paths() []string {
	paths := make([]string, len(l.Photos))
	for i, photo := range l.Photos {
		paths[i] = photo.Path
	}

	return paths
}

func setModTime(t *testing.T, s photoSurface, name string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(s.path(filepath.FromSlash(name)), when, when); err != nil {
		t.Fatal(err)
	}
}

func thumbURL(name string) string { return "/api/files/thumb?path=" + url.QueryEscape(name) }

func previewURL(name string) string { return "/api/files/preview?path=" + url.QueryEscape(name) }

func TestPhotosAreListedNewestFirstWithKindAndSize(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "old.jpg", "12345")
		s.seed(t, "trip/new.HEIC", "xx")
		s.seed(t, "trip/day1/clip.mp4", "xxx")
		s.seed(t, "notes.txt", "text")
		s.seed(t, "empty.jpg", "")
		setModTime(t, s, "old.jpg", time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC))
		setModTime(t, s, "trip/new.HEIC", time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC))
		setModTime(t, s, "trip/day1/clip.mp4", time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC))

		listing := s.list(t, "")
		if got := strings.Join(listing.paths(), ","); got != "trip/new.HEIC,trip/day1/clip.mp4,old.jpg" {
			t.Fatalf("got %s", got)
		}
		if listing.Next != "" {
			t.Fatalf("one page needs no cursor, got %q", listing.Next)
		}
		first := listing.Photos[0]
		if first.Kind != "image" || first.Size != 2 || !first.Date.Equal(time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("bad entry %+v", first)
		}
		if listing.Photos[1].Kind != "video" {
			t.Fatalf("clip.mp4 must be a video: %+v", listing.Photos[1])
		}
		rec := s.get("/api/files/photos")
		if !strings.Contains(rec.Body.String(), `"photos":[`) || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("headers %v body %s", rec.Header(), rec.Body)
		}
	})
}

func TestPhotoPagesFollowTheCursorWithoutGapsOrRepeats(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		for i := range 12 {
			name := fmt.Sprintf("p%02d.jpg", i)
			s.seed(t, name, "x")
			setModTime(t, s, name, time.Date(2020, 1, 1+i/3, 0, 0, 0, 0, time.UTC))
		}
		all := s.list(t, "").paths()
		var paged []string
		cursor := ""
		for range 20 {
			listing := s.list(t, "?limit=5&after="+url.QueryEscape(cursor))
			paged = append(paged, listing.paths()...)
			if listing.Next == "" {
				break
			}
			cursor = listing.Next
		}
		if len(all) != 12 || !slices.Equal(all, paged) {
			t.Fatalf("paging changed the result:\n%v\n%v", paged, all)
		}
	})
}

func TestPhotoListRefusesBadLimitsAndCursors(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a.jpg", "x")
		for _, query := range []string{"?limit=0", "?limit=-1", "?limit=501", "?limit=abc", "?limit=1.5", "?after=nonsense", "?after=x%7Ca", "?limit=%zz", "?after=%zz"} {
			if rec := s.get("/api/files/photos" + query); rec.Code != http.StatusBadRequest {
				t.Errorf("%s got %d, want 400", query, rec.Code)
			}
		}
		if rec := s.get("/api/files/photos?limit=500"); rec.Code != http.StatusOK {
			t.Errorf("limit 500 got %d", rec.Code)
		}
	})
}

func TestPhotoListNeverShowsHiddenFoldersOrSymlinks(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "visible.jpg", "x")
		for _, name := range []string{".thumbs/a.jpg", ".THUMBS/b.jpg", ".trash/c.jpg", ".uploads/d.jpg", "sub/.thumbs/e.jpg", "sub/.Trash/f.jpg"} {
			s.seed(t, name, "x")
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret.jpg"), []byte("secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret.jpg"), s.path("photo.jpg")); err != nil {
			t.Logf("symlinks are not available here: %v", err)
		} else if err := os.Symlink(outside, s.path("linked")); err != nil {
			t.Fatal(err)
		}

		if got := s.list(t, "").paths(); !slices.Equal(got, []string{"visible.jpg"}) {
			t.Fatalf("got %v", got)
		}
	})
}

func decodeJPEG(t *testing.T, rec *httptest.ResponseRecorder) (int, int) {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("body is not a JPEG: %v (%d bytes)", err, rec.Body.Len())
	}

	return img.Bounds().Dx(), img.Bounds().Dy()
}

func TestThumbnailIsAJPEGWithImmutableCachingHeaders(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a b/ünï.jpg", "pixels")

		rec := s.get(thumbURL("a b/ünï.jpg"))
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		if width, _ := decodeJPEG(t, rec); width > system.ThumbSide {
			t.Fatalf("thumbnail is %d wide", width)
		}
		want := map[string]string{
			"Content-Type":            "image/jpeg",
			"X-Content-Type-Options":  "nosniff",
			"Content-Security-Policy": viewPolicy,
			"Cache-Control":           "private, no-cache",
			"Content-Disposition":     "inline",
		}
		for key, value := range want {
			if got := rec.Header().Get(key); got != value {
				t.Errorf("%s = %q, want %q", key, got, value)
			}
		}
		etag := rec.Header().Get("ETag")
		if !strings.HasPrefix(etag, `"`) || len(etag) < 30 {
			t.Fatalf("etag %q", etag)
		}
		if got := s.thumbFiles(); len(got) != 1 || !strings.HasSuffix(got[0], ".t.jpg") {
			t.Fatalf("thumbnail not cached on disk: %v", got)
		}

		again := s.get(thumbURL("a b/ünï.jpg"))
		if again.Code != http.StatusOK || !bytes.Equal(again.Body.Bytes(), rec.Body.Bytes()) || s.tool.Calls() != 1 {
			t.Fatalf("second request must come from the cache: %d, %d renders", again.Code, s.tool.Calls())
		}
		conditional := httptest.NewRecorder()
		req := trashRequest("GET", thumbURL("a b/ünï.jpg"), "", s.cookie)
		req.Header.Set("If-None-Match", etag)
		s.serve(conditional, req)
		if conditional.Code != http.StatusNotModified {
			t.Fatalf("matching ETag got %d", conditional.Code)
		}
	})
}

func TestVideoPostersAndOnlyHeicNeedsAPreview(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "clip.mp4", "movie")
		s.seed(t, "shot.heic", "heic bytes")
		s.seed(t, "plain.jpg", "jpg bytes")
		s.seed(t, "other.HEIF", "heif bytes")

		if rec := s.get(thumbURL("clip.mp4")); rec.Code != http.StatusOK {
			t.Fatalf("poster got %d: %s", rec.Code, rec.Body)
		} else {
			decodeJPEG(t, rec)
		}
		for _, name := range []string{"shot.heic", "other.HEIF"} {
			rec := s.get(previewURL(name))
			if rec.Code != http.StatusOK {
				t.Fatalf("preview of %s got %d: %s", name, rec.Code, rec.Body)
			}
			decodeJPEG(t, rec)
			if rec.Header().Get("Cache-Control") != "private, no-cache" {
				t.Errorf("preview must cache like a thumbnail")
			}
		}
		for _, name := range []string{"plain.jpg", "clip.mp4"} {
			if rec := s.get(previewURL(name)); rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("preview of %s got %d, want 415", name, rec.Code)
			}
		}
		names := s.thumbFiles()
		var previews, small int
		for _, name := range names {
			if strings.HasSuffix(name, ".p.jpg") {
				previews++
			} else {
				small++
			}
		}
		if previews != 2 || small != 1 {
			t.Fatalf("thumbnails and previews are cached under separate names: %v", names)
		}
	})
}

func TestThumbnailRefusesWhatIsNotAPhoto(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "notes.txt", "x")
		s.seed(t, "dir.jpg/inner.txt", "x")
		cases := map[string]int{
			"notes.txt": http.StatusUnsupportedMediaType, "dir.jpg": http.StatusUnsupportedMediaType, "missing.jpg": http.StatusNotFound,
			"nodir/a.jpg": http.StatusNotFound, "": http.StatusBadRequest, "notes.txt/a.jpg": http.StatusUnprocessableEntity,
		}
		for name, want := range cases {
			for _, route := range []string{thumbURL(name), previewURL(name)} {
				got := s.get(route).Code
				if got != want && !(name == "notes.txt/a.jpg" && got == http.StatusNotFound) {
					t.Errorf("%s got %d, want %d", route, got, want)
				}
			}
		}
		if s.tool.Calls() != 0 {
			t.Fatalf("the tool must never run for these: %d", s.tool.Calls())
		}
	})
}

func TestUnreadablePicturesGet422AndAreNotRetriedRightAway(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "broken.jpg", "BAD data, not a picture")
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		s.thumbs.now = func() time.Time { return now }

		for range 3 {
			rec := s.get(thumbURL("broken.jpg"))
			if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "BAD") {
				t.Fatalf("got %d: %s", rec.Code, rec.Body)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("an error must never be cached by the browser")
			}
		}
		if s.tool.Calls() != 1 {
			t.Fatalf("a known bad picture must not run the tool again: %d runs", s.tool.Calls())
		}
		if len(s.thumbFiles()) != 0 {
			t.Fatal("a failed render must leave no thumbnail behind")
		}

		now = now.Add(failureMemory + time.Second)
		s.get(thumbURL("broken.jpg"))
		if s.tool.Calls() != 2 {
			t.Fatalf("after the memory expires it may try again: %d", s.tool.Calls())
		}
	})
}

func TestMissingToolGives503NotACrash(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a.jpg", "x")
		s.tool.Err = fmt.Errorf("%w: vipsthumbnail", system.ErrToolMissing)

		rec := s.get(thumbURL("a.jpg"))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not available") {
			t.Fatalf("got %d: %s", rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "vipsthumbnail") {
			t.Fatal("the tool name belongs in the log, not the response")
		}
		if len(s.thumbFiles()) != 0 {
			t.Fatal("nothing may be cached")
		}

		s.tool.Err = nil
		if rec := s.get(thumbURL("a.jpg")); rec.Code != http.StatusOK {
			t.Fatalf("a missing tool is not remembered as a bad picture: %d", rec.Code)
		}
	})
}

func TestNoThumbnailerConfiguredGives503(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "a.jpg", "x")
	h.portal.thumbs = nil
	amy := h.signedIn(t, "amy", amyPassword)
	if rec := h.get(amy, "/api/files/thumb", "a.jpg"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestSlowToolIsCancelledWithTheRequest(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a.jpg", "x")
		s.tool.Delay = time.Minute

		ctx, cancel := context.WithCancel(context.Background())
		req := trashRequest("GET", thumbURL("a.jpg"), "", s.cookie).WithContext(ctx)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			s.serve(rec, req)
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the handler kept waiting for the tool after the client left")
		}
		if rec.Body.Len() != 0 || len(s.thumbFiles()) != 0 {
			t.Fatalf("a cancelled render must write nothing: %q %v", rec.Body, s.thumbFiles())
		}

		s.tool.Delay = 0
		if rec := s.get(thumbURL("a.jpg")); rec.Code != http.StatusOK {
			t.Fatalf("a cancelled render is not remembered as a failure: %d", rec.Code)
		}
	})
}

func TestConcurrentRequestsForOneThumbnailShareOneRun(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a.jpg", "x")
		s.tool.Delay = 150 * time.Millisecond

		var wg sync.WaitGroup
		codes := make([]int, 12)
		bodies := make([][]byte, 12)
		for i := range codes {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := s.get(thumbURL("a.jpg"))
				codes[i], bodies[i] = rec.Code, rec.Body.Bytes()
			}()
		}
		wg.Wait()
		for i := range codes {
			if codes[i] != http.StatusOK || !bytes.Equal(bodies[i], bodies[0]) {
				t.Fatalf("request %d got %d", i, codes[i])
			}
		}
		if s.tool.Calls() != 1 {
			t.Fatalf("12 requests for one thumbnail ran the tool %d times", s.tool.Calls())
		}
		if got := s.thumbFiles(); len(got) != 1 {
			t.Fatalf("cache holds %v", got)
		}
		s.thumbs.mu.Lock()
		defer s.thumbs.mu.Unlock()
		if len(s.thumbs.flights) != 0 {
			t.Fatal("finished work must not stay registered")
		}
	})
}

func TestWaitersSurviveTheRequestThatStartedTheWork(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "a.jpg", "x")
	h.tool.Delay = 200 * time.Millisecond
	amy := h.signedIn(t, "amy", amyPassword)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := trashRequest("GET", thumbURL("a.jpg"), "", amy).WithContext(leaderCtx)
	go h.portal.ServeHTTP(httptest.NewRecorder(), leader)
	time.Sleep(50 * time.Millisecond)

	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, trashRequest("GET", thumbURL("a.jpg"), "", amy))
		result <- rec
	}()
	time.Sleep(50 * time.Millisecond)
	cancelLeader()

	select {
	case rec := <-result:
		if rec.Code != http.StatusOK {
			t.Fatalf("the waiting request must take over, got %d: %s", rec.Code, rec.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never finished")
	}
}

func TestAtMostTwoToolsRunAtOnceAcrossAllUsers(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.tool.Delay = 60 * time.Millisecond
		var wg sync.WaitGroup
		for i := range 10 {
			name := fmt.Sprintf("p%d.jpg", i)
			s.seed(t, name, name)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if rec := s.get(thumbURL(name)); rec.Code != http.StatusOK {
					t.Errorf("%s got %d", name, rec.Code)
				}
			}()
		}
		wg.Wait()
		if s.tool.Calls() != 10 || s.tool.Peak() != thumbConcurrency {
			t.Fatalf("%d runs with a peak of %d at once, want 10 runs and a peak of %d", s.tool.Calls(), s.tool.Peak(), thumbConcurrency)
		}
	})
}

func TestReplacingAPhotoGivesANewThumbnailAndTheOldOneIsSwept(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a.jpg", "first version")
		first := s.get(thumbURL("a.jpg"))
		if err := os.WriteFile(s.path("a.jpg"), []byte("second, longer version"), 0o644); err != nil {
			t.Fatal(err)
		}
		second := s.get(thumbURL("a.jpg"))

		if first.Header().Get("ETag") == second.Header().Get("ETag") || bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
			t.Fatal("a changed photo must not be served its old thumbnail")
		}
		if s.tool.Calls() != 2 || len(s.thumbFiles()) != 2 {
			t.Fatalf("%d renders, cached %v", s.tool.Calls(), s.thumbFiles())
		}

		files, err := system.NewDirFiles(s.root)
		if err != nil {
			t.Fatal(err)
		}
		defer files.(interface{ Close() error }).Close()
		if removed, err := files.SweepThumbs(time.Now()); err != nil || removed != 1 {
			t.Fatalf("sweep removed %d, %v", removed, err)
		}
		if got := s.thumbFiles(); len(got) != 1 || !strings.HasPrefix(second.Header().Get("ETag"), `"`+strings.TrimSuffix(got[0], ".t.jpg")) {
			t.Fatalf("the sweep must keep the current thumbnail, left %v", got)
		}
	})
}

func TestThumbnailsFollowTheFileIntoTrashAndBack(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "a.jpg", "x")
		s.get(thumbURL("a.jpg"))

		rec := s.call("DELETE", "/api/files?path=a.jpg", "")
		var trashed struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &trashed)
		if rec.Code != http.StatusOK || len(s.list(t, "").Photos) != 0 {
			t.Fatalf("a trashed photo must leave the gallery: %d", rec.Code)
		}
		if rec := s.get(thumbURL("a.jpg")); rec.Code != http.StatusNotFound {
			t.Fatalf("thumbnail of a trashed photo got %d", rec.Code)
		}

		if rec := s.call("POST", "/api/trash/"+trashed.ID+"/restore", ""); rec.Code != http.StatusOK {
			t.Fatalf("restore got %d", rec.Code)
		}
		if got := s.list(t, "").paths(); !slices.Equal(got, []string{"a.jpg"}) {
			t.Fatalf("restored photo missing: %v", got)
		}
		if rec := s.get(thumbURL("a.jpg")); rec.Code != http.StatusOK || s.tool.Calls() != 1 {
			t.Fatalf("restoring keeps size and time, so the cached thumbnail still fits: %d, %d renders", rec.Code, s.tool.Calls())
		}
	})
}

func TestThumbsFolderIsHiddenFromEveryFileRoute(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		s.seed(t, "visible.jpg", "x")
		s.seed(t, "other.txt", "x")
		s.get(thumbURL("visible.jpg"))
		cached := s.thumbFiles()
		if len(cached) != 1 {
			t.Fatalf("setup: %v", cached)
		}
		for _, name := range []string{
			".thumbs", ".THUMBS", ".Thumbs", ".thumbs/" + cached[0], ".thumbs/index.json", ".THUMBS/" + cached[0], "sub/.thumbs", "a/.ThUmBs/b.jpg", ".thumbs/", ".thumbs/../.thumbs",
		} {
			escaped := url.QueryEscape(name)
			checks := map[string]*httptest.ResponseRecorder{
				"list":        s.call("GET", "/api/files?path="+escaped, ""),
				"download":    s.call("GET", "/api/files/download?path="+escaped, ""),
				"view":        s.call("GET", "/api/files/view?path="+escaped, ""),
				"zip":         s.call("GET", "/api/files/zip?path="+escaped, ""),
				"thumb":       s.get(thumbURL(name)),
				"preview":     s.get(previewURL(name)),
				"delete":      s.call("DELETE", "/api/files?path="+escaped, ""),
				"mkdir":       s.call("POST", "/api/files/folder", `{"path":`+quote(name)+`}`),
				"rename-from": s.call("POST", "/api/files/rename", `{"from":`+quote(name)+`,"to":"elsewhere"}`),
				"rename-to":   s.call("POST", "/api/files/rename", `{"from":"other.txt","to":`+quote(name)+`}`),
				"exists":      s.call("POST", "/api/files/exists", `{"paths":[`+quote(name)+`]}`),
				"upload":      s.call("PUT", "/api/files/content?path="+escaped, "planted"),
				"create":      s.call("POST", "/api/uploads", `{"path":`+quote(name)+`,"size":1}`),
			}
			for action, rec := range checks {
				if rec.Code != http.StatusBadRequest {
					t.Errorf("%s %q got %d, want 400", action, name, rec.Code)
				}
			}
		}
		if rec := s.call("GET", "/api/files", ""); strings.Contains(strings.ToLower(rec.Body.String()), "thumbs") {
			t.Fatalf("listing leaks the thumbnail folder: %s", rec.Body)
		}
		if len(s.thumbFiles()) != 1 {
			t.Fatal("a refused request changed the thumbnail folder")
		}
	})
}

func TestHostilePathsNeverReachTheThumbnailRoutes(t *testing.T) {
	s := newFileSession(t)
	s.seed("inside.jpg", "inside")
	if err := os.WriteFile(filepath.Join(s.outside, "secret.jpg"), []byte(secretContent), 0o644); err != nil {
		t.Fatal(err)
	}
	s.plantSymlinks()
	if err := os.Symlink(filepath.Join(s.outside, "secret.jpg"), filepath.Join(s.root, "photo.jpg")); err != nil {
		t.Logf("symlinks are not available here: %v", err)
	}
	extra := map[string]string{"outside jpg": "../outside/secret.jpg", "symlinked photo": "photo.jpg", "through symlinked dir": "escape/secret.jpg", "link to file": "link.txt"}
	for name, raw := range hostilePaths {
		extra[name] = raw
	}

	for name, raw := range extra {
		for _, route := range []string{"/api/files/thumb?path=", "/api/files/preview?path="} {
			rec := s.get(route + raw)
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), secretContent) {
				t.Errorf("%s%s (%s) got %d", route, raw, name, rec.Code)
			}
		}
	}
	if s.tool.Calls() != 0 {
		t.Fatalf("the tool ran %d times for hostile paths", s.tool.Calls())
	}
	entries, _ := os.ReadDir(s.outside)
	for _, entry := range entries {
		if entry.Name() == system.ThumbsDir {
			t.Fatal("a thumbnail folder appeared outside the root")
		}
	}
}

func TestUnusualFileNamesWork(t *testing.T) {
	eachPhotoSurface(t, func(t *testing.T, s photoSurface) {
		names := []string{"-rf.jpg", "--version.heic", "with space.png", "ünï 日本.jpg", strings.Repeat("a", 251) + ".jpg", "a&b=c#d.jpg", "%2e%2e.jpg", "100%.jpg"}
		if runtime.GOOS != "windows" {
			names = append(names, "<script>alert(1)</script>.jpg", "what?.jpg", "quote\"s.jpg")
		}
		for _, name := range names {
			s.seed(t, name, "content of "+name)
		}
		listed := s.list(t, "").paths()
		slices.Sort(listed)
		want := slices.Clone(names)
		slices.Sort(want)
		if !slices.Equal(listed, want) {
			t.Fatalf("listed %q\nwant %q", listed, want)
		}
		for _, name := range names {
			if rec := s.get(thumbURL(name)); rec.Code != http.StatusOK {
				t.Errorf("thumbnail of %q got %d: %s", name, rec.Code, rec.Body)
			}
		}
		if runtime.GOOS != "windows" {
			if err := os.WriteFile(s.path("line\nbreak.jpg"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(s.list(t, "").paths(), "line\nbreak.jpg") {
				t.Fatal("a name no file API accepts must not be listed")
			}
			if rec := s.get(thumbURL("line\nbreak.jpg")); rec.Code != http.StatusBadRequest {
				t.Fatalf("got %d", rec.Code)
			}
		}
	})
}

func TestPortalUsersNeverSeeEachOthersPhotos(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "bob", "private.jpg", "bob only")
	h.write(t, "bob", "trip/private.heic", "bob only")
	h.write(t, "amy", "mine.jpg", "amy only")
	amy := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)

	for _, query := range []string{"", "?user=bob", "?path=../bob/files", "?after=0%7C" + url.QueryEscape("../bob/files/private.jpg")} {
		rec := h.do("GET", "/api/files/photos"+query, "", "192.0.2.10", amy)
		if strings.Contains(rec.Body.String(), "private") || strings.Contains(rec.Body.String(), "bob") {
			t.Fatalf("amy's listing%s leaks: %s", query, rec.Body)
		}
	}
	if rec := h.do("GET", "/api/files/photos", "", "192.0.2.10", bob); !strings.Contains(rec.Body.String(), "private.jpg") || strings.Contains(rec.Body.String(), "mine.jpg") {
		t.Fatalf("bob's own listing: %s", rec.Body)
	}
	h.get(bob, "/api/files/thumb", "private.jpg")

	for _, path := range []string{"../bob/files/private.jpg", "../../bob/files/private.jpg", "bob/files/private.jpg", "/bob/files/private.jpg", "private.jpg", "trip/private.heic", "..\\bob\\files\\private.jpg"} {
		for _, route := range []string{"/api/files/thumb", "/api/files/preview"} {
			rec := h.get(amy, route, path)
			if rec.Code == http.StatusOK {
				t.Errorf("amy got bob's picture through %s?path=%s", route, path)
			}
		}
		if rec := h.do("GET", "/api/files/thumb?user=bob&path="+url.QueryEscape(path), "", "192.0.2.10", amy); rec.Code == http.StatusOK {
			t.Errorf("the user parameter must mean nothing on the portal: %s", path)
		}
	}
	for _, entry := range mustReadDir(t, filepath.Join(h.base, "amy", "files", system.ThumbsDir)) {
		if strings.HasSuffix(entry, ".jpg") {
			t.Fatalf("amy's folder must hold no thumbnail of bob's pictures: %v", entry)
		}
	}
	if h.tool.Calls() != 1 {
		t.Fatalf("only bob's own request may have rendered something, got %d", h.tool.Calls())
	}
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name()
	}

	return names
}

func TestIdenticalPicturesOfTwoUsersKeepSeparateThumbnails(t *testing.T) {
	h := newPortalFilesHarness(t)
	for _, user := range []string{"amy", "bob"} {
		h.write(t, user, "same.jpg", "identical bytes")
		when := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(filepath.Join(h.base, user, "files", "same.jpg"), when, when); err != nil {
			t.Fatal(err)
		}
	}
	h.tool.Delay = 100 * time.Millisecond
	amy := h.signedIn(t, "amy", amyPassword)
	bob := h.signedIn(t, "bob", bobPassword)

	var wg sync.WaitGroup
	for _, cookie := range []*http.Cookie{amy, bob} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.get(cookie, "/api/files/thumb", "same.jpg")
		}()
	}
	wg.Wait()
	if h.tool.Calls() != 2 {
		t.Fatalf("two users must not share one run: %d", h.tool.Calls())
	}
	for _, user := range []string{"amy", "bob"} {
		if entries, _ := os.ReadDir(filepath.Join(h.base, user, "files", system.ThumbsDir)); len(entries) < 1 {
			t.Fatalf("%s has no thumbnail of their own", user)
		}
	}
}

func TestPhotoRoutesNeedASessionAndTheRightMethod(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "amy", "a.jpg", "x")
	for _, route := range []string{"/api/files/photos", "/api/files/thumb?path=a.jpg", "/api/files/preview?path=a.heic"} {
		if rec := h.do("GET", route, "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session got %d", route, rec.Code)
		}
	}
	if rec := h.do("GET", "/photos", "", "192.0.2.10", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("photos page without a session: %d %v", rec.Code, rec.Header())
	}
	amy := h.signedIn(t, "amy", amyPassword)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		for _, route := range []string{"/api/files/photos", "/api/files/thumb?path=a.jpg", "/api/files/preview?path=a.heic"} {
			if rec := h.do(method, route, "", "192.0.2.10", amy); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
				t.Errorf("%s %s got %d", method, route, rec.Code)
			}
		}
	}
	if rec := h.do("GET", "/photos", "", "192.0.2.10", amy); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "photos.js") {
		t.Fatalf("photos page: %d", rec.Code)
	}
	for _, asset := range []string{"/static/photos.js", "/static/viewer.js"} {
		if rec := h.do("GET", asset, "", "192.0.2.10", nil); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("%s got %d", asset, rec.Code)
		}
	}
}

func TestPanelPhotosCanBeScopedToOneUser(t *testing.T) {
	s := newFileSession(t)
	if err := os.WriteFile(filepath.Join(s.usersBase, "amy", "files", "amy-photo.jpg"), []byte("amy"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.seed("root-photo.jpg", "root")
	list := func(query string) photoListing {
		var listing photoListing
		rec := s.get("/api/files/photos" + query)
		if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("list%s got %d: %s", query, rec.Code, rec.Body)
		}

		return listing
	}

	if got := list("").paths(); !slices.Equal(got, []string{"root-photo.jpg"}) {
		t.Fatalf("the panel root: %v", got)
	}
	if got := list("?user=amy").paths(); !slices.Equal(got, []string{"amy-photo.jpg"}) {
		t.Fatalf("amy's files: %v", got)
	}
	if rec := s.get("/api/files/thumb?user=amy&path=amy-photo.jpg"); rec.Code != http.StatusOK {
		t.Fatalf("thumbnail in amy's files: %d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(s.usersBase, "amy", "files", system.ThumbsDir)); err != nil {
		t.Fatal("amy's thumbnail must live in amy's folder")
	}
	if rec := s.get("/api/files/thumb?path=amy-photo.jpg"); rec.Code != http.StatusNotFound {
		t.Fatalf("amy's picture does not exist in the panel root: %d", rec.Code)
	}
	for _, query := range []string{"?user=nobody", "?user=..", "?user=amy/../bob", "?user=%zz", "?user=" + url.QueryEscape("../x")} {
		if rec := s.get("/api/files/photos" + query); rec.Code < 400 || rec.Code >= 500 {
			t.Errorf("%s got %d", query, rec.Code)
		}
	}
	if rec := s.call("DELETE", "/api/files?user=amy&path=amy-photo.jpg", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete in amy's files: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(s.usersBase, "amy", "files", "amy-photo.jpg")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the scoped delete did not remove amy's photo")
	}
}
