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

const tvSharesFile = "tv-shares.json"

var (
	ErrTVShareTaken   = errors.New("share link code is taken")
	ErrTVShareMissing = errors.New("no such share link")
)

// TVShare is one link that plays a single TV channel without signing in. Codes are lowercase; the store
// compares them exactly, so callers lowercase what a visitor typed before asking.
type TVShare struct {
	Code    string    `json:"code"`
	Label   string    `json:"label"`
	Channel string    `json:"channel"`
	Created time.Time `json:"created"`
}

type tvSharesDisk struct {
	Shares  []TVShare `json:"shares"`
	Revoked []string  `json:"revoked"`
}

// TVShares keeps the TV share links next to shares.json. Revoked codes are remembered so a code is never
// given out a second time. Every change is written before it is visible.
//
// Changes take write for as long as the file is being written; lookups only wait for mu, which is held just
// to swap the maps, so a slow disk never stalls a player.
type TVShares struct {
	path string

	write   sync.Mutex
	mu      sync.RWMutex
	byCode  map[string]TVShare
	revoked map[string]bool
}

func OpenTVShares(dir string) (*TVShares, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	shares := &TVShares{path: filepath.Join(dir, tvSharesFile), byCode: map[string]TVShare{}, revoked: map[string]bool{}}
	content, err := os.ReadFile(shares.path)
	if errors.Is(err, os.ErrNotExist) {
		return shares, nil
	}
	if err != nil {
		return nil, err
	}

	var loaded tvSharesDisk
	if err := json.Unmarshal(content, &loaded); err != nil {
		log.Printf("auth: %s is damaged (%v), starting with no TV share links", shares.path, err)
		if err := os.Rename(shares.path, shares.path+".bad"); err != nil {
			log.Printf("auth: could not keep the damaged file as %s.bad: %v", shares.path, err)
		}

		return shares, nil
	}
	for _, share := range loaded.Shares {
		share.Code = strings.ToLower(share.Code)
		shares.byCode[share.Code] = share
	}
	for _, code := range loaded.Revoked {
		shares.revoked[strings.ToLower(code)] = true
	}

	return shares, nil
}

func (s *TVShares) Get(code string) (TVShare, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	share, found := s.byCode[code]

	return share, found
}

// All lists the links newest first.
func (s *TVShares) All() []TVShare {
	s.mu.RLock()
	defer s.mu.RUnlock()

	found := slices.Collect(maps.Values(s.byCode))
	slices.SortFunc(found, func(a, b TVShare) int {
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}

		return strings.Compare(a.Code, b.Code)
	})

	return found
}

// Create refuses a code that is in use or was ever revoked.
func (s *TVShares) Create(share TVShare) error {
	s.write.Lock()
	defer s.write.Unlock()

	if _, taken := s.byCode[share.Code]; taken || s.revoked[share.Code] {
		return ErrTVShareTaken
	}
	next := maps.Clone(s.byCode)
	next[share.Code] = share

	return s.save(next, s.revoked)
}

func (s *TVShares) Revoke(code string) (TVShare, error) {
	s.write.Lock()
	defer s.write.Unlock()

	share, found := s.byCode[code]
	if !found {
		return TVShare{}, ErrTVShareMissing
	}
	if err := s.drop(func(other TVShare) bool { return other.Code == code }); err != nil {
		return TVShare{}, err
	}

	return share, nil
}

// ForgetChannel revokes every link to a channel, for when the channel itself is deleted.
func (s *TVShares) ForgetChannel(channel string) error {
	s.write.Lock()
	defer s.write.Unlock()

	return s.drop(func(share TVShare) bool { return share.Channel == channel })
}

func (s *TVShares) drop(matches func(TVShare) bool) error {
	next, revoked := maps.Clone(s.byCode), maps.Clone(s.revoked)
	for code, share := range s.byCode {
		if matches(share) {
			delete(next, code)
			revoked[code] = true
		}
	}
	if len(next) == len(s.byCode) {
		return nil
	}

	return s.save(next, revoked)
}

func (s *TVShares) save(next map[string]TVShare, revoked map[string]bool) error {
	content, err := json.Marshal(tvSharesDisk{
		Shares:  slices.SortedFunc(maps.Values(next), func(a, b TVShare) int { return strings.Compare(a.Code, b.Code) }),
		Revoked: slices.Sorted(maps.Keys(revoked)),
	})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.path, content); err != nil {
		return err
	}
	s.mu.Lock()
	s.byCode, s.revoked = next, revoked
	s.mu.Unlock()

	return nil
}
