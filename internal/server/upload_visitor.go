package server

import (
	"crypto/rand"
	"errors"
	"log"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	maxVisitorFileBytes = 240
	maxSenderBytes      = 64
	maxNameTries        = 1000
	maxVisits           = 10000
)

var shortExtension = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)

// receiving is what upload links keep between requests. The lock per owner makes the size cap and the choice of a
// free name one decision at a time, also across two links into the same folder. A visit remembers the folder named
// after the visitor, so all their files land together.
type receiving struct {
	locksMu sync.Mutex
	locks   map[string]*ownerLock

	visitsMu sync.Mutex
	visits   map[string]receiveVisit

	notices uploadNotices
}

type ownerLock struct {
	mu   sync.Mutex
	refs int
}

type receiveVisit struct {
	link   string
	folder string
	seen   time.Time
}

func newReceiving() *receiving {
	return &receiving{
		locks:   map[string]*ownerLock{},
		visits:  map[string]receiveVisit{},
		notices: uploadNotices{links: map[string]*uploadNotice{}, after: func(wait time.Duration, run func()) { time.AfterFunc(wait, run) }},
	}
}

func (rc *receiving) lockOwner(owner string) (unlock func()) {
	rc.locksMu.Lock()
	lock := rc.locks[owner]
	if lock == nil {
		lock = &ownerLock{}
		rc.locks[owner] = lock
	}
	lock.refs++
	rc.locksMu.Unlock()

	lock.mu.Lock()

	return func() {
		lock.mu.Unlock()
		rc.locksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(rc.locks, owner)
		}
		rc.locksMu.Unlock()
	}
}

func (rc *receiving) visitFolder(token, link string, now time.Time) (string, bool) {
	rc.visitsMu.Lock()
	defer rc.visitsMu.Unlock()

	visit, found := rc.visits[token]
	if !found || visit.link != link || now.Sub(visit.seen) > system.VisitorUploadMaxAge {
		return "", false
	}
	visit.seen = now
	rc.visits[token] = visit

	return visit.folder, true
}

func (rc *receiving) remember(token string, visit receiveVisit) {
	rc.visitsMu.Lock()
	defer rc.visitsMu.Unlock()

	if len(rc.visits) >= maxVisits {
		oldest := ""
		for key, known := range rc.visits {
			if visit.seen.Sub(known.seen) > system.VisitorUploadMaxAge {
				delete(rc.visits, key)
			} else if oldest == "" || known.seen.Before(rc.visits[oldest].seen) {
				oldest = key
			}
		}
		if len(rc.visits) >= maxVisits {
			delete(rc.visits, oldest)
		}
	}
	rc.visits[token] = visit
}

func (p *Portal) answerUploadLink(w http.ResponseWriter, r *http.Request, link auth.Share, action string) bool {
	files, err := p.openFiles(link.Owner)
	if err != nil {
		return false
	}
	defer closerOf(files)()

	visit := uploadVisit{sharedItem{portal: p, share: link, files: files}}
	if !visit.targetIsLive() {
		return false
	}

	return visit.serve(w, r, action)
}

// uploadVisit is one request through an upload link. A visitor can open the page, learn the folder's name, and start,
// continue and finish their own uploads. Nothing here lists, reads, renames or deletes what is in the folder.
type uploadVisit struct {
	sharedItem
}

func (v uploadVisit) serve(w http.ResponseWriter, r *http.Request, action string) bool {
	switch {
	case action == "" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		v.portal.linkPage(w, r, "receive.html")
		return true
	case action == "info" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"folder": path.Base(v.share.Path)})
		return true
	case action == "uploads" && r.Method == http.MethodPost:
		return v.create(w, r)
	}
	rest, found := strings.CutPrefix(action, "uploads/")
	if !found {
		return false
	}
	switch id, tail, hasTail := strings.Cut(rest, "/"); {
	case !hasTail && r.Method == http.MethodPut:
		return v.chunk(w, r, id)
	case tail == "finish" && r.Method == http.MethodPost:
		return v.finish(w, id)
	}

	return false
}

func (v uploadVisit) handlers() fileHandlers {
	return fileHandlers{files: v.files, maxUpload: v.portal.maxUpload, space: v.portal.space}
}

