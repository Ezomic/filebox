package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var shareEpoch = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func newShare(name, owner string) Share {
	return Share{Name: name, Owner: owner, Path: "docs", Dir: true, Created: shareEpoch}
}

func TestSharesPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	shares, err := OpenShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	expires := shareEpoch.Add(24 * time.Hour)
	share := newShare("Chronos-App", "amy")
	share.Expires = expires
	if err := shares.Create(share, 5); err != nil {
		t.Fatal(err)
	}
	if err := shares.Record("chronos-app", shareEpoch.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, found := reopened.Get("CHRONOS-app")
	if !found || got.Name != "chronos-app" || got.Owner != "amy" || got.Downloads != 1 || !got.Expires.Equal(expires) || !got.LastUsed.Equal(shareEpoch.Add(time.Hour)) {
		t.Fatalf("got %+v %v after reopening", got, found)
	}
}

func TestSharesNamesAreUniqueIgnoringCase(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	if err := shares.Create(newShare("photos-2026", "amy"), 5); err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newShare("PHOTOS-2026", "bob"), 5); !errors.Is(err, ErrShareTaken) {
		t.Fatalf("got %v", err)
	}
}

func TestSharesTwoCreationsOfTheSameNameLeaveExactlyOneWinner(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if shares.Create(newShare("race-name", fmt.Sprintf("user%d", i)), 5) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d creations won", wins.Load())
	}
}

func TestSharesCapLinksPerOwner(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	for i := range 3 {
		if err := shares.Create(newShare(fmt.Sprintf("link-%d", i), "amy"), 3); err != nil {
			t.Fatal(err)
		}
	}
	if err := shares.Create(newShare("link-extra", "amy"), 3); !errors.Is(err, ErrShareLimit) {
		t.Fatalf("got %v", err)
	}
	if err := shares.Create(newShare("link-bob", "bob"), 3); err != nil {
		t.Fatalf("another owner has their own allowance: %v", err)
	}
}

func TestSharesOnlyTheOwnerChangesOrRevokes(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	_ = shares.Create(newShare("amys-link", "amy"), 5)

	if _, err := shares.SetExpiry("bob", "amys-link", shareEpoch); !errors.Is(err, ErrShareMissing) {
		t.Fatalf("got %v", err)
	}
	if err := shares.Revoke("bob", "amys-link"); !errors.Is(err, ErrShareMissing) {
		t.Fatalf("got %v", err)
	}
	if _, found := shares.Get("amys-link"); !found {
		t.Fatal("must still exist")
	}
	if err := shares.Revoke("", "AMYS-LINK"); err != nil {
		t.Fatalf("an empty owner revokes anything: %v", err)
	}
	if _, found := shares.Get("amys-link"); found {
		t.Fatal("must be gone")
	}
}

func TestSharesExpiryBoundaries(t *testing.T) {
	share := newShare("timed", "amy")
	share.Expires = shareEpoch
	cases := map[string]struct {
		at   time.Time
		want bool
	}{
		"one second before": {shareEpoch.Add(-time.Second), false},
		"exactly at":        {shareEpoch, true},
		"one second after":  {shareEpoch.Add(time.Second), true},
	}
	for name, c := range cases {
		if got := share.Expired(c.at); got != c.want {
			t.Errorf("%s: got %v", name, got)
		}
	}
	share.Expires = time.Time{}
	if share.Expired(shareEpoch.Add(1000 * time.Hour)) {
		t.Fatal("a link without expiry never expires")
	}
}

func TestSharesCountDownloadsConcurrently(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	_ = shares.Create(newShare("busy", "amy"), 5)
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = shares.Record("busy", shareEpoch)
		}()
	}
	wg.Wait()
	if got, _ := shares.Get("busy"); got.Downloads != 40 {
		t.Fatalf("got %d", got.Downloads)
	}
}

func TestSharesForgetOwnerLeavesTheOthers(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenShares(dir)
	_ = shares.Create(newShare("a-one", "amy"), 5)
	_ = shares.Create(newShare("a-two", "amy"), 5)
	_ = shares.Create(newShare("b-one", "bob"), 5)

	if err := shares.ForgetOwner("amy"); err != nil {
		t.Fatal(err)
	}
	reopened, _ := OpenShares(dir)
	if len(reopened.All()) != 1 || len(reopened.ByOwner("amy")) != 0 || len(reopened.ByOwner("bob")) != 1 {
		t.Fatalf("got %+v", reopened.All())
	}
}

func TestSharesForgetOwnersNotIn(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	_ = shares.Create(newShare("a-one", "amy"), 5)
	_ = shares.Create(newShare("g-one", "gone"), 5)

	if err := shares.ForgetOwnersNotIn([]string{"amy", "bob"}); err != nil {
		t.Fatal(err)
	}
	if _, found := shares.Get("g-one"); found {
		t.Fatal("the link of an account that no longer exists must go")
	}
	if _, found := shares.Get("a-one"); !found {
		t.Fatal("the link of an existing account must stay")
	}
}

func TestSharesSetAsideADamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, sharesFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	shares, err := OpenShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares.All()) != 0 {
		t.Fatal("a damaged file must start empty")
	}
	if _, err := os.Stat(filepath.Join(dir, sharesFile+".bad")); err != nil {
		t.Fatalf("the damaged file must be kept: %v", err)
	}
	if err := shares.Create(newShare("after-damage", "amy"), 5); err != nil {
		t.Fatalf("a store started after damage must still save: %v", err)
	}
}

func TestSharesStartEmptyWithoutAFile(t *testing.T) {
	shares, err := OpenShares(filepath.Join(t.TempDir(), "new", "dir"))
	if err != nil || len(shares.All()) != 0 {
		t.Fatalf("got %v %v", shares, err)
	}
}

func TestSharesFailedWriteChangesNothing(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenShares(dir)
	if err := os.Mkdir(filepath.Join(dir, sharesFile+".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newShare("never-saved", "amy"), 5); err == nil {
		t.Fatal("the write must fail")
	}
	if _, found := shares.Get("never-saved"); found {
		t.Fatal("a link that could not be saved must not exist in memory either")
	}
}

func newUploadLink(name, owner string) Share {
	link := newShare(name, owner)
	link.Kind, link.Cap, link.Expires, link.Lang = UploadLink, 100, shareEpoch.Add(24*time.Hour), "nl"

	return link
}

func TestUploadLinksShareTheNamesAndTheLimitWithShareLinks(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	if err := shares.Create(newShare("taken-name", "amy"), 2); err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newUploadLink("TAKEN-NAME", "bob"), 2); !errors.Is(err, ErrShareTaken) {
		t.Fatalf("one name is one link whatever its kind: %v", err)
	}
	if err := shares.Create(newUploadLink("drop-box", "amy"), 2); err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newShare("third-one", "amy"), 2); !errors.Is(err, ErrShareLimit) {
		t.Fatalf("upload links count toward the same limit: %v", err)
	}
}

