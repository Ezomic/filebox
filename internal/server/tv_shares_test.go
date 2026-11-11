package server

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const testPortalHost = "files.example.com"

var (
	tvShareCodeShape = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-[abcdefghjkmnpqrstuvwxyz23456789]{8}$`)
	tvFail2ban       = regexp.MustCompile(`portal: (login|password change|tv) (failed|blocked) (user=.* )?ip=(\S+)$`)
)

type tvShareJSON struct {
	Code    string    `json:"code"`
	Label   string    `json:"label"`
	Channel string    `json:"channel"`
	URL     string    `json:"url"`
	Created time.Time `json:"created"`
}

type tvShareSetup struct {
	fake   *fakeStreambox
	tv     *TV
	admin  *tvAdmin
	portal *portalHarness
}

func newTVShareSetup(t *testing.T) *tvShareSetup {
	t.Helper()
	fake := newFakeStreambox(t)
	tv := fake.tv(t)
	admin := newTVAdminWith(t, Deps{TV: tv, PortalHost: func() string { return testPortalHost }})

	return &tvShareSetup{fake: fake, tv: tv, admin: admin, portal: newTVPortal(t, tv)}
}

func shareBody(label, channel, slug string) string {
	body, _ := json.Marshal(map[string]string{"label": label, "channel": channel, "slug": slug})
	return string(body)
}

func (s *tvShareSetup) post(body string) *httptest.ResponseRecorder {
	return s.admin.do("POST", "/api/tv/shares", body, "application/json", true)
}

func (s *tvShareSetup) create(t *testing.T, label, channel, slug string) tvShareJSON {
	t.Helper()
	rec := s.post(shareBody(label, channel, slug))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %q %q %q: %d %s", label, channel, slug, rec.Code, rec.Body)
	}
	var link tvShareJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &link); err != nil {
		t.Fatal(err)
	}

	return link
}

func (s *tvShareSetup) list(t *testing.T) []tvShareJSON {
	t.Helper()
	rec := s.admin.do("GET", "/api/tv/shares", "", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Shares []tvShareJSON `json:"shares"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	return body.Shares
}

func (s *tvShareSetup) revoke(code string) *httptest.ResponseRecorder {
	return s.admin.do("DELETE", "/api/tv/shares/"+url.PathEscape(code), "", "", true)
}

func (s *tvShareSetup) play(code, rest, ip string) *httptest.ResponseRecorder {
	return s.portal.stream("/tv/s/"+url.PathEscape(code)+"/"+rest, ip)
}

func assertNoViewToken(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rec.Body.String(), tvViewToken) {
		t.Errorf("%s: the view token is in the body", what)
	}
	for name, values := range rec.Header() {
		for _, value := range values {
			if strings.Contains(value, tvViewToken) {
				t.Errorf("%s: the view token is in the %s header", what, name)
			}
		}
	}
}

func suffixes(list ...string) func() (string, error) {
	next := 0

	return func() (string, error) {
		suffix := list[next%len(list)]
		next++

		return suffix, nil
	}
}

