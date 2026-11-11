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

const (
	sharesFile      = "shares.json"
	uploadLinksFile = "uploadlinks.json"
)

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
	// Kind is never written. Each kind has a file of its own, so a release that knows no upload links never reads
	// one as a share link and serves its folder.
	Kind string `json:"-"`

	// An upload link accepts at most Cap bytes in all. Charged is what its finished files cost against that,
	// Received and ReceivedBytes what they really are, and Pending its unfinished uploads by upload id. Visits are
	// the visitors who gave a name, by visit token. Unreported files arrived since the summary email sent at
	// Reported, the first of them at UnreportedSince. Lang is the owner's language for that email.
	Cap             int64                    `json:"cap,omitempty"`
	Charged         int64                    `json:"charged,omitempty"`
	Received        int64                    `json:"received,omitempty"`
	ReceivedBytes   int64                    `json:"receivedBytes,omitempty"`
	Pending         map[string]PendingUpload `json:"pending,omitempty"`
	Visits          map[string]Visit         `json:"visits,omitempty"`
	Unreported      int                      `json:"unreported,omitempty"`
	UnreportedSince time.Time                `json:"unreportedSince,omitzero"`
	Reported        time.Time                `json:"reported,omitzero"`
	Lang            string                   `json:"lang,omitempty"`
}

// PendingUpload is an unfinished upload through an upload link: the size it declared, the key the visitor's page
// gave it, the visit it belongs to, and where it will land.
type PendingUpload struct {
	Size  int64  `json:"size"`
	Key   string `json:"key,omitempty"`
	Visit string `json:"visit,omitempty"`
	Path  string `json:"path"`
}

// Visit is the folder named after one visitor. Made turns true once a file was finished into it.
type Visit struct {
	Folder string `json:"folder"`
	Made   bool   `json:"made,omitempty"`
}

func (s Share) IsUploadLink() bool {
	return s.Kind == UploadLink
}

// Expired reports whether the link is past its expiry. The expiry moment itself already counts as expired.
func (s Share) Expired(now time.Time) bool {
	return !s.Expires.IsZero() && !now.Before(s.Expires)
}

// clone gives the maps of a link a copy of their own, so a reader holding the link from before never sees them move.
func (s Share) clone() Share {
	s.Pending, s.Visits = maps.Clone(s.Pending), maps.Clone(s.Visits)

	return s
}

// linksDisk is the shape of both files: shares.json holds Shares, which is all a release without upload links
// reads, and uploadlinks.json holds UploadLinks.
type linksDisk struct {
	Shares      []Share `json:"shares,omitempty"`
	UploadLinks []Share `json:"uploadLinks,omitempty"`
}

// Shares keeps the public links next to contacts.json. Every change is written before it is visible, so a
// failed write leaves memory and disk agreeing. One lock covers both kinds, so names and the limit per owner
// are decided across both.
type Shares struct {
	dir string

	mu     sync.RWMutex
	byName map[string]Share
}

func OpenShares(dir string) (*Shares, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	shares := &Shares{dir: dir, byName: map[string]Share{}}
	stored, err := shares.load(sharesFile)
	if err != nil {
		return nil, err
	}
	for _, share := range stored.Shares {
		share.Name, share.Kind = strings.ToLower(share.Name), ""
		shares.byName[share.Name] = share
	}
	stored, err = shares.load(uploadLinksFile)
	if err != nil {
		return nil, err
	}
	for _, link := range stored.UploadLinks {
		link.Name, link.Kind = strings.ToLower(link.Name), UploadLink
		if _, taken := shares.byName[link.Name]; taken {
			log.Printf("auth: upload link %q has the name of a share link, leaving it out", link.Name)
			continue
		}
		shares.byName[link.Name] = link
	}

	return shares, nil
}

func (s *Shares) load(file string) (linksDisk, error) {
	path := filepath.Join(s.dir, file)
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return linksDisk{}, nil
	}
	if err != nil {
		return linksDisk{}, err
	}
	var loaded linksDisk
	if err := json.Unmarshal(content, &loaded); err != nil {
		log.Printf("auth: %s is damaged (%v), starting without the links in it", path, err)
		if err := os.Rename(path, path+".bad"); err != nil {
			log.Printf("auth: could not keep the damaged file as %s.bad: %v", path, err)
		}

		return linksDisk{}, nil
	}

	return loaded, nil
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

	return s.save(next, share.Kind)
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
	if err := s.save(next, share.Kind); err != nil {
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

	return s.save(next, share.Kind)
}

// Record counts one download of a share link and remembers when it happened.
func (s *Shares) Record(name string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	share, found := s.byName[name]
	if !found || share.IsUploadLink() {
		return ErrShareMissing
	}
	share.Downloads++
	share.LastUsed = at
	next := maps.Clone(s.byName)
	next[name] = share

	return s.save(next, share.Kind)
}

// UpdateUploadLink applies change to a copy of an upload link and saves it. When change fails nothing is saved.
func (s *Shares) UpdateUploadLink(name string, change func(link *Share) error) (Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name = strings.ToLower(name)
	link, found := s.byName[name]
	if !found || !link.IsUploadLink() {
		return Share{}, ErrShareMissing
	}
	link = link.clone()
	if err := change(&link); err != nil {
		return Share{}, err
	}
	link.Name, link.Kind = name, UploadLink
	next := maps.Clone(s.byName)
	next[name] = link
	if err := s.save(next, UploadLink); err != nil {
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

	return s.save(next, "", UploadLink)
}

// save writes the file of every kind that changed, and only then makes next visible.
func (s *Shares) save(next map[string]Share, kinds ...string) error {
	for _, kind := range kinds {
		links := []Share{}
		for _, share := range next {
			if share.Kind == kind {
				links = append(links, share)
			}
		}
		slices.SortFunc(links, func(a, b Share) int { return strings.Compare(a.Name, b.Name) })
		disk, file := linksDisk{Shares: links}, sharesFile
		if kind == UploadLink {
			disk, file = linksDisk{UploadLinks: links}, uploadLinksFile
		}
		content, err := json.Marshal(disk)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(s.dir, file), content); err != nil {
			return err
		}
	}
	s.byName = next

	return nil
}
