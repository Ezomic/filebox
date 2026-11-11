package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/mail"
	"github.com/Ezomic/filebox/internal/system"
)

var guestAddresses atomic.Int32

// Every guest request comes from an address of its own, unless a test means to hit the per-address limits.
func nextGuest() string {
	n := guestAddresses.Add(1)

	return fmt.Sprintf("10.9.%d.%d", n/250, n%250+1)
}

func (h *shareHarness) guestCall(ip, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.RemoteAddr = ip + ":4444"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)

	return rec
}

func startBody(name string, size int, sender, visit string) string {
	body, _ := json.Marshal(map[string]any{"name": name, "size": size, "sender": sender, "visit": visit})

	return string(body)
}

func (h *shareHarness) guestStart(ip, link, body string) *httptest.ResponseRecorder {
	return h.guestCall(ip, "POST", "/"+link+"/uploads", body, "Content-Type", "application/json")
}

func (h *shareHarness) guestChunk(ip, link, id string, offset, total int, data string) *httptest.ResponseRecorder {
	return h.guestCall(ip, "PUT", "/"+link+"/uploads/"+id, data, "Content-Type", "application/octet-stream",
		"Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+len(data)-1, total))
}

func (h *shareHarness) guestFinish(ip, link, id string) *httptest.ResponseRecorder {
	return h.guestCall(ip, "POST", "/"+link+"/uploads/"+id+"/finish", "")
}

type guestUpload struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Visit string `json:"visit"`
}

func (h *shareHarness) mustStart(t *testing.T, link, name string, size int, sender, visit string) guestUpload {
	t.Helper()
	rec := h.guestStart(nextGuest(), link, startBody(name, size, sender, visit))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start %q got %d: %s", name, rec.Code, rec.Body)
	}
	var started guestUpload
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}

	return started
}

func (h *shareHarness) send(t *testing.T, link, name, content, sender, visit string) guestUpload {
	t.Helper()
	started := h.mustStart(t, link, name, len(content), sender, visit)
	if content != "" {
		if rec := h.guestChunk(nextGuest(), link, started.ID, 0, len(content), content); rec.Code != http.StatusOK {
			t.Fatalf("chunk of %q got %d: %s", name, rec.Code, rec.Body)
		}
	}
	if rec := h.guestFinish(nextGuest(), link, started.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("finish of %q got %d: %s", name, rec.Code, rec.Body)
	}

	return started
}

func (h *shareHarness) createUploadLink(cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	return h.do("POST", "/api/upload-links", body, "192.0.2.10", cookie)
}

func (h *shareHarness) mustUploadLink(t *testing.T, cookie *http.Cookie, folder string) uploadLinkView {
	t.Helper()
	rec := h.createUploadLink(cookie, `{"path":`+quote(folder)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload link to %q got %d: %s", folder, rec.Code, rec.Body)
	}
	var view uploadLinkView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}

	return view
}

// smallLink puts a link with a cap of a few bytes straight into the store, since the real choices start at a gigabyte.
func (h *shareHarness) smallLink(t *testing.T, owner, name, folder string, limit int64) {
	t.Helper()
	link := auth.Share{Name: name, Owner: owner, Path: folder, Dir: true, Created: h.now(), Expires: h.now().Add(24 * time.Hour), Kind: auth.UploadLink, Cap: limit}
	if err := h.shares.Create(link, sharesPerOwner); err != nil {
		t.Fatal(err)
	}
}

func (h *shareHarness) read(t *testing.T, user, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.base, user, "files", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("%s/%s: %v", user, name, err)
	}

	return string(data)
}

func (h *shareHarness) ownerLists(t *testing.T, cookie *http.Cookie) (shares []shareView, uploadLinks []uploadLinkView) {
	t.Helper()
	rec := h.do("GET", "/api/shares", "", "192.0.2.10", cookie)
	var body struct {
		Shares      []shareView      `json:"shares"`
		UploadLinks []uploadLinkView `json:"uploadLinks"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}

	return body.Shares, body.UploadLinks
}

func TestUploadLinkCreationChecksTheFolderExpiryAndCap(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	random := regexp.MustCompile(`^[a-z0-9]{10}$`)

	link := h.mustUploadLink(t, h.amy, "docs")
	if !random.MatchString(link.Name) || link.Path != "docs" || link.Cap != 5<<30 || !link.Expires.Equal(shareEpochTime.Add(7*24*time.Hour)) ||
		link.Received != 0 || link.LastUpload != nil || link.Expired {
		t.Fatalf("got %+v", link)
	}
	if stored, _ := h.shares.Get(link.Name); stored.Lang != "en" || !stored.IsUploadLink() || !stored.Dir {
		t.Fatalf("stored %+v", stored)
	}
	var named uploadLinkView
	rec := h.createUploadLink(h.amy, `{"path":"docs","name":"my-own-name","lang":"nl"}`)
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &named) != nil || !random.MatchString(named.Name) {
		t.Fatalf("an upload link name is always random: %d %s", rec.Code, rec.Body)
	}
	if stored, _ := h.shares.Get(named.Name); stored.Lang != "nl" {
		t.Fatalf("the owner's language is kept for the summary emails, got %q", stored.Lang)
	}

	for choice, length := range map[string]time.Duration{"1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour} {
		var view uploadLinkView
		rec := h.createUploadLink(h.amy, `{"path":"docs","expires":`+quote(choice)+`}`)
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &view) != nil || !view.Expires.Equal(shareEpochTime.Add(length)) {
			t.Errorf("expiry %s: %d %s", choice, rec.Code, rec.Body)
		}
	}
	for _, choice := range []string{"never", "2d", "0", "forever", "90d"} {
		if rec := h.createUploadLink(h.amy, `{"path":"docs","expires":`+quote(choice)+`}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), codeShareExpiryInvalid) {
			t.Errorf("expiry %q must be refused, got %d %s", choice, rec.Code, rec.Body)
		}
	}
	for choice, size := range map[string]int64{"1g": 1 << 30, "5g": 5 << 30, "20g": 20 << 30} {
		var view uploadLinkView
		rec := h.createUploadLink(h.amy, `{"path":"docs","cap":`+quote(choice)+`}`)
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &view) != nil || view.Cap != size {
			t.Errorf("cap %s: %d %s", choice, rec.Code, rec.Body)
		}
	}
	for _, choice := range []string{"2g", "100g", "1073741824", "-1", "5G"} {
		if rec := h.createUploadLink(h.amy, `{"path":"docs","cap":`+quote(choice)+`}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), codeUploadLinkCap) {
			t.Errorf("cap %q must be refused, got %d %s", choice, rec.Code, rec.Body)
		}
	}
	for _, folder := range []string{"", "notes.txt", "missing", "docs/a.txt", "../bob/files", "..", "/docs", "docs/", ".trash", "docs/.uploads", "../../_family", "docs/../docs"} {
		if rec := h.createUploadLink(h.amy, `{"path":`+quote(folder)+`}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), codeUploadLinkTarget) {
			t.Errorf("folder %q must be refused, got %d %s", folder, rec.Code, rec.Body)
		}
	}
	if rec := h.createUploadLink(nil, `{"path":"docs"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rec.Code)
	}

	h.symlink(t, filepath.Join(h.base, "bob", "files"), "amy", "to-bob")
	if rec := h.createUploadLink(h.amy, `{"path":"to-bob"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a symlink is never a target, got %d %s", rec.Code, rec.Body)
	}
}