func TestTVShareLinksNeedAnAdminSession(t *testing.T) {
	s := newTVShareSetup(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/tv/shares", ""},
		{"POST", "/api/tv/shares", shareBody("Mum's TV", "news", "news")},
		{"DELETE", "/api/tv/shares/news-k7p3x9qm", ""},
	} {
		if rec := s.admin.do(c.method, c.path, c.body, "application/json", false); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if got := s.tv.shares.All(); len(got) != 0 {
		t.Errorf("created without a session: %+v", got)
	}
	if n := len(s.fake.requests()); n != 0 {
		t.Errorf("%d requests reached streambox without a session", n)
	}
}

func TestTVShareLinkCreateListAndRevoke(t *testing.T) {
	s := newTVShareSetup(t)

	link := s.create(t, "Mum's TV", "news", "news")
	if !tvShareCodeShape.MatchString(link.Code) || !strings.HasPrefix(link.Code, "news-") || len(link.Code) != len("news-")+8 {
		t.Errorf("code %q", link.Code)
	}
	if link.Label != "Mum's TV" || link.Channel != "news" || link.Created.IsZero() {
		t.Errorf("link %+v", link)
	}
	if link.URL != "https://"+testPortalHost+"/tv/s/"+link.Code+"/playlist.m3u" {
		t.Errorf("url %q", link.URL)
	}
	if got := s.list(t); len(got) != 1 || got[0].Code != link.Code || got[0].Label != link.Label || got[0].Channel != link.Channel ||
		got[0].URL != link.URL || !got[0].Created.Equal(link.Created) {
		t.Errorf("list %+v, want %+v", got, link)
	}
	if rec := s.play(link.Code, "playlist.m3u", "192.0.2.10"); rec.Code != http.StatusOK {
		t.Fatalf("playlist: %d %s", rec.Code, rec.Body)
	}

	if rec := s.revoke(link.Code); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	if got := s.list(t); len(got) != 0 {
		t.Errorf("list after revoke %+v", got)
	}
	for _, rest := range []string{"playlist.m3u", "epg.xml", "hls/news/index.m3u8", "hls/news/seg_00001.ts"} {
		if rec := s.play(link.Code, rest, "192.0.2.11"); rec.Code != http.StatusNotFound {
			t.Errorf("%s after revoke: %d", rest, rec.Code)
		}
	}
	if rec := s.revoke(link.Code); rec.Code != http.StatusNotFound {
		t.Errorf("second revoke: %d", rec.Code)
	}

	other := s.create(t, "Dad's TV", "news", "news")
	if rec := s.revoke(strings.ToUpper(other.Code)); rec.Code != http.StatusNoContent {
		t.Errorf("revoke in capitals: %d", rec.Code)
	}
}

func TestTVShareLinkSlugs(t *testing.T) {
	s := newTVShareSetup(t)
	for slug, prefix := range map[string]string{
		"holiday-film":          "holiday-film-",
		"Holiday-Film":          "holiday-film-",
		"  news  ":              "news-",
		"a":                     "a-",
		"2026":                  "2026-",
		strings.Repeat("x", 24): strings.Repeat("x", 24) + "-",
	} {
		link := s.create(t, "Mum's TV", "news", slug)
		if !strings.HasPrefix(link.Code, prefix) || len(link.Code) != len(prefix)+8 || !tvShareCodeShape.MatchString(link.Code) {
			t.Errorf("slug %q gave code %q", slug, link.Code)
		}
	}

	before := len(s.tv.shares.All())
	for _, slug := range []string{
		"", "   ", "-", "---", "-news", "news-", "news--film", strings.Repeat("x", 25), "holiday film", "holiday_film",
		"holiday/film", "%2e%2e", "../news", "hölliday", "Kids", "ｎｅｗｓ", "news\x00", "ne\nws", "<b>x</b>", "news.m3u", "news-é",
	} {
		rec := s.post(shareBody("Mum's TV", "news", slug))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("slug %q: %d %s", slug, rec.Code, rec.Body)
		}
	}
	if got := len(s.tv.shares.All()); got != before {
		t.Errorf("refused slugs created %d links", got-before)
	}
}

func TestTVShareLinkLabels(t *testing.T) {
	s := newTVShareSetup(t)
	for label, stored := range map[string]string{
		"Mum's TV":                  "Mum's TV",
		"  Oma  ":                   "Oma",
		"Grand-mère 📺":              "Grand-mère 📺",
		strings.Repeat("é", 60):     strings.Repeat("é", 60),
		"<script>alert(1)</script>": "<script>alert(1)</script>",
		"Family 👨‍👩‍👧":              "Family 👨‍👩‍👧",
	} {
		if link := s.create(t, label, "news", "news"); link.Label != stored {
			t.Errorf("label %q stored as %q", label, link.Label)
		}
	}

	before := len(s.tv.shares.All())
	for _, label := range []string{
		"", "   ", "\t", "Mum\nTV", "a\x00b", "a\x7fb", strings.Repeat("x", 61), strings.Repeat("é", 61),
		"​", "​‍", "‮VT s'muM", "Mum⁦'s TV", "a‏b",
	} {
		if rec := s.post(shareBody(label, "news", "news")); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("label %q: %d %s", label, rec.Code, rec.Body)
		}
	}
	if got := len(s.tv.shares.All()); got != before {
		t.Errorf("refused labels created %d links", got-before)
	}
}

