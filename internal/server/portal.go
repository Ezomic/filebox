package server

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/mail"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	portalCookieName   = "__Host-filebox_portal"
	portalIdleTTL      = time.Hour
	portalAbsoluteTTL  = 12 * time.Hour
	portalIPFails      = 5
	portalUserFails    = 20
	portalMaxVerifying = 4
	portalVerifyWait   = 2 * time.Second
	maxLoggedName      = 40
)

type PortalDeps struct {
	Verifier system.Verifier
	Users    system.Users
	Roles    Roles
	// OpenFiles returns a Files confined to that user's own folder; it is closed after each request.
	OpenFiles func(user string) (system.Files, error)
	// OpenFamily returns a Files confined to the folder every signed-in user shares; nil switches the Family pages off.
	OpenFamily func() (system.Files, error)
	// OpenVersions returns the earlier versions of that user's files; nil switches Earlier versions off.
	OpenVersions func(user string) (system.Versions, error)
	// OpenFamilyVersions returns the earlier versions of the family folder; nil switches them off for that folder.
	OpenFamilyVersions func() (system.Versions, error)
	MaxUpload          int64
	// Disk reports the usage of the filesystem holding the files; nil turns the storage bar off and never blocks an upload.
	Disk system.DiskSpace
	// TV is the streambox add-on; nil when it is not installed.
	TV *TV
	// Thumbs makes photo thumbnails; nil turns the thumbnail routes into a 503.
	Thumbs *Thumbs
	// Contacts, Mail and PortalURL switch on email addresses and password reset by email; all three are needed.
	Contacts  Contacts
	Mail      mail.Sender
	PortalURL string
	// Shares holds the public share links; nil switches share links off.
	Shares *auth.Shares
}

// Portal is the public, per-user web interface. It is a separate handler from the admin
// panel so none of the admin routes exist on the listener that faces the internet.
type Portal struct {
	verifier   system.Verifier
	users      system.Users
	roles      Roles
	openFiles  func(user string) (system.Files, error)
	openFamily func() (system.Files, error)

	openVersions       func(user string) (system.Versions, error)
	openFamilyVersions func() (system.Versions, error)

	maxUpload  int64
	space      *storageProbe
	thumbs     *Thumbs
	sessions   *userSessions
	ipLimit    *loginLimiter
	userLimit  *loginLimiter
	verifying  chan struct{}
	verifyWait time.Duration
	pages      fs.FS
	mux        *routes
	shares     *auth.Shares
	now        func() time.Time
	shareMiss  *loginLimiter
	shareFlood *loginLimiter
	email      *emailFlow
	tv         *TV
	tvLimit    *loginLimiter
}