func TestUploadLinksAreListedAndRevokedLikeShareLinks(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mkdir(t, "bob", "inbox")
	h.mustCreate(t, h.amy, "amys-share", "notes.txt")
	link := h.mustUploadLink(t, h.amy, "docs")
	bobs := h.mustUploadLink(t, h.bob, "inbox")

	shares, uploadLinks := h.ownerLists(t, h.amy)
	if len(shares) != 1 || shares[0].Name != "amys-share" || len(uploadLinks) != 1 || uploadLinks[0].Name != link.Name {
		t.Fatalf("got %+v and %+v", shares, uploadLinks)
	}
	if _, bobsOwn := h.ownerLists(t, h.bob); len(bobsOwn) != 1 || bobsOwn[0].Name != bobs.Name {
		t.Fatalf("bob sees only his own: %+v", bobsOwn)
	}

	var admin struct {
		Shares      []adminShareView      `json:"shares"`
		UploadLinks []adminUploadLinkView `json:"uploadLinks"`
	}
	rec := h.do("GET", "/api/admin/shares", "", "192.0.2.10", h.amy)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &admin) != nil || len(admin.Shares) != 1 || len(admin.UploadLinks) != 2 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	for _, seen := range admin.UploadLinks {
		if (seen.Name == bobs.Name) != (seen.Owner == "bob" && seen.Path == "inbox") {
			t.Fatalf("the admin sees owner and folder of every link: %+v", seen)
		}
	}
	if rec := h.do("GET", "/api/admin/shares", "", "192.0.2.10", h.bob); rec.Code != http.StatusForbidden {
		t.Fatalf("got %d", rec.Code)
	}

	if rec := h.do("PUT", "/api/shares/"+link.Name, `{"expires":"never"}`, "192.0.2.10", h.amy); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), codeShareNotFound) {
		t.Fatalf("an upload link keeps its expiry, got %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("DELETE", "/api/shares/"+link.Name, "", "192.0.2.10", h.bob); rec.Code != http.StatusNotFound {
		t.Fatalf("bob cannot revoke amy's link, got %d", rec.Code)
	}
	if rec := h.do("DELETE", "/api/admin/shares/"+bobs.Name, "", "192.0.2.10", h.amy); rec.Code != http.StatusNoContent {
		t.Fatalf("the admin revokes any link, got %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("DELETE", "/api/shares/"+link.Name, "", "192.0.2.10", h.amy); rec.Code != http.StatusNoContent {
		t.Fatalf("the owner revokes their own, got %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{link.Name, bobs.Name} {
		if got := h.guestCall(nextGuest(), "GET", "/"+name, ""); got.Code != http.StatusNotFound {
			t.Fatalf("a revoked link is gone at once, got %d", got.Code)
		}
	}
}

func TestUploadLinksCountTowardTheSameLimitAsShareLinks(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	for i := range sharesPerOwner - 1 {
		if rec := h.create(h.amy, fmt.Sprintf("link-%03d", i), "notes.txt", ""); rec.Code != http.StatusCreated {
			t.Fatalf("link %d got %d", i, rec.Code)
		}
	}
	h.mustUploadLink(t, h.amy, "docs")
	if rec := h.createUploadLink(h.amy, `{"path":"docs"}`); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), codeShareLimit) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.create(h.amy, "one-more", "notes.txt", ""); rec.Code != http.StatusConflict {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestVisitorSendsFilesWithoutSeeingOrReplacingAnything(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	page := h.guestCall(nextGuest(), "GET", "/"+link.Name, "", "Cookie", portalCookieName+"="+h.amy.Value)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "/static/receive.js") || !strings.Contains(page.Header().Get("Content-Security-Policy"), "form-action 'none'") {
		t.Fatalf("got %d %v", page.Code, page.Header())
	}
	if head := h.guestCall(nextGuest(), "HEAD", "/"+link.Name, ""); head.Code != http.StatusOK {
		t.Fatalf("got %d", head.Code)
	}
	info := h.guestCall(nextGuest(), "GET", "/"+link.Name+"/info", "")
	if info.Code != http.StatusOK || strings.TrimSpace(info.Body.String()) != `{"folder":"docs"}` {
		t.Fatalf("a visitor learns the folder's name and nothing else: %d %s", info.Code, info.Body)
	}

	first := h.send(t, link.Name, "hello.txt", "hello world", "", "")
	second := h.send(t, link.Name, "hello.txt", "second", "", "")
	seeded := h.send(t, link.Name, "a.txt", "not alpha", "", "")
	empty := h.send(t, link.Name, "empty.txt", "", "", "")
	if first.Name != "hello.txt" || second.Name != "hello (2).txt" || seeded.Name != "a (2).txt" || empty.Name != "empty.txt" || first.Visit != "" {
		t.Fatalf("got %+v %+v %+v %+v", first, second, seeded, empty)
	}
	for name, want := range map[string]string{"docs/hello.txt": "hello world", "docs/hello (2).txt": "second", "docs/a.txt": "alpha", "docs/a (2).txt": "not alpha", "docs/empty.txt": ""} {
		if got := h.read(t, "amy", name); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}

	_, links := h.ownerLists(t, h.amy)
	if len(links) != 1 || links[0].Received != 4 || links[0].ReceivedBytes != int64(len("hello world")+len("second")+len("not alpha")) || links[0].LastUpload == nil {
		t.Fatalf("got %+v", links)
	}
	for _, rec := range []*httptest.ResponseRecorder{page, info} {
		if _, found := rec.Header()["Set-Cookie"]; found || strings.Contains(rec.Body.String(), "amy") || strings.Contains(rec.Body.String(), h.base) {
			t.Fatalf("a visitor response carries a session or owner detail: %v %s", rec.Header(), rec.Body)
		}
		if rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("lacks hardening headers: %v", rec.Header())
		}
	}
}

