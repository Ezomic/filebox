package server

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type userSession struct {
	user    string
	started time.Time
	seen    time.Time
}

type userSessions struct {
	idle     time.Duration
	absolute time.Duration
	now      func() time.Time

	mu       sync.Mutex
	sessions map[string]userSession
}

func newUserSessions(idle, absolute time.Duration) *userSessions {
	return &userSessions{idle: idle, absolute: absolute, now: time.Now, sessions: map[string]userSession{}}
}

func (s *userSessions) create(user string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for existing, session := range s.sessions {
		if s.expired(session, now) {
			delete(s.sessions, existing)
		}
	}
	s.sessions[token] = userSession{user: user, started: now, seen: now}

	return token, nil
}

func (s *userSessions) lookup(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	session, found := s.sessions[token]
	if !found {
		return "", false
	}
	now := s.now()
	if s.expired(session, now) {
		delete(s.sessions, token)
		return "", false
	}
	session.seen = now
	s.sessions[token] = session

	return session.user, true
}

func (s *userSessions) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

func (s *userSessions) destroyUser(user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, session := range s.sessions {
		if session.user == user {
			delete(s.sessions, token)
		}
	}
}

func (s *userSessions) expired(session userSession, now time.Time) bool {
	return now.Sub(session.seen) > s.idle || now.Sub(session.started) > s.absolute
}