func NewPortal(deps PortalDeps) *Portal {
	pages, err := fs.Sub(webFiles, "web/portal")
	if err != nil {
		panic(err)
	}
	shared, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}

	p := &Portal{
		verifier:   deps.Verifier,
		users:      deps.Users,
		roles:      deps.Roles,
		openFiles:  deps.OpenFiles,
		openFamily: deps.OpenFamily,

		openVersions:       deps.OpenVersions,
		openFamilyVersions: deps.OpenFamilyVersions,

		maxUpload:  deps.MaxUpload,
		space:      newStorageProbe(deps.Disk, deps.Users),
		thumbs:     deps.Thumbs,
		sessions:   newUserSessions(portalIdleTTL, portalAbsoluteTTL),
		ipLimit:    newLoginLimiter(portalIPFails, failWindow),
		userLimit:  newLoginLimiter(portalUserFails, failWindow),
		verifying:  make(chan struct{}, portalMaxVerifying),
		verifyWait: portalVerifyWait,
		pages:      pages,
		email:      newEmailFlow(deps),
		tv:         deps.TV,
		tvLimit:    newLoginLimiter(tvIPFails, failWindow),
		shares:     deps.Shares,
		now:        time.Now,
		shareMiss:  newLoginLimiter(shareMissLimit, failWindow),
		shareFlood: newLoginLimiter(shareFloodLimit, shareFloodSpan),
	}
	p.forgetSharesOfGoneUsers()

	p.mux = newRoutes()
	p.emailRoutes()
	p.mux.HandleFunc("GET /robots.txt", robotsTxt)
	p.mux.Handle("GET /static/style.css", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/files.js", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/trash.js", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/viewer.js", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/photos.js", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/storage.js", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/i18n.js", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/nl.json", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.Handle("GET /static/en.json", http.StripPrefix("/static/", http.FileServerFS(sharedAssets{shared})))
	p.mux.HandleFunc("GET /manifest.webmanifest", installAsset(shared, "manifest.webmanifest", "application/manifest+json"))
	for _, icon := range []string{"icon-192.png", "icon-512.png", "icon-maskable-512.png", "apple-touch-icon.png"} {
		p.mux.HandleFunc("GET /"+icon, installAsset(shared, icon, "image/png"))
	}
	p.mux.Handle("GET /static/", http.StripPrefix("/static/", assetsOnly(http.FileServerFS(p.pages))))
	p.mux.HandleFunc("/", p.catchAll)
	p.mux.HandleFunc("GET /{$}", p.homePage)
	p.mux.HandleFunc("GET /login", p.loginPage)
	p.mux.HandleFunc("POST /api/login", p.login)
	p.mux.HandleFunc("POST /api/logout", p.requireSession(p.logout))
	p.mux.HandleFunc("GET /api/me", p.requireSession(p.me))
	p.mux.HandleFunc("PUT /api/me/password", p.requireSession(p.changePassword))
	p.mux.HandleFunc("GET /password", p.passwordPage)
	p.mux.HandleFunc("GET /admin/users", p.adminPage)
	p.mux.HandleFunc("GET /api/admin/users", p.requireAdmin(p.adminList))
	p.mux.HandleFunc("POST /api/admin/users", p.requireAdmin(p.adminCreate))
	p.mux.HandleFunc("PUT /api/admin/users/{name}/password", p.requireAdmin(p.adminSetPassword))
	p.mux.HandleFunc("PUT /api/admin/users/{name}/role", p.requireAdmin(p.adminSetRole))
	p.mux.HandleFunc("PUT /api/admin/users/{name}/quota", p.requireAdmin(p.adminSetQuota))
	p.mux.HandleFunc("DELETE /api/admin/users/{name}/quota", p.requireAdmin(p.adminClearQuota))
	p.mux.HandleFunc("DELETE /api/admin/users/{name}", p.requireAdmin(p.adminDelete))
	p.mux.HandleFunc("GET /api/storage", p.requireSession(p.storage))
	p.fileAPI("GET /api/files", fileHandlers.listFiles)
	p.fileAPI("DELETE /api/files", fileHandlers.deleteFile)
	p.fileAPI("GET /api/files/download", fileHandlers.downloadFile)
	p.fileAPI("GET /api/files/view", fileHandlers.viewFile)
	p.mux.HandleFunc("GET /photos", p.photosPage)
	p.mux.HandleFunc("GET /backup", p.backupPage)
	p.mux.HandleFunc("GET /api/files/photos", p.fileRoute(fileHandlers.listPhotos))
	p.mux.HandleFunc("PUT /api/files/photo-date", p.fileRoute(fileHandlers.setPhotoDate))
	p.mux.HandleFunc("DELETE /api/files/photo-date", p.fileRoute(fileHandlers.resetPhotoDate))
	p.fileAPI("GET /api/files/thumb", fileHandlers.thumbFile)
	p.fileAPI("GET /api/files/preview", fileHandlers.previewFile)
	p.fileAPI("PUT /api/files/content", fileHandlers.uploadFile)
	p.fileAPI("POST /api/files/folder", fileHandlers.createFolder)
	p.fileAPI("POST /api/files/rename", fileHandlers.renameFile)
	p.fileAPI("POST /api/files/exists", fileHandlers.checkExists)
	p.fileAPI("GET /api/files/zip", fileHandlers.downloadZip)
	p.mux.HandleFunc("GET /trash", p.trashPage)
	p.fileAPI("GET /api/trash", fileHandlers.listTrash)
	p.fileAPI("DELETE /api/trash", fileHandlers.emptyTrash)
	p.fileAPI("POST /api/trash/{id}/restore", fileHandlers.restoreTrash)
	p.fileAPI("DELETE /api/trash/{id}", fileHandlers.purgeTrash)
	if p.openFamily != nil {
		p.mux.HandleFunc("GET /family", p.familyPage)
		p.mux.HandleFunc("GET /family/trash", p.familyTrashPage)
		p.mux.HandleFunc("GET /api/family/storage", p.requireSession(p.familyStorage))
	}
	p.mux.HandleFunc("GET /api/tv", p.requireSession(p.tvInfo))
	if p.tv != nil {
		p.mux.HandleFunc("GET /tv", p.tvPage)
		p.mux.HandleFunc("GET /tv/s/{token}/playlist.m3u", p.tvPlaylist)
		p.mux.HandleFunc("GET /tv/s/{token}/epg.xml", p.tvGuide)
		p.mux.HandleFunc("GET /tv/s/{token}/hls/{id}/{file}", p.tvSegment)
	}
	p.fileAPI("POST /api/uploads", fileHandlers.createUpload)
	p.fileAPI("GET /api/uploads/{id}", fileHandlers.uploadStatus)
	p.fileAPI("PUT /api/uploads/{id}", fileHandlers.appendChunk)
	p.fileAPI("POST /api/uploads/{id}/finish", fileHandlers.finishUpload)
	if p.openVersions != nil {
		p.versionRoutes()
	}
	if p.shares != nil {
		p.shareRoutes()
	}

	return p
}