func TestVisitorNameKeepsAVisitTogetherAndNeverReusesAFolder(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.mkdir(t, "amy", "keep")
	link := h.mustUploadLink(t, h.amy, "docs")
	other := h.mustUploadLink(t, h.amy, "keep")

	first := h.send(t, link.Name, "x.txt", "x", "sub", "")
	second := h.send(t, link.Name, "y.txt", "y", "someone else", first.Visit)
	if first.Visit == "" || second.Visit != first.Visit {
		t.Fatalf("a named visit must carry on: %+v %+v", first, second)
	}
	for name, want := range map[string]string{"docs/sub (2)/x.txt": "x", "docs/sub (2)/y.txt": "y", "docs/sub/b.txt": "bravo"} {
		if got := h.read(t, "amy", name); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "docs", "sub", "x.txt")); err == nil {
		t.Fatal("a visitor never adds to a folder that was there before")
	}

	h.send(t, link.Name, "a.txt", "1", "Anna", "")
	h.send(t, link.Name, "a.txt", "2", "Anna", "")
	h.send(t, link.Name, "a.txt", "3", "Anna", "not-a-visit-anyone-was-given")
	h.send(t, other.Name, "z.txt", "z", "", first.Visit)
	for name, want := range map[string]string{"docs/Anna/a.txt": "1", "docs/Anna (2)/a.txt": "2", "docs/Anna (3)/a.txt": "3", "keep/z.txt": "z"} {
		if got := h.read(t, "amy", name); got != want {
			t.Errorf("%s holds %q, want %q", name, got, want)
		}
	}

	for sender, folder := range map[string]string{"../../bob": "-..-bob", ".trash": "trash", "  .hidden  ": "hidden", "a\x00b\nc": "abc", "x/y\\z": "x-y-z"} {
		h.send(t, link.Name, "n.txt", "n", sender, "")
		if got := h.read(t, "amy", "docs/"+folder+"/n.txt"); got != "n" {
			t.Errorf("sender %q must land in docs/%s", sender, folder)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.base, "bob", "files", "secret.txt")); err != nil || string(data) != bobSecret {
		t.Fatalf("bob's file changed: %q %v", data, err)
	}
}

func TestVisitorFileNamesAreCleanedOrRefused(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	for raw, want := range map[string]string{
		"../../bob/files/secret.txt": "-..-bob-files-secret.txt",
		"..\\evil.txt":               "-evil.txt",
		".bashrc":                    "bashrc",
		".uploads":                   "uploads",
		"report. ":                   "report",
		"a\x00b.txt":                 "ab.txt",
	} {
		if got := h.send(t, link.Name, raw, "x", "", ""); got.Name != want {
			t.Errorf("%q was stored as %q, want %q", raw, got.Name, want)
		}
	}
	long := h.send(t, link.Name, strings.Repeat("é", 200)+".jpg", "x", "", "")
	if len(long.Name) > maxVisitorFileBytes || !strings.HasSuffix(long.Name, ".jpg") || !utf8.ValidString(long.Name) {
		t.Fatalf("got %q", long.Name)
	}
	for _, raw := range []string{"", ".", "..", "...", " . ", "\x01", " "} {
		rec := h.guestStart(nextGuest(), link.Name, startBody(raw, 1, "", ""))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), codePathInvalid) {
			t.Errorf("%q must be refused, got %d %s", raw, rec.Code, rec.Body)
		}
	}
	for _, body := range []string{`{"name":"a.txt"}`, `{"name":"a.txt","size":-1}`, `{"name":"a.txt","size":"1"}`, `not json`} {
		if rec := h.guestStart(nextGuest(), link.Name, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s must be refused, got %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := h.guestStart(nextGuest(), link.Name, startBody("big.bin", 1<<20+1, "", "")); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("the per-file limit holds, got %d", rec.Code)
	}
}

func TestVisitorCannotTouchUploadsItDidNotStartThroughThisLink(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")
	other := h.mustUploadLink(t, h.amy, "docs")

	own := h.do("POST", "/api/uploads", `{"path":"docs/own.bin","size":3}`, "192.0.2.10", h.amy)
	var ownUpload struct {
		ID string `json:"id"`
	}
	if own.Code != http.StatusCreated || json.Unmarshal(own.Body.Bytes(), &ownUpload) != nil {
		t.Fatalf("got %d %s", own.Code, own.Body)
	}
	started := h.mustStart(t, link.Name, "v.bin", 3, "", "")

	unknown := h.guestCall(nextGuest(), "GET", "/never-existed", "")
	attempts := map[string]*httptest.ResponseRecorder{
		"owner's upload chunk":  h.guestChunk(nextGuest(), link.Name, ownUpload.ID, 0, 3, "abc"),
		"owner's upload finish": h.guestFinish(nextGuest(), link.Name, ownUpload.ID),
		"other link chunk":      h.guestChunk(nextGuest(), other.Name, started.ID, 0, 3, "abc"),
		"other link finish":     h.guestFinish(nextGuest(), other.Name, started.ID),
		"status":                h.guestCall(nextGuest(), "GET", "/"+link.Name+"/uploads/"+started.ID, ""),
		"delete":                h.guestCall(nextGuest(), "DELETE", "/"+link.Name+"/uploads/"+started.ID, ""),
		"chunk with a tail":     h.guestCall(nextGuest(), "PUT", "/"+link.Name+"/uploads/"+started.ID+"/x", "abc"),
		"list":                  h.guestCall(nextGuest(), "GET", "/"+link.Name+"/list", ""),
		"download":              h.guestCall(nextGuest(), "GET", "/"+link.Name+"/download"+query("a.txt"), ""),
		"zip":                   h.guestCall(nextGuest(), "GET", "/"+link.Name+"/zip", ""),
		"start with GET":        h.guestCall(nextGuest(), "GET", "/"+link.Name+"/uploads", ""),
		"delete the link":       h.guestCall(nextGuest(), "DELETE", "/"+link.Name, ""),
		"finish with a tail":    h.guestCall(nextGuest(), "POST", "/"+link.Name+"/uploads/"+started.ID+"/finish/now", ""),
	}
	for label, got := range attempts {
		sameResponse(t, label, unknown, got)
	}

	if rec := h.do("PUT", "/api/uploads/"+ownUpload.ID, "abc", "192.0.2.10", h.amy); rec.Code == http.StatusNotFound {
		t.Fatalf("the owner's upload must be untouched, got %d", rec.Code)
	}
	if rec := h.guestChunk(nextGuest(), link.Name, started.ID, 0, 3, "abc"); rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.guestFinish(nextGuest(), link.Name, started.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if got := h.read(t, "amy", "docs/v.bin"); got != "abc" {
		t.Fatalf("got %q", got)
	}
}

func TestUploadLinkCapCountsUnfinishedUploadsAndAMegabytePerFile(t *testing.T) {
	h := newShareHarnessWith(t, func(deps *PortalDeps) { deps.MaxUpload = 8 << 20 })
	h.seed(t)
	h.smallLink(t, "amy", "small-link", "docs", 5<<20)

	first := h.mustStart(t, "small-link", "a.bin", 3<<20, "", "")
	if rec := h.guestStart(nextGuest(), "small-link", startBody("b.bin", 3<<20, "", "")); rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeUploadLinkFull) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	h.mustStart(t, "small-link", "tiny.txt", 1, "", "")
	data := strings.Repeat("a", 3<<20)
	if rec := h.guestChunk(nextGuest(), "small-link", first.ID, 0, len(data), data); rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.guestFinish(nextGuest(), "small-link", first.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	h.mustStart(t, "small-link", "empty.txt", 0, "", "")
	if rec := h.guestStart(nextGuest(), "small-link", startBody("one-byte.txt", 1, "", "")); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("3 MB received and two tiny files at a megabyte each fill 5 MB, got %d", rec.Code)
	}

	files, err := system.OpenUserFiles(h.base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer closerOf(files)()
	if n, err := files.SweepVisitorUploads(time.Now().Add(time.Minute)); err != nil || n != 2 {
		t.Fatalf("swept %d %v", n, err)
	}
	last := h.mustStart(t, "small-link", "c.bin", 2<<20, "", "")
	link, _ := h.shares.Get("small-link")
	if len(link.Pending) != 1 || link.Pending[last.ID].Size != 2<<20 || link.ReceivedBytes != 3<<20 || link.Charged != 3<<20 {
		t.Fatalf("a swept upload frees its share of the cap: %+v", link)
	}
}

func TestAFloodOfEmptyStartsStaysBounded(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.smallLink(t, "amy", "flooded", "docs", 20<<30)
	before, _ := os.ReadDir(filepath.Join(h.base, "amy", "files", "docs"))

	accepted := 0
	for i := range maxPendingPerLink + 20 {
		rec := h.guestStart(nextGuest(), "flooded", startBody("x.txt", 0, fmt.Sprintf("stranger %d", i), ""))
		switch {
		case rec.Code == http.StatusCreated:
			accepted++
		case rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeUploadLinkFull):
			t.Fatalf("start %d got %d %s", i, rec.Code, rec.Body)
		}
	}
	link, _ := h.shares.Get("flooded")
	if accepted != maxPendingPerLink || len(link.Pending) != maxPendingPerLink || len(link.Visits) != maxPendingPerLink {
		t.Fatalf("accepted %d, pending %d, visits %d", accepted, len(link.Pending), len(link.Visits))
	}
	if after, _ := os.ReadDir(filepath.Join(h.base, "amy", "files", "docs")); len(after) != len(before) {
		t.Fatalf("unfinished visits must leave no folders, docs went from %d to %d entries", len(before), len(after))
	}
	if stored, err := os.ReadFile(filepath.Join(h.dataDir, "uploadlinks.json")); err != nil || len(stored) > 64<<10 {
		t.Fatalf("the stored link must stay small, it is %d bytes (%v)", len(stored), err)
	}
}

