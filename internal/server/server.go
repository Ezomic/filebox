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

type Server struct {
	store    *auth.Store
	host     system.Host
	sessions *sessions
	limiter  *loginLimiter
	pages    fs.FS
}

func New(store *auth.Store, host system.Host) http.Handler {
	pages, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}

	s := &Server{
		store:    store,
		host:     host,
		sessions: newSessions(sessionTTL),
		limiter:  newLoginLimiter(maxLoginFails, failWindow),
		pages:    pages,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(s.pages)))
	mux.HandleFunc("GET /{$}", s.dashboardPage)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.requireSession(s.logout))
	mux.HandleFunc("GET /api/metrics", s.requireSession(s.metrics))

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-store")
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

func (s *Server) servePage(w http.ResponseWriter, r *http.Request, name string) {
	http.ServeFileFS(w, r, s.pages, name)
}

type credentials struct {
	Password string `json:"password"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var body credentials
	if !decodeJSON(w, r, &body) {
		return
	}

	switch err := s.store.SetInitialPassword(body.Password); {
	case errors.Is(err, auth.ErrAlreadyConfigured):
		writeError(w, http.StatusForbidden, "setup is already complete")
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
	if !s.limiter.allowed(key) {
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return
	}
	if !s.store.Verify(body.Password) {
		s.limiter.recordFailure(key)
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
