package server

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	tvAdminToken = "admin-token-admin-token-1234"
	tvViewToken  = "view-token-view-token-123456"
)

type seen struct {
	method, path, auth, cookie, origin, body string
	header                                   http.Header
}

type fakeChannel struct {
	id, name, logo, group string
	programmes            []string
}

// fakeStreambox records what reaches it and answers like the real one would. The playlist and
// guide are written the way streambox writes them (internal/server/guide.go in Ezomic/streambox),
// so every stream URL in them carries the view token.
type fakeStreambox struct {
	srv      *httptest.Server
	mu       sync.Mutex
	seen     []seen
	channels []fakeChannel
	// beforeLookup, when set, runs before a channel lookup through the admin API is answered.
	beforeLookup func()
}

func newFakeStreambox(t *testing.T) *fakeStreambox {
	t.Helper()
	f := &fakeStreambox{channels: []fakeChannel{
		{id: "news", name: "News", programmes: []string{"Morning news"}},
		{id: "sports", name: "Sports & More", logo: "https://logo.example/sports.png", group: "Sport", programmes: []string{"Match of the day"}},
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /s/{token}/playlist.m3u", f.viewer(f.playlist))
	mux.HandleFunc("GET /s/{token}/epg.xml", f.viewer(f.guide))
	mux.HandleFunc("GET /s/{token}/hls/{id}/{file}", f.viewer(f.hls))
	mux.HandleFunc("GET /api/channels/{id}", f.channel)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":"streambox"}`))
			return
		}
		_, _ = w.Write([]byte("#EXTM3U"))
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seen{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Header.Get("Origin"), string(body), r.Header.Clone()})
		f.mu.Unlock()
		w.Header().Set("Cache-Control", "no-cache")
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeStreambox) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.seen...)
}

func (f *fakeStreambox) list() []fakeChannel {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeChannel(nil), f.channels...)
}

func (f *fakeStreambox) setChannels(channels ...fakeChannel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels = channels
}

func (f *fakeStreambox) viewer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("token") != tvViewToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"wrong token"}`))
			return
		}
		next(w, r)
	}
}

func fakeBaseURL(r *http.Request) string {
	scheme, host, prefix := "http", r.Host, ""
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	if v := r.Header.Get("X-Forwarded-Prefix"); v != "" {
		prefix = v
	}
	return scheme + "://" + host + prefix
}

func (f *fakeStreambox) playlist(w http.ResponseWriter, r *http.Request) {
	base := fakeBaseURL(r) + "/s/" + r.PathValue("token")
	var b strings.Builder
	fmt.Fprintf(&b, "#EXTM3U x-tvg-url=\"%s/epg.xml\"\n", base)
	for _, c := range f.list() {
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" tvg-logo=\"%s\" group-title=\"%s\",%s\n%s/hls/%s/index.m3u8\n",
			c.id, c.name, c.logo, c.group, c.name, base, c.id)
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl")
	_, _ = w.Write([]byte(b.String()))
}

type fakeGuide struct {
	XMLName    xml.Name          `xml:"tv"`
	Generator  string            `xml:"generator-info-name,attr"`
	Channels   []fakeGuideItem   `xml:"channel"`
	Programmes []fakeGuideListed `xml:"programme"`
}

type fakeGuideItem struct {
	ID          string `xml:"id,attr"`
	DisplayName string `xml:"display-name"`
	Icon        *struct {
		Src string `xml:"src,attr"`
	} `xml:"icon,omitempty"`
}

type fakeGuideListed struct {
	Start   string `xml:"start,attr"`
	Stop    string `xml:"stop,attr"`
	Channel string `xml:"channel,attr"`
	Title   string `xml:"title"`
}

func (f *fakeStreambox) guide(w http.ResponseWriter, _ *http.Request) {
	doc := fakeGuide{Generator: "streambox"}
	for _, c := range f.list() {
		item := fakeGuideItem{ID: c.id, DisplayName: c.name}
		if c.logo != "" {
			item.Icon = &struct {
				Src string `xml:"src,attr"`
			}{Src: c.logo}
		}
		doc.Channels = append(doc.Channels, item)
		for _, title := range c.programmes {
			doc.Programmes = append(doc.Programmes, fakeGuideListed{Start: "20261106180000 +0000", Stop: "20261106190000 +0000", Channel: c.id, Title: title})
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	_ = enc.Encode(doc)
}

func (f *fakeStreambox) hls(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.PathValue("file"), ".m3u8") {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXTINF:4.000000,\nseg_00001.ts\n"))
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	_, _ = w.Write([]byte("segment of " + r.PathValue("id")))
}

func (f *fakeStreambox) channel(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+tvAdminToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	hook := f.beforeLookup
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	w.Header().Set("Content-Type", "application/json")
	for _, c := range f.list() {
		if c.id == r.PathValue("id") {
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":%q,"from":"streambox"}`, c.id, c.name)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"error":"channel not found"}`))
}

func (f *fakeStreambox) tv(t *testing.T) *TV {
	t.Helper()
	u, err := url.Parse(f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	shares, err := auth.OpenTVShares(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return newTV(u, tvAdminToken, tvViewToken, shares)
}

type tvAdmin struct {
	handler http.Handler
	cookie  *http.Cookie
}

func newTVAdmin(t *testing.T, tv *TV) *tvAdmin {
	t.Helper()
	return newTVAdminWith(t, Deps{TV: tv})
}

func newTVAdminWith(t *testing.T, deps Deps) *tvAdmin {
	t.Helper()
	store, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps.Host = &system.Fake{}
	h := New(store, deps)
	if err := store.SetInitialPassword(store.SetupToken(), testPassword); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"`+testPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.10:5555"
	h.ServeHTTP(rec, req)
	return &tvAdmin{handler: h, cookie: sessionCookie(t, rec)}
}

