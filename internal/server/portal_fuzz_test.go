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
)

const bobSecret = "bob-private-contents"

func FuzzPortalFilePathsStayInsideTheUser(f *testing.F) {
	for _, seed := range []string{
		"", "a.txt", "../bob/files/secret.txt", "..%2fbob%2ffiles%2fsecret.txt", "/etc/passwd", `..\bob`,
		"%2e%2e/bob", "a\x00b", "link", "link/secret.txt", "....//bob", "docs/../../bob/files/secret.txt",
	} {
		f.Add(uint8(0), seed, seed)
		f.Add(uint8(3), seed, "x")
		f.Add(uint8(6), seed, "x")
		f.Add(uint8(7), seed, seed)
		f.Add(uint8(8), seed, "x")
		f.Add(uint8(9), seed, "x")
		f.Add(uint8(10), seed, "x")
		f.Add(uint8(11), seed, "x")
		f.Add(uint8(12), seed, "x")
	}
	f.Fuzz(func(t *testing.T, op uint8, first, second string) {
		h := newPortalFilesHarness(t)
		h.write(t, "bob", "secret.txt", bobSecret)
		h.write(t, "amy", "a.txt", "amy-file")
		_ = os.Symlink(filepath.Join(h.base, "bob", "files"), filepath.Join(h.base, "amy", "files", "link"))
		amy := h.signedIn(t, "amy", amyPassword)

		var rec *httptest.ResponseRecorder
		switch op % 13 {
		case 0:
			rec = h.get(amy, "/api/files", first)
		case 1:
			rec = h.get(amy, "/api/files/download", first)
		case 2:
			rec = h.upload(amy, first, "planted")
		case 3:
			rec = h.do("POST", "/api/files/folder", `{"path":`+quote(first)+`}`, "192.0.2.10", amy)
		case 4:
			rec = h.do("POST", "/api/files/rename", `{"from":`+quote(first)+`,"to":`+quote(second)+`}`, "192.0.2.10", amy)
		case 5:
			rec = h.do("DELETE", "/api/files?path="+url.QueryEscape(first), "", "192.0.2.10", amy)
		case 6:
			rec = h.get(amy, "/api/files/zip", first)
		case 7:
			rec = h.do("POST", "/api/files/exists", `{"paths":[`+quote(first)+`,`+quote(second)+`]}`, "192.0.2.10", amy)
		case 8:
			rec = h.do("POST", "/api/uploads", `{"path":`+quote(first)+`,"size":1}`, "192.0.2.10", amy)
		case 9:
			rec = h.do("POST", "/api/trash/"+url.PathEscape(first)+"/restore", "", "192.0.2.10", amy)
		case 10:
			rec = h.do("DELETE", "/api/trash/"+url.PathEscape(first), "", "192.0.2.10", amy)
		case 11:
			rec = h.do("DELETE", "/api/trash?user="+url.QueryEscape(first), "", "192.0.2.10", amy)
		case 12:
			rec = h.do("DELETE", "/api/files?path="+url.QueryEscape(first), "", "192.0.2.10", amy)
			var trashed struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(rec.Body.Bytes(), &trashed) == nil && trashed.ID != "" {
				rec = h.do("POST", "/api/trash/"+trashed.ID+"/restore", "", "192.0.2.10", amy)
			}
		}

		if rec.Code >= 500 && runtime.GOOS != "windows" {
			t.Fatalf("op %d %q %q gave a server error %d", op%13, first, second, rec.Code)
		}
		if strings.Contains(rec.Body.String(), bobSecret) {
			t.Fatalf("op %d %q %q leaked bob's file", op%13, first, second)
		}
		if data, err := os.ReadFile(filepath.Join(h.base, "bob", "files", "secret.txt")); err != nil || string(data) != bobSecret {
			t.Fatalf("op %d %q %q damaged bob's file: %q %v", op%13, first, second, data, err)
		}
		if entries, err := os.ReadDir(filepath.Join(h.base, "bob", "files")); err != nil || len(entries) != 1 {
			t.Fatalf("op %d %q %q changed bob's folder: %v %v", op%13, first, second, entries, err)
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
