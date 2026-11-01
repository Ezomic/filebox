package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type viewSurface struct {
	name   string
	get    func(target string) *httptest.ResponseRecorder
	head   func(target string) *httptest.ResponseRecorder
	header func(target string, headers map[string]string) *httptest.ResponseRecorder
	seed   func(t *testing.T, path, content string)
	root   string
}

func viewURL(path string) string {
	return "/api/files/view?path=" + url.QueryEscape(path)
}

func viewSurfaces(t *testing.T) []viewSurface {
	t.Helper()
	panel := newFileSession(t)
	portal := newPortalFilesHarness(t)
	amy := portal.signedIn(t, "amy", amyPassword)

	portalCall := func(method, target string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = "192.0.2.10:5555"
		req.AddCookie(amy)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		portal.portal.ServeHTTP(rec, req)

		return rec
	}
	panelCall := func(method, target string, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.RemoteAddr = "192.0.2.10:5555"
		req.AddCookie(panel.cookie)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		rec := httptest.NewRecorder()
		panel.handler.ServeHTTP(rec, req)

		return rec
	}

	return []viewSurface{
		{
			name: "panel",
			get:  func(target string) *httptest.ResponseRecorder { return panelCall("GET", target, nil) },
			head: func(target string) *httptest.ResponseRecorder { return panelCall("HEAD", target, nil) },
			header: func(target string, h map[string]string) *httptest.ResponseRecorder {
				return panelCall("GET", target, h)
			},
			seed: func(t *testing.T, path, content string) { t.Helper(); panel.seed(path, content) },
			root: panel.root,
		},
		{
			name: "portal",
			get:  func(target string) *httptest.ResponseRecorder { return portalCall("GET", target, nil) },
			head: func(target string) *httptest.ResponseRecorder { return portalCall("HEAD", target, nil) },
			header: func(target string, h map[string]string) *httptest.ResponseRecorder {
				return portalCall("GET", target, h)
			},
			seed: func(t *testing.T, path, content string) { t.Helper(); portal.write(t, "amy", path, content) },
			root: filepath.Join(portal.base, "amy", "files"),
		},
	}
}

func eachSurface(t *testing.T, run func(t *testing.T, s viewSurface)) {
	t.Helper()
	for _, s := range viewSurfaces(t) {
		t.Run(s.name, func(t *testing.T) { run(t, s) })
	}
}

func TestViewServesAllowlistedTypesWithFixedHeaders(t *testing.T) {
	cases := map[string]string{
		"a.png": "image/png", "a.jpg": "image/jpeg", "a.jpeg": "image/jpeg", "a.gif": "image/gif",
		"a.webp": "image/webp", "a.avif": "image/avif",
		"a.mp4": "video/mp4", "a.webm": "video/webm", "a.mov": "video/quicktime", "a.m4v": "video/mp4",
		"a.mp3": "audio/mpeg", "a.m4a": "audio/mp4", "a.wav": "audio/wav", "a.ogg": "audio/ogg", "a.flac": "audio/flac",
		"a.pdf": "application/pdf",
		"a.txt": "text/plain; charset=utf-8", "a.md": "text/plain; charset=utf-8", "a.log": "text/plain; charset=utf-8",
		"a.json": "text/plain; charset=utf-8", "a.csv": "text/plain; charset=utf-8",
		"A.PNG": "image/png", "Photo.JpG": "image/jpeg", "evil.html.png": "image/png", "notes.tar.txt": "text/plain; charset=utf-8",
		"ünï cödé 日本.png": "image/png", "dir/inner.mp4": "video/mp4",
	}
	eachSurface(t, func(t *testing.T, s viewSurface) {
		for name, want := range cases {
			s.seed(t, name, "content of "+name)
			rec := s.get(viewURL(name))
			if rec.Code != http.StatusOK || rec.Body.String() != "content of "+name {
				t.Errorf("%q: got %d %q", name, rec.Code, rec.Body)
				continue
			}
			header := rec.Header()
			if header.Get("Content-Type") != want {
				t.Errorf("%q: Content-Type %q, want %q", name, header.Get("Content-Type"), want)
			}
			if !strings.HasPrefix(header.Get("Content-Disposition"), "inline") {
				t.Errorf("%q: Content-Disposition %q", name, header.Get("Content-Disposition"))
			}
			if header.Get("X-Content-Type-Options") != "nosniff" {
				t.Errorf("%q: nosniff missing", name)
			}
			if header.Get("Accept-Ranges") != "bytes" {
				t.Errorf("%q: Accept-Ranges %q", name, header.Get("Accept-Ranges"))
			}
			csp := header.Get("Content-Security-Policy")
			if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "media-src 'self'") {
				t.Errorf("%q: CSP %q", name, csp)
			}
		}
	})
}

