package auth

import (
	"errors"
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

	if err := store.SetInitialPassword("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if !store.Verify("correct horse battery") || store.Verify("wrong password!!") {
		t.Fatal("verify mismatch")
	}
	if err := store.SetInitialPassword("another long password"); !errors.Is(err, ErrAlreadyConfigured) {
		t.Fatalf("got %v", err)
	}
}

func TestPasswordSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	first, _ := Open(dir)
	if err := first.SetInitialPassword("correct horse battery"); err != nil {
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
	if err := store.SetInitialPassword("short"); !errors.Is(err, ErrPasswordTooShort) {
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