func TestUploadLinksKeepTheirExpiry(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	if err := shares.Create(newUploadLink("drop-box", "amy"), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.SetExpiry("amy", "drop-box", time.Time{}); !errors.Is(err, ErrShareMissing) {
		t.Fatalf("an upload link must never become permanent: %v", err)
	}
	if link, _ := shares.Get("drop-box"); !link.Expires.Equal(shareEpoch.Add(24 * time.Hour)) {
		t.Fatalf("got %v", link.Expires)
	}
}

func TestUploadLinkUpdatesSaveACopyAndSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenShares(dir)
	if err := shares.Create(newUploadLink("drop-box", "amy"), 5); err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newShare("plain-share", "amy"), 5); err != nil {
		t.Fatal(err)
	}
	if _, err := shares.UpdateUploadLink("plain-share", func(*Share) error { return nil }); !errors.Is(err, ErrShareMissing) {
		t.Fatalf("a share link takes no uploads: %v", err)
	}
	if err := shares.Record("drop-box", shareEpoch); !errors.Is(err, ErrShareMissing) {
		t.Fatalf("an upload link has no downloads: %v", err)
	}
	_, err := shares.UpdateUploadLink("drop-box", func(link *Share) error {
		link.Pending = map[string]PendingUpload{"a": {Size: 10, Key: "k", Path: "docs/a.txt"}}
		link.Visits = map[string]Visit{"v": {Folder: "docs/Anna"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := shares.Get("drop-box")
	finished, err := shares.UpdateUploadLink("drop-box", func(link *Share) error {
		delete(link.Pending, "a")
		link.Visits["v"] = Visit{Folder: "docs/Anna", Made: true}
		link.Received, link.ReceivedBytes, link.Charged, link.Unreported = 1, 10, 1<<20, 1
		return nil
	})
	if err != nil || finished.Received != 1 {
		t.Fatalf("got %+v %v", finished, err)
	}
	if len(before.Pending) != 1 || before.Visits["v"].Made {
		t.Fatalf("a link read earlier must not change under its reader: %+v", before)
	}
	if _, err := shares.UpdateUploadLink("drop-box", func(link *Share) error {
		link.Received = 99
		return errors.New("changed my mind")
	}); err == nil {
		t.Fatal("a failed change must say so")
	}
	if link, _ := shares.Get("drop-box"); link.Received != 1 {
		t.Fatalf("a failed change must not stick: %+v", link)
	}

	reopened, _ := OpenShares(dir)
	link, _ := reopened.Get("drop-box")
	if !link.IsUploadLink() || link.Cap != 100 || link.Lang != "nl" || link.Received != 1 || link.Charged != 1<<20 || link.Unreported != 1 ||
		len(link.Pending) != 0 || !link.Visits["v"].Made {
		t.Fatalf("got %+v after reopening", link)
	}
	if plain, _ := reopened.Get("plain-share"); plain.IsUploadLink() {
		t.Fatal("a share link stays a share link")
	}
}

// legacyShare is the share link as the release before upload links reads it.
type legacyShare struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
}

func TestAnOlderReleaseNeverSeesUploadLinks(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenShares(dir)
	for _, link := range []Share{newShare("plain-share", "amy"), newUploadLink("drop-box", "amy"), newUploadLink("bobs-drop", "bob")} {
		if err := shares.Create(link, 5); err != nil {
			t.Fatal(err)
		}
	}
	content, err := os.ReadFile(filepath.Join(dir, sharesFile))
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		Shares []legacyShare `json:"shares"`
	}
	if err := json.Unmarshal(content, &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Shares) != 1 || old.Shares[0].Name != "plain-share" {
		t.Fatalf("shares.json must hold the share links only, an older release reads %+v", old.Shares)
	}
	for _, word := range []string{"drop-box", "bobs-drop", "upload", "kind"} {
		if strings.Contains(string(content), word) {
			t.Fatalf("shares.json mentions %q: %s", word, content)
		}
	}
	if links, err := os.ReadFile(filepath.Join(dir, uploadLinksFile)); err != nil || !strings.Contains(string(links), "drop-box") {
		t.Fatalf("the upload links live in their own file: %s %v", links, err)
	}
}

func TestALegacySharesFileLoadsAsShareLinks(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"shares":[{"name":"Old-Link","owner":"amy","path":"docs","dir":true,"created":"2026-11-01T10:00:00Z","downloads":4},` +
		`{"name":"note","owner":"bob","path":"n.txt","dir":false,"created":"2026-11-02T10:00:00Z","downloads":0}]}`
	if err := os.WriteFile(filepath.Join(dir, sharesFile), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	shares, err := OpenShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	link, found := shares.Get("old-link")
	if !found || link.IsUploadLink() || !link.Dir || link.Path != "docs" || link.Downloads != 4 {
		t.Fatalf("got %+v %v", link, found)
	}
	if note, _ := shares.Get("note"); note.IsUploadLink() || note.Owner != "bob" {
		t.Fatalf("got %+v", note)
	}
	if err := shares.Create(newUploadLink("OLD-LINK", "cleo"), 5); !errors.Is(err, ErrShareTaken) {
		t.Fatalf("a legacy name is still taken for upload links: %v", err)
	}
}

func TestADamagedUploadLinksFileLeavesTheShareLinks(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenShares(dir)
	if err := shares.Create(newShare("plain-share", "amy"), 5); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, uploadLinksFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if all := reopened.All(); len(all) != 1 || all[0].Name != "plain-share" {
		t.Fatalf("got %+v", all)
	}
}

func TestForgettingAnOwnerAlsoRemovesTheirUploadLinks(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	for _, link := range []Share{newUploadLink("amy-drop", "amy"), newShare("amy-share", "amy"), newUploadLink("bob-drop", "bob")} {
		if err := shares.Create(link, 5); err != nil {
			t.Fatal(err)
		}
	}
	if err := shares.ForgetOwner("amy"); err != nil {
		t.Fatal(err)
	}
	if all := shares.All(); len(all) != 1 || all[0].Name != "bob-drop" {
		t.Fatalf("got %+v", all)
	}
}

func TestSharesOwnerChangesExpiryAndItSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenShares(dir)
	share := newShare("holiday", "amy")
	share.Expires = shareEpoch.Add(24 * time.Hour)
	if err := shares.Create(share, 5); err != nil {
		t.Fatal(err)
	}

	later := shareEpoch.Add(30 * 24 * time.Hour)
	changed, err := shares.SetExpiry("amy", "HOLIDAY", later)
	if err != nil || changed.Name != "holiday" || !changed.Expires.Equal(later) {
		t.Fatalf("got %+v %v", changed, err)
	}
	reopened, _ := OpenShares(dir)
	if got, _ := reopened.Get("holiday"); !got.Expires.Equal(later) {
		t.Fatalf("expiry %v after reopening", got.Expires)
	}

	if _, err := shares.SetExpiry("amy", "holiday", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := shares.Get("holiday"); !got.Expires.IsZero() {
		t.Fatalf("a zero time must make it permanent, got %v", got.Expires)
	}
}

func TestSharesExpiryOfAMissingLinkIsMissing(t *testing.T) {
	shares, _ := OpenShares(t.TempDir())
	if _, err := shares.SetExpiry("amy", "nothing-here", shareEpoch); !errors.Is(err, ErrShareMissing) {
		t.Fatalf("got %v", err)
	}
}