func TestViewSandboxesEverythingButPDF(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		s.seed(t, "a.txt", "x")
		s.seed(t, "a.pdf", "x")
		txt := s.get(viewURL("a.txt")).Header().Get("Content-Security-Policy")
		if !strings.HasPrefix(txt, "sandbox;") || !strings.Contains(txt, "frame-ancestors 'none'") {
			t.Errorf("text CSP %q", txt)
		}
		pdf := s.get(viewURL("a.pdf")).Header().Get("Content-Security-Policy")
		if !strings.Contains(pdf, "frame-ancestors 'self'") || strings.Contains(pdf, "script-src") {
			t.Errorf("pdf CSP %q", pdf)
		}
	})
}

func TestViewIgnoresWhatTheFileContains(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		for name, want := range map[string]string{"page.txt": "text/plain; charset=utf-8", "page.png": "image/png", "page.pdf": "application/pdf", "page.html.mp4": "video/mp4"} {
			s.seed(t, name, "<!doctype html><script>alert(1)</script>")
			rec := s.get(viewURL(name))
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != want ||
				rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
				t.Errorf("%q: %d %v", name, rec.Code, rec.Header())
			}
		}
	})
}

func TestViewRefusesTypesOutsideTheAllowlist(t *testing.T) {
	names := []string{
		"a.heic", "a.HEIF", "a.svg", "a.html", "a.htm", "a.xhtml", "a.js", "a.exe", "a.zip", "a.xml", "a.png.exe", "a.svg.png.svg",
		"noext", ".png", "a.", "a.png.", ".txt.", "dot.", "plain.TXT.bak",
	}
	if runtime.GOOS != "windows" {
		names = append(names, "trail.png ", "space.txt  ", "tab.png\t", "a.png ")
	}
	eachSurface(t, func(t *testing.T, s viewSurface) {
		for _, name := range names {
			if runtime.GOOS == "windows" && strings.HasSuffix(name, ".") {
				continue
			}
			s.seed(t, name, "x")
			rec := s.get(viewURL(name))
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%q: got %d, want 415", name, rec.Code)
			}
			apiError(t, rec)
		}
	})
}

func TestViewPathErrorsMatchTheDownloadEndpoint(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		s.seed(t, "dir/f.png", "x")
		s.seed(t, "real.png", "x")
		if err := os.MkdirAll(filepath.Join(s.root, "folder.png"), 0o755); err != nil {
			t.Fatal(err)
		}
		cases := map[string]int{
			"":                                   http.StatusBadRequest,
			"missing.png":                        http.StatusNotFound,
			"dir":                                http.StatusUnprocessableEntity,
			"folder.png":                         http.StatusUnprocessableEntity,
			"real.png/x.png":                     http.StatusUnprocessableEntity,
			"real.png/":                          http.StatusBadRequest,
			"../real.png":                        http.StatusBadRequest,
			"dir/../../real.png":                 http.StatusBadRequest,
			"/real.png":                          http.StatusBadRequest,
			"a\x00.png":                          http.StatusBadRequest,
			`dir\..\real.png`:                    http.StatusBadRequest,
			strings.Repeat("a", 300) + ".png":    http.StatusBadRequest,
			strings.Repeat("d/", 3000) + "x.png": http.StatusBadRequest,
		}
		for path, want := range cases {
			rec := s.get(viewURL(path))
			download := s.get("/api/files/download?path=" + url.QueryEscape(path))
			if rec.Code != want && !(want == http.StatusBadRequest && rec.Code == http.StatusNotFound) {
				t.Errorf("view %q: got %d want %d", path, rec.Code, want)
			}
			if rec.Code != download.Code && !(download.Code == http.StatusOK && rec.Code == http.StatusUnsupportedMediaType) {
				t.Errorf("view %q: %d but download says %d", path, rec.Code, download.Code)
			}
		}
		if rec := s.get("/api/files/view"); rec.Code != http.StatusBadRequest {
			t.Errorf("no query: %d", rec.Code)
		}
		if rec := s.get("/api/files/view?path=%zz"); rec.Code != http.StatusBadRequest {
			t.Errorf("broken escape: %d", rec.Code)
		}
	})
}