func TestTVShareLinkChannels(t *testing.T) {
	s := newTVShareSetup(t)
	for _, channel := range []string{"", " news", "News", "NEWS", "../news", "news/..", "news%2f", "-news", strings.Repeat("a", 33), "news\x00"} {
		if rec := s.post(shareBody("Mum's TV", channel, "news")); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("channel %q: %d %s", channel, rec.Code, rec.Body)
		}
	}
	if n := len(s.fake.requests()); n != 0 {
		t.Errorf("malformed channel ids reached streambox: %+v", s.fake.requests())
	}

	if rec := s.post(shareBody("Mum's TV", "ghost", "ghost")); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no such channel") {
		t.Errorf("a channel streambox does not have: %d %s", rec.Code, rec.Body)
	}
	got := s.fake.requests()
	if len(got) != 1 || got[0].method != "GET" || got[0].path != "/api/channels/ghost" || got[0].auth != "Bearer "+tvAdminToken {
		t.Errorf("channel check sent %+v", got)
	}
	if n := len(s.tv.shares.All()); n != 0 {
		t.Errorf("%d links created for channels that do not exist", n)
	}

	s.fake.srv.Close()
	if rec := s.post(shareBody("Mum's TV", "news", "news")); rec.Code != http.StatusBadGateway {
		t.Errorf("streambox down: %d %s", rec.Code, rec.Body)
	}
}

func TestTVShareLinkRequestBodies(t *testing.T) {
	s := newTVShareSetup(t)
	if rec := s.admin.do("POST", "/api/tv/shares", "label=x&channel=news&slug=news", "application/x-www-form-urlencoded", true); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form body: %d", rec.Code)
	}
	for _, body := range []string{
		`{"label":5,"channel":"news","slug":"news"}`, `{"label":"x","channel":["news"],"slug":"news"}`, `{"label":"x","channel":"news","slug":true}`,
		`[]`, `"news"`, `{`, `{"label":"` + strings.Repeat("x", maxBodyBytes) + `","channel":"news","slug":"news"}`,
	} {
		if rec := s.post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %.60q: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := s.post(`null`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("null body: %d", rec.Code)
	}
	if n := len(s.tv.shares.All()); n != 0 {
		t.Errorf("%d links created from bad bodies", n)
	}

	rec := s.post(`{"label":"Mum's TV","channel":"news","slug":"news","code":"chosen-aaaaaaaa","suffix":"aaaaaaaa"}`)
	var link tvShareJSON
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &link) != nil || link.Code == "chosen-aaaaaaaa" || link.Code == "news-aaaaaaaa" {
		t.Errorf("a code from the request was used: %d %s", rec.Code, rec.Body)
	}
}

func TestTVShareLinksWithTheSameSlugAreSeparateLinks(t *testing.T) {
	s := newTVShareSetup(t)
	mum, dad := s.create(t, "Mum's TV", "news", "news"), s.create(t, "Dad's TV", "news", "news")
	if mum.Code == dad.Code {
		t.Fatalf("both got %q", mum.Code)
	}
	if rec := s.revoke(mum.Code); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if rec := s.play(dad.Code, "playlist.m3u", "192.0.2.10"); rec.Code != http.StatusOK {
		t.Errorf("revoking one link stopped the other: %d", rec.Code)
	}
}

func TestTVShareLinkCodesAreNeverGivenOutTwice(t *testing.T) {
	s := newTVShareSetup(t)
	s.tv.newSuffix = suffixes("k7p3x9qm")
	first := s.create(t, "Mum's TV", "news", "news")
	if first.Code != "news-k7p3x9qm" {
		t.Fatalf("code %q", first.Code)
	}
	if rec := s.revoke(first.Code); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}

	s.tv.newSuffix = suffixes("k7p3x9qm", "k7p3x9qm", "abcdefgh")
	if again := s.create(t, "Mum's TV", "news", "news"); again.Code != "news-abcdefgh" {
		t.Errorf("after a revoke the code was %q", again.Code)
	}

	s.tv.newSuffix = suffixes("k7p3x9qm")
	if rec := s.post(shareBody("Mum's TV", "news", "news")); rec.Code != http.StatusInternalServerError {
		t.Errorf("no free code: %d %s", rec.Code, rec.Body)
	}
	if got := s.list(t); len(got) != 1 || got[0].Code != "news-abcdefgh" {
		t.Errorf("links %+v", got)
	}
}

