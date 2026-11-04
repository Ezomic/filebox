package server

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

func FuzzShareRoutesStayConfined(f *testing.F) {
	for _, seed := range []string{
		"", "a.txt", "sub", "sub/b.txt", "..", "../outside.txt", "..%2foutside.txt", "%2e%2e/outside.txt", "..\\outside.txt", "/etc/passwd", "a\x00b",
		".trash", ".TRASH/old.txt", ".uploads", ".thumbs/x", "to-bob", "to-bob/secret.txt", "up-link.txt", "sub/../../outside.txt", "sub//b.txt", "./a.txt",
		strings.Repeat("a/", 600), strings.Repeat("..", 100),
	} {
		for op := range uint8(10) {
			f.Add(op, seed, "my-docs")
		}
		f.Add(uint8(10), seed, "my-docs")
		f.Add(uint8(11), seed, "MY-DOCS")
		f.Add(uint8(12), seed, "my-file")
	}
	f.Fuzz(func(t *testing.T, op uint8, text, name string) {
		h := newShareHarness(t)
		h.seed(t)
		h.write(t, "amy", "outside.txt", []byte("amy-outside-share"))
		_ = os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, "amy", "files", "docs", "to-bob"))
		_ = os.Symlink("../outside.txt", filepath.Join(h.base, "amy", "files", "docs", "up-link.txt"))
		h.mustCreate(t, h.amy, "my-docs", "docs")
		h.mustCreate(t, h.amy, "my-file", "notes.txt")

		var rec *httptest.ResponseRecorder
		switch actions := []string{"list", "download", "preview", "zip"}; {
		case op < 8:
			rec = h.visit("GET", "/"+url.PathEscape(name)+"/"+actions[op%4]+query(text))
			if rec.Code >= 500 && runtime.GOOS != "windows" {
				t.Fatalf("%q %q gave a server error %d", name, text, rec.Code)
			}
		default:
			req := httptest.NewRequest("GET", "/x", nil)
			req.URL.Path = "/" + text
			req.URL.RawQuery = ""
			req.RemoteAddr = visitorIP + ":1"
			rec = httptest.NewRecorder()
			h.portal.ServeHTTP(rec, req)
		}

		body := rec.Body.String()
		for _, secret := range []string{bobSecret, "amy-outside-share", "trashed", "partial"} {
			if strings.Contains(body, secret) {
				t.Fatalf("op %d %q %q leaked %q", op, text, name, secret)
			}
		}
		if data, err := os.ReadFile(filepath.Join(h.base, "bob", "files", "secret.txt")); err != nil || string(data) != bobSecret {
			t.Fatalf("bob's file changed: %q %v", data, err)
		}
		if _, found := rec.Header()["Set-Cookie"]; found {
			t.Fatalf("a visitor got a cookie from %q", text)
		}
	})
}

func FuzzShareName(f *testing.F) {
	for _, seed := range []string{"chronos-app", "Chronos-App", "api", "LOGIN", "a", "ab", "a--b", "-a", "a-", "Kelvin", "a\x00b", "%61pi", strings.Repeat("x", 41), "<b>x</b>", "static", "robots.txt"} {
		f.Add(seed)
	}
	base := f.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "amy", "files"), 0o755); err != nil {
		f.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "amy", "files", "n.txt"), []byte("n"), 0o644); err != nil {
		f.Fatal(err)
	}
	portal := fullyEnabledPortal2(f, base)
	reserved := map[string]bool{}
	for segment := range portal.mux.first {
		reserved[segment] = true
	}

	f.Fuzz(func(t *testing.T, raw string) {
		name := normalizeShareName(raw)
		problem := portal.shareNameProblem(name)
		if problem != "" {
			return
		}
		if len(name) < shareNameMin || len(name) > shareNameMax || !shareNamePattern.MatchString(name) || strings.Contains(name, "--") ||
			strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || name != strings.ToLower(name) {
			t.Fatalf("%q was accepted as %q", raw, name)
		}
		if reserved[name] {
			t.Fatalf("%q is a route and was accepted", name)
		}
		if first, _, ok := splitSharePath("/" + name); !ok || first != name {
			t.Fatalf("an accepted name %q must be routable, got %q %v", name, first, ok)
		}
	})
}

func fullyEnabledPortal2(f *testing.F, base string) *Portal {
	f.Helper()
	shares, err := auth.OpenShares(f.TempDir())
	if err != nil {
		f.Fatal(err)
	}

	return NewPortal(PortalDeps{
		Users:      system.NewFakeUsers("amy"),
		OpenFiles:  func(user string) (system.Files, error) { return system.OpenUserFiles(base, user) },
		OpenFamily: func() (system.Files, error) { return system.NewFakeFiles(), nil },
		TV:         &TV{},
		Shares:     shares,
	})
}