func TestViewNeverServesTheUploadsFolder(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		s.seed(t, ".uploads/partial.png", "partial bytes")
		s.seed(t, ".UPLOADS/partial.png", "partial bytes")
		s.seed(t, ".Uploads/partial.txt", "partial bytes")
		for _, path := range []string{
			".uploads/partial.png", ".uploads", ".uploads/", "./.uploads/partial.png", "a/../.uploads/partial.png",
			".Uploads/partial.txt", ".UPLOADS/partial.png", ".uploads//partial.png",
		} {
			rec := s.get(viewURL(path))
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "partial bytes") {
				t.Errorf("%q: got %d %q", path, rec.Code, rec.Body)
			}
		}
	})
}

func TestViewFollowsNoSymlinkOutOfTheRoot(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "secret.png"), []byte("outside secret"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(s.root, "dirlink")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := os.Symlink(filepath.Join(outside, "secret.png"), filepath.Join(s.root, "filelink.png")); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"dirlink/secret.png", "filelink.png", "dirlink"} {
			rec := s.get(viewURL(path))
			if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "outside secret") {
				t.Errorf("%q followed a symlink: %d %q", path, rec.Code, rec.Body)
			}
		}
	})
}

func TestViewRangeRequests(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		s.seed(t, "clip.mp4", "0123456789")

		rec := s.header(viewURL("clip.mp4"), map[string]string{"Range": "bytes=2-5"})
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
			t.Errorf("valid range: %d %q %q", rec.Code, rec.Body, rec.Header().Get("Content-Range"))
		}
		if rec.Header().Get("Content-Type") != "video/mp4" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("range response lost its headers: %v", rec.Header())
		}

		rec = s.header(viewURL("clip.mp4"), map[string]string{"Range": "bytes=7-"})
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "789" {
			t.Errorf("open ended: %d %q", rec.Code, rec.Body)
		}
		rec = s.header(viewURL("clip.mp4"), map[string]string{"Range": "bytes=-3"})
		if rec.Code != http.StatusPartialContent || rec.Body.String() != "789" {
			t.Errorf("suffix: %d %q", rec.Code, rec.Body)
		}
		rec = s.header(viewURL("clip.mp4"), map[string]string{"Range": "bytes=50-60"})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable || rec.Header().Get("Content-Range") != "bytes */10" {
			t.Errorf("out of range: %d %q", rec.Code, rec.Header().Get("Content-Range"))
		}
		rec = s.header(viewURL("clip.mp4"), map[string]string{"Range": "bytes=0-1,4-5"})
		if rec.Code != http.StatusPartialContent || !strings.HasPrefix(rec.Header().Get("Content-Type"), "multipart/byteranges") ||
			!strings.Contains(rec.Body.String(), "01") || !strings.Contains(rec.Body.String(), "45") ||
			rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("multiple ranges: %d %v %q", rec.Code, rec.Header(), rec.Body)
		}
		rec = s.header(viewURL("clip.mp4"), map[string]string{"Range": "bytes=garbage"})
		if rec.Code != http.StatusRequestedRangeNotSatisfiable && rec.Code != http.StatusOK {
			t.Errorf("garbage range: %d", rec.Code)
		}
	})
}

