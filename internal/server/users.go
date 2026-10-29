package server

import (
	"log"
	"net/http"
	"slices"

	"github.com/Ezomic/filebox/internal/system"
)

type userEntry struct {
	Name  string  `json:"name"`
	Bytes *uint64 `json:"bytes"`
}

type newUser struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request) {
	switch {
	case !s.store.Configured():
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	case !s.hasSession(r):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		s.servePage(w, r, "users.html")
	}
}

func (s *Server) listUsers(w http.ResponseWriter, _ *http.Request) {
	names, err := s.users.List()
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "could not list users")
		return
	}

	sizes, ready := s.usage.Sizes()
	entries := make([]userEntry, 0, len(names))
	for _, name := range names {
		entry := userEntry{Name: name}
		if size, ok := sizes[name]; ready && ok {
			entry.Bytes = &size
		}
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": entries, "usageReady": ready})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body newUser
	if !decodeJSON(w, r, &body) || !validName(w, body.Name) || !validPassword(w, body.Password) {
		return
	}

	switch exists, ok := s.userExists(w, body.Name); {
	case !ok:
	case exists:
		writeError(w, http.StatusConflict, "a user with that name already exists")
	default:
		if err := s.users.Create(body.Name, body.Password); err != nil {
			log.Printf("create user %q: %v", body.Name, err)
			writeError(w, http.StatusInternalServerError, "could not create the user")
			return
		}
		writeJSON(w, http.StatusCreated, userEntry{Name: body.Name})
	}
}

func (s *Server) changeUserPassword(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body credentials
	if !validName(w, name) || !decodeJSON(w, r, &body) || !validPassword(w, body.Password) {
		return
	}

	switch exists, ok := s.userExists(w, name); {
	case !ok:
	case !exists:
		writeError(w, http.StatusNotFound, "no such user")
	default:
		if err := s.users.SetPassword(name, body.Password); err != nil {
			log.Printf("change password for %q: %v", name, err)
			writeError(w, http.StatusInternalServerError, "could not change the password")
			return
		}
		s.endUserSessions(name)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(w, name) {
		return
	}

	switch exists, ok := s.userExists(w, name); {
	case !ok:
	case !exists:
		writeError(w, http.StatusNotFound, "no such user")
	default:
		if err := s.users.Delete(name); err != nil {
			log.Printf("delete user %q: %v", name, err)
			writeError(w, http.StatusInternalServerError, "could not delete the user")
			return
		}
		s.endUserSessions(name)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) endUserSessions(name string) {
	if s.onUserChanged != nil {
		s.onUserChanged(name)
	}
}

func (s *Server) userExists(w http.ResponseWriter, name string) (exists, ok bool) {
	names, err := s.users.List()
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "could not list users")
		return false, false
	}

	return slices.Contains(names, name), true
}

func validName(w http.ResponseWriter, name string) bool {
	if !system.ValidUsername(name) {
		writeError(w, http.StatusUnprocessableEntity, "username must be 3 to 32 characters: lowercase letters, digits, - and _, starting with a letter")
		return false
	}

	return true
}

func validPassword(w http.ResponseWriter, password string) bool {
	if system.ValidatePassword(password) != nil {
		writeError(w, http.StatusUnprocessableEntity, "password must be 12 to 128 characters on a single line")
		return false
	}

	return true
}
