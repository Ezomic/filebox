package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Ezomic/filebox/internal/system"
)

const bobSecret = "bob-private-contents"

const fuzzFamily = 0x80

func FuzzPortalFilePathsStayInsideTheUser(f *testing.F) {
	for _, seed := range []string{
		"", "a.txt", "../bob/files/secret.txt", "..%2fbob%2ffiles%2fsecret.txt", "/etc/passwd", `..\bob`,
		"%2e%2e/bob", "a\x00b", "link", "link/secret.txt", "....//bob", "docs/../../bob/files/secret.txt",
		".thumbs", ".THUMBS/x.t.jpg", "a/.thumbs/b", "photo.jpg",
		"../_family/shared.txt", "_family", "../../_family", "shared.txt", ".trash", ".TRASH/x", "a/.uploads", "../outside.txt",
	} {
		for _, scope := range []uint8{0, fuzzFamily} {
			f.Add(scope|0, seed, seed)
			f.Add(scope|3, seed, "x")
			f.Add(scope|6, seed, "x")
			f.Add(scope|7, seed, seed)
			f.Add(scope|8, seed, "x")
			f.Add(scope|9, seed, "x")
			f.Add(scope|10, seed, "x")
			f.Add(scope|11, seed, "x")
			f.Add(scope|12, seed, "x")
			f.Add(scope|13, seed, "x")
			f.Add(scope|14, seed, "x")
			f.Add(scope|15, seed, "x")
		}
	}
	f.Fuzz(func(t *testing.T, op uint8, first, second string) {
		h := newFamilyHarness(t)
		h.write(t, "bob", "secret.txt", bobSecret)
		h.write(t, "amy", "a.txt", "amy-file")
		if err := os.WriteFile(filepath.Join(h.base, "outside.txt"), []byte("outside"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(h.base, system.FamilyDir, "shared.txt"), []byte("family-file"), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, "amy", "files", "link"))
		_ = os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, system.FamilyDir, "link"))
		_ = os.Symlink(filepath.Join(h.base, "amy", "files"), filepath.Join(h.base, system.FamilyDir, "amylink"))
		amy := h.amy
		amyEntries, _ := os.ReadDir(filepath.Join(h.base, "amy", "files"))

		api := "/api"
		if op&fuzzFamily != 0 {
			api = "/api/family"
		}
		var rec *httptest.ResponseRecorder
		switch op & 0x7f % 16 {
		case 0:
			rec = h.get(amy, api+"/files", first)
		case 1:
			rec = h.get(amy, api+"/files/download", first)
		case 2:
			rec = h.call("PUT", api+"/files/content?path="+url.QueryEscape(first), "planted", amy)
		case 3:
			rec = h.do("POST", api+"/files/folder", `{"path":`+quote(first)+`}`, "192.0.2.10", amy)
		case 4:
			rec = h.do("POST", api+"/files/rename", `{"from":`+quote(first)+`,"to":`+quote(second)+`}`, "192.0.2.10", amy)
		case 5:
			rec = h.do("DELETE", api+"/files?path="+url.QueryEscape(first), "", "192.0.2.10", amy)
		case 6:
			rec = h.get(amy, api+"/files/zip", first)
		case 7:
			rec = h.do("POST", api+"/files/exists", `{"paths":[`+quote(first)+`,`+quote(second)+`]}`, "192.0.2.10", amy)
		case 8:
			rec = h.do("POST", api+"/uploads", `{"path":`+quote(first)+`,"size":1}`, "192.0.2.10", amy)
		case 9:
			rec = h.do("POST", api+"/trash/"+url.PathEscape(first)+"/restore", "", "192.0.2.10", amy)
		case 10:
			rec = h.do("DELETE", api+"/trash/"+url.PathEscape(first), "", "192.0.2.10", amy)
		case 11:
			rec = h.do("DELETE", api+"/trash?user="+url.QueryEscape(first), "", "192.0.2.10", amy)
		case 12:
			rec = h.do("DELETE", api+"/files?path="+url.QueryEscape(first), "", "192.0.2.10", amy)
			var trashed struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &trashed) == nil && trashed.ID != "" {
				rec = h.do("POST", api+"/trash/"+trashed.ID+"/restore", "", "192.0.2.10", amy)
			}
		case 13:
			rec = h.get(amy, api+"/files/thumb", first)
		case 14:
			rec = h.get(amy, api+"/files/preview", first)
		case 15:
			rec = h.do("GET", api+"/files/photos?limit=500&after="+url.QueryEscape(first), "", "192.0.2.10", amy)
		}

		if rec.Code >= 500 && runtime.GOOS != "windows" {
			t.Fatalf("op %#x %q %q gave a server error %d", op, first, second, rec.Code)
		}
		if strings.Contains(rec.Body.String(), bobSecret) || strings.Contains(rec.Body.String(), "outside") {
			t.Fatalf("op %#x %q %q leaked a file from outside its root", op, first, second)
		}
		if op&fuzzFamily == 0 && strings.Contains(rec.Body.String(), "family-file") {
			t.Fatalf("op %#x %q %q reached the family folder from a home endpoint", op, first, second)
		}
		if data, err := os.ReadFile(filepath.Join(h.base, "bob", "files", "secret.txt")); err != nil || string(data) != bobSecret {
			t.Fatalf("op %#x %q %q damaged bob's file: %q %v", op, first, second, data, err)
		}
		if entries, err := os.ReadDir(filepath.Join(h.base, "bob", "files")); err != nil || len(entries) != 1 {
			t.Fatalf("op %#x %q %q changed bob's folder: %v %v", op, first, second, entries, err)
		}
		if data, err := os.ReadFile(filepath.Join(h.base, "outside.txt")); err != nil || string(data) != "outside" {
			t.Fatalf("op %#x %q %q damaged a file at the data root: %q %v", op, first, second, data, err)
		}
		if entries, err := os.ReadDir(h.base); err != nil || len(entries) != 4 {
			t.Fatalf("op %#x %q %q created something at the data root: %v %v", op, first, second, entries, err)
		}
		if op&fuzzFamily != 0 {
			if data, err := os.ReadFile(filepath.Join(h.base, "amy", "files", "a.txt")); err != nil || string(data) != "amy-file" {
				t.Fatalf("op %#x %q %q reached amy's home from the family folder: %q %v", op, first, second, data, err)
			}
			if entries, err := os.ReadDir(filepath.Join(h.base, "amy", "files")); err != nil || len(entries) != len(amyEntries) {
				t.Fatalf("op %#x %q %q changed amy's home from the family folder: %v %v", op, first, second, entries, err)
			}
		} else if data, err := os.ReadFile(filepath.Join(h.base, system.FamilyDir, "shared.txt")); err != nil || string(data) != "family-file" {
			t.Fatalf("op %#x %q %q damaged the family file from a home endpoint: %q %v", op, first, second, data, err)
		}
	})
}

