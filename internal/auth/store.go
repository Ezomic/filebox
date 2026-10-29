package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	MinPasswordLength = 12
	iterations        = 600_000
	keyLength         = 32
	tokenBytes        = 16
	tokenFile         = "setup-token"
)

var (
	ErrAlreadyConfigured = errors.New("admin password is already set")
	ErrPasswordTooShort  = errors.New("password is too short")
	ErrInvalidSetupToken = errors.New("setup token is wrong")
)

type record struct {
	Salt       []byte `json:"salt"`
	Hash       []byte `json:"hash"`
	Iterations int    `json:"iterations"`
}

type Store struct {
	path      string
	tokenPath string

	mu         sync.RWMutex
	record     *record
	setupToken string
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	store := &Store{path: filepath.Join(dir, "admin.json"), tokenPath: filepath.Join(dir, tokenFile)}

	content, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		if store.setupToken, err = loadOrCreateToken(store.tokenPath); err != nil {
			return nil, err
		}

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

// SetupToken is empty once an admin exists.
func (s *Store) SetupToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.setupToken
}

func (s *Store) SetInitialPassword(token, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.record != nil {
		return ErrAlreadyConfigured
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.setupToken)) != 1 {
		return ErrInvalidSetupToken
	}
	if len(password) < MinPasswordLength {
		return ErrPasswordTooShort
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
	s.setupToken = ""
	_ = os.Remove(s.tokenPath)

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
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	return os.Rename(temp, s.path)
}

// The token is kept on disk so it survives the restarts that happen before tailscale is up.
func loadOrCreateToken(path string) (string, error) {
	if content, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(content)); len(token) == tokenBytes*2 {
			return token, nil
		}
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)

	return token, os.WriteFile(path, []byte(token+"\n"), 0o600)
}
