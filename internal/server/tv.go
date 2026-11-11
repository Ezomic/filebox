package server

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
)

const (
	tvMaxBody      = 1 << 20
	tvPortalPrefix = "/tv"
	tvIPFails      = 10
)

// TV connects the admin panel and the public portal to the streambox add-on, which listens
// on loopback only. Admins manage channels through the panel; family members reach the
// playlist and streams through the portal. Neither ever sees streambox's admin token.
type TV struct {
	target     *url.URL
	adminToken string
	viewToken  string
	admin      *httputil.ReverseProxy
	public     *httputil.ReverseProxy
	client     *http.Client
	shares     *auth.TVShares
	newSuffix  func() (string, error)
	// channelLinks keeps a channel delete and the creation of a link to that channel from interleaving.
	channelLinks sync.Mutex
}

// NewTV reads streambox's tokens from its env file (the one its systemd unit uses) and
// talks to the API at target, which must be a loopback http address so the tokens never
// leave the machine.
func NewTV(target, envFile string, shares *auth.TVShares) (*TV, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("streambox url must look like http://127.0.0.1:8090")
	}
	if ip := net.ParseIP(u.Hostname()); (ip == nil || !ip.IsLoopback()) && u.Hostname() != "localhost" {
		return nil, fmt.Errorf("streambox url must be a loopback address, not %s", u.Hostname())
	}
	values, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	admin, view := values["STREAMBOX_ADMIN_TOKEN"], values["STREAMBOX_VIEW_TOKEN"]
	if len(admin) < 24 || len(view) < 24 {
		return nil, fmt.Errorf("%s needs STREAMBOX_ADMIN_TOKEN and STREAMBOX_VIEW_TOKEN of 24+ characters", envFile)
	}
	return newTV(u, admin, view, shares), nil
}

func newTV(target *url.URL, admin, view string, shares *auth.TVShares) *TV {
	t := &TV{target: target, adminToken: admin, viewToken: view, shares: shares, newSuffix: randomTVShareSuffix}
	t.client = &http.Client{
		Transport:     tvTransport(),
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.admin = t.proxy(func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Path = "/api/" + strings.TrimPrefix(pr.In.URL.Path, "/api/tv/")
		pr.Out.Header.Set("Authorization", "Bearer "+t.adminToken)
	})
	t.public = t.proxy(func(pr *httputil.ProxyRequest) {
		// The portal serves streambox under /tv, so tell it where players really connect.
		pr.Out.Header.Set("X-Forwarded-Proto", "https")
		pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
		pr.Out.Header.Set("X-Forwarded-Prefix", tvPortalPrefix)
	})
	return t
}

func (t *TV) proxy(rewrite func(*httputil.ProxyRequest)) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(t.target)
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = ""
			// Nothing the browser holds is meant for streambox.
			for _, h := range []string{"Cookie", "Authorization", "Origin", "Referer", "X-Forwarded-Prefix"} {
				pr.Out.Header.Del(h)
			}
			rewrite(pr)
		},
		Transport: tvTransport(),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("tv: streambox: %v", err)
			writeError(w, http.StatusBadGateway, "streambox is not reachable")
		},
	}
}

func tvTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}

func readEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read streambox settings: %w", err)
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return values, scanner.Err()
}

func sameToken(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// What the admin panel may reach: the channel list and the media list, nothing else.
var (
	tvAdminPath = regexp.MustCompile(`^/api/tv/(channels|media)$|^/api/tv/channels/[a-z0-9][a-z0-9-]{0,31}(/restart)?$`)
	tvChannelID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	tvSegment   = regexp.MustCompile(`^(index\.m3u8|seg_[0-9]{5,}\.ts)$`)
)

func (s *Server) tvStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.tv != nil})
}

func (s *Server) tvPage(w http.ResponseWriter, r *http.Request) {
	s.protectedPage(w, r, "tv.html")
}

