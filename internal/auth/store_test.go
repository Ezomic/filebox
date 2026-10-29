package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFirstRunSetsPasswordOnce(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.Configured() {
		t.Fatal("fresh store must not be configured")
	}

	if err := store.SetInitialPassword(store.SetupToken(), "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if !store.Verify("correct horse battery") || store.Verify("wrong password!!") {
		t.Fatal("verify mismatch")
	}
	if err := store.SetInitialPassword(store.SetupToken(), "another long password"); !errors.Is(err, ErrAlreadyConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestPasswordSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	first, _ := Open(dir)
	if err := first.SetInitialPassword(first.SetupToken(), "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Configured() || !second.Verify("correct horse battery") {
		t.Fatal("password lost after reopen")
	}
}

func TestShortPasswordRejected(t *testing.T) {
	store, _ := Open(t.TempDir())
	if err := store.SetInitialPassword(store.SetupToken(), "short"); !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("got %v", err)
	}
	if store.Configured() {
		t.Fatal("must stay unconfigured")
	}
}

func TestVerifyBeforeSetupFails(t *testing.T) {
	store, _ := Open(t.TempDir())
	if store.Verify("") || store.Verify("anything at all") {
		t.Fatal("unconfigured store must reject everything")
	}
}

func TestSetupTokenIsRequiredAndSingleUse(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(dir)
	token := store.SetupToken()
	if len(token) != 32 {
		t.Fatalf("token %q", token)
	}

	for _, bad := range []string{"", "nope", token + "x", token[:31]} {
		if err := store.SetInitialPassword(bad, "correct horse battery"); !errors.Is(err, ErrInvalidSetupToken) {
			t.Fatalf("token %q: got %v", bad, err)
		}
	}
	if err := store.SetInitialPassword(token, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if store.SetupToken() != "" {
		t.Fatal("token must be cleared after setup")
	}
	if _, err := os.Stat(filepath.Join(dir, "setup-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file must be removed, got %v", err)
	}
	if err := store.SetInitialPassword(token, "another long password"); !errors.Is(err, ErrAlreadyConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestSetupTokenSurvivesRestartUntilUsed(t *testing.T) {
	dir := t.TempDir()
	first, _ := Open(dir)
	second, _ := Open(dir)
	if first.SetupToken() == "" || first.SetupToken() != second.SetupToken() {
		t.Fatal("token must be stable across restarts")
	}

	if err := second.SetInitialPassword(second.SetupToken(), "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	third, _ := Open(dir)
	if third.SetupToken() != "" {
		t.Fatal("no token once configured")
	}
}

func TestWrongTokenCheckedBeforePasswordLength(t *testing.T) {
	store, _ := Open(t.TempDir())
	if err := store.SetInitialPassword("wrong", "short"); !errors.Is(err, ErrInvalidSetupToken) {
		t.Fatalf("got %v", err)
	}
}
