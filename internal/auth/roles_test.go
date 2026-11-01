package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openRoles(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	return store, dir
}

func TestNobodyIsAnAdminByDefault(t *testing.T) {
	store, _ := openRoles(t)
	if store.IsAdmin("amy") || store.IsAdmin("") {
		t.Fatal("a fresh store must have no admins")
	}
}

func TestPromoteSurvivesReopenAndDemoteRemoves(t *testing.T) {
	store, dir := openRoles(t)
	if err := store.Promote("amy"); err != nil {
		t.Fatal(err)
	}
	if err := store.Promote("amy"); err != nil {
		t.Fatalf("promoting twice must be fine, got %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.IsAdmin("amy") || reopened.IsAdmin("bob") {
		t.Fatal("role lost after reopen")
	}

	if err := reopened.Demote("amy"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Demote("bob"); err != nil {
		t.Fatalf("demoting a plain user must be a no-op, got %v", err)
	}
	again, _ := Open(dir)
	if again.IsAdmin("amy") {
		t.Fatal("demotion lost after reopen")
	}
}

func TestDemoteUnlessLastKeepsTheLastAdmin(t *testing.T) {
	store, _ := openRoles(t)
	_ = store.Promote("amy")
	_ = store.Promote("bob")

	if err := store.DemoteUnlessLast("bob"); err != nil {
		t.Fatal(err)
	}
	if err := store.DemoteUnlessLast("amy"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("got %v", err)
	}
	if !store.IsAdmin("amy") {
		t.Fatal("the last admin must stay")
	}
	if err := store.DemoteUnlessLast("cleo"); err != nil {
		t.Fatalf("a plain user is not an admin to remove, got %v", err)
	}
}

func TestRolesFileIsPrivateAtomicAndLeavesNoTemp(t *testing.T) {
	store, dir := openRoles(t)
	if err := store.Promote("amy"); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "roles.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Admins []string `json:"admins"`
	}
	if err := json.Unmarshal(content, &decoded); err != nil || len(decoded.Admins) != 1 || decoded.Admins[0] != "amy" {
		t.Fatalf("got %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "roles.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

func TestFailedWriteLeavesTheRoleUnchanged(t *testing.T) {
	store, dir := openRoles(t)
	if err := os.Mkdir(filepath.Join(dir, "roles.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := store.Promote("amy"); err == nil {
		t.Fatal("expected the write to fail")
	}
	if store.IsAdmin("amy") {
		t.Fatal("a role that could not be saved must not apply")
	}
}

func TestCorruptRolesFileRefusesToOpen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "roles.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a damaged roles file must not silently mean no admins")
	}
}

func TestConcurrentRoleChangesAreAllKeptAndFileStaysValid(t *testing.T) {
	store, dir := openRoles(t)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.Promote(fmt.Sprintf("user%02d", i)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		if !reopened.IsAdmin(fmt.Sprintf("user%02d", i)) {
			t.Fatalf("user%02d lost", i)
		}
	}
}

func TestConcurrentDemotionsNeverRemoveTheLastAdmin(t *testing.T) {
	store, _ := openRoles(t)
	_ = store.Promote("amy")
	_ = store.Promote("bob")

	var wg sync.WaitGroup
	for _, name := range []string{"amy", "bob"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = store.DemoteUnlessLast(name)
		}()
	}
	wg.Wait()

	if store.IsAdmin("amy") == store.IsAdmin("bob") {
		t.Fatal("exactly one admin must remain")
	}
}