func TestTVShareLinkSuffixesUseTheWholeAlphabetAndNoLookalikes(t *testing.T) {
	tv := newFakeStreambox(t).tv(t)
	shape := regexp.MustCompile(`^[abcdefghjkmnpqrstuvwxyz23456789]{8}$`)
	seen := map[rune]bool{}
	for range 2000 {
		suffix, err := tv.newSuffix()
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(suffix) {
			t.Fatalf("suffix %q", suffix)
		}
		for _, r := range suffix {
			seen[r] = true
		}
	}
	if len(seen) != len("abcdefghjkmnpqrstuvwxyz23456789") {
		t.Errorf("only %d letters ever used", len(seen))
	}
}

func TestDeletingAChannelRemovesItsShareLinks(t *testing.T) {
	s := newTVShareSetup(t)
	news, sports := s.create(t, "Mum's TV", "news", "news"), s.create(t, "Dad's TV", "sports", "sports")

	if rec := s.admin.do("POST", "/api/tv/channels/news/restart", "", "", true); rec.Code != http.StatusOK {
		t.Fatalf("restart: %d", rec.Code)
	}
	if rec := s.admin.do("PUT", "/api/tv/channels/news", `{"name":"Evening news"}`, "application/json", true); rec.Code != http.StatusOK {
		t.Fatalf("rename: %d", rec.Code)
	}
	s.fake.setChannels(fakeChannel{id: "news", name: "Evening news"}, fakeChannel{id: "sports", name: "Sports"})
	if got := s.list(t); len(got) != 2 {
		t.Fatalf("restart or rename touched the links: %+v", got)
	}
	rec := s.play(news.Code, "playlist.m3u", "192.0.2.10")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), ",Evening news\n") || !strings.Contains(rec.Body.String(), "/tv/s/"+news.Code+"/") {
		t.Errorf("renamed channel through the same code: %d %s", rec.Code, rec.Body)
	}

	if rec := s.admin.do("DELETE", "/api/tv/channels/news", "", "", true); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	if got := s.list(t); len(got) != 1 || got[0].Code != sports.Code {
		t.Errorf("after deleting news: %+v", got)
	}
	if rec := s.play(news.Code, "hls/news/index.m3u8", "192.0.2.11"); rec.Code != http.StatusNotFound {
		t.Errorf("link of the deleted channel: %d", rec.Code)
	}
	if rec := s.play(sports.Code, "playlist.m3u", "192.0.2.11"); rec.Code != http.StatusOK {
		t.Errorf("link of another channel: %d", rec.Code)
	}
	last := s.fake.requests()
	deleted := false
	for _, r := range last {
		deleted = deleted || (r.method == "DELETE" && r.path == "/api/channels/news")
	}
	if !deleted {
		t.Error("the delete never reached streambox")
	}
}

