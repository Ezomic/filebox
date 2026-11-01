package server

import (
	"errors"
	"log"
	"net/http"

	"github.com/Ezomic/filebox/internal/auth"
)

const (
	roleUser  = "user"
	roleAdmin = "admin"
)

// Roles is read on every request and never copied into the session, so a change applies to open sessions at once.
type Roles interface {
	IsAdmin(name string) bool
	Promote(name string) error
	Demote(name string) error
	DemoteUnlessLast(name string) error
}

type adminConfirm struct {
	Confirm string `json:"confirm"`
}

type adminPasswordReset struct {
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

type adminRoleChange struct {
	Role    string `json:"role"`
	Confirm string `json:"confirm"`
}

func (p *Portal) requireAdmin(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return p.requireSession(func(w http.ResponseWriter, r *http.Request, user string) {
		if !p.roles.IsAdmin(user) {
			writeCoded(w, http.StatusForbidden, codeAdminsOnly, "admins only")
			return
		}
		next(w, r, user)
	})
}

func (p *Portal) adminPage(w http.ResponseWriter, r *http.Request) {
	user, ok := p.currentUser(r)
	switch {
	case !ok:
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case !p.roles.IsAdmin(user):
		p.notFound(w, r)
	default:
		http.ServeFileFS(w, r, p.pages, "admin.html")
	}
}

func (p *Portal) adminList(w http.ResponseWriter, _ *http.Request, _ string) {
	names, err := p.users.List()
	if err != nil {
		log.Printf("portal: list users: %v", err)
		writeCoded(w, http.StatusInternalServerError, codeUserListFailed, "could not list users")
		return
	}

	quotas, quotasOn := loadQuotas(p.users)
	entries := make([]roleEntry, 0, len(names))
	for _, name := range names {
		entry := roleEntry{Name: name, Admin: p.roles.IsAdmin(name)}
		entry.fill(quotas, name)
		entries = append(entries, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": entries, "quotas": quotasOn})
}

type roleEntry struct {
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	quotaView
}

func (p *Portal) adminCreate(w http.ResponseWriter, r *http.Request, admin string) {
	var body newUser
	if !decodeJSON(w, r, &body) || !validName(w, body.Name) || !validPassword(w, body.Password) {
		return
	}

	exists, ok := userExists(w, p.users, body.Name)
	switch {
	case !ok:
		return
	case exists:
		writeCoded(w, http.StatusConflict, codeUserExists, "a user with that name already exists")
		return
	}
	if err := p.roles.Demote(body.Name); err != nil {
		log.Printf("portal: clear role of %s: %v", logName(body.Name), err)
		writeCoded(w, http.StatusInternalServerError, codeUserCreateFailed, "could not create the user")
		return
	}
	if err := p.users.Create(body.Name, body.Password); err != nil {
		log.Printf("portal: create user %s: %v", logName(body.Name), err)
		writeCoded(w, http.StatusInternalServerError, codeUserCreateFailed, "could not create the user")
		return
	}
	p.audit(r, "create", admin, body.Name)
	writeJSON(w, http.StatusCreated, roleEntry{Name: body.Name})
}

func (p *Portal) adminSetPassword(w http.ResponseWriter, r *http.Request, admin string) {
	name := r.PathValue("name")
	var body adminPasswordReset
	if !validName(w, name) || !decodeJSON(w, r, &body) || !validPassword(w, body.Password) {
		return
	}
	if !p.confirmAdmin(w, r, admin, body.Confirm) || !p.targetExists(w, name) {
		return
	}
	if p.roles.IsAdmin(name) {
		writeCoded(w, http.StatusForbidden, codeAdminProtected, "admins cannot reset another admin's password")
		return
	}

	if err := p.users.SetPassword(name, body.Password); err != nil {
		log.Printf("portal: reset password for %s: %v", logName(name), err)
		writeCoded(w, http.StatusInternalServerError, codePasswordChangeFailed, "could not change the password")
		return
	}
	p.sessions.destroyUser(name)
	p.audit(r, "reset-password", admin, name)
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) adminSetRole(w http.ResponseWriter, r *http.Request, admin string) {
	name := r.PathValue("name")
	var body adminRoleChange
	if !validName(w, name) || !decodeJSON(w, r, &body) {
		return
	}
	if body.Role != roleUser && body.Role != roleAdmin {
		writeCoded(w, http.StatusUnprocessableEntity, codeRoleInvalid, "role must be user or admin")
		return
	}
	if body.Role == roleUser && name == admin {
		writeCoded(w, http.StatusConflict, codeOwnAdminRole, "you cannot remove your own admin role")
		return
	}
	if !p.confirmAdmin(w, r, admin, body.Confirm) || !p.targetExists(w, name) {
		return
	}

	action := "promote"
	err := p.roles.Promote(name)
	if body.Role == roleUser {
		action = "demote"
		err = p.roles.DemoteUnlessLast(name)
	}
	switch {
	case errors.Is(err, auth.ErrLastAdmin):
		writeCoded(w, http.StatusConflict, codeLastAdmin, "there must always be at least one admin")
		return
	case err != nil:
		log.Printf("portal: %s %s: %v", action, logName(name), err)
		writeCoded(w, http.StatusInternalServerError, codeRoleChangeFailed, "could not change the role")
		return
	}
	p.audit(r, action, admin, name)
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) adminDelete(w http.ResponseWriter, r *http.Request, admin string) {
	name := r.PathValue("name")
	var body adminConfirm
	if !validName(w, name) || !decodeJSON(w, r, &body) {
		return
	}
	if name == admin {
		writeCoded(w, http.StatusConflict, codeOwnAccount, "you cannot delete your own account")
		return
	}
	if !p.confirmAdmin(w, r, admin, body.Confirm) || !p.targetExists(w, name) {
		return
	}

	wasAdmin := p.roles.IsAdmin(name)
	switch err := p.roles.DemoteUnlessLast(name); {
	case errors.Is(err, auth.ErrLastAdmin):
		writeCoded(w, http.StatusConflict, codeLastAdmin, "there must always be at least one admin")
		return
	case err != nil:
		log.Printf("portal: clear role of %s: %v", logName(name), err)
		writeCoded(w, http.StatusInternalServerError, codeUserDeleteFailed, "could not delete the user")
		return
	}
	if err := p.users.Delete(name); err != nil {
		if wasAdmin {
			if restoreErr := p.roles.Promote(name); restoreErr != nil {
				log.Printf("portal: restore role of %s: %v", logName(name), restoreErr)
			}
		}
		log.Printf("portal: delete user %s: %v", logName(name), err)
		writeCoded(w, http.StatusInternalServerError, codeUserDeleteFailed, "could not delete the user")
		return
	}
	p.EndSessions(name)
	p.audit(r, "delete", admin, name)
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) confirmAdmin(w http.ResponseWriter, r *http.Request, admin, password string) bool {
	return p.confirmPassword(w, r, admin, password, "admin confirmation", codeWrongPassword, "your password is wrong")
}

func (p *Portal) targetExists(w http.ResponseWriter, name string) bool {
	exists, ok := userExists(w, p.users, name)
	if ok && !exists {
		writeCoded(w, http.StatusNotFound, codeUserNotFound, "no such user")
	}

	return ok && exists
}

func (p *Portal) audit(r *http.Request, action, admin, target string) {
	log.Printf("portal: admin %s by=%s target=%s ip=%s", action, logName(admin), logName(target), ipKey(r))
}