func (a *tvAdmin) do(method, path, body, contentType string, signedIn bool, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:5555"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	if signedIn {
		req.AddCookie(a.cookie)
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

func TestTVAdminRoutesNeedASession(t *testing.T) {
	fake := newFakeStreambox(t)
	a := newTVAdmin(t, fake.tv(t))
	for _, path := range []string{"/api/tv/channels", "/api/tv/media", "/api/tv/channels/news/restart"} {
		if rec := a.do("GET", path, "", "", false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := a.do("GET", "/tv", "", "", false); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("page: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("%d requests reached streambox without a session", n)
	}
}

func TestTVAdminProxyAddsTheTokenAndHidesTheBrowser(t *testing.T) {
	fake := newFakeStreambox(t)
	a := newTVAdmin(t, fake.tv(t))

	rec := a.do("POST", "/api/tv/channels", `{"id":"news"}`, "application/json", true,
		"Authorization", "Bearer attacker", "Origin", "https://evil.example", "X-Forwarded-Prefix", "/evil")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"streambox"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	got := fake.requests()
	if len(got) != 1 {
		t.Fatalf("requests: %d", len(got))
	}
	r := got[0]
	if r.method != "POST" || r.path != "/api/channels" || r.body != `{"id":"news"}` {
		t.Errorf("forwarded %+v", r)
	}
	if r.auth != "Bearer "+tvAdminToken {
		t.Errorf("authorization %q", r.auth)
	}
	if r.cookie != "" || r.origin != "" || r.header.Get("X-Forwarded-Prefix") != "" {
		t.Errorf("browser state leaked: cookie=%q origin=%q header=%v", r.cookie, r.origin, r.header)
	}

	for _, c := range []struct{ method, path, want string }{
		{"GET", "/api/tv/channels", "/api/channels"},
		{"GET", "/api/tv/channels/news", "/api/channels/news"},
		{"PUT", "/api/tv/channels/news", "/api/channels/news"},
		{"DELETE", "/api/tv/channels/news", "/api/channels/news"},
		{"POST", "/api/tv/channels/news/restart", "/api/channels/news/restart"},
		{"GET", "/api/tv/media", "/api/media"},
	} {
		body, ct := "", ""
		if c.method == "PUT" {
			body, ct = `{}`, "application/json"
		}
		before := len(fake.requests())
		if rec := a.do(c.method, c.path, body, ct, true); rec.Code != http.StatusOK {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
		if now := fake.requests(); len(now) != before+1 || now[before].path != c.want {
			t.Errorf("%s %s forwarded as %+v, want %s", c.method, c.path, now[before:], c.want)
		}
	}
}

func TestTVAdminProxyRefusesWhatItShould(t *testing.T) {
	fake := newFakeStreambox(t)
	a := newTVAdmin(t, fake.tv(t))

	notFound := []string{
		"/api/tv/healthz", "/api/tv/s/" + tvViewToken + "/playlist.m3u", "/api/tv/channels/UPPER", "/api/tv/channels/a/b",
		"/api/tv/channels/a/restart/x", "/api/tv/media/x", "/api/tv/channels/-x", "/api/tv/channels/%2e%2e", "/api/tv/",
	}
	for _, path := range notFound {
		if rec := a.do("GET", path, "", "", true); rec.Code != http.StatusNotFound && rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if rec := a.do("POST", "/api/tv/channels", "id=x", "application/x-www-form-urlencoded", true); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form post: %d", rec.Code)
	}
	if rec := a.do("PATCH", "/api/tv/channels", `{}`, "application/json", true); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("patch: %d", rec.Code)
	}
	big := strings.Repeat("a", tvMaxBody+10)
	if rec := a.do("POST", "/api/tv/channels", big, "application/json", true); rec.Code == http.StatusOK {
		t.Errorf("oversized body accepted")
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("%d refused requests reached streambox: %+v", n, fake.requests())
	}
}

func TestTVAdminProxyWhenStreamboxIsDown(t *testing.T) {
	fake := newFakeStreambox(t)
	tv := fake.tv(t)
	fake.srv.Close()
	a := newTVAdmin(t, tv)
	rec := a.do("GET", "/api/tv/channels", "", "", true)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not reachable") {
		t.Errorf("got %d %s", rec.Code, rec.Body)
	}
}

func TestTVIsOffWithoutTheAddon(t *testing.T) {
	a := newTVAdmin(t, nil)
	rec := a.do("GET", "/api/tv", "", "", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("status: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/tv", "/api/tv/channels"} {
		if rec := a.do("GET", path, "", "", true); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}

	fake := newFakeStreambox(t)
	on := newTVAdmin(t, fake.tv(t))
	if rec := on.do("GET", "/api/tv", "", "", true); !strings.Contains(rec.Body.String(), `"enabled":true`) {
		t.Errorf("status with add-on: %s", rec.Body)
	}
	if rec := on.do("GET", "/tv", "", "", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Filebox TV</title>") {
		t.Errorf("page: %d", rec.Code)
	}
}

func newTVPortal(t *testing.T, tv *TV) *portalHarness {
	t.Helper()
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword}}
	return &portalHarness{portal: NewPortal(PortalDeps{Verifier: verifier, TV: tv}), verifier: verifier}
}

func (h *portalHarness) stream(path, ip string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.Host = "files.example.com"
	req.RemoteAddr = ip + ":5555"
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)
	return rec
}

