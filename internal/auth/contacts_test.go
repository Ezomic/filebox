package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestContactsRefuseToConfirmAnAddressAnotherAccountHolds(t *testing.T) {
	contacts, _ := OpenContacts(t.TempDir())
	_ = contacts.Set("amy", "Shared@Example.com")
	_ = contacts.Set("bob", "shared@example.COM")
	if ok, err := contacts.Confirm("amy", "Shared@Example.com"); !ok || err != nil {
		t.Fatalf("the first account must confirm, got %v %v", ok, err)
	}

	if ok, err := contacts.Confirm("bob", "shared@example.COM"); ok || err != nil {
		t.Fatalf("a second account must not confirm the same address, got %v %v", ok, err)
	}
	if got, _ := contacts.Get("bob"); got.Confirmed {
		t.Fatal("the refused account must stay unconfirmed")
	}
	if ok, _ := contacts.Confirm("amy", "shared@example.com"); !ok {
		t.Fatal("confirming again on the owning account must stay a success")
	}
}

func TestContactsLetOneOfTwoRacingAccountsConfirm(t *testing.T) {
	contacts, _ := OpenContacts(t.TempDir())
	const accounts = 12
	for i := range accounts {
		_ = contacts.Set(fmt.Sprintf("user%02d", i), "race@example.com")
	}

	var mu sync.Mutex
	wins := 0
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := contacts.Confirm(fmt.Sprintf("user%02d", i), "race@example.com")
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if ok {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d accounts confirmed one address, want exactly 1", wins)
	}
}

func TestContactsLookupFindsOnlyConfirmedAddressesIgnoringCase(t *testing.T) {
	contacts, _ := OpenContacts(t.TempDir())
	_ = contacts.Set("amy", "Amy@Example.com")
	_ = contacts.Set("bob", "bob@example.com")
	_, _ = contacts.Confirm("amy", "amy@example.com")

	if user, ok := contacts.Lookup("AMY@example.COM"); !ok || user != "amy" {
		t.Fatalf("got %q %v", user, ok)
	}
	if _, ok := contacts.Lookup("bob@example.com"); ok {
		t.Fatal("an unconfirmed address must not resolve")
	}
	for _, address := range []string{"", " amy@example.com", "nobody@example.com", "amy@example.com."} {
		if user, ok := contacts.Lookup(address); ok {
			t.Fatalf("%q resolved to %q", address, user)
		}
	}
}

func TestContactsLookupRefusesAnAddressConfirmedTwiceInOldData(t *testing.T) {
	dir := t.TempDir()
	old := `{"users":{"amy":{"email":"dup@example.com","confirmed":true},"bob":{"email":"DUP@example.com","confirmed":true},"cleo":{"email":"cleo@example.com","confirmed":true}}}`
	if err := os.WriteFile(filepath.Join(dir, contactsFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	contacts, err := OpenContacts(dir)
	if err != nil {
		t.Fatal(err)
	}

	if user, ok := contacts.Lookup("dup@example.com"); ok {
		t.Fatalf("an ambiguous address resolved to %q", user)
	}
	if user, ok := contacts.Lookup("cleo@example.com"); !ok || user != "cleo" {
		t.Fatalf("got %q %v", user, ok)
	}
}