func TestUploadLinkFilesAndVisitorFoldersAreCapped(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.smallLink(t, "amy", "busy", "docs", 20<<30)
	visits := map[string]auth.Visit{}
	for i := range maxVisitsPerLink {
		visits[fmt.Sprintf("visit-%d", i)] = auth.Visit{Folder: fmt.Sprintf("docs/guest %d", i), Made: i%2 == 0}
	}
	if _, err := h.shares.UpdateUploadLink("busy", func(link *auth.Share) error {
		link.Visits, link.Received = visits, maxFilesPerLink-2
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	named := h.mustStart(t, "busy", "a.txt", 1, "Newcomer", "")
	if link, _ := h.shares.Get("busy"); len(link.Visits) != maxVisitsPerLink/2+1 || named.Visit == "" {
		t.Fatalf("visits that never got a file make way for new ones, have %d", len(link.Visits))
	}
	h.mustStart(t, "busy", "b.txt", 1, "", "")
	if rec := h.guestStart(nextGuest(), "busy", startBody("c.txt", 1, "", "")); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("received and unfinished files stop at %d, got %d", maxFilesPerLink, rec.Code)
	}

	h.smallLink(t, "amy", "crowded", "docs", 20<<30)
	made := map[string]auth.Visit{}
	for i := range maxVisitsPerLink {
		made[fmt.Sprintf("visit-%d", i)] = auth.Visit{Folder: fmt.Sprintf("docs/crowd %d", i), Made: true}
	}
	if _, err := h.shares.UpdateUploadLink("crowded", func(link *auth.Share) error {
		link.Visits = made
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := h.guestStart(nextGuest(), "crowded", startBody("d.txt", 1, "One too many", "")); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("a link holds at most %d visitor folders, got %d", maxVisitsPerLink, rec.Code)
	}
	h.mustStart(t, "crowded", "e.txt", 1, "", "")
}

func TestAVisitorFolderIsMadeOnlyForAFinishedFile(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")
	docs := filepath.Join(h.base, "amy", "files", "docs")

	started := h.mustStart(t, link.Name, "a.txt", 1, "Anna", "")
	if _, err := os.Stat(filepath.Join(docs, "Anna")); err == nil {
		t.Fatal("the folder must wait for a finished file")
	}
	again := h.mustStart(t, link.Name, "a.txt", 1, "Anna", "")
	if rec := h.guestChunk(nextGuest(), link.Name, again.ID, 0, 1, "2"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if rec := h.guestFinish(nextGuest(), link.Name, again.ID); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	if got := h.read(t, "amy", "docs/Anna (2)/a.txt"); got != "2" {
		t.Fatalf("got %q", got)
	}
	if rec := h.guestChunk(nextGuest(), link.Name, started.ID, 0, 1, "1"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	if rec := h.guestFinish(nextGuest(), link.Name, started.ID); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	if got := h.read(t, "amy", "docs/Anna/a.txt"); got != "1" {
		t.Fatalf("a reserved name stays the first visitor's, got %q", got)
	}

	h.mustStart(t, link.Name, "never.txt", 1, "Ghost", "")
	files, err := system.OpenUserFiles(h.base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer closerOf(files)()
	if _, err := files.SweepVisitorUploads(time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(docs, "Ghost")); err == nil {
		t.Fatal("an upload that never finished leaves no folder")
	}
}

func TestFinishCountsTheFileBeforeItLandsAndTakesItBackOnFailure(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	started := h.mustStart(t, link.Name, "a.txt", 1, "Anna", "")
	if rec := h.guestChunk(nextGuest(), link.Name, started.ID, 0, 1, "x"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}
	blocker := filepath.Join(h.dataDir, "uploadlinks.json.tmp")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if rec := h.guestFinish(nextGuest(), link.Name, started.ID); rec.Code != http.StatusInternalServerError {
		t.Fatalf("a file that cannot be counted must not land, got %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "docs", "Anna")); err == nil {
		t.Fatal("neither the file nor its folder may be there")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if rec := h.guestFinish(nextGuest(), link.Name, started.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("the visitor can finish once the count saves, got %d %s", rec.Code, rec.Body)
	}
	if got := h.read(t, "amy", "docs/Anna/a.txt"); got != "x" {
		t.Fatalf("got %q", got)
	}

	clash := h.mustStart(t, link.Name, "b.txt", 1, "", "")
	h.guestChunk(nextGuest(), link.Name, clash.ID, 0, 1, "y")
	h.write(t, "amy", "docs/b.txt", []byte("owner"))
	if rec := h.guestFinish(nextGuest(), link.Name, clash.ID); rec.Code != http.StatusConflict {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	stored, _ := h.shares.Get(link.Name)
	if stored.Received != 1 || stored.ReceivedBytes != 1 || stored.Charged != minUploadCharge {
		t.Fatalf("a file that did not land is taken back off the count: %+v", stored)
	}
	if _, pending := stored.Pending[clash.ID]; !pending {
		t.Fatal("the upload stays pending")
	}
	if got := h.read(t, "amy", "docs/b.txt"); got != "owner" {
		t.Fatalf("got %q", got)
	}
}

func TestARepeatedStartGivesBackTheSameUpload(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	body := `{"name":"a.txt","size":3,"sender":"Anna","key":"0123456789abcdef"}`
	var first, second guestUpload
	for _, into := range []*guestUpload{&first, &second} {
		rec := h.guestStart(nextGuest(), link.Name, body)
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), into) != nil {
			t.Fatalf("got %d %s", rec.Code, rec.Body)
		}
	}
	if first.ID != second.ID || first.Name != second.Name || first.Visit != second.Visit {
		t.Fatalf("got %+v and %+v", first, second)
	}
	if stored, _ := h.shares.Get(link.Name); len(stored.Pending) != 1 || len(stored.Visits) != 1 {
		t.Fatalf("a repeated start makes nothing new: %+v", stored)
	}
	h.guestChunk(nextGuest(), link.Name, first.ID, 0, 3, "ab")
	var resumed struct {
		Offset int `json:"offset"`
	}
	if rec := h.guestStart(nextGuest(), link.Name, body); json.Unmarshal(rec.Body.Bytes(), &resumed) != nil || resumed.Offset != 2 {
		t.Fatalf("the repeated start tells where the upload stands: %s", rec.Body)
	}
	other := h.mustStart(t, link.Name, "a.txt", 3, "", "")
	if other.ID == first.ID {
		t.Fatal("another key is another upload")
	}
	if rec := h.guestStart(nextGuest(), link.Name, `{"name":"a.txt","size":1,"key":"`+strings.Repeat("k", maxUploadKey+1)+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("an overlong key is refused, got %d", rec.Code)
	}
}

func TestAVisitSurvivesARestart(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")
	first := h.send(t, link.Name, "a.txt", "a", "Anna", "")

	restarted := NewPortal(PortalDeps{
		Users:     h.users,
		OpenFiles: func(user string) (system.Files, error) { return system.OpenUserFiles(h.base, user) },
		MaxUpload: 1 << 20,
		Shares:    h.shares,
	})
	restarted.now = h.now
	h.portal = restarted
	h.send(t, link.Name, "b.txt", "b", "Anna", first.Visit)
	if got := h.read(t, "amy", "docs/Anna/b.txt"); got != "b" {
		t.Fatalf("one visitor's files stay together across a restart, got %q", got)
	}
}

func TestAVisitorLearnsWhenTheLinkClosedDuringTheirUpload(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	var expiring uploadLinkView
	if rec := h.createUploadLink(h.amy, `{"path":"docs","expires":"1d"}`); rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &expiring) != nil {
		t.Fatal(rec.Body)
	}
	revoked := h.mustUploadLink(t, h.amy, "docs")
	beforeChunks := h.mustStart(t, expiring.Name, "a.bin", 4, "", "")
	beforeFinish := h.mustStart(t, revoked.Name, "b.bin", 2, "", "")
	if rec := h.guestChunk(nextGuest(), revoked.Name, beforeFinish.ID, 0, 2, "bb"); rec.Code != http.StatusOK {
		t.Fatal(rec.Body)
	}

	h.advance(25 * time.Hour)
	if rec := h.do("DELETE", "/api/shares/"+revoked.Name, "", "192.0.2.10", h.amy); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	for label, rec := range map[string]*httptest.ResponseRecorder{
		"chunk after expiry":      h.guestChunk(nextGuest(), expiring.Name, beforeChunks.ID, 0, 4, "aaaa"),
		"finish after expiry":     h.guestFinish(nextGuest(), expiring.Name, beforeChunks.ID),
		"finish after revocation": h.guestFinish(nextGuest(), revoked.Name, beforeFinish.ID),
		"chunk after revocation":  h.guestChunk(nextGuest(), revoked.Name, beforeFinish.ID, 2, 4, "bb"),
	} {
		if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), codeUploadLinkClosed) {
			t.Errorf("%s got %d %s", label, rec.Code, rec.Body)
		}
	}

	unknown := h.guestCall(nextGuest(), "GET", "/never-existed", "")
	for label, rec := range map[string]*httptest.ResponseRecorder{
		"another id on the expired link": h.guestFinish(nextGuest(), expiring.Name, strings.Repeat("ab", 16)),
		"another id on the revoked link": h.guestChunk(nextGuest(), revoked.Name, strings.Repeat("cd", 16), 0, 1, "x"),
		"an id of the other link":        h.guestFinish(nextGuest(), revoked.Name, beforeChunks.ID),
		"the page of the expired link":   h.guestCall(nextGuest(), "GET", "/"+expiring.Name, ""),
		"a start on the revoked link":    h.guestStart(nextGuest(), revoked.Name, startBody("c.bin", 1, "", "")),
	} {
		sameResponse(t, label, unknown, rec)
	}
	if _, err := os.Stat(filepath.Join(h.base, "amy", "files", "docs", "b.bin")); err == nil {
		t.Fatal("nothing lands after the link closed")
	}
}

func TestOneHouseholdCanSendHundredsOfFilesFromOneAddress(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.smallLink(t, "amy", "party", "docs", 20<<30)
	home := "198.51.100.70"
	for i := range shareFloodLimit - 20 {
		start := h.guestStart(home, "party", startBody(fmt.Sprintf("IMG_%04d.jpg", i), 2, "", ""))
		var started guestUpload
		if start.Code != http.StatusCreated || json.Unmarshal(start.Body.Bytes(), &started) != nil {
			t.Fatalf("file %d start got %d", i, start.Code)
		}
		for offset := range 2 {
			if rec := h.guestChunk(home, "party", started.ID, offset, 2, "j"); rec.Code != http.StatusOK {
				t.Fatalf("file %d chunk got %d", i, rec.Code)
			}
		}
		if rec := h.guestFinish(home, "party", started.ID); rec.Code != http.StatusNoContent {
			t.Fatalf("file %d finish got %d", i, rec.Code)
		}
	}
	if link, _ := h.shares.Get("party"); link.Received != shareFloodLimit-20 {
		t.Fatalf("got %d files", link.Received)
	}
}

func TestVisitorNamesLoseInvisibleFormatCharacters(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")
	for raw, want := range map[string]string{
		"photo\u202egpj.exe": "photogpj.exe",
		"inv\u200boice.pdf":  "invoice.pdf",
		"\ufeffnotes.txt":    "notes.txt",
		"a\u2066b\u2069.txt": "ab.txt",
	} {
		if got := h.send(t, link.Name, raw, "x", "", ""); got.Name != want {
			t.Errorf("%q was stored as %q, want %q", raw, got.Name, want)
		}
	}
	h.send(t, link.Name, "x.txt", "x", "Anna\u202e\u200d", "")
	if got := h.read(t, "amy", "docs/Anna/x.txt"); got != "x" {
		t.Fatalf("got %q", got)
	}
	if rec := h.guestStart(nextGuest(), link.Name, startBody("\u202e\u200b", 1, "", "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("a name of only format characters is refused, got %d", rec.Code)
	}
}

func TestTheFamilyFolderIsNeverATarget(t *testing.T) {
	family := t.TempDir()
	h := newShareHarnessWith(t, func(deps *PortalDeps) {
		deps.OpenFamily = func() (system.Files, error) { return system.NewDirFiles(family) }
	})
	if err := os.MkdirAll(filepath.Join(h.base, system.FamilyDir), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, folder := range []string{
		"_family", "../_family", "../../_family", "/_family", "_family/", "Family", "family", "../../" + system.FamilyDir + "/x", "..\\..\\_family",
		"%2e%2e/%2e%2e/_family", "docs/../../_family", "C:/_family", "./_family", "../../../_family", "api/family",
	} {
		if rec := h.createUploadLink(h.amy, `{"path":`+quote(folder)+`}`); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%q must be refused, got %d %s", folder, rec.Code, rec.Body)
		}
	}

	h.mkdir(t, "amy", "Family")
	own := h.mustUploadLink(t, h.amy, "Family")
	h.send(t, own.Name, "into-own.txt", "mine", "", "")
	if got := h.read(t, "amy", "Family/into-own.txt"); got != "mine" {
		t.Fatalf("amy's own folder called Family is hers, got %q", got)
	}
	for _, root := range []string{filepath.Join(h.base, system.FamilyDir), family} {
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("nothing may reach the family folder %s: %v", root, entries)
		}
	}
}

func TestUploadLinkCapAndNamesHoldUnderConcurrentStarts(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.smallLink(t, "amy", "busy-link", "docs", 3<<20)
	h.smallLink(t, "amy", "same-name", "docs/sub", 100<<20)
	h.smallLink(t, "amy", "same-place", "docs/sub", 100<<20)

	var accepted atomic.Int32
	var mu sync.Mutex
	ids := map[string]string{}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec := h.guestStart(nextGuest(), "busy-link", startBody(fmt.Sprintf("f%d.bin", i), 30, "", ""))
			switch {
			case rec.Code == http.StatusCreated:
				accepted.Add(1)
			case rec.Code != http.StatusInsufficientStorage:
				t.Errorf("got %d %s", rec.Code, rec.Body)
			}
		}()
		go func() {
			defer wg.Done()
			link := []string{"same-name", "same-place"}[i%2]
			rec := h.guestStart(nextGuest(), link, startBody("photo.jpg", 1, "", ""))
			var started guestUpload
			if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &started) != nil {
				t.Errorf("got %d %s", rec.Code, rec.Body)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if _, taken := ids[started.Name]; taken {
				t.Errorf("two uploads were given %q", started.Name)
			}
			ids[started.Name] = link + "/" + started.ID
		}()
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("a cap of 3 MB takes three small files, took %d", accepted.Load())
	}
	for name, target := range ids {
		link, id, _ := strings.Cut(target, "/")
		h.guestChunk(nextGuest(), link, id, 0, 1, "p")
		if rec := h.guestFinish(nextGuest(), link, id); rec.Code != http.StatusNoContent {
			t.Errorf("%s got %d %s", name, rec.Code, rec.Body)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(h.base, "amy", "files", "docs", "sub")); len(entries) != 13 {
		t.Fatalf("every photo must land under a name of its own, have %d entries", len(entries))
	}
}

func TestVisitorUploadsStopAtTheQuotaAndAFullDisk(t *testing.T) {
	disk := system.NewFakeDisk(system.DiskUsage{Total: 1000, Used: 10, Free: 990})
	h := newShareHarnessWith(t, func(deps *PortalDeps) { deps.Disk = disk })
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	if err := h.users.SetQuota("amy", 1<<20); err != nil {
		t.Fatal(err)
	}
	h.users.SetUsed("amy", 1<<20-10)
	h.portal.space.invalidate()
	if rec := h.guestStart(nextGuest(), link.Name, startBody("big.bin", 20, "", "")); rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeQuotaExceeded) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	fits := h.mustStart(t, link.Name, "small.bin", 5, "", "")

	disk.Set(system.DiskUsage{Total: 1000, Used: 990, Free: 10})
	h.portal.space.invalidate()
	if rec := h.guestStart(nextGuest(), link.Name, startBody("x.bin", 1, "", "")); rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeDiskFull) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.guestChunk(nextGuest(), link.Name, fits.ID, 0, 5, "small"); rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeDiskFull) {
		t.Fatalf("a full disk stops a running upload too, got %d %s", rec.Code, rec.Body)
	}

	disk.Set(system.DiskUsage{Total: 1000, Used: 10, Free: 990})
	h.portal.space.invalidate()
	if rec := h.guestChunk(nextGuest(), link.Name, fits.ID, 0, 5, "small"); rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.guestFinish(nextGuest(), link.Name, fits.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestUnavailableUploadLinksAnswerLikeUnknownOnes(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	for _, folder := range []string{"inbox", "renamed", "deleted", "swapped"} {
		h.mkdir(t, "amy", folder)
	}
	h.mkdir(t, "cleo", "inbox")
	var expiring uploadLinkView
	if rec := h.createUploadLink(h.amy, `{"path":"inbox","expires":"1d"}`); rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &expiring) != nil {
		t.Fatal(rec.Body)
	}
	revoked := h.mustUploadLink(t, h.amy, "inbox")
	renamed := h.mustUploadLink(t, h.amy, "renamed")
	deleted := h.mustUploadLink(t, h.amy, "deleted")
	swapped := h.mustUploadLink(t, h.amy, "swapped")
	cleos := h.mustUploadLink(t, h.signedInAs(t, "cleo", cleoPassword), "inbox")
	h.mustCreate(t, h.amy, "plain-share", "docs")
	inflight := map[string]guestUpload{}
	for _, link := range []uploadLinkView{expiring, revoked, renamed, deleted, swapped, cleos} {
		inflight[link.Name] = h.mustStart(t, link.Name, "part.bin", 3, "", "")
	}

	h.advance(25 * time.Hour)
	if rec := h.do("DELETE", "/api/shares/"+revoked.Name, "", "192.0.2.10", h.amy); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Body)
	}
	amy := filepath.Join(h.base, "amy", "files")
	if err := os.Rename(filepath.Join(amy, "renamed"), filepath.Join(amy, "renamed-later")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(amy, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(amy, "swapped")); err != nil {
		t.Fatal(err)
	}
	h.write(t, "amy", "swapped", []byte("now a file"))
	if err := os.RemoveAll(filepath.Join(h.base, "cleo")); err != nil {
		t.Fatal(err)
	}

	unknown := h.guestCall(nextGuest(), "GET", "/never-existed", "")
	if unknown.Code != http.StatusNotFound || unknown.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("got %d %v", unknown.Code, unknown.Header())
	}
	inflight["never-existed"] = guestUpload{ID: strings.Repeat("ab", 16)}
	closed := map[string]bool{expiring.Name: true, revoked.Name: true}
	names := []string{expiring.Name, revoked.Name, renamed.Name, deleted.Name, swapped.Name, cleos.Name, "never-existed"}
	for _, name := range names {
		id := inflight[name].ID
		for label, got := range map[string]*httptest.ResponseRecorder{
			"page":   h.guestCall(nextGuest(), "GET", "/"+name, ""),
			"info":   h.guestCall(nextGuest(), "GET", "/"+name+"/info", ""),
			"start":  h.guestStart(nextGuest(), name, startBody("new.bin", 1, "Anna", "")),
			"chunk":  h.guestChunk(nextGuest(), name, id, 0, 3, "abc"),
			"finish": h.guestFinish(nextGuest(), name, id),
			"guess":  h.guestFinish(nextGuest(), name, strings.Repeat("cd", 16)),
		} {
			if closed[name] && (label == "chunk" || label == "finish") {
				if got.Code != http.StatusGone {
					t.Errorf("%s %s: the holder of an upload of a closed link hears so, got %d", name, label, got.Code)
				}
				continue
			}
			sameResponse(t, name+" "+label, unknown, got)
		}
	}
	for label, got := range map[string]*httptest.ResponseRecorder{
		"start on a share link": h.guestStart(nextGuest(), "plain-share", startBody("new.bin", 1, "", "")),
		"chunk on a share link": h.guestChunk(nextGuest(), "plain-share", inflight[cleos.Name].ID, 0, 3, "abc"),
	} {
		sameResponse(t, label, unknown, got)
	}
	if _, err := os.Stat(filepath.Join(amy, "renamed-later", "Anna")); err == nil {
		t.Fatal("a link whose folder moved must not follow it")
	}
}

