package server

import (
	"crypto/rand"
	"errors"
	"io"
	"log"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	maxVisitorFileBytes = 240
	maxSenderBytes      = 64
	maxUploadKey        = 64
	maxNameTries        = 1000
	maxClosedUploads    = 10000

	// Every number a stranger can push up is bounded, so a flood of empty uploads fills nothing but the link itself.
	maxPendingPerLink = 50
	maxFilesPerLink   = 5000
	maxVisitsPerLink  = 200
	minUploadCharge   = 1 << 20
)

var (
	shortExtension = regexp.MustCompile(`^\.[A-Za-z0-9]{1,8}$`)
	errLinkFull    = errors.New("the upload link is full")
)

// receiving is what upload links keep in memory between requests. The lock per owner makes the cap, the free names
// and the bookkeeping one decision at a time, also across two links into the same folder. Closed remembers the
// unfinished uploads of revoked links for a day, so their visitors can be told what happened.
type receiving struct {
	locksMu sync.Mutex
	locks   map[string]*ownerLock

	closedMu sync.Mutex
	closed   map[string]closedUpload

	notices uploadNotices
}

type ownerLock struct {
	mu   sync.Mutex
	refs int
}

type closedUpload struct {
	link string
	at   time.Time
}

func newReceiving() *receiving {
	return &receiving{
		locks:   map[string]*ownerLock{},
		closed:  map[string]closedUpload{},
		notices: uploadNotices{scheduled: map[string]bool{}, after: func(wait time.Duration, run func()) { time.AfterFunc(wait, run) }},
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

func (rc *receiving) rememberClosed(link auth.Share, now time.Time) {
	rc.closedMu.Lock()
	defer rc.closedMu.Unlock()

	for id := range link.Pending {
		rc.closed[id] = closedUpload{link: link.Name, at: now}
	}
	if len(rc.closed) <= maxClosedUploads {
		return
	}
	for id, old := range rc.closed {
		if now.Sub(old.at) > system.VisitorUploadMaxAge {
			delete(rc.closed, id)
		}
	}
	for id := range rc.closed {
		if len(rc.closed) <= maxClosedUploads {
			break
		}
		delete(rc.closed, id)
	}
}

func (rc *receiving) wasClosed(id, link string, now time.Time) bool {
	rc.closedMu.Lock()
	defer rc.closedMu.Unlock()
	closed, found := rc.closed[id]

	return found && closed.link == link && now.Sub(closed.at) <= system.VisitorUploadMaxAge
}

// uploadContinuation is the upload id of a request that carries on an upload: a chunk or a finish.
func uploadContinuation(r *http.Request, action string) (string, bool) {
	rest, found := strings.CutPrefix(action, "uploads/")
	if !found {
		return "", false
	}
	id, tail, hasTail := strings.Cut(rest, "/")
	chunk := !hasTail && r.Method == http.MethodPut
	finish := tail == "finish" && r.Method == http.MethodPost

	return id, id != "" && (chunk || finish)
}

// continuesUpload is true for a chunk or finish of an upload this live link started. Those requests are a visitor
// getting on with a file, not a guess, so they are not counted against the address.
func (p *Portal) continuesUpload(r *http.Request, name, action string) bool {
	id, ok := uploadContinuation(r, action)
	if !ok {
		return false
	}
	link, found := p.shares.Get(name)
	_, pending := link.Pending[id]

	return found && link.IsUploadLink() && !link.Expired(p.now()) && pending
}

// answerClosedUpload tells a visitor whose link expired or was revoked during their upload why it stopped. Only the
// holder of one of its upload ids learns this, and they already knew the link. Every other request about a gone
// link gets the same answer as an unknown name.
func (p *Portal) answerClosedUpload(w http.ResponseWriter, r *http.Request, name, action string, link auth.Share, found bool) bool {
	id, ok := uploadContinuation(r, action)
	if !ok {
		return false
	}
	_, pending := link.Pending[id]
	if !(found && link.IsUploadLink() && pending) && !p.receive.wasClosed(id, name, p.now()) {
		return false
	}
	// Over HTTP/1.1 an answer sent before the chunk is read ends in a reset connection, and the visitor's page would
	// only see a network error. The chunk is at most one, and only its uploader gets here.
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxChunkBytes))
	writeCoded(w, http.StatusGone, codeUploadLinkClosed, "this link closed while your file was uploading")

	return true
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
	id, ok := uploadContinuation(r, action)
	switch {
	case !ok:
		return false
	case r.Method == http.MethodPut:
		return v.chunk(w, r, id)
	}

	return v.finish(w, id)
}