func (s *Server) tvAdminProxy(w http.ResponseWriter, r *http.Request) {
	if !tvAdminPath.MatchString(r.URL.Path) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodDelete:
	case http.MethodPost, http.MethodPut:
		if r.ContentLength != 0 && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusUnsupportedMediaType, "expected application/json")
			return
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// The links go first, so a delete that fails halfway leaves a channel without links rather than links
	// that would come back to life with a later channel of the same id.
	if id, found := strings.CutPrefix(r.URL.Path, "/api/tv/channels/"); found && r.Method == http.MethodDelete && tvChannelID.MatchString(id) {
		s.tv.channelLinks.Lock()
		defer s.tv.channelLinks.Unlock()
		if err := s.tv.shares.ForgetChannel(id); err != nil {
			log.Printf("tv: remove share links of channel %s: %v", id, err)
			writeError(w, http.StatusInternalServerError, "could not remove the channel's share links")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, tvMaxBody)
	s.tv.admin.ServeHTTP(w, r)
}

// tvInfo tells a signed-in portal user where their player should connect. The URL
// carries the view token, so it is only shown to someone who has logged in.
func (p *Portal) tvInfo(w http.ResponseWriter, r *http.Request, _ string) {
	if p.tv == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":     true,
		"playlistUrl": "https://" + r.Host + tvPortalPrefix + "/s/" + p.tv.viewToken + "/playlist.m3u",
	})
}

func (p *Portal) tvPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.currentUser(r); !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.ServeFileFS(w, r, p.pages, "tv.html")
}

// tvStream serves what streambox hands to players. Access is the token in the path: the view token opens
// every channel, a share link code opens one. Guesses are rate limited per address and logged for fail2ban
// like failed logins are.
func (p *Portal) tvStream(w http.ResponseWriter, r *http.Request, rest string, shared func(http.ResponseWriter, *http.Request, auth.TVShare)) {
	share, global, ok := p.tvAccess(w, r)
	if !ok {
		return
	}
	// streambox sets its own caching headers, and the portal default would be added to them.
	w.Header().Del("Cache-Control")
	if !global {
		shared(w, r, share)
		return
	}
	p.tv.serveView(w, r, rest)
}

// tvAccess writes the refusal itself when the token opens nothing. A slot is held while the token is checked,
// so a burst of parallel guesses cannot all slip past the limit before the first miss is counted.
func (p *Portal) tvAccess(w http.ResponseWriter, r *http.Request) (share auth.TVShare, global, ok bool) {
	ip := ipKey(r)
	if !p.tvLimit.begin(ip) {
		log.Printf("portal: tv blocked ip=%s", ip)
		writeError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return auth.TVShare{}, false, false
	}
	defer p.tvLimit.end(ip)

	token := r.PathValue("token")
	if sameToken(token, p.tv.viewToken) {
		return auth.TVShare{}, true, true
	}
	if share, found := p.tv.share(token); found {
		return share, false, true
	}
	p.tvLimit.fail(ip)
	log.Printf("portal: tv failed ip=%s", ip)
	// A revoked share link, or one whose channel was deleted, is gone; a wrong view token is unauthorized as before.
	status := http.StatusUnauthorized
	if tvShareCode.MatchString(normalizeShareName(token)) {
		status = http.StatusNotFound
	}
	writeError(w, status, "wrong link")

	return auth.TVShare{}, false, false
}

func (t *TV) share(token string) (auth.TVShare, bool) {
	code := normalizeShareName(token)
	if t.shares == nil || !tvShareCode.MatchString(code) {
		return auth.TVShare{}, false
	}

	return t.shares.Get(code)
}

func (t *TV) serveView(w http.ResponseWriter, r *http.Request, rest string) {
	r.URL.Path = "/s/" + t.viewToken + "/" + rest
	t.public.ServeHTTP(w, r)
}

func (p *Portal) tvPlaylist(w http.ResponseWriter, r *http.Request) {
	p.tvStream(w, r, "playlist.m3u", p.sharedPlaylist)
}

func (p *Portal) tvGuide(w http.ResponseWriter, r *http.Request) {
	p.tvStream(w, r, "epg.xml", p.sharedGuide)
}

func (p *Portal) tvSegment(w http.ResponseWriter, r *http.Request) {
	id, file := r.PathValue("id"), r.PathValue("file")
	if !tvChannelID.MatchString(id) || !tvSegment.MatchString(file) {
		http.NotFound(w, r)
		return
	}
	rest := "hls/" + id + "/" + file
	p.tvStream(w, r, rest, func(w http.ResponseWriter, r *http.Request, share auth.TVShare) {
		if id != share.Channel {
			http.NotFound(w, r)
			return
		}
		p.tv.serveView(w, r, rest)
	})
}