func TestUploadLinkMissesAndFloodsAreLimitedPerAddress(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	guesser := "198.51.100.60"
	for i := range shareMissLimit {
		if rec := h.guestChunk(guesser, link.Name, fmt.Sprintf("%032x", i), 0, 1, "x"); rec.Code != http.StatusNotFound {
			t.Fatalf("guess %d got %d", i, rec.Code)
		}
	}
	if rec := h.guestCall(guesser, "GET", "/"+link.Name+"/info", ""); rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("guessed upload ids count as misses, got %d", rec.Code)
	}

	flooder := "198.51.100.61"
	var last int
	for range shareFloodLimit + 5 {
		last = h.guestStart(flooder, link.Name, startBody("f.bin", 0, "", "")).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("got %d", last)
	}
	if rec := h.guestCall(nextGuest(), "GET", "/"+link.Name+"/info", ""); rec.Code != http.StatusOK {
		t.Fatalf("other addresses are unaffected, got %d", rec.Code)
	}
}

func TestDeletingAnAccountRemovesItsUploadLinks(t *testing.T) {
	h := newShareHarness(t)
	h.mkdir(t, "bob", "inbox")
	link := h.mustUploadLink(t, h.bob, "inbox")
	if err := h.users.Delete("bob"); err != nil {
		t.Fatal(err)
	}
	h.portal.EndSessions("bob")
	if _, found := h.shares.Get(link.Name); found {
		t.Fatal("the upload link must go with the account")
	}
}

