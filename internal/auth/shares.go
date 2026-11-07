package auth

import (
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const sharesFile = "shares.json"

var (
	ErrShareTaken   = errors.New("share name is taken")
	ErrShareLimit   = errors.New("too many share links")
	ErrShareMissing = errors.New("no such share link")
)

// UploadLink is the Kind of a link that only receives files into its folder.
const UploadLink = "upload"

// Share is one public link in its owner's files folder: a read-only share link to a file or folder, or an upload
// link into a folder. Both kinds share one set of names, stored lowercase. A zero Expires means the link never expires.
type Share struct {
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	Path      string    `json:"path"`
	Dir       bool      `json:"dir"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires,omitzero"`
	Downloads int64     `json:"downloads"`
	// LastUsed is the last download of a share link and the last finished file of an upload link.
	LastUsed time.Time `json:"lastUsed,omitzero"`
	Kind     string    `json:"kind,omitempty"`

	// An upload link accepts at most Cap bytes in all. Received and ReceivedBytes count the finished files, and
	// Pending holds the size each unfinished upload declared, by upload id, so the cap counts them too. Lang is
	// the owner's language for the summary emails.
	Cap           int64            `json:"cap,omitempty"`
	Received      int64            `json:"received,omitempty"`
	ReceivedBytes int64            `json:"receivedBytes,omitempty"`
	Pending       map[string]int64 `json:"pending,omitempty"`
	Lang          string           `json:"lang,omitempty"`
}

func (s Share) IsUploadLink() bool {
	return s.Kind == UploadLink
}

// Taken is what an upload link has accepted plus what its unfinished uploads may still add, leaving out stale ids.
func (s Share) Taken(stale []string) int64 {
	taken := s.ReceivedBytes
	for id, size := range s.Pending {
		if !slices.Contains(stale, id) {
			taken += size
		}
	}

	return taken
}

// Expired reports whether the link is past its expiry. The expiry moment itself already counts as expired.
func (s Share) Expired(now time.Time) bool {
	return !s.Expires.IsZero() && !now.Before(s.Expires)
}

type sharesDisk struct {
	Shares []Share `json:"shares"`
}

// Shares keeps the public links next to contacts.json. Every change is written before it is visible, so a
// failed write leaves memory and disk agreeing.
type Shares struct {
	path string

	mu     sync.RWMutex
	byName map[string]Share
}

func OpenShares(dir string) (*Shares, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	shares := &Shares{path: filepath.Join(dir, sharesFile), byName: map[string]Share{}}
	content, err := os.ReadFile(shares.path)
	if errors.Is(err, os.ErrNotExist) {
		return shares, nil
	}
	if err != nil {
		return nil, err
	}

	var loaded sharesDisk
	if err := json.Unmarshal(content, &loaded); err != nil {
		log.Printf("auth: %s is damaged (%v), starting with no share links", shares.path, err)
		if err := os.Rename(shares.path, shares.path+".bad"); err != nil {
			log.Printf("auth: could not keep the damaged file as %s.bad: %v", shares.path, err)
		}

		return shares, nil
	}
	for _, share := range loaded.Shares {
		share.Name = strings.ToLower(share.Name)
		shares.byName[share.Name] = share
	}

	return shares, nil
}

func (s *Shares) Get(name string) (Share, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, found := s.byName[strings.ToLower(name)]

	return share, found
}

func (s *Shares) ByOwner(owner string) []Share {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.sorted(func(share Share) bool { return share.Owner == owner })
}

func (s *Shares) All() []Share {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.sorted(func(Share) bool { return true })
}

func (s *Shares) sorted(keep func(Share) bool) []Share {
	found := []Share{}
	for _, share := range s.byName {
		if keep(share) {
			found = append(found, share)
		}
	}
	slices.SortFunc(found, func(a, b Share) int {
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}

		return strings.Compare(a.Name, b.Name)
	})

	return found
}