func TestPortalStreamsNeedOnlyTheLink(t *testing.T) {
	fake := newFakeStreambox(t)
	h := newTVPortal(t, fake.tv(t))

	for path, want := range map[string]string{
		"/tv/s/" + tvViewToken + "/playlist.m3u":            "/s/" + tvViewToken + "/playlist.m3u",
		"/tv/s/" + tvViewToken + "/epg.xml":                 "/s/" + tvViewToken + "/epg.xml",
		"/tv/s/" + tvViewToken + "/hls/news/index.m3u8":     "/s/" + tvViewToken + "/hls/news/index.m3u8",
		"/tv/s/" + tvViewToken + "/hls/news-2/seg_00012.ts": "/s/" + tvViewToken + "/hls/news-2/seg_00012.ts",
	} {
		before := len(fake.requests())
		rec := h.stream(path, "192.0.2.10", "Cookie", "secret=1", "Authorization", "Bearer x", "X-Forwarded-Prefix", "/evil")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		now := fake.requests()
		if len(now) != before+1 {
			t.Fatalf("%s: %d requests", path, len(now)-before)
		}
		r := now[before]
		if r.path != want {
			t.Errorf("forwarded %s, want %s", r.path, want)
		}
		if r.auth != "" || r.cookie != "" {
			t.Errorf("credentials leaked to streambox: auth=%q cookie=%q", r.auth, r.cookie)
		}
		if r.header.Get("X-Forwarded-Proto") != "https" || r.header.Get("X-Forwarded-Host") != "files.example.com" || r.header.Get("X-Forwarded-Prefix") != "/tv" {
			t.Errorf("forwarded headers %v", r.header)
		}
		if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-cache" {
			t.Errorf("Cache-Control %v", got)
		}
	}
}