// A link created while its channel is being deleted must not survive the delete: otherwise a later channel
// with the same id would bring it back.
func TestTVShareLinkCreatedDuringAChannelDeleteDoesNotSurviveIt(t *testing.T) {
	s := newTVShareSetup(t)
	checking, release := make(chan struct{}), make(chan struct{})
	s.fake.mu.Lock()
	s.fake.beforeLookup = func() {
		close(checking)
		<-release
	}
	s.fake.mu.Unlock()

	created := make(chan int, 1)
	go func() { created <- s.post(shareBody("Mum's TV", "news", "news")).Code }()
	<-checking
	deleted := make(chan int, 1)
	go func() { deleted <- s.admin.do("DELETE", "/api/tv/channels/news", "", "", true).Code }()
	select {
	case code := <-deleted:
		t.Errorf("the delete finished (%d) while a link to the channel was half made", code)
		deleted <- code
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	if code := <-created; code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	select {
	case code := <-deleted:
		if code != http.StatusOK {
			t.Fatalf("delete: %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the delete never finished")
	}
	if got := s.tv.shares.All(); len(got) != 0 {
		t.Errorf("links outlived their channel: %+v", got)
	}
}

func TestTVShareLinksWithoutThePortalHaveNoURL(t *testing.T) {
	fake := newFakeStreambox(t)
	tv := fake.tv(t)
	s := &tvShareSetup{fake: fake, tv: tv, admin: newTVAdmin(t, tv)}
	if link := s.create(t, "Mum's TV", "news", "news"); link.URL != "" || link.Code == "" {
		t.Errorf("link %+v", link)
	}
}

func TestShareLinkPlaylistListsOnlyItsChannel(t *testing.T) {
	s := newTVShareSetup(t)
	link := s.create(t, "Dad's TV", "sports", "sports")

	rec := s.play(link.Code, "playlist.m3u", "192.0.2.10")
	base := "https://" + testPortalHost + "/tv/s/" + link.Code
	want := "#EXTM3U x-tvg-url=\"" + base + "/epg.xml\"\n" +
		"#EXTINF:-1 tvg-id=\"sports\" tvg-name=\"Sports & More\" tvg-logo=\"https://logo.example/sports.png\" group-title=\"Sport\",Sports & More\n" +
		base + "/hls/sports/index.m3u8\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Fatalf("got %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	if rec.Header().Get("Content-Type") != "audio/x-mpegurl" {
		t.Errorf("content type %q", rec.Header().Get("Content-Type"))
	}
	if got := rec.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-cache" {
		t.Errorf("Cache-Control %v", got)
	}
	assertNoViewToken(t, "playlist", rec)

	got := s.fake.requests()
	r := got[len(got)-1]
	if r.path != "/s/"+tvViewToken+"/playlist.m3u" || r.auth != "" || r.cookie != "" {
		t.Errorf("streambox saw %+v", r)
	}
}

func TestShareLinkGuideListsOnlyItsChannel(t *testing.T) {
	s := newTVShareSetup(t)
	link := s.create(t, "Dad's TV", "sports", "sports")

	rec := s.play(link.Code, "epg.xml", "192.0.2.10")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("got %d %q %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	assertNoViewToken(t, "guide", rec)
	var guide fakeGuide
	if err := xml.Unmarshal(rec.Body.Bytes(), &guide); err != nil {
		t.Fatalf("not XML: %v\n%s", err, rec.Body)
	}
	if guide.Generator != "streambox" || len(guide.Channels) != 1 || guide.Channels[0].ID != "sports" || guide.Channels[0].DisplayName != "Sports & More" ||
		guide.Channels[0].Icon == nil || guide.Channels[0].Icon.Src != "https://logo.example/sports.png" {
		t.Errorf("channels %+v", guide)
	}
	if len(guide.Programmes) != 1 || guide.Programmes[0].Channel != "sports" || guide.Programmes[0].Title != "Match of the day" {
		t.Errorf("programmes %+v", guide.Programmes)
	}
	if strings.Contains(rec.Body.String(), "Morning news") || strings.Contains(rec.Body.String(), `"news"`) {
		t.Errorf("another channel leaked into the guide:\n%s", rec.Body)
	}
}

func TestShareLinkPlaysOnlyItsChannel(t *testing.T) {
	s := newTVShareSetup(t)
	link := s.create(t, "Mum's TV", "news", "news")

	for rest, want := range map[string]string{
		"hls/news/index.m3u8":   "/s/" + tvViewToken + "/hls/news/index.m3u8",
		"hls/news/seg_00001.ts": "/s/" + tvViewToken + "/hls/news/seg_00001.ts",
	} {
		before := len(s.fake.requests())
		rec := s.play(link.Code, rest, "192.0.2.10")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", rest, rec.Code, rec.Body)
		}
		assertNoViewToken(t, rest, rec)
		if now := s.fake.requests(); len(now) != before+1 || now[before].path != want || now[before].auth != "" {
			t.Errorf("%s forwarded as %+v", rest, now[before:])
		}
	}

	before := len(s.fake.requests())
	for _, rest := range []string{
		"hls/sports/index.m3u8", "hls/sports/seg_00001.ts", "hls/%73ports/index.m3u8", "hls/NEWS/index.m3u8", "hls/news-2/index.m3u8",
		"hls/news/../sports/index.m3u8", "hls/sports%2findex.m3u8", "hls/news%2f..%2fsports/index.m3u8", "hls/news/concat.txt",
		"s/" + tvViewToken + "/playlist.m3u", "hls/" + tvViewToken + "/index.m3u8", "api/channels", "hls/news/seg_1.ts", "hls/../../playlist.m3u",
	} {
		rec := s.portal.stream("/tv/s/"+link.Code+"/"+rest, "192.0.2.20")
		if rec.Code == http.StatusOK {
			t.Errorf("%s served", rest)
		}
		if location := rec.Header().Get("Location"); location != "" {
			if followed := s.portal.stream(location, "192.0.2.20"); followed.Code == http.StatusOK {
				t.Errorf("%s redirects to %s, which is served", rest, location)
			}
		}
	}
	for _, r := range s.fake.requests()[before:] {
		t.Errorf("a refused path reached streambox as %s", r.path)
	}
}

func TestShareLinkCodesMatchIgnoringASCIICaseOnly(t *testing.T) {
	s := newTVShareSetup(t)
	link := s.create(t, "Kids' TV", "news", "kids")

	for _, code := range []string{strings.ToUpper(link.Code), "Kids-" + strings.ToUpper(strings.TrimPrefix(link.Code, "kids-"))} {
		rec := s.play(code, "playlist.m3u", "192.0.2.10")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/tv/s/"+link.Code+"/hls/news/") || strings.Contains(rec.Body.String(), code) {
			t.Errorf("%q: %d %s", code, rec.Code, rec.Body)
		}
	}
	if rec := s.portal.stream("/tv/s/%6B"+strings.TrimPrefix(link.Code, "k")+"/playlist.m3u", "192.0.2.10"); rec.Code != http.StatusOK {
		t.Errorf("percent-encoded plain letter: %d", rec.Code)
	}

	fullwidth := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - 'a' + 'ａ'
		}
		return r
	}, link.Code)
	for _, code := range []string{"Kids" + strings.TrimPrefix(link.Code, "kids"), fullwidth, link.Code + " ", " " + link.Code, link.Code + "\x00", strings.TrimSuffix(link.Code, link.Code[len(link.Code)-1:])} {
		if rec := s.play(code, "playlist.m3u", "192.0.2.30"); rec.Code == http.StatusOK {
			t.Errorf("%q was accepted", code)
		}
	}
}

