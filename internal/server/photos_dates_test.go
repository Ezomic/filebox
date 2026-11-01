package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

type photoDatesPortal struct {
	*portalFilesHarness
	amy, bob *http.Cookie
}

func newPhotoDatesPortal(t *testing.T) photoDatesPortal {
	t.Helper()
	h := newPortalFilesHarness(t)

	return photoDatesPortal{h, h.signedIn(t, "amy", amyPassword), h.signedIn(t, "bob", bobPassword)}
}

func (p photoDatesPortal) listing(t *testing.T, cookie *http.Cookie, query string) photoListing {
	t.Helper()
	rec := p.do("GET", "/api/files/photos"+query, "", "192.0.2.10", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list%s got %d: %s", query, rec.Code, rec.Body)
	}
	var listing photoListing
	if err := json.Unmarshal(rec.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}

	return listing
}

func (p photoDatesPortal) setDate(cookie *http.Cookie, path, date string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"path": path, "date": date})

	return p.do("PUT", "/api/files/photo-date", string(body), "192.0.2.10", cookie)
}

func (p photoDatesPortal) resetDate(cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	return p.do("DELETE", "/api/files/photo-date?path="+url.QueryEscape(path), "", "192.0.2.10", cookie)
}

func (p photoDatesPortal) stamp(t *testing.T, user, name string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(filepath.Join(p.base, user, "files", filepath.FromSlash(name)), when, when); err != nil {
		t.Fatal(err)
	}
}

func TestPhotoListFiltersByTypeBeforePaging(t *testing.T) {
	p := newPhotoDatesPortal(t)
	for i, name := range []string{"a.jpg", "b.mp4", "c.png", "d.mov", "e.jpeg"} {
		p.write(t, "amy", name, "x")
		p.stamp(t, "amy", name, time.Date(2020, 1, 1+i, 0, 0, 0, 0, time.UTC))
	}

	if got := p.listing(t, p.amy, "?type=video").paths(); !slices.Equal(got, []string{"d.mov", "b.mp4"}) {
		t.Fatalf("videos: %v", got)
	}
	if got := p.listing(t, p.amy, "?type=image").paths(); !slices.Equal(got, []string{"e.jpeg", "c.png", "a.jpg"}) {
		t.Fatalf("images: %v", got)
	}
	if got := p.listing(t, p.amy, "?type=").paths(); len(got) != 5 {
		t.Fatalf("an empty type means everything: %v", got)
	}
	if got := p.listing(t, p.amy, "").paths(); len(got) != 5 {
		t.Fatalf("no type means everything: %v", got)
	}

	var paged []string
	cursor := ""
	for range 10 {
		page := p.listing(t, p.amy, "?type=image&limit=2&after="+url.QueryEscape(cursor))
		paged = append(paged, page.paths()...)
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if !slices.Equal(paged, []string{"e.jpeg", "c.png", "a.jpg"}) {
		t.Fatalf("a filtered cursor must neither skip nor repeat: %v", paged)
	}
}

func TestPhotoListRefusesUnknownTypes(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "a.jpg", "x")
	for _, query := range []string{"?type=audio", "?type=photo", "?type=IMAGE", "?type=image,video", "?type=%20image", "?type=*", "?type=%zz", "?type=image&type=bogus", "?type=bogus&type=image"} {
		if rec := p.do("GET", "/api/files/photos"+query, "", "192.0.2.10", p.amy); rec.Code != http.StatusBadRequest {
			t.Errorf("%s got %d, want 400", query, rec.Code)
		}
	}
	if rec := p.do("GET", "/api/files/photos?type=image&type=image", "", "192.0.2.10", p.amy); rec.Code != http.StatusBadRequest {
		t.Errorf("a repeated type is ambiguous and must be refused, got %d", rec.Code)
	}
}

