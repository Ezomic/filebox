package server

import (
	"fmt"
	"net/http"
	"strings"
)

// Share links own every single-segment path no route claims, so a first path segment that any route uses, now or
// later, can never be a share name. Routes that exist only when an add-on is installed are listed here too.
var reservedShareNames = []string{
	"admin", "api", "backup", "confirm-email", "family", "favicon.ico", "files", "forgot", "login", "logout", "manifest.webmanifest",
	"password", "photos", "reset", "robots.txt", "settings", "share", "shared", "shares", "sign-in", "sign-in-link", "static", "trash",
	"tv", "upload", "uploads", "users", "apple-touch-icon.png", "icon-192.png", "icon-512.png", "icon-maskable-512.png",
	"download", "downloads", "index", "health", "healthz", "status", "s",
}

// routes is a ServeMux that remembers the first path segment of every pattern registered on it. That is the one
// place the router and the share name check agree, so a new route reserves its own name.
type routes struct {
	mux   *http.ServeMux
	first map[string]bool
}

func newRoutes() *routes {
	return &routes{mux: http.NewServeMux(), first: map[string]bool{}}
}

func (r *routes) Handle(pattern string, handler http.Handler) {
	r.note(pattern)
	r.mux.Handle(pattern, handler)
}

func (r *routes) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	r.note(pattern)
	r.mux.HandleFunc(pattern, handler)
}

func (r *routes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}

func (r *routes) note(pattern string) {
	segment, wildcardRoot := firstSegment(pattern)
	if wildcardRoot {
		panic(fmt.Sprintf("route %q has a wildcard as its first segment, which would shadow every share link", pattern))
	}
	if segment != "" {
		r.first[segment] = true
	}
}

func (r *routes) claims(name string) bool {
	return r.first[name]
}

// firstSegment returns the literal first path segment of a mux pattern. The root patterns "/" and "/{$}" have none.
func firstSegment(pattern string) (segment string, wildcardRoot bool) {
	if _, path, found := strings.Cut(pattern, " "); found {
		pattern = path
	}
	rest := strings.TrimPrefix(pattern, "/")
	if rest == "" || rest == "{$}" {
		return "", false
	}
	segment, _, _ = strings.Cut(rest, "/")
	if strings.Contains(segment, "{") {
		return "", true
	}

	return segment, false
}