func TestShareLinkMissesAreLimitedAndLoggedForFail2ban(t *testing.T) {
	logged := captureLog(t)
	s := newTVShareSetup(t)
	revoked := s.create(t, "Mum's TV", "news", "news")
	if rec := s.revoke(revoked.Code); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	live := s.create(t, "Dad's TV", "news", "news")

	for i := range tvIPFails {
		if rec := s.play(revoked.Code, "playlist.m3u", "192.0.2.50"); rec.Code != http.StatusNotFound {
			t.Fatalf("miss %d: %d", i, rec.Code)
		}
	}
	if rec := s.play(live.Code, "playlist.m3u", "192.0.2.50"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("live link from a blocked address: %d", rec.Code)
	}
	if rec := s.play(tvViewToken, "playlist.m3u", "192.0.2.50"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("view token from a blocked address: %d", rec.Code)
	}
	if rec := s.play(live.Code, "playlist.m3u", "192.0.2.51"); rec.Code != http.StatusOK {
		t.Errorf("other address: %d", rec.Code)
	}

	failed, blocked := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
		if strings.Contains(line, revoked.Code) || strings.Contains(line, live.Code) || strings.Contains(line, tvViewToken) {
			t.Errorf("a code reached the log: %q", line)
		}
		m := tvFail2ban.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[4] != "192.0.2.50" {
			t.Errorf("fail2ban would ban %q from %q", m[4], line)
		}
		switch m[2] {
		case "failed":
			failed++
		case "blocked":
			blocked++
		}
	}
	if failed != tvIPFails || blocked != 2 {
		t.Errorf("logged %d failed and %d blocked lines:\n%s", failed, blocked, logged)
	}
}