func TestViewHead(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		s.seed(t, "clip.mp4", "0123456789")
		rec := s.head(viewURL("clip.mp4"))
		if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "10" ||
			rec.Header().Get("Content-Type") != "video/mp4" || rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
			rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("head: %d %v", rec.Code, rec.Header())
		}
	})
}

func TestViewTextWithHTMLNameIsPlain(t *testing.T) {
	eachSurface(t, func(t *testing.T, s viewSurface) {
		name := `<img src=x onerror=alert(1)>"q.txt`
		if runtime.GOOS == "windows" {
			name = `img onerror=alert(1) q.txt`
		}
		s.seed(t, name, "hello")
		rec := s.get(viewURL(name))
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" ||
			strings.ContainsAny(rec.Header().Get("Content-Disposition"), "\r\n") {
			t.Errorf("%d %v", rec.Code, rec.Header())
		}
		if got := rec.Header().Values("Content-Disposition"); len(got) != 1 {
			t.Errorf("Content-Disposition %v", got)
		}
	})
}

func TestPortalViewIsPrivateToTheUser(t *testing.T) {
	h := newPortalFilesHarness(t)
	h.write(t, "bob", "private.png", "bob image")
	h.write(t, "bob", "private.txt", "bob text")
	h.write(t, "amy", "mine.png", "amy image")
	amy := h.signedIn(t, "amy", amyPassword)

	if rec := h.get(amy, "/api/files/view", "mine.png"); rec.Code != http.StatusOK || rec.Body.String() != "amy image" {
		t.Fatalf("own file: %d %q", rec.Code, rec.Body)
	}
	for _, path := range []string{
		"private.png", "../bob/files/private.png", "../../bob/files/private.txt", "bob/files/private.png", "/bob/files/private.png",
		"mine.png/../../bob/files/private.png", "..%2fbob%2ffiles%2fprivate.png", "....//bob/files/private.png", "//bob/files/private.png",
	} {
		rec := h.get(amy, "/api/files/view", path)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "bob ") {
			t.Errorf("%q leaked: %d %q", path, rec.Code, rec.Body)
		}
	}
	for _, raw := range []string{
		"..%2Fbob%2Ffiles%2Fprivate.png", "%2e%2e%2Fbob%2Ffiles%2Fprivate.png", "%2e%2e/bob/files/private.png", "%252e%252e%252Fbob%252Ffiles%252Fprivate.png",
	} {
		rec := h.do("GET", "/api/files/view?path="+raw, "", "192.0.2.10", amy)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "bob ") {
			t.Errorf("raw %q leaked: %d %q", raw, rec.Code, rec.Body)
		}
	}

	bob := h.signedIn(t, "bob", bobPassword)
	if rec := h.get(bob, "/api/files/view", "mine.png"); rec.Code != http.StatusNotFound {
		t.Errorf("bob must not see amy's file: %d %q", rec.Code, rec.Body)
	}
}

func TestViewRoutesRequireASession(t *testing.T) {
	panel := newHarness(t)
	panel.signedIn(t)
	for _, cookie := range []*http.Cookie{nil, {Name: cookieName, Value: "not-a-real-session"}} {
		if rec := panel.do("GET", "/api/files/view?path=a.png", "", cookie); rec.Code != http.StatusUnauthorized {
			t.Errorf("panel: %d", rec.Code)
		}
	}
	portal := newPortalFilesHarness(t)
	portal.write(t, "amy", "a.png", "x")
	for _, cookie := range []*http.Cookie{nil, {Name: portalCookieName, Value: "not-a-real-session"}} {
		if rec := portal.do("GET", "/api/files/view?path=a.png", "", "192.0.2.10", cookie); rec.Code != http.StatusUnauthorized {
			t.Errorf("portal: %d", rec.Code)
		}
	}
}
