package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Ezomic/filebox/internal/auth"
)

const (
	tvShareSlugMax      = 24
	tvShareLabelMax     = 60
	tvShareSuffixLength = 8
	tvShareTries        = 8
	tvShareAlphabet     = "abcdefghjkmnpqrstuvwxyz23456789"
)

// A share link code is a readable slug, a hyphen and a random suffix without look-alike characters (no 0, o, 1,
// l or i), so it can be read out and typed on a TV remote.
var tvShareCode = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*-[` + tvShareAlphabet + `]{8}$`)

func randomTVShareSuffix() (string, error) {
	return randomFrom(tvShareAlphabet, tvShareSuffixLength)
}

type tvShareView struct {
	auth.TVShare
	// URL is empty when the portal's public name is not known, because share links are only served there.
	URL string `json:"url"`
}

func (s *Server) tvShareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tv/shares", s.requireSession(s.listTVShares))
	mux.HandleFunc("POST /api/tv/shares", s.requireSession(s.createTVShare))
	mux.HandleFunc("DELETE /api/tv/shares/{code}", s.requireSession(s.revokeTVShare))
}

func (s *Server) tvShareView(share auth.TVShare) tvShareView {
	view := tvShareView{TVShare: share}
	if s.portalHost != nil {
		if host := s.portalHost(); host != "" {
			view.URL = "https://" + host + tvPortalPrefix + "/s/" + share.Code + "/playlist.m3u"
		}
	}

	return view
}

func (s *Server) listTVShares(w http.ResponseWriter, _ *http.Request) {
	views := []tvShareView{}
	for _, share := range s.tv.shares.All() {
		views = append(views, s.tvShareView(share))
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": views})
}

type tvShareRequest struct {
	Label   string `json:"label"`
	Channel string `json:"channel"`
	Slug    string `json:"slug"`
}

func (s *Server) createTVShare(w http.ResponseWriter, r *http.Request) {
	var body tvShareRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	label, slug := strings.TrimSpace(body.Label), normalizeShareName(strings.TrimSpace(body.Slug))
	switch {
	case !validTVShareLabel(label):
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("a label is 1 to %d characters without control characters", tvShareLabelMax))
		return
	case len(slug) > tvShareSlugMax || !shareNamePattern.MatchString(slug):
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("a link name is 1 to %d letters, digits and single hyphens, not starting or ending with a hyphen", tvShareSlugMax))
		return
	case !tvChannelID.MatchString(body.Channel):
		writeError(w, http.StatusUnprocessableEntity, "no such channel")
		return
	}
	// Held until the link is stored, so a channel delete cannot slip in between the check and the save
	// and leave a link that a later channel with the same id would bring back.
	s.tv.channelLinks.Lock()
	defer s.tv.channelLinks.Unlock()
	exists, err := s.tv.channelExists(r.Context(), body.Channel)
	switch {
	case err != nil:
		log.Printf("tv: streambox: %v", err)
		writeError(w, http.StatusBadGateway, "streambox is not reachable")
		return
	case !exists:
		writeError(w, http.StatusUnprocessableEntity, "no such channel")
		return
	}

	share, err := s.tv.createShare(label, slug, body.Channel, time.Now())
	if err != nil {
		log.Printf("tv: save share link: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the link")
		return
	}
	log.Printf("tv: share link created channel=%s", share.Channel)
	writeJSON(w, http.StatusCreated, s.tvShareView(share))
}

// A label must show something, and may not carry control or text-direction characters that would make it read
// differently from what it holds. Joiners inside emoji are fine.
func validTVShareLabel(label string) bool {
	if utf8.RuneCountInString(label) > tvShareLabelMax || !strings.ContainsFunc(label, unicode.IsGraphic) {
		return false
	}

	return !strings.ContainsFunc(label, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) })
}

func (s *Server) revokeTVShare(w http.ResponseWriter, r *http.Request) {
	share, err := s.tv.shares.Revoke(normalizeShareName(r.PathValue("code")))
	switch {
	case errors.Is(err, auth.ErrTVShareMissing):
		writeError(w, http.StatusNotFound, "no such link")
	case err != nil:
		log.Printf("tv: revoke share link: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save the link")
	default:
		log.Printf("tv: share link revoked channel=%s", share.Channel)
		w.WriteHeader(http.StatusNoContent)
	}
}

// createShare draws suffixes until it finds a code that was never given out.
func (t *TV) createShare(label, slug, channel string, now time.Time) (auth.TVShare, error) {
	for range tvShareTries {
		suffix, err := t.newSuffix()
		if err != nil {
			return auth.TVShare{}, err
		}
		share := auth.TVShare{Code: slug + "-" + suffix, Label: label, Channel: channel, Created: now}
		switch err := t.shares.Create(share); {
		case err == nil:
			return share, nil
		case !errors.Is(err, auth.ErrTVShareTaken):
			return auth.TVShare{}, err
		}
	}

	return auth.TVShare{}, errors.New("no free share link code")
}

func (t *TV) channelExists(ctx context.Context, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.target.JoinPath("api", "channels", id).String(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+t.adminToken)
	resp, err := t.client.Do(req)
	if err != nil {
		return false, withoutURL(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, tvMaxBody))
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}

	return false, fmt.Errorf("streambox answered %d to a channel lookup", resp.StatusCode)
}

// The URL of a request to streambox can hold the view token, so only the underlying error is logged.
func withoutURL(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}