func TestChangingAndResettingAPhotoDateThroughThePortal(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "old.jpg", "x")
	p.write(t, "amy", "new.jpg", "xy")
	p.stamp(t, "amy", "old.jpg", time.Date(2018, 3, 4, 10, 0, 0, 0, time.UTC))
	p.stamp(t, "amy", "new.jpg", time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC))

	if rec := p.setDate(p.amy, "old.jpg", "2024-05-17"); rec.Code != http.StatusNoContent {
		t.Fatalf("set got %d: %s", rec.Code, rec.Body)
	}
	listing := p.listing(t, p.amy, "")
	if got := listing.paths(); !slices.Equal(got, []string{"old.jpg", "new.jpg"}) {
		t.Fatalf("order: %v", got)
	}
	if !listing.Photos[0].Date.Equal(time.Date(2024, 5, 17, 0, 0, 0, 0, time.UTC)) || !listing.Photos[0].Custom || listing.Photos[1].Custom {
		t.Fatalf("a day is shown as UTC midnight and flagged custom: %+v", listing.Photos)
	}
	if !strings.Contains(p.do("GET", "/api/files/photos", "", "192.0.2.10", p.amy).Body.String(), `"date":"2024-05-17T00:00:00Z"`) {
		t.Fatal("the shown form is RFC 3339 at UTC midnight")
	}

	if rec := p.resetDate(p.amy, "old.jpg"); rec.Code != http.StatusNoContent {
		t.Fatalf("reset got %d: %s", rec.Code, rec.Body)
	}
	listing = p.listing(t, p.amy, "")
	if got := listing.paths(); !slices.Equal(got, []string{"new.jpg", "old.jpg"}) || listing.Photos[1].Custom || listing.Photos[1].Date.Year() != 2018 {
		t.Fatalf("reset must restore the original: %+v", listing.Photos)
	}
	if rec := p.resetDate(p.amy, "old.jpg"); rec.Code != http.StatusNoContent {
		t.Fatalf("resetting twice got %d", rec.Code)
	}

	data, err := os.ReadFile(p.originalFile("amy", "old.jpg"))
	if err != nil || string(data) != "x" {
		t.Fatalf("the original must be untouched: %q %v", data, err)
	}
}

func (p photoDatesPortal) originalFile(user, name string) string {
	return filepath.Join(p.base, user, "files", filepath.FromSlash(name))
}

func TestCustomDateKeepsTypeFilterAndPagingConsistent(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "clip.mp4", "x")
	p.write(t, "amy", "pic.jpg", "x")
	p.stamp(t, "amy", "clip.mp4", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	p.stamp(t, "amy", "pic.jpg", time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if rec := p.setDate(p.amy, "clip.mp4", "2023-01-01"); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	first := p.listing(t, p.amy, "?limit=1")
	second := p.listing(t, p.amy, "?limit=1&after="+url.QueryEscape(first.Next))
	if first.paths()[0] != "clip.mp4" || second.paths()[0] != "pic.jpg" {
		t.Fatalf("%v then %v", first.paths(), second.paths())
	}
	if got := p.listing(t, p.amy, "?type=video").Photos; len(got) != 1 || !got[0].Custom {
		t.Fatalf("%+v", got)
	}
}

func TestPhotoDateRefusesBadDates(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "a.jpg", "x")
	twoDaysAhead := time.Now().UTC().AddDate(0, 0, 2).Format("2006-01-02")
	for name, date := range map[string]string{
		"empty":          "",
		"whitespace":     "   ",
		"trailing space": "2024-05-17 ",
		"other format":   "17-05-2024",
		"slashes":        "2024/05/17",
		"with time":      "2024-05-17T10:00:00Z",
		"month 13":       "2026-13-45",
		"feb 30":         "2024-02-30",
		"non leap":       "2023-02-29",
		"far future":     "2999-01-01",
		"two days ahead": twoDaysAhead,
		"year 10000":     "10000-01-01",
		"year 99999":     "99999-01-01",
		"pre 1900":       "1899-12-31",
		"year zero":      "0000-01-01",
		"negative":       "-001-01-01",
		"unicode digits": "２０２４-０５-１７",
		"arabic digits":  "٢٠٢٤-٠٥-١٧",
		"very long":      strings.Repeat("2024-05-17", 10000),
		"newline":        "2024-05-17\n",
		"sql":            "2024-05-17'; DROP",
		"null byte":      "2024-05-17\x00",
	} {
		if rec := p.setDate(p.amy, "a.jpg", date); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, rec.Code)
		}
	}
	if listing := p.listing(t, p.amy, ""); listing.Photos[0].Custom {
		t.Fatal("a refused date must not be stored")
	}
}