// Create refuses a name that is taken, compared case-insensitively, and an owner who already holds maxPerOwner links.
func (s *Shares) Create(share Share, maxPerOwner int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	share.Name = strings.ToLower(share.Name)
	if _, taken := s.byName[share.Name]; taken {
		return ErrShareTaken
	}
	held := 0
	for _, other := range s.byName {
		if other.Owner == share.Owner {
			held++
		}
	}
	if held >= maxPerOwner {
		return ErrShareLimit
	}
	next := maps.Clone(s.byName)
	next[share.Name] = share

	return s.save(next)
}

// SetExpiry changes the expiry of a share link the owner holds. A zero time makes it permanent. An upload link keeps
// the expiry it was made with, so it can never become permanent.
func (s *Shares) SetExpiry(owner, name string, expires time.Time) (Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	share, found := s.byName[name]
	if !found || share.Owner != owner || share.IsUploadLink() {
		return Share{}, ErrShareMissing
	}
	share.Expires = expires
	next := maps.Clone(s.byName)
	next[name] = share
	if err := s.save(next); err != nil {
		return Share{}, err
	}

	return share, nil
}

// Revoke removes a link. An empty owner revokes it whoever holds it, which is what an admin does.
func (s *Shares) Revoke(owner, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	share, found := s.byName[name]
	if !found || (owner != "" && share.Owner != owner) {
		return ErrShareMissing
	}
	next := maps.Clone(s.byName)
	delete(next, name)

	return s.save(next)
}

// Record counts one download and remembers when it happened.
func (s *Shares) Record(name string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	share, found := s.byName[name]
	if !found {
		return ErrShareMissing
	}
	share.Downloads++
	share.LastUsed = at
	next := maps.Clone(s.byName)
	next[name] = share

	return s.save(next)
}

// StartUpload adds an unfinished upload to an upload link and forgets the stale ids, whose uploads are gone.
func (s *Shares) StartUpload(name, id string, size int64, stale []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	link, found := s.byName[name]
	if !found || !link.IsUploadLink() {
		return ErrShareMissing
	}
	pending := maps.Clone(link.Pending)
	if pending == nil {
		pending = map[string]int64{}
	}
	for _, gone := range stale {
		delete(pending, gone)
	}
	pending[id] = size
	link.Pending = pending
	next := maps.Clone(s.byName)
	next[name] = link

	return s.save(next)
}

// FinishUpload counts a pending upload as a received file of the size it declared.
func (s *Shares) FinishUpload(name, id string, at time.Time) (Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	link, found := s.byName[name]
	size, pending := link.Pending[id]
	if !found || !pending {
		return Share{}, ErrShareMissing
	}
	link.Pending = maps.Clone(link.Pending)
	delete(link.Pending, id)
	link.Received++
	link.ReceivedBytes += size
	link.LastUsed = at
	next := maps.Clone(s.byName)
	next[name] = link
	if err := s.save(next); err != nil {
		return Share{}, err
	}

	return link, nil
}

// ForgetOwner removes every link of one account, so a later account with the same name starts with none.
func (s *Shares) ForgetOwner(owner string) error {
	return s.forgetWhere(func(share Share) bool { return share.Owner == owner })
}

// ForgetOwnersNotIn removes the links of every account that is not in names.
func (s *Shares) ForgetOwnersNotIn(names []string) error {
	return s.forgetWhere(func(share Share) bool { return !slices.Contains(names, share.Owner) })
}

func (s *Shares) forgetWhere(drop func(Share) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := maps.Clone(s.byName)
	for name, share := range s.byName {
		if drop(share) {
			delete(next, name)
		}
	}
	if len(next) == len(s.byName) {
		return nil
	}

	return s.save(next)
}

func (s *Shares) save(next map[string]Share) error {
	content, err := json.Marshal(sharesDisk{Shares: slices.SortedFunc(maps.Values(next), func(a, b Share) int {
		return strings.Compare(a.Name, b.Name)
	})})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, content); err != nil {
		return err
	}
	s.byName = next

	return nil
}