func (p *Portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Strict-Transport-Security", "max-age=31536000")
	header.Set("X-Robots-Tag", "noindex, nofollow")
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)

		return
	}
	securityHeaders(p.mux).ServeHTTP(w, r)
}

func sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)

	return err == nil && parsed.Host == r.Host
}

// EndSessions signs a user out everywhere, for example after an admin changes or deletes the account.
func (p *Portal) EndSessions(user string) {
	p.sessions.destroyUser(user)
	p.email.dropIfGone(user, p.users)
	p.forgetSharesOfGoneUser(user)
}

func robotsTxt(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "User-agent: *\nDisallow: /\n")
}

// sharedAssets exposes only the stylesheet and the file browser script from the admin
// assets, never the admin pages or its other scripts.
type sharedAssets struct{ fs.FS }

func (s sharedAssets) Open(name string) (fs.File, error) {
	switch name {
	case "style.css", "files.js", "trash.js", "viewer.js", "photos.js", "storage.js", "i18n.js", "nl.json", "en.json":
		return s.FS.Open(name)
	}

	return nil, fs.ErrNotExist
}

// The manifest and icons are public so the browser can fetch them on the sign-in page, and they hold nothing but static files.
func installAsset(shared fs.FS, name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		http.ServeFileFS(w, r, shared, name)
	}
}

// Pages that must not reveal themselves answer with this too, so they look like any other unknown path.
func (p *Portal) notFound(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(p.pages, "notfound.html")
	if strings.HasPrefix(r.URL.Path, "/api/") || err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(page)
}

func (p *Portal) homePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "index.html")
}

func (p *Portal) trashPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "trash.html")
}

func (p *Portal) photosPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "photos.html")
}

func (p *Portal) backupPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "backup.html")
}

func (p *Portal) passwordPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "password.html")
}

func (p *Portal) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "login.html")
}

type portalCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (p *Portal) login(w http.ResponseWriter, r *http.Request) {
	var body portalCredentials
	if !decodeJSON(w, r, &body) {
		return
	}

	ip := ipKey(r)
	if !p.ipLimit.begin(ip) {
		log.Printf("portal: login blocked ip=%s", ip)
		writeCoded(w, http.StatusTooManyRequests, codeRateLimited, "too many failed attempts, try again later")
		return
	}
	defer p.ipLimit.end(ip)
	target, valid := p.signInTarget(body.Username)
	if !valid {
		p.ipLimit.fail(ip)
		log.Printf("portal: login failed user=%s ip=%s", logName(body.Username), ip)
		writeCoded(w, http.StatusUnauthorized, codeInvalidCredentials, "wrong username or password")
		return
	}
	if !target.limit.begin(target.limitKey) {
		log.Printf("portal: login blocked user=%s ip=%s", logName(body.Username), ip)
		writeCoded(w, http.StatusTooManyRequests, codeRateLimited, "too many failed attempts, try again later")
		return
	}
	defer target.limit.end(target.limitKey)

	generation := p.sessions.generation(target.account)
	ok, err := p.verify(target.account, body.Password)
	switch {
	case errors.Is(err, errPortalBusy):
		writeCoded(w, http.StatusServiceUnavailable, codeBusy, "busy, try again in a moment")
		return
	case err != nil:
		log.Printf("portal: password check failed: %v", err)
		writeCoded(w, http.StatusInternalServerError, codePasswordCheckFailed, "could not check the password right now")
		return
	case !ok || !target.real:
		p.ipLimit.fail(ip)
		target.limit.fail(target.limitKey)
		log.Printf("portal: login failed user=%s ip=%s", logName(body.Username), ip)
		writeCoded(w, http.StatusUnauthorized, codeInvalidCredentials, "wrong username or password")
		return
	}

	if !p.startSession(w, target.account, generation) {
		return
	}
	target.limit.reset(target.limitKey)
	log.Printf("portal: login ok user=%s ip=%s", target.account, ip)
}

