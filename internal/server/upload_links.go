package server

import (
	"log"
	"net/http"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	uploadLinkDefaultExpiry = "7d"
	uploadLinkDefaultCap    = "5g"
)

var uploadLinkExpiries = map[string]time.Duration{"1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

var uploadLinkCaps = map[string]int64{"1g": 1 << 30, "5g": 5 << 30, "20g": 20 << 30}

type uploadLinkView struct {
	Name          string     `json:"name"`
	Path          string     `json:"path"`
	Created       time.Time  `json:"created"`
	Expires       time.Time  `json:"expires"`
	Expired       bool       `json:"expired"`
	Cap           int64      `json:"cap"`
	Received      int64      `json:"received"`
	ReceivedBytes int64      `json:"receivedBytes"`
	LastUpload    *time.Time `json:"lastUpload"`
}

type adminUploadLinkView struct {
	uploadLinkView
	Owner string `json:"owner"`
}

func (p *Portal) uploadLinkViewOf(link auth.Share) uploadLinkView {
	view := uploadLinkView{
		Name: link.Name, Path: link.Path, Created: link.Created, Expires: link.Expires, Expired: link.Expired(p.now()),
		Cap: link.Cap, Received: link.Received, ReceivedBytes: link.ReceivedBytes,
	}
	if !link.LastUsed.IsZero() {
		view.LastUpload = &link.LastUsed
	}

	return view
}

type uploadLinkRequest struct {
	Path    string `json:"path"`
	Expires string `json:"expires"`
	Cap     string `json:"cap"`
	Lang    any    `json:"lang"`
}

// An upload link always gets a random name, because anyone who guesses a name can upload into it.
func (p *Portal) createUploadLink(w http.ResponseWriter, r *http.Request, user string) {
	var body uploadLinkRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	length, known := uploadLinkExpiries[orDefault(body.Expires, uploadLinkDefaultExpiry)]
	if !known {
		writeCoded(w, http.StatusUnprocessableEntity, codeShareExpiryInvalid, "expiry must be 1d, 7d or 30d")
		return
	}
	limit, known := uploadLinkCaps[orDefault(body.Cap, uploadLinkDefaultCap)]
	if !known {
		writeCoded(w, http.StatusUnprocessableEntity, codeUploadLinkCap, "the size cap must be 1g, 5g or 20g")
		return
	}
	folder, ok := p.uploadLinkTarget(w, user, body.Path)
	if !ok {
		return
	}
	lang := "en"
	if chosenMailLang(body.Lang) == mailDutch {
		lang = "nl"
	}
	now := p.now()
	link := auth.Share{Owner: user, Path: folder, Dir: true, Created: now, Expires: now.Add(length), Kind: auth.UploadLink, Cap: limit, Lang: lang}

	created, ok := p.storeShare(w, link, "")
	if !ok {
		return
	}
	log.Printf("portal: upload link created user=%s ip=%s", logName(user), ipKey(r))
	writeJSON(w, http.StatusCreated, p.uploadLinkViewOf(created))
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// uploadLinkTarget accepts only a real folder in the user's own files, never the top of it and never through a symlink.
// The family folder is a different root, so it cannot be named here at all.
func (p *Portal) uploadLinkTarget(w http.ResponseWriter, user, requested string) (string, bool) {
	clean, err := system.CleanPath(requested)
	if err != nil || clean == "" {
		writeCoded(w, http.StatusUnprocessableEntity, codeUploadLinkTarget, "choose a folder in your own files")
		return "", false
	}
	files, closeFiles, ok := p.openUserFiles(w, user)
	if !ok {
		return "", false
	}
	defer closeFiles()

	if info, err := lstatWithoutLinks(files, clean); err != nil || !info.IsDir {
		writeCoded(w, http.StatusUnprocessableEntity, codeUploadLinkTarget, "choose a folder in your own files")
		return "", false
	}

	return clean, true
}
