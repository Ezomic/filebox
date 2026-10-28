package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	MinPasswordLength = 12
	iterations        = 600_000
	keyLength         = 32
)

var (
	ErrAlreadyConfigured = errors.New("admin password is already set")
	ErrPasswordTooShort  = errors.New("password is too short")
)

type record struct {
	Salt       []byte `json:"salt"`
	Hash       []byte `json:"hash"`
	Iterations int    `json:"iterations"`
}

type Store struct {
	path string

	mu     sync.RWMutex
	record *record
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	store := &Store{path: filepath.Join(dir, "admin.json")}

	content, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}

	var loaded record
	if err := json.Unmarshal(content, &loaded); err != nil {
		return nil, err
	}
	store.record = &loaded

	return store, nil
}

func (s *Store) Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.record != nil
}

func (s *Store) SetInitialPassword(password string) error {
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.record != nil {
		return ErrAlreadyConfigured
	}

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, iterations, keyLength)
	if err != nil {
		return err
	}

	created := &record{Salt: salt, Hash: hash, Iterations: iterations}
	if err := s.persist(created); err != nil {
		return err
	}
	s.record = created

	return nil
}

func (s *Store) Verify(password string) bool {
	s.mu.RLock()
	current := s.record
	s.mu.RUnlock()

	if current == nil {
		return false
	}
	hash, err := pbkdf2.Key(sha256.New, password, current.Salt, current.Iterations, keyLength)
	if err != nil {
		return false
	}

	return subtle.ConstantTimeCompare(hash, current.Hash) == 1
}

func (s *Store) persist(r *record) error {
	content, err := json.Marshal(r)
	if err != nil {
		return err
	}

	temp := s.path + ".tmp"
	if err := os.WriteFile(temp, content, 0o600); err != nil {
		return err
	}

	return os.Rename(temp, s.path)
}