type mailHarness struct {
	*shareHarness
	sender   *mail.Fake
	contacts *auth.Contacts
	deps     PortalDeps
	waits    []time.Duration
	runs     []func()
}

func newMailHarness(t *testing.T) *mailHarness {
	t.Helper()
	contacts, err := auth.OpenContacts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := &mail.Fake{}
	h := &mailHarness{sender: sender, contacts: contacts}
	h.shareHarness = newShareHarnessWith(t, func(deps *PortalDeps) {
		deps.Contacts, deps.Mail, deps.PortalURL = contacts, sender, testPortalURL
		h.deps = *deps
	})
	h.record(h.portal)
	h.confirm(t, "amy")

	return h
}

func (h *mailHarness) record(portal *Portal) {
	portal.receive.notices.after = func(wait time.Duration, run func()) {
		h.waits = append(h.waits, wait)
		h.runs = append(h.runs, run)
	}
}

func (h *mailHarness) confirm(t *testing.T, user string) {
	t.Helper()
	if err := h.contacts.Set(user, user+"@example.com"); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.contacts.Confirm(user, user+"@example.com"); !ok || err != nil {
		t.Fatalf("confirm %v %v", ok, err)
	}
}

// restart starts a new portal on the same stores, as the panel does after an update.
func (h *mailHarness) restart() {
	portal := NewPortal(h.deps)
	portal.now = h.now
	h.record(portal)
	h.portal = portal
	portal.ResumeUploadSummaries()
}

