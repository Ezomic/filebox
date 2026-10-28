package server

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type sessions struct {
	ttl time.Duration

	mu      sync.Mutex
	expires map[string]time.Time
}

func newSessions(ttl time.Duration) *sessions {
	return &sessions{ttl: ttl, expires: map[string]time.Time{}}
}

func (s *sessions) create() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.expires[token] = time.Now().Add(s.ttl)

	return token, nil
}

func (s *sessions) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	expiry, found := s.expires[token]
	if !found {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.expires, token)
		return false
	}

	return true
}

func (s *sessions) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expires, token)
}