func TestPhotoDateRefusesBadBodies(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "a.jpg", "x")
	for name, body := range map[string]string{
		"not json":     "nope",
		"empty object": `{}`,
		"number date":  `{"path":"a.jpg","date":20240517}`,
		"null date":    `{"path":"a.jpg","date":null}`,
		"array date":   `{"path":"a.jpg","date":["2024-05-17"]}`,
		"object date":  `{"path":"a.jpg","date":{"y":2024}}`,
		"bool date":    `{"path":"a.jpg","date":true}`,
		"number path":  `{"path":5,"date":"2024-05-17"}`,
		"array body":   `[]`,
		"missing path": `{"date":"2024-05-17"}`,
		"missing date": `{"path":"a.jpg"}`,
	} {
		if rec := p.do("PUT", "/api/files/photo-date", body, "192.0.2.10", p.amy); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400: %s", name, rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest("PUT", "/api/files/photo-date", strings.NewReader(`{"path":"a.jpg","date":"2024-05-17"}`))
	req.Header.Set("Content-Type", "text/plain")
	req.AddCookie(p.amy)
	rec := httptest.NewRecorder()
	p.portal.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("a non JSON body got %d", rec.Code)
	}
	big := `{"path":"a.jpg","date":"` + strings.Repeat("a", 2<<20) + `"}`
	if rec := p.do("PUT", "/api/files/photo-date", big, "192.0.2.10", p.amy); rec.Code != http.StatusBadRequest {
		t.Errorf("an oversized body got %d", rec.Code)
	}
}

