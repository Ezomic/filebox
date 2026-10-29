package server

import (
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	portalCookieName   = "filebox_portal"
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
}

// Portal is the public, per-user web interface. It is a separate handler from the admin
// panel so none of the admin routes exist on the listener that faces the internet.
type Portal struct {
	verifier  system.Verifier
	sessions  *userSessions
	ipLimit   *loginLimiter
	userLimit *loginLimiter
	verifying chan struct{}
	verifyWait time.Duration
	pages     fs.FS
	mux       *http.ServeMux
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
		verifier:  deps.Verifier,
		sessions:  newUserSessions(portalIdleTTL, portalAbsoluteTTL),
		ipLimit:   newLoginLimiter(portalIPFails, failWindow),
		userLimit: newLoginLimiter(portalUserFails, failWindow),
		verifying: make(chan struct{}, portalMaxVerifying),
		verifyWait: portalVerifyWait,
		pages:     pages,
	}

	p.mux = http.NewServeMux()
	p.mux.Handle("GET /static/style.css", http.StripPrefix("/static/", http.FileServerFS(sharedStyle{shared})))
	p.mux.Handle("GET /static/", http.StripPrefix("/static/", assetsOnly(http.FileServerFS(p.pages))))
	p.mux.HandleFunc("GET /{$}", p.homePage)
	p.mux.HandleFunc("GET /login", p.loginPage)
	p.mux.HandleFunc("POST /api/login", p.login)
	p.mux.HandleFunc("POST /api/logout", p.requireSession(p.logout))
	p.mux.HandleFunc("GET /api/me", p.requireSession(p.me))

	return p
}

func (p *Portal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Strict-Transport-Security", "max-age=31536000")
	securityHeaders(p.mux).ServeHTTP(w, r)
}

// EndSessions signs a user out everywhere, for example after an admin changes or deletes the account.
func (p *Portal) EndSessions(user string) {
	p.sessions.destroyUser(user)
}

// sharedStyle exposes only the panel stylesheet from the admin assets, never its scripts or pages.
type sharedStyle struct{ fs.FS }

func (s sharedStyle) Open(name string) (fs.File, error) {
	if name != "style.css" {
		return nil, fs.ErrNotExist
	}

	return s.FS.Open(name)
}

func (p *Portal) homePage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "index.html")
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
	if p.ipLimit.blocked(ip) {
		log.Printf("portal: login blocked ip=%s", ip)
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if !system.ValidUsername(body.Username) {
		p.ipLimit.fail(ip)
		log.Printf("portal: login failed user=%s ip=%s", logName(body.Username), ip)
		writeError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	if p.userLimit.blocked(body.Username) {
		log.Printf("portal: login blocked user=%s ip=%s", logName(body.Username), ip)
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}

	generation := p.sessions.generation(body.Username)
	ok, err := p.verify(body.Username, body.Password)
	switch {
	case errors.Is(err, errPortalBusy):
		writeError(w, http.StatusServiceUnavailable, "busy, try again in a moment")
		return
	case err != nil:
		log.Printf("portal: password check failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not check the password right now")
		return
	case !ok:
		p.ipLimit.fail(ip)
		p.userLimit.fail(body.Username)
		log.Printf("portal: login failed user=%s ip=%s", logName(body.Username), ip)
		writeError(w, http.StatusUnauthorized, "wrong username or password")
		return
	}

	token, err := p.sessions.create(body.Username, generation)
	switch {
	case errors.Is(err, errStaleLogin):
		writeError(w, http.StatusConflict, "your account just changed, try again")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	p.userLimit.reset(body.Username)
	log.Printf("portal: login ok user=%s ip=%s", body.Username, ip)
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
	writeJSON(w, http.StatusOK, map[string]string{"user": user})
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
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r, user)
	}
}