func TestShareLinkMissesAreCountedAtomically(t *testing.T) {
	s := newTVShareSetup(t)
	results := make(chan int, 40)
	for range 40 {
		go func() { results <- s.play("ghost-aaaaaaaa", "playlist.m3u", "192.0.2.60").Code }()
	}
	missed := 0
	for range 40 {
		if <-results == http.StatusNotFound {
			missed++
		}
	}
	if missed > tvIPFails {
		t.Errorf("%d parallel guesses were answered before the block, want at most %d", missed, tvIPFails)
	}
}

func TestShareLinkForAChannelStreamboxNoLongerHas(t *testing.T) {
	s := newTVShareSetup(t)
	link := s.create(t, "Mum's TV", "news", "news")
	s.fake.setChannels(fakeChannel{id: "sports", name: "Sports"})

	for range tvIPFails + 2 {
		for _, rest := range []string{"playlist.m3u", "epg.xml"} {
			if rec := s.play(link.Code, rest, "192.0.2.70"); rec.Code != http.StatusNotFound {
				t.Fatalf("%s: %d %s", rest, rec.Code, rec.Body)
			}
		}
	}
	s.fake.setChannels(fakeChannel{id: "news", name: "News"})
	if rec := s.play(link.Code, "playlist.m3u", "192.0.2.70"); rec.Code != http.StatusOK {
		t.Errorf("a channel that was briefly gone got the address blocked: %d", rec.Code)
	}
}

func TestShareLinkNeverSendsABodyThatHoldsTheViewToken(t *testing.T) {
	s := newTVShareSetup(t)
	link := s.create(t, "Mum's TV", "news", "news")
	s.fake.setChannels(fakeChannel{id: "news", name: "leak " + tvViewToken, programmes: []string{"leak " + tvViewToken}})

	for _, rest := range []string{"playlist.m3u", "epg.xml"} {
		rec := s.play(link.Code, rest, "192.0.2.80")
		if rec.Code == http.StatusOK {
			t.Errorf("%s was sent", rest)
		}
		assertNoViewToken(t, rest, rec)
	}
}

func TestShareLinkWhenStreamboxIsDown(t *testing.T) {
	logged := captureLog(t)
	s := newTVShareSetup(t)
	link := s.create(t, "Mum's TV", "news", "news")
	s.fake.srv.Close()
	for _, rest := range []string{"playlist.m3u", "epg.xml", "hls/news/index.m3u8"} {
		if rec := s.play(link.Code, rest, "192.0.2.90"); rec.Code != http.StatusBadGateway {
			t.Errorf("%s: %d", rest, rec.Code)
		}
	}
	if strings.Contains(logged.String(), tvViewToken) {
		t.Errorf("the view token reached the log:\n%s", logged)
	}
}

func TestGlobalLinkStillListsEveryChannel(t *testing.T) {
	s := newTVShareSetup(t)
	s.create(t, "Mum's TV", "news", "news")
	rec := s.play(tvViewToken, "playlist.m3u", "192.0.2.10")
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "https://"+testPortalHost+"/tv/s/"+tvViewToken+"/hls/news/index.m3u8") ||
		!strings.Contains(body, "https://"+testPortalHost+"/tv/s/"+tvViewToken+"/hls/sports/index.m3u8") {
		t.Errorf("global playlist: %d\n%s", rec.Code, body)
	}
	if rec := s.play(tvViewToken, "hls/sports/index.m3u8", "192.0.2.10"); rec.Code != http.StatusOK {
		t.Errorf("global link, any channel: %d", rec.Code)
	}
}

