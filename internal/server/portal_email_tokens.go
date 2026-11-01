package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"slices"
	"sync"
	"time"
)

type linkKind uint8

const (
	kindReset linkKind = iota + 1
	kindConfirm
	kindSignIn
)

const (
	linkTokenLength = 43
	maxLinkTokens   = 1000
)

type linkToken struct {
	hash       [sha256.Size]byte
	kind       linkKind
	user       string
	email      string
	generation uint64
	expires    time.Time
}

// linkTokens holds the one-time links that were emailed. Only the hash of a token is kept, in memory,
// so a restart invalidates every open link, which is acceptable for links that live for under an hour.
type linkTokens struct {
	now func() time.Time

	mu    sync.Mutex
	items []linkToken
}

func newLinkTokens() *linkTokens {
	return &linkTokens{now: time.Now}
}

// issue replaces an earlier confirmation link of the user. Reset links are kept side by side, so someone
// asking for resets in a victim's name cannot kill the link already sitting in the victim's inbox; the send limits bound how many exist.
func (l *linkTokens) issue(kind linkKind, user, email string, generation uint64, ttl time.Duration) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	kept := l.items[:0]
	for _, item := range l.items {
		if now.Before(item.expires) && !(kind == kindConfirm && item.kind == kind && item.user == user) {
			kept = append(kept, item)
		}
	}
	l.items = kept
	if len(l.items) >= maxLinkTokens {
		l.items = l.items[1:]
	}
	l.items = append(l.items, linkToken{
		hash:       sha256.Sum256([]byte(token)),
		kind:       kind,
		user:       user,
		email:      email,
		generation: generation,
		expires:    now.Add(ttl),
	})

	return token, nil
}

// take removes the matching link whether or not it has expired, so a link can never be tried twice.
// A link presented for the wrong purpose is refused and left alone.
func (l *linkTokens) take(kind linkKind, token string) (linkToken, bool) {
	if len(token) != linkTokenLength {
		return linkToken{}, false
	}
	hash := sha256.Sum256([]byte(token))

	l.mu.Lock()
	defer l.mu.Unlock()
	found := -1
	for i, item := range l.items {
		if subtle.ConstantTimeCompare(item.hash[:], hash[:]) == 1 {
			found = i
		}
	}
	if found < 0 {
		return linkToken{}, false
	}
	item := l.items[found]
	if item.kind != kind {
		return linkToken{}, false
	}
	l.items = append(l.items[:found], l.items[found+1:]...)

	return item, l.now().Before(item.expires)
}

func (l *linkTokens) purgeUser(user string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.items[:0]
	for _, item := range l.items {
		if item.user != user {
			kept = append(kept, item)
		}
	}
	l.items = kept
}

// purgeKinds drops the user's links of the given kinds and leaves the rest, such as a pending confirmation.
func (l *linkTokens) purgeKinds(user string, kinds ...linkKind) {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.items[:0]
	for _, item := range l.items {
		if item.user != user || !slices.Contains(kinds, item.kind) {
			kept = append(kept, item)
		}
	}
	l.items = kept
}
