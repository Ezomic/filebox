package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContactsPersistAndStartUnconfirmed(t *testing.T) {
	dir := t.TempDir()
	contacts, err := OpenContacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := contacts.Get("amy"); found {
		t.Fatal("a new store must be empty")
	}
	if err := contacts.Set("amy", "Amy@Example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := contacts.Get("amy"); got.Email != "Amy@Example.com" || got.Confirmed {
		t.Fatalf("got %+v", got)
	}
	if ok, err := contacts.Confirm("amy", "amy@example.COM"); !ok || err != nil {
		t.Fatalf("addresses compare case-insensitively, got %v %v", ok, err)
	}

	reopened, err := OpenContacts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := reopened.Get("amy"); got.Email != "Amy@Example.com" || !got.Confirmed {
		t.Fatalf("got %+v after reopening", got)
	}
}

func TestContactsConfirmNeedsTheCurrentAddress(t *testing.T) {
	contacts, _ := OpenContacts(t.TempDir())
	_ = contacts.Set("amy", "old@example.com")
	_ = contacts.Set("amy", "new@example.com")

	if ok, _ := contacts.Confirm("amy", "old@example.com"); ok {
		t.Fatal("a link for an address that was replaced must not confirm")
	}
	if ok, _ := contacts.Confirm("bob", "new@example.com"); ok {
		t.Fatal("another user must not confirm it")
	}
	if got, _ := contacts.Get("amy"); got.Confirmed {
		t.Fatal("must still be unconfirmed")
	}
}

func TestContactsSetResetsConfirmation(t *testing.T) {
	contacts, _ := OpenContacts(t.TempDir())
	_ = contacts.Set("amy", "a@example.com")
	_, _ = contacts.Confirm("amy", "a@example.com")
	_ = contacts.Set("amy", "b@example.com")

	if got, _ := contacts.Get("amy"); got.Confirmed {
		t.Fatal("a new address must start unconfirmed")
	}
}

func TestContactsForget(t *testing.T) {
	dir := t.TempDir()
	contacts, _ := OpenContacts(dir)
	_ = contacts.Set("amy", "a@example.com")
	if err := contacts.Forget("amy"); err != nil {
		t.Fatal(err)
	}
	reopened, _ := OpenContacts(dir)
	if _, found := reopened.Get("amy"); found {
		t.Fatal("forgotten address came back")
	}
}

func TestContactsSetAsideADamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, contactsFile), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	contacts, err := OpenContacts(dir)
	if err != nil {
		t.Fatalf("a damaged file must not stop the panel: %v", err)
	}
	if _, found := contacts.Get("amy"); found {
		t.Fatal("must start empty")
	}
	if _, err := os.Stat(filepath.Join(dir, contactsFile+".bad")); err != nil {
		t.Fatal("the damaged file should be kept as .bad")
	}
}