func TestPortalCannotManageShareLinks(t *testing.T) {
	fake := newFakeStreambox(t)
	tv := fake.tv(t)
	if err := tv.shares.Create(auth.TVShare{Code: "news-k7p3x9qm", Label: "Mum's TV", Channel: "news", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	roles, err := auth.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.Promote("amy"); err != nil {
		t.Fatal(err)
	}
	verifier := &system.FakeVerifier{Passwords: map[string]string{"amy": amyPassword, "bob": bobPassword}}
	h := &portalHarness{portal: NewPortal(PortalDeps{Verifier: verifier, Users: system.NewFakeUsers("amy", "bob"), Roles: roles, TV: tv}), verifier: verifier}
	sessions := []*http.Cookie{h.signedIn(t, "amy", amyPassword), h.signedIn(t, "bob", bobPassword), nil}

	for _, path := range []string{
		"/api/tv/shares", "/api/tv/shares/news-k7p3x9qm", "/api/admin/tv/shares", "/api/admin/tv/shares/news-k7p3x9qm", "/api/admin/tv-shares",
		"/api/tv/channels", "/api/tv/channels/news", "/tv/shares", "/admin/tv", "/tv/api/shares",
	} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
			for _, cookie := range sessions {
				rec := h.do(method, path, shareBody("x", "news", "x"), "192.0.2.10", cookie)
				if rec.Code < 300 {
					t.Errorf("%s %s (signed in %v): %d %s", method, path, cookie != nil, rec.Code, rec.Body)
				}
				if strings.Contains(rec.Body.String(), "news-k7p3x9qm") {
					t.Errorf("%s %s listed a share link", method, path)
				}
			}
		}
	}
	if got := tv.shares.All(); len(got) != 1 || got[0].Code != "news-k7p3x9qm" {
		t.Errorf("share links changed through the portal: %+v", got)
	}
	for _, r := range fake.requests() {
		t.Errorf("the portal reached streambox as %s %s", r.method, r.path)
	}
}

const fuzzShareCode = "news-k7p3x9qm"

var shareForwarded = regexp.MustCompile(`^/s/` + tvViewToken + `/(playlist\.m3u|epg\.xml|hls/news/(index\.m3u8|seg_[0-9]{5,}\.ts))$`)

// Through a share link, streambox only ever sees the link's own channel, and the view token never comes back.
func FuzzTVShareLinksReachOnlyTheirChannel(f *testing.F) {
	for _, seed := range [][2]string{
		{fuzzShareCode, "playlist.m3u"}, {fuzzShareCode, "epg.xml"}, {fuzzShareCode, "hls/news/index.m3u8"}, {fuzzShareCode, "hls/sports/index.m3u8"},
		{strings.ToUpper(fuzzShareCode), "hls/news/seg_00001.ts"}, {fuzzShareCode, "hls/../../api/channels"}, {fuzzShareCode, "hls/%73ports/index.m3u8"},
		{fuzzShareCode, "hls/news/../sports/index.m3u8"}, {fuzzShareCode + "x", "playlist.m3u"}, {"news-k7p3x9q", "playlist.m3u"}, {"", "playlist.m3u"},
		{fuzzShareCode + "/..", "playlist.m3u"}, {"Kews-k7p3x9qm", "playlist.m3u"}, {fuzzShareCode, "hls//index.m3u8"}, {fuzzShareCode, "playlist.m3u\x00"},
		{"news-k7p3x9q%6D", "hls/news/index.m3u8"}, {"%4EEWS-K7P3X9QM", "hls/%6Eews/seg_00001.ts"}, {fuzzShareCode, "hls/news%2f..%2fsports/index.m3u8"},
		{fuzzShareCode, "hls/news/%2e%2e/sports/index.m3u8"}, {fuzzShareCode + "%2f..%2f..", "hls/sports/index.m3u8"}, {fuzzShareCode, "epg.xml?x=1"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, code, rest string) {
		target, err := url.ParseRequestURI("/tv/s/" + code + "/" + rest)
		if err != nil || strings.Contains(target.Path, tvViewToken) || strings.Contains(target.RawPath, tvViewToken) {
			return
		}
		path := target.String()
		fake := newFakeStreambox(t)
		tv := fake.tv(t)
		if err := tv.shares.Create(auth.TVShare{Code: fuzzShareCode, Label: "x", Channel: "news", Created: time.Now()}); err != nil {
			t.Fatal(err)
		}
		h := newTVPortal(t, tv)
		req := httptest.NewRequest("GET", "/", nil)
		req.URL = target
		req.Host = testPortalHost
		req.RemoteAddr = "192.0.2.77:5555"
		rec := httptest.NewRecorder()
		h.portal.ServeHTTP(rec, req)
		for _, r := range fake.requests() {
			if !shareForwarded.MatchString(r.path) || r.auth != "" {
				t.Fatalf("%q reached streambox as %q (auth %q)", path, r.path, r.auth)
			}
		}
		assertNoViewToken(t, path, rec)
	})
}
