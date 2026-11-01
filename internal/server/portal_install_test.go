package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"testing"
)

type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

type webManifest struct {
	Name            string         `json:"name"`
	ShortName       string         `json:"short_name"`
	StartURL        string         `json:"start_url"`
	Scope           string         `json:"scope"`
	Display         string         `json:"display"`
	ThemeColor      string         `json:"theme_color"`
	BackgroundColor string         `json:"background_color"`
	Icons           []manifestIcon `json:"icons"`
}

func TestPortalServesTheManifestAndIconsToAnyone(t *testing.T) {
	h := newPortalHarness(t)
	cases := map[string]string{
		"/manifest.webmanifest":  "application/manifest+json",
		"/icon-192.png":          "image/png",
		"/icon-512.png":          "image/png",
		"/icon-maskable-512.png": "image/png",
		"/apple-touch-icon.png":  "image/png",
	}
	for route, kind := range cases {
		rec := h.do("GET", route, "", "192.0.2.10", nil)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), kind) {
			t.Errorf("%s got %d %q", route, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s must be served with nosniff", route)
		}
	}
	for _, route := range []string{"/manifest.webmanifest/", "/static/manifest.webmanifest", "/icon-192.png/x", "/icon-1024.png", "/static/icon-192.png", "/service-worker.js", "/sw.js"} {
		if rec := h.do("GET", route, "", "192.0.2.10", nil); rec.Code == http.StatusOK {
			t.Errorf("%s must not be served", route)
		}
	}
	if rec := h.do("POST", "/manifest.webmanifest", "", "192.0.2.10", nil); rec.Code == http.StatusOK {
		t.Error("the manifest is read only")
	}
}

func TestManifestIsValidAndPointsAtRealIcons(t *testing.T) {
	h := newPortalHarness(t)
	rec := h.do("GET", "/manifest.webmanifest", "", "192.0.2.10", nil)
	var manifest webManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("the manifest is not JSON: %v", err)
	}
	if manifest.Name == "" || manifest.ShortName == "" || manifest.StartURL != "/" || manifest.Scope != "/" || manifest.Display != "standalone" {
		t.Errorf("manifest basics are wrong: %+v", manifest)
	}
	for _, colour := range []string{manifest.ThemeColor, manifest.BackgroundColor} {
		if len(colour) != 7 || colour[0] != '#' {
			t.Errorf("colour %q is not a #rrggbb value", colour)
		}
	}

	seen := map[string]bool{}
	for _, icon := range manifest.Icons {
		if icon.Type != "image/png" || !strings.HasPrefix(icon.Src, "/") {
			t.Errorf("icon %+v needs a PNG type and an absolute path", icon)
			continue
		}
		served := h.do("GET", icon.Src, "", "192.0.2.10", nil)
		decoded, err := png.Decode(bytes.NewReader(served.Body.Bytes()))
		if served.Code != http.StatusOK || err != nil {
			t.Errorf("%s did not serve a PNG: %d %v", icon.Src, served.Code, err)
			continue
		}
		size := decoded.Bounds().Size()
		if icon.Sizes != fmt.Sprintf("%dx%d", size.X, size.Y) || size.X != size.Y {
			t.Errorf("%s declares %s but is %dx%d", icon.Src, icon.Sizes, size.X, size.Y)
		}
		seen[icon.Sizes+"/"+icon.Purpose] = true
	}
	for _, want := range []string{"192x192/any", "512x512/any", "512x512/maskable"} {
		if !seen[want] {
			t.Errorf("the manifest lacks the %s icon", want)
		}
	}
}

func TestManifestHoldsNoUserDataAndNoServiceWorker(t *testing.T) {
	raw, err := fs.ReadFile(webFiles, "web/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"amy", "bob", "token", "session", "@", "serviceWorker", "service_worker"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Errorf("the manifest must stay static, found %q", forbidden)
		}
	}
}

func TestEveryPortalPageDeclaresTheInstallMetadata(t *testing.T) {
	entries, err := fs.ReadDir(webFiles, "web/portal")
	if err != nil {
		t.Fatal(err)
	}
	required := []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`<meta name="theme-color"`,
	}
	for _, entry := range entries {
		if path.Ext(entry.Name()) != ".html" {
			continue
		}
		raw, err := fs.ReadFile(webFiles, "web/portal/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		head := string(raw)[:strings.Index(string(raw), "</head>")]
		for _, want := range required {
			if !strings.Contains(head, want) {
				t.Errorf("%s lacks %s in its head", entry.Name(), want)
			}
		}
	}
}

func TestAdminPanelDoesNotServeThePortalManifestOrBackupPage(t *testing.T) {
	h := newHarness(t)
	for _, route := range []string{"/manifest.webmanifest", "/icon-192.png", "/icon-512.png", "/icon-maskable-512.png", "/apple-touch-icon.png", "/backup", "/static/backup.js", "/static/upload.js"} {
		if rec := h.do("GET", route, "", nil); rec.Code == http.StatusOK {
			t.Errorf("%s got %d on the admin panel", route, rec.Code)
		}
	}
}

func TestBackupPageNeedsASessionAndLinksTheScripts(t *testing.T) {
	h := newPortalHarness(t)
	if rec := h.do("GET", "/backup", "", "192.0.2.10", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("signed out got %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	amy := h.signedIn(t, "amy", amyPassword)
	rec := h.do("GET", "/backup", "", "192.0.2.10", amy)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `accept="image/*,video/*"`) {
		t.Fatalf("signed in got %d", rec.Code)
	}
	for _, script := range []string{"/static/upload.js", "/static/backup.js"} {
		if !strings.Contains(rec.Body.String(), script) {
			t.Errorf("the page does not load %s", script)
		}
		if got := h.do("GET", script, "", "192.0.2.10", nil); got.Code != http.StatusOK {
			t.Errorf("%s got %d", script, got.Code)
		}
	}
}
