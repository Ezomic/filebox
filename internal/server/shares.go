package server

import (
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	shareNameMin     = 3
	shareNameMax     = 40
	sharesPerOwner   = 100
	randomShareName  = 10
	randomNameTries  = 8
	randomAlphabet   = "abcdefghijklmnopqrstuvwxyz0123456789"
	randomByteCutoff = 256 - 256%len(randomAlphabet)
)

var shareNamePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var shareExpiries = map[string]time.Duration{"1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "never": 0}

// Only ASCII capitals are folded. A full Unicode lowercase would turn look-alikes such as the Kelvin sign into a plain letter.
func normalizeShareName(raw string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}

		return r
	}, raw)
}

// shareNameProblem returns the error code a name earns, or "" when it may be used. The name must already be normalized.
func (p *Portal) shareNameProblem(name string) string {
	switch {
	case len(name) < shareNameMin || len(name) > shareNameMax || !shareNamePattern.MatchString(name):
		return codeShareNameInvalid
	case slices.Contains(reservedShareNames, name) || p.mux.claims(name):
		return codeShareNameReserved
	}

	return ""
}

func randomShareNameCandidate() (string, error) {
	name := make([]byte, 0, randomShareName)
	buf := make([]byte, 32)
	for len(name) < randomShareName {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < randomByteCutoff && len(name) < randomShareName {
				name = append(name, randomAlphabet[int(b)%len(randomAlphabet)])
			}
		}
	}

	return string(name), nil
}

type shareView struct {
	Name      string     `json:"name"`
	Path      string     `json:"path"`
	Dir       bool       `json:"dir"`
	Created   time.Time  `json:"created"`
	Expires   *time.Time `json:"expires"`
	Expired   bool       `json:"expired"`
	Downloads int64      `json:"downloads"`
	LastUsed  *time.Time `json:"lastUsed"`
}

type adminShareView struct {
	shareView
	Owner string `json:"owner"`
}

func (p *Portal) viewOf(share auth.Share) shareView {
	view := shareView{Name: share.Name, Path: share.Path, Dir: share.Dir, Created: share.Created, Expired: share.Expired(p.now()), Downloads: share.Downloads}
	if !share.Expires.IsZero() {
		view.Expires = &share.Expires
	}
	if !share.LastUsed.IsZero() {
		view.LastUsed = &share.LastUsed
	}

	return view
}

func (p *Portal) shareRoutes() {
	p.mux.HandleFunc("GET /shares", p.signedInPageHandler("shares.html"))
	p.mux.HandleFunc("GET /api/shares", p.requireSession(p.listShares))
	p.mux.HandleFunc("POST /api/shares", p.requireSession(p.createShare))
	p.mux.HandleFunc("PUT /api/shares/{name}", p.requireSession(p.setShareExpiry))
	p.mux.HandleFunc("DELETE /api/shares/{name}", p.requireSession(p.revokeShare))
	p.mux.HandleFunc("GET /api/admin/shares", p.requireAdmin(p.adminListShares))
	p.mux.HandleFunc("DELETE /api/admin/shares/{name}", p.requireAdmin(p.adminRevokeShare))
}

func (p *Portal) signedInPageHandler(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p.signedInPage(w, r, page)
	}
}

func (p *Portal) listShares(w http.ResponseWriter, _ *http.Request, user string) {
	views := []shareView{}
	for _, share := range p.shares.ByOwner(user) {
		views = append(views, p.viewOf(share))
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": views, "limit": sharesPerOwner})
}

type shareRequest struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Expires string `json:"expires"`
}

func (p *Portal) createShare(w http.ResponseWriter, r *http.Request, user string) {
	var body shareRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	expires, ok := p.expiryFrom(w, body.Expires)
	if !ok {
		return
	}
	name := normalizeShareName(body.Name)
	if name != "" {
		if code := p.shareNameProblem(name); code != "" {
			writeShareNameError(w, code)
			return
		}
	}
	share, ok := p.shareTarget(w, user, body.Path)
	if !ok {
		return
	}
	share.Owner, share.Created, share.Expires = user, p.now(), expires

	created, ok := p.storeShare(w, share, name)
	if !ok {
		return
	}
	log.Printf("portal: share created user=%s ip=%s", logName(user), ipKey(r))
	writeJSON(w, http.StatusCreated, p.viewOf(created))
}

func (p *Portal) expiryFrom(w http.ResponseWriter, choice string) (time.Time, bool) {
	if choice == "" {
		choice = "never"
	}
	length, known := shareExpiries[choice]
	if !known {
		writeCoded(w, http.StatusUnprocessableEntity, codeShareExpiryInvalid, "expiry must be 1d, 7d, 30d or never")
		return time.Time{}, false
	}
	if length == 0 {
		return time.Time{}, true
	}

	return p.now().Add(length), true
}

func writeShareNameError(w http.ResponseWriter, code string) {
	if code == codeShareNameReserved {
		writeCoded(w, http.StatusUnprocessableEntity, code, "that name is used by the site itself, choose another")
		return
	}
	writeCoded(w, http.StatusUnprocessableEntity, code, "a name is 3 to 40 lowercase letters, digits and single hyphens, not starting or ending with a hyphen")
}