func (h *mailHarness) runLast() {
	h.runs[len(h.runs)-1]()
}

func TestUploadLinkSummaryMailIsHourlyAndNamesNoFiles(t *testing.T) {
	h := newMailHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")

	for i := range 3 {
		h.send(t, link.Name, fmt.Sprintf("secret-name-%d.txt", i), "x", "Visitor Vera", "")
	}
	if len(h.waits) != 1 || h.waits[0] != uploadMailSettle || len(h.sender.Sent()) != 0 {
		t.Fatalf("one summary waits a little for more files: %v, %d sent", h.waits, len(h.sender.Sent()))
	}
	h.advance(uploadMailSettle)
	h.runs[0]()
	want := mail.Message{
		To:      "amy@example.com",
		Subject: "3 files arrived in docs",
		Text: "Hello,\n\n3 files arrived through your upload link in the folder \"docs\" on Filebox. Open the folder to see them:\n\n" + testPortalURL +
			"/#docs\n\nWhile files keep arriving, you get at most one email an hour about this link. To stop receiving files, revoke the link under Shared links.\n",
	}
	if sent := h.sender.Sent(); len(sent) != 1 || sent[0] != want {
		t.Fatalf("got %+v", sent)
	}

	h.send(t, link.Name, "later.txt", "x", "", "")
	if len(h.waits) != 2 || h.waits[1] != uploadMailGap {
		t.Fatalf("the next summary waits out the hour: %v", h.waits)
	}
	h.send(t, link.Name, "later-still.txt", "x", "", "")
	if len(h.waits) != 2 {
		t.Fatalf("one summary at a time: %v", h.waits)
	}
	h.advance(uploadMailGap)
	h.runs[1]()
	if sent := h.sender.Sent(); len(sent) != 2 || sent[1].Subject != "2 files arrived in docs" {
		t.Fatalf("got %+v", sent)
	}
	if stored, _ := h.shares.Get(link.Name); stored.Unreported != 0 || !stored.Reported.Equal(h.now()) {
		t.Fatalf("a sent summary is written down: %+v", stored)
	}
	for _, message := range h.sender.Sent() {
		for _, leak := range []string{"secret-name", "Vera", "later", link.Name} {
			if strings.Contains(message.Subject, leak) || strings.Contains(message.Text, leak) {
				t.Fatalf("a summary must not carry %q: %+v", leak, message)
			}
		}
	}
}