func FuzzPortalLoginBody(f *testing.F) {
	for _, seed := range []string{
		`{"username":"amy","password":"` + amyPassword + `"}`, `{}`, ``, `[`, `{"username":1}`, `{"username":"amy"}`,
		`{"username":"../bob","password":"x"}`, `{"username":"amy","password":"` + strings.Repeat("a", 5000) + `"}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		h := newPortalFilesHarness(t)
		rec := h.do("POST", "/api/login", body, "192.0.2.10", nil)
		if rec.Code >= 500 {
			t.Fatalf("body %q gave a server error %d", body, rec.Code)
		}
		if rec.Code == 200 || rec.Code == 204 {
			if !strings.Contains(body, "amy") {
				t.Fatalf("body %q signed someone in", body)
			}
		}
	})
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20:
			b.WriteString(`\u00`)
			b.WriteString(string("0123456789abcdef"[r>>4]))
			b.WriteString(string("0123456789abcdef"[r&0xf]))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')

	return b.String()
}

func FuzzPortalAdminRequests(f *testing.F) {
	for _, seed := range []string{
		`{"role":"admin","confirm":"` + amyPassword + `"}`, `{"confirm":"` + amyPassword + `"}`, `{"role":"user"}`, `{}`, ``, `[`,
		`{"password":"` + newPassword + `","confirm":"` + bobPassword + `"}`, `{"role":1,"confirm":5}`,
		`{"bytes":10485760,"confirm":"` + amyPassword + `"}`, `{"bytes":1e9}`, `{"bytes":"10GB","confirm":"x"}`,
	} {
		f.Add(seed, "cleo", uint8(0))
	}
	f.Fuzz(func(t *testing.T, body, name string, op uint8) {
		h := newPortalHarness(t)
		amy := h.signedIn(t, "amy", amyPassword)
		method, suffix := "PUT", "/role"
		switch op % 5 {
		case 1:
			suffix = "/password"
		case 2:
			method, suffix = "DELETE", ""
		case 3:
			suffix = "/quota"
		case 4:
			method, suffix = "DELETE", "/quota"
		}
		rec := h.do(method, "/api/admin/users/"+url.PathEscape(name)+suffix, body, "192.0.2.10", amy)
		if rec.Code >= 500 {
			t.Fatalf("%s %q %q gave a server error %d", method, name, body, rec.Code)
		}
		if !strings.Contains(body, amyPassword) && !strings.Contains(body, `\`) && (h.spy.mutations != 0 || h.roles.IsAdmin("cleo") || h.roles.IsAdmin("bob")) {
			t.Fatalf("%s %q %q changed something without the admin's password", method, name, body)
		}
		if !h.roles.IsAdmin("amy") {
			t.Fatalf("%s %q %q removed the only admin", method, name, body)
		}
	})
}