func (p *Portal) startSession(w http.ResponseWriter, user string, generation uint64) bool {
	token, err := p.sessions.create(user, generation)
	switch {
	case errors.Is(err, errStaleLogin):
		writeCoded(w, http.StatusConflict, codeAccountChanged, "your account just changed, try again")
		return false
	case err != nil:
		writeCoded(w, http.StatusInternalServerError, codeSessionFailed, "could not start session")
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     portalCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(portalAbsoluteTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)

	return true
}

var errPortalBusy = errors.New("too many password checks running")

// Each check spawns a root helper that runs a deliberately slow hash, so only a few run at once.
// A short wait smooths over bursts; a flood is turned away instead of queueing without bound.
func (p *Portal) verify(name, password string) (bool, error) {
	timer := time.NewTimer(p.verifyWait)
	defer timer.Stop()

	select {
	case p.verifying <- struct{}{}:
		defer func() { <-p.verifying }()
	case <-timer.C:
		return false, errPortalBusy
	}

	return p.verifier.Verify(name, password)
}

// IPv6 clients are limited per /64 because one subscriber normally owns a whole prefix.
func ipKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	if addr.Is6() {
		if prefix, err := addr.Prefix(64); err == nil {
			return prefix.String()
		}
	}

	return addr.String()
}

// Attacker-chosen names are cut short and quoted so they cannot flood or forge log lines.
func logName(name string) string {
	if len(name) > maxLoggedName {
		name = name[:maxLoggedName]
	}

	return strconv.Quote(name)
}