func TestUploadLinkSummaryMailFollowsTheOwnersLanguage(t *testing.T) {
	h := newMailHarness(t)
	h.mkdir(t, "amy", "Feest")
	var link uploadLinkView
	if rec := h.createUploadLink(h.amy, `{"path":"Feest","lang":"nl"}`); rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &link) != nil {
		t.Fatal(rec.Body)
	}
	h.send(t, link.Name, "foto.jpg", "x", "", "")
	h.advance(uploadMailSettle)
	h.runs[0]()
	want := mail.Message{
		To:      "amy@example.com",
		Subject: "1 bestand ontvangen in Feest",
		Text: "Hallo,\n\nVia je uploadlink is 1 bestand binnengekomen in de map \"Feest\" op Filebox. Open de map om het te bekijken:\n\n" + testPortalURL +
			"/#Feest\n\nZolang er bestanden binnenkomen, krijg je over deze link hoogstens een e-mail per uur. Wil je niets meer ontvangen, trek de link dan in onder Gedeelde links.\n",
	}
	if sent := h.sender.Sent(); len(sent) != 1 || sent[0] != want {
		t.Fatalf("got %+v", sent)
	}
}

// The file browser reads its #path with decodeURIComponent, which turns %2F back into a slash.
func TestUploadLinkSummaryLinksANestedFolder(t *testing.T) {
	h := newMailHarness(t)
	h.mkdir(t, "amy", "Party photos/2025")
	link := h.mustUploadLink(t, h.amy, "Party photos/2025")
	h.send(t, link.Name, "a.jpg", "x", "", "")
	h.advance(uploadMailSettle)
	h.runs[0]()
	if sent := h.sender.Sent(); len(sent) != 1 || !strings.Contains(sent[0].Text, testPortalURL+"/#Party%20photos%2F2025\n") || sent[0].Subject != "1 file arrived in 2025" {
		t.Fatalf("got %+v", sent)
	}
}

func TestUploadLinkSummaryNeedsMailAndAConfirmedAddress(t *testing.T) {
	h := newMailHarness(t)
	h.mkdir(t, "bob", "inbox")
	bobs := h.mustUploadLink(t, h.bob, "inbox")
	h.send(t, bobs.Name, "a.txt", "x", "", "")
	h.advance(uploadMailSettle)
	h.runs[0]()
	if sent := h.sender.Sent(); len(sent) != 0 {
		t.Fatalf("bob has no confirmed address, got %+v", sent)
	}
	if stored, _ := h.shares.Get(bobs.Name); stored.Unreported != 1 || len(h.waits) != 2 || h.waits[1] != uploadMailGap {
		t.Fatalf("the count waits for the next slot: %+v %v", stored, h.waits)
	}
	h.confirm(t, "bob")
	h.advance(uploadMailGap)
	h.runLast()
	if sent := h.sender.Sent(); len(sent) != 1 || sent[0].To != "bob@example.com" || sent[0].Subject != "1 file arrived in inbox" {
		t.Fatalf("got %+v", sent)
	}

	quiet := newShareHarness(t)
	quiet.portal.receive.notices.after = func(time.Duration, func()) { t.Fatal("without mail nothing is scheduled") }
	quiet.mkdir(t, "amy", "inbox")
	link := quiet.mustUploadLink(t, quiet.amy, "inbox")
	quiet.send(t, link.Name, "a.txt", "x", "", "")
	quiet.portal.ResumeUploadSummaries()
}

func TestAFailedSummaryKeepsItsCountForTheNextHour(t *testing.T) {
	h := newMailHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")
	h.sender.Err = errors.New("mail service down")
	h.send(t, link.Name, "a.txt", "x", "", "")
	h.send(t, link.Name, "b.txt", "x", "", "")
	h.advance(uploadMailSettle)
	h.runs[0]()
	if stored, _ := h.shares.Get(link.Name); stored.Unreported != 2 || !stored.Reported.IsZero() || len(h.waits) != 2 || h.waits[1] != uploadMailGap {
		t.Fatalf("a failed send keeps the count and tries again in an hour: %+v %v", stored, h.waits)
	}

	h.sender.Err = nil
	h.send(t, link.Name, "c.txt", "x", "", "")
	h.advance(uploadMailGap)
	h.runLast()
	h.send(t, link.Name, "d.txt", "x", "", "")
	h.advance(uploadMailGap)
	h.runLast()
	sent := h.sender.Sent()
	if len(sent) != 2 || sent[0].Subject != "3 files arrived in docs" || sent[1].Subject != "1 file arrived in docs" {
		t.Fatalf("got %+v", sent)
	}
}

func TestARestartNeitherDropsNorRepeatsASummary(t *testing.T) {
	h := newMailHarness(t)
	h.seed(t)
	link := h.mustUploadLink(t, h.amy, "docs")
	h.send(t, link.Name, "a.txt", "x", "", "")
	h.send(t, link.Name, "b.txt", "x", "", "")

	h.restart()
	if len(h.waits) != 2 || h.waits[1] != uploadMailSettle {
		t.Fatalf("the restarted portal plans the summary it was waiting for: %v", h.waits)
	}
	h.advance(uploadMailSettle)
	h.runLast()
	if sent := h.sender.Sent(); len(sent) != 1 || sent[0].Subject != "2 files arrived in docs" {
		t.Fatalf("got %+v", sent)
	}

	h.send(t, link.Name, "c.txt", "x", "", "")
	h.restart()
	if h.waits[len(h.waits)-1] != uploadMailGap {
		t.Fatalf("after a restart the hour still counts from the last summary: %v", h.waits)
	}
	h.advance(uploadMailSettle)
	h.runLast()
	if sent := h.sender.Sent(); len(sent) != 1 {
		t.Fatalf("an early wake-up must not send a second summary within the hour: %+v", sent)
	}
	h.advance(uploadMailGap)
	h.runLast()
	if sent := h.sender.Sent(); len(sent) != 2 || sent[1].Subject != "1 file arrived in docs" {
		t.Fatalf("got %+v", sent)
	}
}