func TestPortalStreamsRejectWrongLinksAndBlockGuessers(t *testing.T) {
	fake := newFakeStreambox(t)
	h := newTVPortal(t, fake.tv(t))

	// These guesses look like a view token. Guesses shaped like a share link code answer 404 instead, see tv_shares_test.go.
	for i := 0; i < tvIPFails; i++ {
		if rec := h.stream("/tv/s/wrongtoken"+strings.Repeat("x", i)+"/playlist.m3u", "192.0.2.50"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, rec.Code)
		}
	}
	// Once blocked, even the right link is refused for that address; others are unaffected.
	if rec := h.stream("/tv/s/"+tvViewToken+"/playlist.m3u", "192.0.2.50"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("blocked address: %d", rec.Code)
	}
	if rec := h.stream("/tv/s/"+tvViewToken+"/playlist.m3u", "192.0.2.51"); rec.Code != http.StatusOK {
		t.Errorf("other address: %d", rec.Code)
	}
	if n := len(fake.requests()); n != 1 {
		t.Errorf("%d requests reached streambox, want 1", n)
	}
}

func TestPortalStreamsOnlyServeStreamShapes(t *testing.T) {
	fake := newFakeStreambox(t)
	h := newTVPortal(t, fake.tv(t))
	base := "/tv/s/" + tvViewToken
	for _, path := range []string{
		base + "/hls/news/concat.txt", base + "/hls/news/seg_1.ts", base + "/hls/UPPER/index.m3u8", base + "/hls/-x/index.m3u8",
		base + "/hls/news/../../api/channels", base + "/hls/%2e%2e/news/index.m3u8", base + "/api/channels", base + "/healthz",
		"/tv/api/channels", "/tv/healthz",
	} {
		if rec := h.stream(path, "192.0.2.60"); rec.Code == http.StatusOK {
			t.Errorf("%s served", path)
		}
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("%d requests reached streambox: %+v", n, fake.requests())
	}
}

func TestPortalWithoutTheAddonHasNoStreamRoutes(t *testing.T) {
	h := newTVPortal(t, nil)
	if rec := h.stream("/tv/s/"+tvViewToken+"/playlist.m3u", "192.0.2.10"); rec.Code != http.StatusNotFound {
		t.Errorf("stream: %d", rec.Code)
	}
	cookie := h.signedIn(t, "amy", amyPassword)
	rec := h.do("GET", "/api/tv", "", "192.0.2.10", cookie)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("info: %d %s", rec.Code, rec.Body)
	}
	if rec := h.do("GET", "/tv", "", "192.0.2.10", cookie); rec.Code != http.StatusNotFound {
		t.Errorf("page: %d", rec.Code)
	}
}

