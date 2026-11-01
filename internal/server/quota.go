package server

import (
	"errors"
	"log"
	"net/http"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	quotasUnavailableMessage = "quotas are not enabled for this user: the server needs btrfs quotas and the user's folder converted to a subvolume"
	overQuotaMessage         = "you are over your storage quota: empty the trash, delete files, or ask an admin for more space"
)

// quotaRequest takes whole bytes only. The friendly units live in the page, so the wire format has one meaning.
type quotaRequest struct {
	Bytes *int64 `json:"bytes"`
}

type adminQuotaRequest struct {
	Bytes   *int64 `json:"bytes"`
	Confirm string `json:"confirm"`
}

// quotaView is what both Users pages show for one user. Bytes comes from btrfs when it can, so it matches what the quota enforces.
type quotaView struct {
	Bytes      *uint64 `json:"bytes"`
	Quota      *uint64 `json:"quota"`
	QuotaReady bool    `json:"quotaReady"`
}

func loadQuotas(users system.Users) (map[string]system.Quota, bool) {
	quotas, err := users.Quotas()
	if err != nil {
		if !errors.Is(err, system.ErrQuotasUnavailable) {
			log.Printf("read quotas: %v", err)
		}
		return nil, false
	}

	return quotas, true
}

func (v *quotaView) fill(quotas map[string]system.Quota, name string) {
	quota, ok := quotas[name]
	if !ok {
		return
	}
	used := quota.Used
	v.Bytes, v.QuotaReady = &used, true
	if quota.Limit > 0 {
		limit := quota.Limit
		v.Quota = &limit
	}
}

func validQuota(w http.ResponseWriter, bytes *int64) bool {
	if bytes == nil || !system.ValidQuotaBytes(*bytes) {
		writeCoded(w, http.StatusUnprocessableEntity, codeQuotaInvalid, "the quota must be a whole number of bytes between 1 MB and 1 PB")
		return false
	}

	return true
}

// quotaFailed reports a failed helper call without its detail, which stays in the log.
func quotaFailed(w http.ResponseWriter, action, name string, err error) {
	if errors.Is(err, system.ErrQuotasUnavailable) {
		writeCoded(w, http.StatusConflict, codeQuotasUnavailable, quotasUnavailableMessage)
		return
	}
	log.Printf("%s %s: %v", action, logName(name), err)
	writeCoded(w, http.StatusInternalServerError, codeQuotaChangeFailed, "could not change the quota")
}

func (s *Server) setUserQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body quotaRequest
	if !validName(w, name) || !decodeJSON(w, r, &body) || !validQuota(w, body.Bytes) || !s.existingUser(w, name) {
		return
	}
	if err := s.users.SetQuota(name, *body.Bytes); err != nil {
		quotaFailed(w, "set quota", name, err)
		return
	}
	log.Printf("panel: owner set-quota target=%s bytes=%d", logName(name), *body.Bytes)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) clearUserQuota(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName(w, name) || !s.existingUser(w, name) {
		return
	}
	if err := s.users.ClearQuota(name); err != nil {
		quotaFailed(w, "clear quota", name, err)
		return
	}
	log.Printf("panel: owner clear-quota target=%s", logName(name))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) existingUser(w http.ResponseWriter, name string) bool {
	return existingUser(w, s.users, name)
}

func existingUser(w http.ResponseWriter, users system.Users, name string) bool {
	exists, ok := userExists(w, users, name)
	if ok && !exists {
		writeCoded(w, http.StatusNotFound, codeUserNotFound, "no such user")
	}

	return ok && exists
}

// A quota stops a user's writes, so an admin can set one on plain users only, like a password reset.
func (p *Portal) adminSetQuota(w http.ResponseWriter, r *http.Request, admin string) {
	name := r.PathValue("name")
	var body adminQuotaRequest
	if !validName(w, name) || !decodeJSON(w, r, &body) || !validQuota(w, body.Bytes) || !p.checkQuotaTarget(w, r, admin, name, body.Confirm) {
		return
	}
	defer p.space.invalidate()
	if err := p.users.SetQuota(name, *body.Bytes); err != nil {
		quotaFailed(w, "portal: set quota", name, err)
		return
	}
	log.Printf("portal: admin set-quota by=%s target=%s bytes=%d ip=%s", logName(admin), logName(name), *body.Bytes, ipKey(r))
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) adminClearQuota(w http.ResponseWriter, r *http.Request, admin string) {
	name := r.PathValue("name")
	var body adminConfirm
	if !validName(w, name) || !decodeJSON(w, r, &body) || !p.checkQuotaTarget(w, r, admin, name, body.Confirm) {
		return
	}
	defer p.space.invalidate()
	if err := p.users.ClearQuota(name); err != nil {
		quotaFailed(w, "portal: clear quota", name, err)
		return
	}
	p.audit(r, "clear-quota", admin, name)
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) checkQuotaTarget(w http.ResponseWriter, r *http.Request, admin, name, confirm string) bool {
	if !p.confirmAdmin(w, r, admin, confirm) || !p.targetExists(w, name) {
		return false
	}
	if p.roles.IsAdmin(name) {
		writeCoded(w, http.StatusForbidden, codeQuotaAdminTarget, "admins cannot set a quota on an admin")
		return false
	}

	return true
}
