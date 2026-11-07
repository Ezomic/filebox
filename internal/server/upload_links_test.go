package server

import (
	"encoding/json"
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

func TestUploadLinkCapCountsUnfinishedUploads(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.smallLink(t, "amy", "small-link", "docs", 100)

	first := h.mustStart(t, "small-link", "a.bin", 60, "", "")
	if rec := h.guestStart(nextGuest(), "small-link", startBody("b.bin", 50, "", "")); rec.Code != http.StatusInsufficientStorage || !strings.Contains(rec.Body.String(), codeUploadLinkFull) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	h.mustStart(t, "small-link", "b.bin", 40, "", "")
	if rec := h.guestChunk(nextGuest(), "small-link", first.ID, 0, 60, strings.Repeat("a", 60)); rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if rec := h.guestFinish(nextGuest(), "small-link", first.ID); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d", rec.Code)
	}
	if rec := h.guestStart(nextGuest(), "small-link", startBody("c.bin", 1, "", "")); rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("received and unfinished files fill the cap, got %d", rec.Code)
	}

	files, err := system.OpenUserFiles(h.base, "amy")
	if err != nil {
		t.Fatal(err)
	}
	defer closerOf(files)()
	if n, err := files.SweepVisitorUploads(time.Now().Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("swept %d %v", n, err)
	}
	last := h.mustStart(t, "small-link", "c.bin", 40, "", "")
	if link, _ := h.shares.Get("small-link"); len(link.Pending) != 1 || link.Pending[last.ID] != 40 || link.ReceivedBytes != 60 {
		t.Fatalf("a swept upload frees its share of the cap: %+v", link)
	}
}

func TestUploadLinkCapAndNamesHoldUnderConcurrentStarts(t *testing.T) {
	h := newShareHarness(t)
	h.seed(t)
	h.smallLink(t, "amy", "busy-link", "docs", 100)
	h.smallLink(t, "amy", "same-name", "docs/sub", 1<<20)
	h.smallLink(t, "amy", "same-place", "docs/sub", 1<<20)

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
		t.Fatalf("a cap of 100 takes three files of 30, took %d", accepted.Load())
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
	names := []string{expiring.Name, revoked.Name, renamed.Name, deleted.Name, swapped.Name, cleos.Name, "never-existed"}
	for _, name := range names {
		id := inflight[name].ID
		for label, got := range map[string]*httptest.ResponseRecorder{
			"page":   h.guestCall(nextGuest(), "GET", "/"+name, ""),
			"info":   h.guestCall(nextGuest(), "GET", "/"+name+"/info", ""),
			"start":  h.guestStart(nextGuest(), name, startBody("new.bin", 1, "Anna", "")),
			"chunk":  h.guestChunk(nextGuest(), name, id, 0, 3, "abc"),
			"finish": h.guestFinish(nextGuest(), name, id),
		} {
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
	sender *mail.Fake
	waits  []time.Duration
	runs   []func()
}

func newMailHarness(t *testing.T) *mailHarness {
	t.Helper()
	contacts, err := auth.OpenContacts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sender := &mail.Fake{}
	h := &mailHarness{sender: sender}
	h.shareHarness = newShareHarnessWith(t, func(deps *PortalDeps) {
		deps.Contacts, deps.Mail, deps.PortalURL = contacts, sender, testPortalURL
	})
	h.portal.receive.notices.after = func(wait time.Duration, run func()) {
		h.waits = append(h.waits, wait)
		h.runs = append(h.runs, run)
	}
	if err := contacts.Set("amy", "amy@example.com"); err != nil {
		t.Fatal(err)
	}
	if ok, err := contacts.Confirm("amy", "amy@example.com"); !ok || err != nil {
		t.Fatalf("confirm %v %v", ok, err)
	}

	return h
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

func TestUploadLinkSummaryNeedsMailAndAConfirmedAddress(t *testing.T) {
	h := newMailHarness(t)
	h.mkdir(t, "bob", "inbox")
	bobs := h.mustUploadLink(t, h.bob, "inbox")
	h.send(t, bobs.Name, "a.txt", "x", "", "")
	h.runs[0]()
	if sent := h.sender.Sent(); len(sent) != 0 {
		t.Fatalf("bob has no confirmed address, got %+v", sent)
	}

	quiet := newShareHarness(t)
	quiet.portal.receive.notices.after = func(time.Duration, func()) { t.Fatal("without mail nothing is scheduled") }
	quiet.mkdir(t, "amy", "inbox")
	link := quiet.mustUploadLink(t, quiet.amy, "inbox")
	quiet.send(t, link.Name, "a.txt", "x", "", "")
}