func (p *Portal) logout(w http.ResponseWriter, r *http.Request, _ string) {
	if cookie, err := r.Cookie(portalCookieName); err == nil {
		p.sessions.destroy(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: portalCookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (p *Portal) me(w http.ResponseWriter, _ *http.Request, user string) {
	writeJSON(w, http.StatusOK, map[string]any{"user": user, "admin": p.roles.IsAdmin(user)})
}

func (p *Portal) currentUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(portalCookieName)
	if err != nil {
		return "", false
	}

	return p.sessions.lookup(cookie.Value)
}

func (p *Portal) requireSession(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := p.currentUser(r)
		if !ok {
			writeCoded(w, http.StatusUnauthorized, codeNotSignedIn, "not signed in")
			return
		}
		next(w, r, user)
	}
}

// The user's files folder is opened per request, so a handler can only ever see the signed-in user's own files.
func (p *Portal) fileRoute(call func(fileHandlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return p.scopedRoute(p.openUserFiles, call)
}

// The family folder is the same file API opened on a different root. Which root is decided here, by the route, and
// never by a parameter the client sends, so a request cannot choose its way from one folder into the other.
func (p *Portal) familyRoute(call func(fileHandlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return p.scopedRoute(p.openFamilyFiles, call)
}

// fileAPI serves one file route for the signed-in user's own folder, and under /api/family for the shared folder.
func (p *Portal) fileAPI(pattern string, call func(fileHandlers, http.ResponseWriter, *http.Request)) {
	p.mux.HandleFunc(pattern, p.fileRoute(call))
	if p.openFamily == nil {
		return
	}
	method, path, _ := strings.Cut(pattern, " ")
	p.mux.HandleFunc(method+" /api/family"+strings.TrimPrefix(path, "/api"), p.familyRoute(call))
}

func (p *Portal) scopedRoute(open func(http.ResponseWriter, string) (system.Files, func(), bool), call func(fileHandlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return p.requireSession(func(w http.ResponseWriter, r *http.Request, user string) {
		files, closeFiles, ok := open(w, user)
		if !ok {
			return
		}
		defer closeFiles()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			defer p.space.invalidate()
		}
		call(fileHandlers{files: files, maxUpload: p.maxUpload, space: p.space, thumbs: p.thumbs}, w, r)
	})
}

func (p *Portal) openUserFiles(w http.ResponseWriter, user string) (files system.Files, closeFiles func(), ok bool) {
	files, err := p.openFiles(user)
	if err != nil {
		log.Printf("portal: open files for %q: %v", user, err)
		writeCoded(w, http.StatusInternalServerError, codeFilesUnavailable, "could not open your files")
		return nil, nil, false
	}

	return files, closerOf(files), true
}

func (p *Portal) openFamilyFiles(w http.ResponseWriter, _ string) (files system.Files, closeFiles func(), ok bool) {
	files, err := p.openFamily()
	if err != nil {
		log.Printf("portal: open family folder: %v", err)
		writeCoded(w, http.StatusInternalServerError, codeFamilyUnavailable, "could not open the family folder")
		return nil, nil, false
	}

	return files, closerOf(files), true
}

func closerOf(files system.Files) func() {
	if closer, isCloser := files.(io.Closer); isCloser {
		return func() { _ = closer.Close() }
	}

	return func() {}
}

func (p *Portal) familyPage(w http.ResponseWriter, r *http.Request) {
	p.signedInPage(w, r, "family.html")
}

func (p *Portal) familyTrashPage(w http.ResponseWriter, r *http.Request) {
	p.signedInPage(w, r, "family-trash.html")
}

func (p *Portal) signedInPage(w http.ResponseWriter, r *http.Request, page string) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, page)
}

type passwordChange struct {
	Current  string `json:"current"`
	Password string `json:"password"`
}

func (p *Portal) changePassword(w http.ResponseWriter, r *http.Request, user string) {
	var body passwordChange
	if !decodeJSON(w, r, &body) || !validPassword(w, body.Password) {
		return
	}
	if !p.confirmPassword(w, r, user, body.Current, "password change", codeWrongCurrentPassword, "your current password is wrong") {
		return
	}

	if err := p.users.SetPassword(user, body.Password); err != nil {
		log.Printf("portal: change password for %s: %v", logName(user), err)
		writeCoded(w, http.StatusInternalServerError, codePasswordChangeFailed, "could not change the password")
		return
	}
	log.Printf("portal: password changed user=%s ip=%s", user, ipKey(r))
	p.sessions.destroyUser(user)
	w.WriteHeader(http.StatusNoContent)
}

// confirmPassword checks the signed-in user's own password through the same limits as login and
// writes the error response itself when the check does not pass.
func (p *Portal) confirmPassword(w http.ResponseWriter, r *http.Request, user, password, action, wrongCode, wrongMessage string) bool {
	ip := ipKey(r)
	if !p.ipLimit.begin(ip) {
		log.Printf("portal: %s blocked user=%s ip=%s", action, logName(user), ip)
		writeCoded(w, http.StatusTooManyRequests, codeRateLimited, "too many failed attempts, try again later")
		return false
	}
	defer p.ipLimit.end(ip)
	if !p.userLimit.begin(user) {
		log.Printf("portal: %s blocked user=%s ip=%s", action, logName(user), ip)
		writeCoded(w, http.StatusTooManyRequests, codeRateLimited, "too many failed attempts, try again later")
		return false
	}
	defer p.userLimit.end(user)

	ok, err := p.verify(user, password)
	switch {
	case errors.Is(err, errPortalBusy):
		writeCoded(w, http.StatusServiceUnavailable, codeBusy, "busy, try again in a moment")
		return false
	case err != nil:
		log.Printf("portal: password check failed: %v", err)
		writeCoded(w, http.StatusInternalServerError, codePasswordCheckFailed, "could not check the password right now")
		return false
	case !ok:
		p.ipLimit.fail(ip)
		p.userLimit.fail(user)
		log.Printf("portal: %s failed user=%s ip=%s", action, logName(user), ip)
		writeCoded(w, http.StatusUnauthorized, wrongCode, wrongMessage)
		return false
	}

	return true
}