func TestPhotoDateRefusesPathsThatAreNotYourPhotos(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "notes.txt", "text")
	p.write(t, "amy", "a.jpg", "x")
	if err := os.MkdirAll(filepath.Join(p.base, "amy", "files", "folder.jpg"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.write(t, "bob", "secret.jpg", "bob")

	cases := map[string]int{
		"notes.txt":                  http.StatusUnsupportedMediaType,
		"folder.jpg":                 http.StatusUnsupportedMediaType,
		"missing.jpg":                http.StatusNotFound,
		"secret.jpg":                 http.StatusNotFound,
		"../bob/files/secret.jpg":    http.StatusBadRequest,
		"../../bob/files/secret.jpg": http.StatusBadRequest,
		"/a.jpg":                     http.StatusBadRequest,
		"..":                         http.StatusBadRequest,
		"a.jpg/..":                   http.StatusBadRequest,
		".thumbs/index.json":         http.StatusBadRequest,
		".thumbs/dates.json":         http.StatusBadRequest,
		".trash/a.jpg":               http.StatusBadRequest,
		"..\\bob\\files\\secret.jpg": http.StatusBadRequest,
		"":                           http.StatusBadRequest,
	}
	for path, want := range cases {
		if rec := p.setDate(p.amy, path, "2024-05-17"); rec.Code != want {
			t.Errorf("set %q got %d, want %d", path, rec.Code, want)
		}
		if rec := p.resetDate(p.amy, path); rec.Code != want {
			t.Errorf("reset %q got %d, want %d", path, rec.Code, want)
		}
	}
	if rec := p.do("PUT", "/api/files/photo-date?user=bob", `{"path":"secret.jpg","date":"2024-05-17"}`, "192.0.2.10", p.amy); rec.Code != http.StatusNotFound {
		t.Errorf("the user parameter must mean nothing: %d", rec.Code)
	}
	if rec := p.do("DELETE", "/api/files/photo-date?path=%zz", "", "192.0.2.10", p.amy); rec.Code != http.StatusBadRequest {
		t.Errorf("a broken escape got %d", rec.Code)
	}
	if listing := p.listing(t, p.bob, ""); listing.Photos[0].Custom {
		t.Fatal("bob's photo changed")
	}
}

func TestUsersCanNotReadOrChangeEachOthersPhotoDates(t *testing.T) {
	p := newPhotoDatesPortal(t)
	for _, user := range []string{"amy", "bob"} {
		p.write(t, user, "same.jpg", "identical bytes")
		p.stamp(t, user, "same.jpg", time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC))
	}

	if rec := p.setDate(p.amy, "same.jpg", "2024-05-17"); rec.Code != http.StatusNoContent {
		t.Fatalf("amy set got %d", rec.Code)
	}
	bobs := p.listing(t, p.bob, "")
	if bobs.Photos[0].Custom || bobs.Photos[0].Date.Year() != 2018 {
		t.Fatalf("amy's date reached bob: %+v", bobs.Photos[0])
	}

	if rec := p.resetDate(p.bob, "same.jpg"); rec.Code != http.StatusNoContent {
		t.Fatalf("bob's reset of his own photo got %d", rec.Code)
	}
	if amys := p.listing(t, p.amy, ""); !amys.Photos[0].Custom {
		t.Fatal("bob's reset reached amy's date")
	}

	if _, err := os.Stat(filepath.Join(p.base, "bob", "files", system.ThumbsDir, "dates.json")); err == nil {
		t.Fatal("bob's folder must hold no dates file he never wrote")
	}
	if _, err := os.Stat(filepath.Join(p.base, "amy", "files", system.ThumbsDir, "dates.json")); err != nil {
		t.Fatalf("the dates file lives in amy's own data: %v", err)
	}
}

func TestPhotoDateRoutesNeedASessionAndTheRightMethod(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "a.jpg", "x")
	if rec := p.setDate(nil, "a.jpg", "2024-05-17"); rec.Code != http.StatusUnauthorized {
		t.Errorf("set without a session got %d", rec.Code)
	}
	if rec := p.resetDate(nil, "a.jpg"); rec.Code != http.StatusUnauthorized {
		t.Errorf("reset without a session got %d", rec.Code)
	}
	for _, method := range []string{"GET", "POST", "PATCH"} {
		if rec := p.do(method, "/api/files/photo-date?path=a.jpg", `{"path":"a.jpg","date":"2024-05-17"}`, "192.0.2.10", p.amy); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Errorf("%s got %d", method, rec.Code)
		}
	}
}

func TestACorruptDatesFileNeverBreaksTheListing(t *testing.T) {
	p := newPhotoDatesPortal(t)
	p.write(t, "amy", "a.jpg", "x")
	p.stamp(t, "amy", "a.jpg", time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC))
	p.write(t, "amy", system.ThumbsDir+"/dates.json", "{corrupt")

	listing := p.listing(t, p.amy, "")
	if len(listing.Photos) != 1 || listing.Photos[0].Custom || listing.Photos[0].Date.Year() != 2018 {
		t.Fatalf("%+v", listing.Photos)
	}
	if rec := p.setDate(p.amy, "a.jpg", "2024-05-17"); rec.Code != http.StatusNoContent {
		t.Fatalf("setting over a corrupt file got %d", rec.Code)
	}
	if !p.listing(t, p.amy, "").Photos[0].Custom {
		t.Fatal("the date must be stored again")
	}
}

func TestThePhotosPageOffersFiltersAndTheDateControls(t *testing.T) {
	p := newPhotoDatesPortal(t)
	rec := p.do("GET", "/photos", "", "192.0.2.10", p.amy)
	for _, want := range []string{`id="type-filter"`, `id="jump-year"`, `id="jump-month"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the photos page lacks %s", want)
		}
	}
}
