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

func newTVShare(code, channel string) TVShare {
	return TVShare{Code: code, Label: "Mum's TV", Channel: channel, Created: shareEpoch}
}

func TestTVSharesPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	shares, err := OpenTVShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newTVShare("holiday-film-k7p3x9qm", "news")); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenTVShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, found := reopened.Get("holiday-film-k7p3x9qm")
	if !found || got != newTVShare("holiday-film-k7p3x9qm", "news") {
		t.Fatalf("got %+v %v after reopening", got, found)
	}
}

// The portal lowercases a code before it asks, so the store itself compares exactly.
func TestTVSharesCompareCodesExactly(t *testing.T) {
	shares, _ := OpenTVShares(t.TempDir())
	if err := shares.Create(newTVShare("news-k7p3x9qm", "news")); err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"NEWS-K7P3X9QM", "news-k7p3x9q", "news-k7p3x9qm ", "news"} {
		if _, found := shares.Get(code); found {
			t.Errorf("%q found", code)
		}
	}
}

func TestTVSharesLowercaseCodesEditedByHand(t *testing.T) {
	dir := t.TempDir()
	content := `{"shares":[{"code":"News-K7P3X9QM","label":"x","channel":"news","created":"2026-11-04T12:00:00Z"}],"revoked":["OLD-AAAAAAAA"]}`
	if err := os.WriteFile(filepath.Join(dir, tvSharesFile), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	shares, err := OpenTVShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, found := shares.Get("news-k7p3x9qm"); !found || got.Code != "news-k7p3x9qm" {
		t.Errorf("got %+v %v", got, found)
	}
	if err := shares.Create(newTVShare("old-aaaaaaaa", "news")); !errors.Is(err, ErrTVShareTaken) {
		t.Errorf("a revoked code written in capitals was given out again: %v", err)
	}
}

func TestTVSharesNeverReuseACode(t *testing.T) {
	dir := t.TempDir()
	shares, _ := OpenTVShares(dir)
	if err := shares.Create(newTVShare("news-k7p3x9qm", "news")); err != nil {
		t.Fatal(err)
	}
	if err := shares.Create(newTVShare("news-k7p3x9qm", "sports")); !errors.Is(err, ErrTVShareTaken) {
		t.Fatalf("a live code was given out twice: %v", err)
	}
	revoked, err := shares.Revoke("news-k7p3x9qm")
	if err != nil || revoked.Channel != "news" {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, found := shares.Get("news-k7p3x9qm"); found {
		t.Fatal("a revoked code still resolves")
	}
	if _, err := shares.Revoke("news-k7p3x9qm"); !errors.Is(err, ErrTVShareMissing) {
		t.Fatalf("second revoke: %v", err)
	}
	if err := shares.Create(newTVShare("news-k7p3x9qm", "news")); !errors.Is(err, ErrTVShareTaken) {
		t.Fatalf("a revoked code was given out again: %v", err)
	}

	reopened, _ := OpenTVShares(dir)
	if err := reopened.Create(newTVShare("news-k7p3x9qm", "news")); !errors.Is(err, ErrTVShareTaken) {
		t.Fatalf("a revoked code was given out again after a restart: %v", err)
	}
}

func TestTVSharesForgetAChannel(t *testing.T) {
	shares, _ := OpenTVShares(t.TempDir())
	for _, share := range []TVShare{newTVShare("news-aaaaaaaa", "news"), newTVShare("news-bbbbbbbb", "news"), newTVShare("sport-cccccccc", "sports")} {
		if err := shares.Create(share); err != nil {
			t.Fatal(err)
		}
	}
	if err := shares.ForgetChannel("news"); err != nil {
		t.Fatal(err)
	}
	if got := shares.All(); len(got) != 1 || got[0].Code != "sport-cccccccc" {
		t.Fatalf("left %+v", got)
	}
	if err := shares.Create(newTVShare("news-aaaaaaaa", "news")); !errors.Is(err, ErrTVShareTaken) {
		t.Fatalf("a code of a deleted channel was given out again: %v", err)
	}
	if err := shares.ForgetChannel("ghost"); err != nil {
		t.Fatalf("a channel without links: %v", err)
	}
}

func TestTVSharesListNewestFirst(t *testing.T) {
	shares, _ := OpenTVShares(t.TempDir())
	for i, code := range []string{"b-aaaaaaaa", "a-bbbbbbbb", "c-cccccccc"} {
		share := newTVShare(code, "news")
		share.Created = shareEpoch.Add(time.Duration(i%2) * time.Hour)
		if err := shares.Create(share); err != nil {
			t.Fatal(err)
		}
	}
	var codes []string
	for _, share := range shares.All() {
		codes = append(codes, share.Code)
	}
	if fmt.Sprint(codes) != "[a-bbbbbbbb b-aaaaaaaa c-cccccccc]" {
		t.Fatalf("order %v", codes)
	}
}

func TestTVSharesTwoCreationsOfTheSameCodeLeaveExactlyOneWinner(t *testing.T) {
	shares, _ := OpenTVShares(t.TempDir())
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if shares.Create(newTVShare("race-aaaaaaaa", "news")) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d creations won", wins.Load())
	}
}

func TestTVSharesStartEmptyFromADamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, tvSharesFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	shares, err := OpenTVShares(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := shares.All(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, tvSharesFile+".bad")); err != nil {
		t.Fatalf("the damaged file was not kept: %v", err)
	}
}