// shareTarget checks the path against the user's own files folder and fills in what the link needs to remember about it.
func (p *Portal) shareTarget(w http.ResponseWriter, user, requested string) (auth.Share, bool) {
	clean, err := system.CleanPath(requested)
	if err != nil || clean == "" {
		writeCoded(w, http.StatusUnprocessableEntity, codeShareTargetInvalid, "choose a file or folder in your own files")
		return auth.Share{}, false
	}
	files, closeFiles, ok := p.openUserFiles(w, user)
	if !ok {
		return auth.Share{}, false
	}
	defer closeFiles()

	info, err := lstatWithoutLinks(files, clean)
	if err != nil {
		writeCoded(w, http.StatusUnprocessableEntity, codeShareTargetInvalid, "choose a file or folder in your own files")
		return auth.Share{}, false
	}

	return auth.Share{Path: clean, Dir: info.IsDir}, true
}

func (p *Portal) storeShare(w http.ResponseWriter, share auth.Share, name string) (auth.Share, bool) {
	tries := 1
	if name == "" {
		tries = randomNameTries
	}
	for range tries {
		share.Name = name
		if name == "" {
			candidate, err := randomShareNameCandidate()
			if err != nil {
				log.Printf("portal: random share name: %v", err)
				writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")
				return auth.Share{}, false
			}
			if p.shareNameProblem(candidate) != "" {
				continue
			}
			share.Name = candidate
		}

		switch err := p.shares.Create(share, sharesPerOwner); {
		case err == nil:
			return share, true
		case errors.Is(err, auth.ErrShareLimit):
			writeCoded(w, http.StatusConflict, codeShareLimit, "you have reached the limit of share links, revoke one first")
			return auth.Share{}, false
		case errors.Is(err, auth.ErrShareTaken):
			if name != "" {
				writeCoded(w, http.StatusConflict, codeShareNameTaken, "that name is already taken")
				return auth.Share{}, false
			}
		default:
			log.Printf("portal: save share: %v", err)
			writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")
			return auth.Share{}, false
		}
	}
	log.Print("portal: could not find a free random share name")
	writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")

	return auth.Share{}, false
}

func (p *Portal) setShareExpiry(w http.ResponseWriter, r *http.Request, user string) {
	var body shareRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	expires, ok := p.expiryFrom(w, body.Expires)
	if !ok {
		return
	}
	share, err := p.shares.SetExpiry(user, normalizeShareName(r.PathValue("name")), expires)
	if !p.shareChanged(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p.viewOf(share))
}

func (p *Portal) revokeShare(w http.ResponseWriter, r *http.Request, user string) {
	if p.shareChanged(w, p.shares.Revoke(user, normalizeShareName(r.PathValue("name")))) {
		w.WriteHeader(http.StatusNoContent)
	}
}

// A link that is not the caller's answers the same as one that does not exist.
func (p *Portal) shareChanged(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, auth.ErrShareMissing):
		writeCoded(w, http.StatusNotFound, codeShareNotFound, "no such link")
	default:
		log.Printf("portal: change share: %v", err)
		writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")
	}

	return false
}

func (p *Portal) adminListShares(w http.ResponseWriter, _ *http.Request, _ string) {
	views := []adminShareView{}
	for _, share := range p.shares.All() {
		views = append(views, adminShareView{shareView: p.viewOf(share), Owner: share.Owner})
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": views})
}

func (p *Portal) adminRevokeShare(w http.ResponseWriter, r *http.Request, admin string) {
	name := normalizeShareName(r.PathValue("name"))
	if !p.shareChanged(w, p.shares.Revoke("", name)) {
		return
	}
	log.Printf("portal: admin revoke share by=%s ip=%s", logName(admin), ipKey(r))
	w.WriteHeader(http.StatusNoContent)
}

// forgetSharesOfGoneUser drops the links of an account that no longer exists, so a later account with the same name starts with none.
func (p *Portal) forgetSharesOfGoneUser(user string) {
	if p.shares == nil || p.users == nil {
		return
	}
	names, err := p.users.List()
	if err != nil || slices.Contains(names, user) {
		return
	}
	if err := p.shares.ForgetOwner(user); err != nil {
		log.Printf("portal: forget shares of %s: %v", logName(user), err)
	}
}

// Links of accounts that were removed while the panel was down must not outlive them.
func (p *Portal) forgetSharesOfGoneUsers() {
	if p.shares == nil || p.users == nil {
		return
	}
	names, err := p.users.List()
	if err != nil || len(names) == 0 {
		return
	}
	if err := p.shares.ForgetOwnersNotIn(names); err != nil {
		log.Printf("portal: forget shares of removed accounts: %v", err)
	}
}

// lstatWithoutLinks describes a path in a files folder and refuses it if the path or any folder on the way is a
// symlink, so a share can never be pointed through a link at something its owner did not choose.
func lstatWithoutLinks(files system.Files, path string) (system.FileInfo, error) {
	parts := strings.Split(path, "/")
	var info system.FileInfo
	for i := range parts {
		var err error
		info, err = files.Lstat(strings.Join(parts[:i+1], "/"))
		switch {
		case err != nil:
			return system.FileInfo{}, err
		case info.Symlink:
			return system.FileInfo{}, system.ErrNotFound
		case i < len(parts)-1 && !info.IsDir:
			return system.FileInfo{}, system.ErrNotDir
		}
	}

	return info, nil
}
