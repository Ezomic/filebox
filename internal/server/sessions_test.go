package server

import (
	"testing"
	"time"
)

func TestAdminSessionIsValidUntilDestroyed(t *testing.T) {
	sessions := newSessions(time.Hour)
	token, err := sessions.create()
	if err != nil {
		t.Fatal(err)
	}
	if !sessions.valid(token) {
		t.Fatal("a fresh session must be valid")
	}
	if sessions.valid("") || sessions.valid(token+"x") {
		t.Fatal("an unknown token must not be valid")
	}
	sessions.destroy(token)
	if sessions.valid(token) {
		t.Fatal("a destroyed session must not be valid")
	}
}

func TestAdminSessionTokensAreUnique(t *testing.T) {
	sessions := newSessions(time.Hour)
	seen := map[string]bool{}
	for range 100 {
		token, err := sessions.create()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] || len(token) != 43 {
			t.Fatalf("token %q repeated or not 32 random bytes", token)
		}
		seen[token] = true
	}
}

func TestAdminSessionExpiresAndIsForgotten(t *testing.T) {
	sessions := newSessions(time.Millisecond)
	token, _ := sessions.create()
	time.Sleep(5 * time.Millisecond)

	if sessions.valid(token) {
		t.Fatal("a session past its TTL must not be valid")
	}
	if _, found := sessions.expires[token]; found {
		t.Fatal("an expired session must be removed when it is checked")
	}
}

func TestCreatingAnAdminSessionPrunesExpiredOnes(t *testing.T) {
	sessions := newSessions(time.Millisecond)
	stale, _ := sessions.create()
	time.Sleep(5 * time.Millisecond)
	sessions.ttl = time.Hour

	fresh, _ := sessions.create()
	if _, found := sessions.expires[stale]; found {
		t.Fatal("an expired session must be pruned when another is created")
	}
	if !sessions.valid(fresh) {
		t.Fatal("the new session must be valid")
	}
}
