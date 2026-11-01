package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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

func TestDamagedRolesFileStartsWithNoAdminsAndKeepsTheEvidence(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt json":     "{not json",
		"array":            `["amy"]`,
		"empty file":       "",
		"non-string entry": `{"admins":["amy",5]}`,
		"wrong type":       `{"admins":"amy"}`,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "roles.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		var logged strings.Builder
		previous := log.Writer()
		log.SetOutput(&logged)

		store, err := Open(dir)
		log.SetOutput(previous)
		if err != nil {
			t.Fatalf("%s: Open failed: %v", name, err)
		}
		if store.IsAdmin("amy") {
			t.Fatalf("%s: a damaged file must not grant admin", name)
		}
		if kept, err := os.ReadFile(path + ".bad"); err != nil || string(kept) != content {
			t.Fatalf("%s: damaged file not kept: %q %v", name, kept, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: damaged file still in place: %v", name, err)
		}
		if !strings.Contains(logged.String(), path) || (content != "" && strings.Contains(logged.String(), content)) {
			t.Fatalf("%s: log must name the file and not show its contents: %q", name, logged.String())
		}

		if err := store.Promote("bob"); err != nil {
			t.Fatalf("%s: owner recovery failed: %v", name, err)
		}
		reopened, err := Open(dir)
		if err != nil || !reopened.IsAdmin("bob") {
			t.Fatalf("%s: new roles file not written: %v", name, err)
		}
	}
}

func TestOlderBadFileIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.json")
	_ = os.WriteFile(path+".bad", []byte("old"), 0o600)
	_ = os.WriteFile(path, []byte("{new damage"), 0o600)

	if _, err := Open(dir); err != nil {
		t.Fatal(err)
	}
	if kept, _ := os.ReadFile(path + ".bad"); string(kept) != "{new damage" {
		t.Fatalf("got %q", kept)
	}
}

func TestDamagedRolesFileStillOpensWhenItCannotBeRenamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "roles.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	if err := os.MkdirAll(filepath.Join(path+".bad", "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if err != nil || store.IsAdmin("amy") {
		t.Fatalf("got %v", err)
	}
}

func TestUnreadableRolesFileStaysFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "roles.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("a file that cannot be read is a real fault, not an empty admin list")
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
