package server

import (
	"encoding/xml"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/Ezomic/filebox/internal/auth"
)

// streambox's playlist and guide for every channel, read whole so they can be cut down to one.
const tvMaxListing = 32 << 20

// sharedPlaylist is streambox's playlist cut down to the link's channel. Its header and stream URL are written
// here, because streambox puts the view token in both; only the channel's #EXTINF line is passed on.
func (p *Portal) sharedPlaylist(w http.ResponseWriter, r *http.Request, share auth.TVShare) {
	body, ok := p.tv.viewListing(w, r, "playlist.m3u")
	if !ok {
		return
	}
	entry, found := playlistEntry(string(body), share.Channel)
	if !found {
		http.NotFound(w, r)
		return
	}
	base := "https://" + r.Host + tvPortalPrefix + "/s/" + share.Code
	playlist := "#EXTM3U x-tvg-url=\"" + base + "/epg.xml\"\n" + entry + "\n" + base + "/hls/" + share.Channel + "/index.m3u8\n"
	p.tv.writeShared(w, "audio/x-mpegurl", playlist)
}

// playlistEntry finds the #EXTINF line that comes before the channel's stream URL.
func playlistEntry(playlist, channel string) (string, bool) {
	lines := strings.Split(playlist, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.HasSuffix(lines[i], "/hls/"+channel+"/index.m3u8") && strings.HasPrefix(lines[i-1], "#EXTINF:") {
			return lines[i-1], true
		}
	}

	return "", false
}

func (p *Portal) sharedGuide(w http.ResponseWriter, r *http.Request, share auth.TVShare) {
	body, ok := p.tv.viewListing(w, r, "epg.xml")
	if !ok {
		return
	}
	guide, found, err := guideFor(body, share.Channel)
	switch {
	case err != nil:
		log.Printf("tv: streambox guide: %v", err)
		writeError(w, http.StatusBadGateway, "streambox is not reachable")
	case !found:
		http.NotFound(w, r)
	default:
		p.tv.writeShared(w, "application/xml", guide)
	}
}

// xmltvDoc keeps every attribute and the raw content of each top-level element, so the guide of the one
// channel comes out as streambox wrote it.
type xmltvDoc struct {
	XMLName xml.Name
	Attrs   []xml.Attr  `xml:",any,attr"`
	Items   []xmltvItem `xml:",any"`
}

type xmltvItem struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Inner   []byte     `xml:",innerxml"`
}

// guideFor keeps the channel's <channel> element and its <programme> elements, and drops everything else.
func guideFor(body []byte, channel string) (guide string, found bool, err error) {
	var doc xmltvDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return "", false, err
	}
	if doc.XMLName.Local != "tv" {
		return "", false, errors.New("the guide is not XMLTV")
	}
	var kept []xmltvItem
	for _, item := range doc.Items {
		switch {
		case item.XMLName.Local == "channel" && xmlAttr(item.Attrs, "id") == channel:
			found = true
			kept = append(kept, item)
		case item.XMLName.Local == "programme" && xmlAttr(item.Attrs, "channel") == channel:
			kept = append(kept, item)
		}
	}
	doc.Items = kept
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", false, err
	}

	return xml.Header + string(out) + "\n", found, nil
}

func xmlAttr(attrs []xml.Attr, name string) string {
	for _, attr := range attrs {
		if attr.Name.Space == "" && attr.Name.Local == name {
			return attr.Value
		}
	}

	return ""
}

// viewListing fetches the playlist or guide of every channel from streambox, with the view token. It writes
// the error itself when streambox does not answer.
func (t *TV) viewListing(w http.ResponseWriter, r *http.Request, name string) ([]byte, bool) {
	body, err := t.fetchListing(r, name)
	if err != nil {
		log.Printf("tv: streambox: %v", err)
		writeError(w, http.StatusBadGateway, "streambox is not reachable")
		return nil, false
	}

	return body, true
}

func (t *TV) fetchListing(r *http.Request, name string) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, t.target.JoinPath("s", t.viewToken, name).String(), nil)
	if err != nil {
		return nil, withoutURL(err)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, withoutURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("streambox answered " + resp.Status + " for the " + name)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, tvMaxListing+1))
	if err != nil {
		return nil, withoutURL(err)
	}
	if len(body) > tvMaxListing {
		return nil, errors.New("the " + name + " from streambox is too large")
	}

	return body, nil
}

// writeShared sends a body built for a share link. The view token opens every channel, so a body that still
// holds it, for example because a channel was named after it, is refused rather than sent.
func (t *TV) writeShared(w http.ResponseWriter, contentType, body string) {
	if strings.Contains(body, t.viewToken) {
		log.Print("tv: refused to send a share link response that holds the view token")
		writeError(w, http.StatusInternalServerError, "could not build the playlist")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, body)
}
