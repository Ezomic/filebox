package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