type visitorUploadBody struct {
	Name   string `json:"name"`
	Size   *int64 `json:"size"`
	Sender string `json:"sender"`
	Visit  string `json:"visit"`
}

func (v uploadVisit) create(w http.ResponseWriter, r *http.Request) bool {
	var body visitorUploadBody
	if !decodeJSON(w, r, &body) {
		return true
	}
	p := v.portal
	name := cleanVisitorName(body.Name, maxVisitorFileBytes)
	switch {
	case body.Size == nil || *body.Size < 0:
		writeCoded(w, http.StatusBadRequest, codeUploadInvalid, "size must be a number of bytes")
		return true
	case *body.Size > p.maxUpload:
		writeCoded(w, http.StatusRequestEntityTooLarge, codeUploadTooLarge, "the file is larger than the upload limit")
		return true
	case name == "":
		writeCoded(w, http.StatusBadRequest, codePathInvalid, "invalid file name")
		return true
	}
	if v.handlers().refuseWhenFull(w) || v.refuseOverQuota(w, *body.Size) {
		return true
	}
	defer p.space.invalidate()

	unlock := p.receive.lockOwner(v.share.Owner)
	defer unlock()
	link, found := p.shares.Get(v.share.Name)
	if !found || link.Expired(p.now()) {
		return false
	}
	v.share = link
	stale := v.staleUploads()
	if link.Taken(stale)+*body.Size > link.Cap {
		writeCoded(w, http.StatusInsufficientStorage, codeUploadLinkFull, "this link cannot take more files")
		return true
	}

	target, visit, err := v.target(body.Visit, body.Sender, name)
	if errors.Is(err, system.ErrNotFound) || errors.Is(err, system.ErrNotDir) {
		return false
	}
	var info system.UploadInfo
	if err == nil {
		info, err = v.files.CreateVisitorUpload(target, *body.Size)
	}
	if err != nil {
		fileError(w, "upload link", "", err)
		return true
	}
	switch err := p.shares.StartUpload(link.Name, info.ID, info.Size, stale); {
	case errors.Is(err, auth.ErrShareMissing):
		return false
	case err != nil:
		log.Printf("portal: save upload link: %v", err)
		writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")
		return true
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": info.ID, "name": path.Base(target), "size": info.Size, "offset": 0, "visit": visit})

	return true
}

// The owner's quota is also enforced by btrfs on every write. This only turns a file that cannot fit away before any
// of it is sent.
func (v uploadVisit) refuseOverQuota(w http.ResponseWriter, size int64) bool {
	quota, ok := v.portal.space.quota(v.share.Owner)
	if !ok || quota.Limit == 0 || quota.Used+uint64(size) <= quota.Limit {
		return false
	}
	writeCoded(w, http.StatusInsufficientStorage, codeQuotaExceeded, "there is no room left for this file")

	return true
}

// staleUploads are pending uploads that are gone from the disk, swept or never finished, so they no longer hold
// part of the cap.
func (v uploadVisit) staleUploads() []string {
	var stale []string
	for id := range v.share.Pending {
		if _, err := v.files.UploadStatus(id); errors.Is(err, system.ErrNotFound) {
			stale = append(stale, id)
		}
	}

	return stale
}

func (v uploadVisit) target(token, sender, name string) (target, visit string, err error) {
	folder, visit, err := v.visitorFolder(token, sender)
	if err != nil {
		return "", "", err
	}
	taken := v.inFlightPaths()
	for attempt := 1; attempt <= maxNameTries; attempt++ {
		candidate := folder + "/" + numberedFile(name, attempt)
		if taken[candidate] {
			continue
		}
		switch _, err := v.files.Lstat(candidate); {
		case errors.Is(err, system.ErrNotFound):
			return candidate, visit, nil
		case err != nil:
			return "", "", err
		}
	}

	return "", "", system.ErrExists
}

// visitorFolder is the folder named after the visitor, made on their first file and reused for the rest of the
// visit, or the link's own folder when they gave no name. A name that is taken gets a number, so a visitor never adds
// to a folder that was already there.
func (v uploadVisit) visitorFolder(token, sender string) (folder, visit string, err error) {
	now := v.portal.now()
	if known, found := v.portal.receive.visitFolder(token, v.share.Name, now); found {
		if info, err := lstatWithoutLinks(v.files, known); err == nil && info.IsDir {
			return known, token, nil
		}
	}
	name := cleanVisitorName(sender, maxSenderBytes)
	if name == "" {
		return v.share.Path, "", nil
	}
	for attempt := 1; attempt <= maxNameTries; attempt++ {
		candidate := v.share.Path + "/" + numberedFolder(name, attempt)
		switch err := v.files.Mkdir(candidate); {
		case err == nil:
			visit = rand.Text()
			v.portal.receive.remember(visit, receiveVisit{link: v.share.Name, folder: candidate, seen: now})
			return candidate, visit, nil
		case !errors.Is(err, system.ErrExists):
			return "", "", err
		}
	}

	return "", "", system.ErrExists
}

// inFlightPaths are where the owner's unfinished visitor uploads will land, so a new one never picks the same name.
func (v uploadVisit) inFlightPaths() map[string]bool {
	paths := map[string]bool{}
	for _, link := range v.portal.shares.ByOwner(v.share.Owner) {
		for id := range link.Pending {
			if info, err := v.files.UploadStatus(id); err == nil {
				paths[info.Path] = true
			}
		}
	}

	return paths
}

// owns is true only for an unfinished visitor upload that this link started and that lands inside its folder, so a
// visitor can never reach the owner's own uploads or those of another link.
func (v uploadVisit) owns(id string) bool {
	if _, pending := v.share.Pending[id]; !pending {
		return false
	}
	info, err := v.files.UploadStatus(id)

	return err == nil && info.Visitor && strings.HasPrefix(info.Path, v.share.Path+"/")
}

func (v uploadVisit) chunk(w http.ResponseWriter, r *http.Request, id string) bool {
	if !v.owns(id) {
		return false
	}
	handlers := v.handlers()
	defer v.portal.space.invalidate()
	if !handlers.refuseWhenFull(w) {
		handlers.appendChunkTo(w, r, id)
	}

	return true
}

func (v uploadVisit) finish(w http.ResponseWriter, id string) bool {
	p := v.portal
	unlock := p.receive.lockOwner(v.share.Owner)
	defer unlock()
	link, found := p.shares.Get(v.share.Name)
	if !found || link.Expired(p.now()) {
		return false
	}
	v.share = link
	if !v.owns(id) {
		return false
	}
	defer p.space.invalidate()

	switch err := v.files.FinishUpload(id); {
	case errors.Is(err, system.ErrUploadIncomplete):
		info, _ := v.files.UploadStatus(id)
		writeOffsetError(w, http.StatusConflict, codeUploadOutOfSync, "the upload is not complete", info.Offset)
		return true
	case err != nil:
		fileError(w, "finish upload link", "", err)
		return true
	}
	if updated, err := p.shares.FinishUpload(link.Name, id, p.now()); err != nil {
		log.Printf("portal: count upload link file: %v", err)
	} else {
		p.noteArrival(updated)
	}
	w.WriteHeader(http.StatusNoContent)

	return true
}

// cleanVisitorName turns a name a visitor typed, or their browser sent, into one usable file or folder name, or "".
// Leading dots go, so nothing a visitor sends is hidden or can name the hidden folders.
func cleanVisitorName(raw string, maxBytes int) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case r < 0x20 || r == 0x7f:
			return -1
		}

		return r
	}, raw)
	name = strings.Trim(strings.TrimSpace(name), ". ")
	if len(name) > maxBytes {
		stem, ext := splitExtension(name)
		for len(stem)+len(ext) > maxBytes {
			_, size := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-size]
		}
		name = strings.TrimRight(stem, ". ") + ext
	}
	if clean, err := system.CleanPath(name); err != nil || clean == "" || strings.Contains(clean, "/") {
		return ""
	}

	return name
}

func splitExtension(name string) (stem, ext string) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 || !shortExtension.MatchString(name[dot:]) {
		return name, ""
	}

	return name[:dot], name[dot:]
}

func numberedFile(name string, attempt int) string {
	if attempt == 1 {
		return name
	}
	stem, ext := splitExtension(name)

	return stem + " (" + strconv.Itoa(attempt) + ")" + ext
}

func numberedFolder(name string, attempt int) string {
	if attempt == 1 {
		return name
	}

	return name + " (" + strconv.Itoa(attempt) + ")"
}
