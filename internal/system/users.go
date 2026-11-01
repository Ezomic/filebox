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

	ErrInvalidQuota      = errors.New("invalid quota")
	ErrQuotasUnavailable = errors.New("quotas are not available")
)

// Quotas are whole bytes. The lower bound catches a unit mistake such as typing 10 for 10 GB; the upper bound keeps
// every value far inside what the helper, btrfs and JSON numbers all handle exactly.
const (
	MinQuotaBytes int64 = 1 << 20
	MaxQuotaBytes int64 = 1 << 50
)

// Quota is what btrfs reports for one user's files folder. Limit 0 means no limit.
type Quota struct {
	Used  uint64
	Limit uint64
}

var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{2,31}$`)

type Users interface {
	List() ([]string, error)
	Create(name, password string) error
	SetPassword(name, password string) error
	Delete(name string) error

	// Quotas reports the btrfs usage and limit of every user whose files folder is a subvolume. Users that are not
	// converted yet are missing from the map. It fails with ErrQuotasUnavailable when the box has no btrfs quotas.
	Quotas() (map[string]Quota, error)
	SetQuota(name string, bytes int64) error
	ClearQuota(name string) error
}

func ValidQuotaBytes(bytes int64) bool {
	return bytes >= MinQuotaBytes && bytes <= MaxQuotaBytes
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
	// QuotasOff makes the quota calls fail like a box without btrfs quotas.
	QuotasOff bool

	used   map[string]uint64
	limits map[string]uint64

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
	delete(f.used, name)
	delete(f.limits, name)

	return nil
}

func (f *FakeUsers) SetUsed(name string, bytes uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.used == nil {
		f.used = map[string]uint64{}
	}
	f.used[name] = bytes
}

func (f *FakeUsers) Quotas() (map[string]Quota, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.QuotasOff {
		return nil, ErrQuotasUnavailable
	}
	quotas := make(map[string]Quota, len(f.Passwords))
	for name := range f.Passwords {
		quotas[name] = Quota{Used: f.used[name], Limit: f.limits[name]}
	}

	return quotas, nil
}

func (f *FakeUsers) SetQuota(name string, bytes int64) error {
	if !ValidQuotaBytes(bytes) {
		return ErrInvalidQuota
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch _, exists := f.Passwords[name]; {
	case f.QuotasOff:
		return ErrQuotasUnavailable
	case !exists:
		return ErrUserNotFound
	}
	if f.limits == nil {
		f.limits = map[string]uint64{}
	}
	f.limits[name] = uint64(bytes)

	return nil
}

func (f *FakeUsers) ClearQuota(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch _, exists := f.Passwords[name]; {
	case f.QuotasOff:
		return ErrQuotasUnavailable
	case !exists:
		return ErrUserNotFound
	}
	delete(f.limits, name)

	return nil
}
