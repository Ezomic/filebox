package server

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

//go:embed web
var webFiles embed.FS

const (
	cookieName    = "filebox_session"
	sessionTTL    = 12 * time.Hour
	maxLoginFails = 5
	failWindow    = 15 * time.Minute
	maxBodyBytes  = 4096
)

type Deps struct {
	Host      system.Host
	Users     system.Users
	Files     system.Files
	Logs      system.Logs
	Power     system.Power
	Usage     system.Usage
	Smart     system.Smart
	MaxUpload int64
	// PowerDelay is how long shutdown and reboot wait so the response can reach the browser first.
	PowerDelay time.Duration
	// OnUserChanged runs after an admin changes a user's password or deletes the user.
	OnUserChanged func(name string)
	// PortalCertExpiry reports when the public portal certificate expires; nil when the portal is off.
	PortalCertExpiry func() time.Time
}

type Server struct {
	store      *auth.Store
	host       system.Host
	users      system.Users
	files      system.Files
	logs       system.Logs
	power      system.Power
	usage      system.Usage
	smart      system.Smart
	powerDelay time.Duration
	maxUpload  int64
	sessions   *sessions
	limiter    *loginLimiter
	pages      fs.FS
	fh         fileHandlers

	onUserChanged    func(name string)
	portalCertExpiry func() time.Time
}

func New(store *auth.Store, deps Deps) http.Handler {
	pages, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}

	s := &Server{
		store:      store,
		host:       deps.Host,
		users:      deps.Users,
		files:      deps.Files,
		logs:       deps.Logs,
		power:      deps.Power,
		usage:      deps.Usage,
		smart:      deps.Smart,
		powerDelay: deps.PowerDelay,
		maxUpload:  deps.MaxUpload,
		sessions:   newSessions(sessionTTL),
		limiter:    newLoginLimiter(maxLoginFails, failWindow),
		pages:      pages,
		fh:         fileHandlers{files: deps.Files, maxUpload: deps.MaxUpload},

		onUserChanged:    deps.OnUserChanged,
		portalCertExpiry: deps.PortalCertExpiry,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", assetsOnly(http.FileServerFS(s.pages))))
	mux.HandleFunc("GET /{$}", s.dashboardPage)
	mux.HandleFunc("GET /users", s.usersPage)
	mux.HandleFunc("GET /files", s.filesPage)
	mux.HandleFunc("GET /logs", s.logsPage)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.requireSession(s.logout))
	mux.HandleFunc("GET /api/metrics", s.requireSession(s.metrics))
	mux.HandleFunc("GET /api/portal", s.requireSession(s.portalStatus))
	mux.HandleFunc("GET /api/users", s.requireSession(s.listUsers))
	mux.HandleFunc("POST /api/users", s.requireSession(s.createUser))
	mux.HandleFunc("PUT /api/users/{name}/password", s.requireSession(s.changeUserPassword))
	mux.HandleFunc("DELETE /api/users/{name}", s.requireSession(s.deleteUser))
	mux.HandleFunc("GET /api/files", s.requireSession(s.fh.listFiles))
	mux.HandleFunc("DELETE /api/files", s.requireSession(s.fh.deleteFile))
	mux.HandleFunc("GET /api/files/download", s.requireSession(s.fh.downloadFile))
	mux.HandleFunc("PUT /api/files/content", s.requireSession(s.fh.uploadFile))
	mux.HandleFunc("POST /api/files/folder", s.requireSession(s.fh.createFolder))
	mux.HandleFunc("POST /api/files/rename", s.requireSession(s.fh.renameFile))
	mux.HandleFunc("GET /api/disks", s.requireSession(s.disks))
	mux.HandleFunc("GET /api/logs", s.requireSession(s.readLogs))
	mux.HandleFunc("POST /api/system/restart-sshd", s.requireSession(s.powerAction(system.RestartSSHD)))
	mux.HandleFunc("POST /api/system/shutdown", s.requireSession(s.powerAction(system.Shutdown)))
	mux.HandleFunc("POST /api/system/reboot", s.requireSession(s.powerAction(system.Reboot)))

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func assetsOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".css") && !strings.HasSuffix(r.URL.Path, ".js") {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) dashboardPage(w http.ResponseWriter, r *http.Request) {
	switch {
	case !s.store.Configured():
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	case !s.hasSession(r):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		s.servePage(w, r, "index.html")
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	switch {
	case !s.store.Configured():
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	case s.hasSession(r):
		http.Redirect(w, r, "/", http.StatusSeeOther)
	default:
		s.servePage(w, r, "login.html")
	}
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.store.Configured() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.servePage(w, r, "setup.html")
}

func (s *Server) protectedPage(w http.ResponseWriter, r *http.Request, name string) {
	switch {
	case !s.store.Configured():
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	case !s.hasSession(r):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	default:
		s.servePage(w, r, name)
	}
}

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, name string) {
	http.ServeFileFS(w, r, s.pages, name)
}

type credentials struct {
	Password string `json:"password"`
}

type setupRequest struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var body setupRequest
	if !decodeJSON(w, r, &body) {
		return
	}

	switch err := s.store.SetInitialPassword(body.Token, body.Password); {
	case errors.Is(err, auth.ErrAlreadyConfigured):
		writeError(w, http.StatusForbidden, "setup is already complete")
	case errors.Is(err, auth.ErrInvalidSetupToken):
		writeError(w, http.StatusForbidden, "wrong setup token")
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeError(w, http.StatusUnprocessableEntity, "password must be at least 12 characters")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "could not save password")
	default:
		s.startSession(w)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}

	key := clientIP(r)
	if !s.limiter.reserve(key) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if !s.store.Verify(body.Password) {
		writeError(w, http.StatusUnauthorized, "wrong password")
		return
	}

	s.limiter.reset(key)
	s.startSession(w)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		s.sessions.destroy(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) portalStatus(w http.ResponseWriter, _ *http.Request) {
	if s.portalCertExpiry == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	expires := s.portalCertExpiry()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":      true,
		"certExpires":  expires.UTC().Format(time.RFC3339),
		"certDaysLeft": int(time.Until(expires).Hours() / 24),
	})
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	metrics, err := s.host.Metrics()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read system metrics")
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) startSession(w http.ResponseWriter) {
	token, err := s.sessions.create()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) hasSession(r *http.Request) bool {
	cookie, err := r.Cookie(cookieName)

	return err == nil && s.sessions.valid(cookie.Value)
}

func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.hasSession(r) {
			writeError(w, http.StatusUnauthorized, "not signed in")
			return
		}
		next(w, r)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}

	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}
