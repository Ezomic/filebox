package server

import (
	"io"
	"log"
	"net/http"
	"slices"
	"time"
)

const (
	healthzLimit = 60
	healthzSpan  = time.Minute
)

// healthz is for an outside monitor: "ok", or a bare 503 while any problem is open. It never says which problem.
func (p *Portal) healthz(w http.ResponseWriter, r *http.Request) {
	if !p.healthLimit.reserve(ipKey(r)) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if p.healthy != nil && !p.healthy() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

// A share link made before a route took its name, such as "healthz", now opens that route instead.
func (p *Portal) warnShadowedShares() {
	if p.shares == nil {
		return
	}
	for _, share := range p.shares.All() {
		if slices.Contains(reservedShareNames, share.Name) || p.mux.claims(share.Name) {
			log.Printf("portal: share link %q of %s no longer opens, because /%s is a page of the portal now; ask them to make a new link",
				share.Name, logName(share.Owner), share.Name)
		}
	}
}
