package auth

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
)

const rolesFile = "roles.json"

var ErrLastAdmin = errors.New("cannot remove the last admin")

// Linux accounts have no field for a role, so the portal admins are listed in a file next to admin.json.
// A missing entry means a plain user.
type roles struct {
	Admins []string `json:"admins"`
}

func loadAdmins(path string) (map[string]struct{}, error) {
	admins := map[string]struct{}{}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return admins, nil
	}
	if err != nil {
		return nil, err
	}

	var loaded roles
	if err := json.Unmarshal(content, &loaded); err != nil {
		return nil, err
	}
	for _, name := range loaded.Admins {
		admins[name] = struct{}{}
	}

	return admins, nil
}

func (s *Store) IsAdmin(name string) bool {
	s.rolesMu.RLock()
	defer s.rolesMu.RUnlock()
	_, found := s.admins[name]

	return found
}

func (s *Store) Promote(name string) error {
	s.rolesMu.Lock()
	defer s.rolesMu.Unlock()
	if _, found := s.admins[name]; found {
		return nil
	}
	next := maps.Clone(s.admins)
	next[name] = struct{}{}

	return s.saveAdmins(next)
}

func (s *Store) Demote(name string) error {
	s.rolesMu.Lock()
	defer s.rolesMu.Unlock()

	return s.removeAdmin(name, false)
}

// DemoteUnlessLast is for admins acting through the portal; the owner uses Demote to recover from having no admin.
func (s *Store) DemoteUnlessLast(name string) error {
	s.rolesMu.Lock()
	defer s.rolesMu.Unlock()

	return s.removeAdmin(name, true)
}

func (s *Store) removeAdmin(name string, keepLast bool) error {
	if _, found := s.admins[name]; !found {
		return nil
	}
	if keepLast && len(s.admins) == 1 {
		return ErrLastAdmin
	}
	next := maps.Clone(s.admins)
	delete(next, name)

	return s.saveAdmins(next)
}

func (s *Store) saveAdmins(next map[string]struct{}) error {
	content, err := json.Marshal(roles{Admins: slices.Sorted(maps.Keys(next))})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.rolesPath, content); err != nil {
		return err
	}
	s.admins = next

	return nil
}