func TestPortalShowsThePlaylistLinkOnlyToUsers(t *testing.T) {
	fake := newFakeStreambox(t)
	h := newTVPortal(t, fake.tv(t))

	if rec := h.do("GET", "/api/tv", "", "192.0.2.10", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous info: %d", rec.Code)
	}
	if rec := h.do("GET", "/tv", "", "192.0.2.10", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("anonymous page: %d", rec.Code)
	}

	cookie := h.signedIn(t, "amy", amyPassword)
	req := httptest.NewRequest("GET", "/api/tv", nil)
	req.Host = "files.example.com"
	req.RemoteAddr = "192.0.2.10:5555"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.portal.ServeHTTP(rec, req)
	want := `"playlistUrl":"https://files.example.com/tv/s/` + tvViewToken + `/playlist.m3u"`
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
		t.Errorf("info: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), tvAdminToken) {
		t.Error("admin token in the portal response")
	}
	if rec := h.do("GET", "/tv", "", "192.0.2.10", cookie); rec.Code != http.StatusOK {
		t.Errorf("page: %d", rec.Code)
	}
}

func TestNewTV(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "streambox.env")
	content := "# tokens\nSTREAMBOX_ADMIN_TOKEN=" + tvAdminToken + "\nSTREAMBOX_VIEW_TOKEN=\"" + tvViewToken + "\"\nSTREAMBOX_LISTEN=127.0.0.1:8090\n"
	if err := os.WriteFile(good, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	short := filepath.Join(dir, "short.env")
	if err := os.WriteFile(short, []byte("STREAMBOX_ADMIN_TOKEN=abc\nSTREAMBOX_VIEW_TOKEN=def\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	shares, err := auth.OpenTVShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := NewTV("http://127.0.0.1:8090", good, shares)
	if err != nil || tv.adminToken != tvAdminToken || tv.viewToken != tvViewToken || tv.shares != shares {
		t.Fatalf("good config: %v %+v", err, tv)
	}
	for name, c := range map[string][2]string{
		"remote host":   {"http://example.com:8090", good},
		"lan host":      {"http://192.168.1.5:8090", good},
		"https":         {"https://127.0.0.1:8090", good},
		"with path":     {"http://127.0.0.1:8090/x", good},
		"with userinfo": {"http://u:p@127.0.0.1:8090", good},
		"not a url":     {"::", good},
		"missing file":  {"http://127.0.0.1:8090", filepath.Join(dir, "nope.env")},
		"short tokens":  {"http://127.0.0.1:8090", short},
	} {
		if _, err := NewTV(c[0], c[1], shares); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := NewTV("http://localhost:8090", good, shares); err != nil {
		t.Errorf("localhost: %v", err)
	}
}

var tvForwarded = regexp.MustCompile(`^/s/` + tvViewToken + `/(playlist\.m3u|epg\.xml|hls/[a-z0-9][a-z0-9-]{0,31}/(index\.m3u8|seg_[0-9]{5,}\.ts))$`)

// Whatever the path, streambox only ever sees one of the three stream shapes under the view token.
func FuzzPortalStreamPathsReachOnlyStreams(f *testing.F) {
	for _, seed := range []string{
		"playlist.m3u", "epg.xml", "hls/news/index.m3u8", "hls/news/seg_00001.ts", "hls/../api/channels", "hls/%2e%2e/x/index.m3u8",
		"api/channels", "../healthz", "hls/news/concat.txt", "hls/news/index.m3u8/../../x", "hls//index.m3u8", "playlist.m3u\x00",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, rest string) {
		fake := newFakeStreambox(t)
		h := newTVPortal(t, fake.tv(t))
		req := httptest.NewRequest("GET", "/", nil)
		req.URL = &url.URL{Path: "/tv/s/" + tvViewToken + "/" + rest}
		req.RemoteAddr = "192.0.2.77:5555"
		h.portal.ServeHTTP(httptest.NewRecorder(), req)
		for _, r := range fake.requests() {
			if !tvForwarded.MatchString(r.path) || r.auth != "" {
				t.Fatalf("path %q reached streambox as %q (auth %q)", rest, r.path, r.auth)
			}
		}
	})
}
