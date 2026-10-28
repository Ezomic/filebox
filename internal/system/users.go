package system

import (
	"errors"
	"regexp"
	"slices"
	"sync"
	"unicode/utf8"
)

const (
	minPasswordLength = 12
	maxPasswordLength = 128
)

var (
	ErrInvalidUsername = errors.New("invalid username")
	ErrInvalidPassword = errors.New("invalid password")
	ErrUserExists      = errors.New("user already exists")
	ErrUserNotFound    = errors.New("user not found")
	ErrHelperFailed    = errors.New("user helper failed")
)

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)

type Users interface {
	List() ([]string, error)
	Create(name, password string) error
	SetPassword(name, password string) error
	Delete(name string) error
}

func ValidUsername(name string) bool {
	return usernamePattern.MatchString(name)
}

// Line breaks are rejected because the helper reads the password as a single line.
func ValidatePassword(password string) error {
	length := utf8.RuneCountInString(password)
	if length < minPasswordLength || length > maxPasswordLength {
		return ErrInvalidPassword
	}
	for _, r := range password {
		if r == 0 || r == '\n' || r == '\r' {
			return ErrInvalidPassword
		}
	}

	return nil
}

type FakeUsers struct {
	ListErr   error
	Passwords map[string]string

	mu sync.Mutex
}

func NewFakeUsers(names ...string) *FakeUsers {
	users := &FakeUsers{Passwords: map[string]string{}}
	for _, name := range names {
		users.Passwords[name] = ""
	}

	return users
}

func (f *FakeUsers) List() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ListErr != nil {
		return nil, f.ListErr
	}
	names := make([]string, 0, len(f.Passwords))
	for name := range f.Passwords {
		names = append(names, name)
	}
	slices.Sort(names)

	return names, nil
}

func (f *FakeUsers) Create(name, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.Passwords[name]; exists {
		return ErrUserExists
	}
	f.Passwords[name] = password

	return nil
}

func (f *FakeUsers) SetPassword(name, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.Passwords[name]; !exists {
		return ErrUserNotFound
	}
	f.Passwords[name] = password

	return nil
}

func (f *FakeUsers) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, exists := f.Passwords[name]; !exists {
		return ErrUserNotFound
	}
	delete(f.Passwords, name)

	return nil
}