func (v uploadVisit) handlers() fileHandlers {
	return fileHandlers{files: v.files, maxUpload: v.portal.maxUpload, space: v.portal.space}
}

type visitorUploadBody struct {
	Name   string `json:"name"`
	Size   *int64 `json:"size"`
	Sender string `json:"sender"`
	Visit  string `json:"visit"`
	Key    string `json:"key"`
}

func (v uploadVisit) create(w http.ResponseWriter, r *http.Request) bool {
	var body visitorUploadBody
	if !decodeJSON(w, r, &body) {
		return true
	}
	p := v.portal
	name := cleanVisitorName(body.Name, maxVisitorFileBytes)
	switch {
	case body.Size == nil || *body.Size < 0 || len(body.Key) > maxUploadKey:
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
	if id, upload, repeated := v.repeated(body.Key, stale); repeated {
		return v.answerStarted(w, id, upload)
	}
	if v.full(stale, *body.Size) {
		writeLinkFull(w)
		return true
	}

	plan, err := v.plan(body.Visit, body.Sender, name)
	switch {
	case errors.Is(err, errLinkFull):
		writeLinkFull(w)
		return true
	case errors.Is(err, system.ErrNotFound) || errors.Is(err, system.ErrNotDir):
		return false
	}
	var info system.UploadInfo
	if err == nil {
		info, err = v.files.CreateVisitorUpload(plan.target, *body.Size)
	}
	if err != nil {
		fileError(w, "upload link", "", err)
		return true
	}
	upload := auth.PendingUpload{Size: info.Size, Key: body.Key, Visit: plan.visit, Path: info.Path}
	_, err = p.shares.UpdateUploadLink(link.Name, func(saved *auth.Share) error {
		plan.apply(saved, stale)
		if saved.Pending == nil {
			saved.Pending = map[string]auth.PendingUpload{}
		}
		saved.Pending[info.ID] = upload
		return nil
	})
	switch {
	case errors.Is(err, auth.ErrShareMissing):
		return false
	case err != nil:
		log.Printf("portal: save upload link: %v", err)
		writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")
		return true
	}

	return v.answerStarted(w, info.ID, upload)
}

func writeLinkFull(w http.ResponseWriter) {
	writeCoded(w, http.StatusInsufficientStorage, codeUploadLinkFull, "this link cannot take more files")
}

func (v uploadVisit) answerStarted(w http.ResponseWriter, id string, upload auth.PendingUpload) bool {
	info, err := v.files.UploadStatus(id)
	if err != nil {
		fileError(w, "upload link", "", err)
		return true
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": path.Base(upload.Path), "size": upload.Size, "offset": info.Offset, "visit": upload.Visit})

	return true
}

// repeated finds the upload a visitor's page already started under the same key, so a start whose answer was lost
// and is sent again neither makes a second upload nor takes more of the cap.
func (v uploadVisit) repeated(key string, stale []string) (string, auth.PendingUpload, bool) {
	if key == "" {
		return "", auth.PendingUpload{}, false
	}
	for id, upload := range v.share.Pending {
		if upload.Key == key && !slices.Contains(stale, id) {
			return id, upload, true
		}
	}

	return "", auth.PendingUpload{}, false
}

func charge(size int64) int64 {
	return max(size, minUploadCharge)
}

// full counts every file as at least a megabyte, so the cap also bounds how many files a link takes.
func (v uploadVisit) full(stale []string, size int64) bool {
	link := v.share
	live := int64(len(link.Pending) - len(stale))
	taken := link.Charged
	for id, upload := range link.Pending {
		if !slices.Contains(stale, id) {
			taken += charge(upload.Size)
		}
	}

	return live >= maxPendingPerLink || link.Received+live >= maxFilesPerLink || taken+charge(size) > link.Cap
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

// startPlan is where a new upload lands, and the visit it starts or carries on.
type startPlan struct {
	target   string
	visit    string
	newVisit bool
	drop     []string
}

func (plan startPlan) apply(link *auth.Share, stale []string) {
	for _, id := range stale {
		delete(link.Pending, id)
	}
	for _, token := range plan.drop {
		delete(link.Visits, token)
	}
	if !plan.newVisit {
		return
	}
	if link.Visits == nil {
		link.Visits = map[string]auth.Visit{}
	}
	link.Visits[plan.visit] = auth.Visit{Folder: path.Dir(plan.target)}
}

// plan picks the folder and a free name. A visitor who gave a name gets a folder of their own for the whole visit,
// made only once their first file is finished. A name that is taken gets a number, so a visitor never adds to a
// folder that was already there.
func (v uploadVisit) plan(token, sender, name string) (startPlan, error) {
	var plan startPlan
	folder := v.share.Path
	if known, found := v.share.Visits[token]; found && token != "" {
		folder, plan.visit = known.Folder, token
	} else if visitor := cleanVisitorName(sender, maxSenderBytes); visitor != "" {
		if len(v.share.Visits) >= maxVisitsPerLink {
			plan.drop = v.deadVisits()
			if len(v.share.Visits)-len(plan.drop) >= maxVisitsPerLink {
				return plan, errLinkFull
			}
		}
		chosen, err := v.freeFolder(visitor)
		if err != nil {
			return plan, err
		}
		folder, plan.visit, plan.newVisit = chosen, rand.Text(), true
	}
	target, err := v.freeName(folder, name)
	plan.target = target

	return plan, err
}

// deadVisits never got a file in and wait for none, so they can make way for new visitors.
func (v uploadVisit) deadVisits() []string {
	waiting := map[string]bool{}
	for _, upload := range v.share.Pending {
		waiting[upload.Visit] = true
	}
	var dead []string
	for token, visit := range v.share.Visits {
		if !visit.Made && !waiting[token] {
			dead = append(dead, token)
		}
	}

	return dead
}

func (v uploadVisit) freeFolder(visitor string) (string, error) {
	reserved := map[string]bool{}
	for _, link := range v.portal.shares.ByOwner(v.share.Owner) {
		for _, visit := range link.Visits {
			reserved[visit.Folder] = true
		}
	}
	for attempt := 1; attempt <= maxNameTries; attempt++ {
		candidate := v.share.Path + "/" + numberedFolder(visitor, attempt)
		if reserved[candidate] {
			continue
		}
		switch _, err := v.files.Lstat(candidate); {
		case errors.Is(err, system.ErrNotFound):
			return candidate, nil
		case err != nil:
			return "", err
		}
	}

	return "", system.ErrExists
}

// freeName also skips the names the owner's unfinished visitor uploads will take.
func (v uploadVisit) freeName(folder, name string) (string, error) {
	taken := map[string]bool{}
	for _, link := range v.portal.shares.ByOwner(v.share.Owner) {
		for _, upload := range link.Pending {
			taken[upload.Path] = true
		}
	}
	for attempt := 1; attempt <= maxNameTries; attempt++ {
		candidate := folder + "/" + numberedFile(name, attempt)
		if taken[candidate] {
			continue
		}
		switch _, err := v.files.Lstat(candidate); {
		case errors.Is(err, system.ErrNotFound):
			return candidate, nil
		case err != nil:
			return "", err
		}
	}

	return "", system.ErrExists
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

// finish counts the file before it lands and takes the count back if it does not, so a received file is never
// left out of the cap. A visitor's folder is made here, for their first finished file.
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
	upload := link.Pending[id]
	if info, _ := v.files.UploadStatus(id); info.Offset != info.Size {
		writeOffsetError(w, http.StatusConflict, codeUploadOutOfSync, "the upload is not complete", info.Offset)
		return true
	}
	folder := path.Dir(upload.Path)
	made, err := v.ensureFolder(folder)
	if err != nil {
		fileError(w, "finish upload link", "", err)
		return true
	}

	now, report := p.now(), p.email.enabled()
	counted, err := p.shares.UpdateUploadLink(link.Name, func(saved *auth.Share) error {
		countFinished(saved, id, upload, now, report)
		return nil
	})
	if err != nil {
		v.removeIfMade(folder, made)
		log.Printf("portal: count upload link file: %v", err)
		writeCoded(w, http.StatusInternalServerError, codeShareSaveFailed, "could not save the link")
		return true
	}
	if err := v.files.FinishUpload(id); err != nil {
		if _, undoErr := p.shares.UpdateUploadLink(link.Name, func(saved *auth.Share) error {
			uncountFinished(saved, id, upload, link.LastUsed, made, report)
			return nil
		}); undoErr != nil {
			log.Printf("portal: take back upload link file: %v", undoErr)
		}
		v.removeIfMade(folder, made)
		fileError(w, "finish upload link", "", err)
		return true
	}
	p.scheduleSummary(counted, time.Time{})
	w.WriteHeader(http.StatusNoContent)

	return true
}

func countFinished(link *auth.Share, id string, upload auth.PendingUpload, now time.Time, report bool) {
	delete(link.Pending, id)
	link.Received++
	link.ReceivedBytes += upload.Size
	link.Charged += charge(upload.Size)
	link.LastUsed = now
	if visit, found := link.Visits[upload.Visit]; found {
		visit.Made = true
		link.Visits[upload.Visit] = visit
	}
	if report {
		if link.Unreported == 0 {
			link.UnreportedSince = now
		}
		link.Unreported++
	}
}

func uncountFinished(link *auth.Share, id string, upload auth.PendingUpload, lastUsed time.Time, made, report bool) {
	if link.Pending == nil {
		link.Pending = map[string]auth.PendingUpload{}
	}
	link.Pending[id] = upload
	link.Received--
	link.ReceivedBytes -= upload.Size
	link.Charged -= charge(upload.Size)
	link.LastUsed = lastUsed
	if visit, found := link.Visits[upload.Visit]; found && made {
		visit.Made = false
		link.Visits[upload.Visit] = visit
	}
	if report && link.Unreported > 0 {
		link.Unreported--
		if link.Unreported == 0 {
			link.UnreportedSince = time.Time{}
		}
	}
}

// ensureFolder makes a visitor's folder when it is not there yet and reports whether it did.
func (v uploadVisit) ensureFolder(folder string) (bool, error) {
	if folder == v.share.Path {
		return false, nil
	}
	info, err := lstatWithoutLinks(v.files, folder)
	switch {
	case err == nil && info.IsDir:
		return false, nil
	case err == nil:
		return false, system.ErrNotDir
	case !errors.Is(err, system.ErrNotFound):
		return false, err
	}
	if err := v.files.Mkdir(folder); err != nil {
		return false, err
	}

	return true, nil
}

// removeIfMade takes away a folder made for a file that did not land. Remove refuses a folder with anything in it.
func (v uploadVisit) removeIfMade(folder string, made bool) {
	if made {
		_ = v.files.Remove(folder)
	}
}

// cleanVisitorName turns a name a visitor typed, or their browser sent, into one usable file or folder name, or "".
// Leading dots go, so nothing a visitor sends is hidden or can name the hidden folders, and so do invisible format
// characters such as a right-to-left override, which could make a name show another extension than it has.
func cleanVisitorName(raw string, maxBytes int) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '-'
		case r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r):
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
