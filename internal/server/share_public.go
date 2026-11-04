package server

import (
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/auth"
	"github.com/Ezomic/filebox/internal/system"
)

const (
	shareMissLimit  = 30
	shareFloodLimit = 120
	shareFloodSpan  = time.Minute

	downloadPolicy = "default-src 'none'; sandbox"
)

// Everything a visitor can do is one of these. A visitor never names a folder or file other than through the
// path query, which is resolved below the shared item and nowhere else.
const (
	shareActionPage     = ""
	shareActionList     = "list"
	shareActionDownload = "download"
	shareActionPreview  = "preview"
	shareActionZip      = "zip"
)

// catchAll answers every path no route claims: a share link when the first segment names a live one, the usual
// not-found page otherwise. Both outcomes of a failed lookup are that same page.
func (p *Portal) catchAll(w http.ResponseWriter, r *http.Request) {
	if p.shares != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) && p.serveShare(w, r) {
		return
	}
	p.notFound(w, r)
}

func splitSharePath(urlPath string) (name, action string, ok bool) {
	rest, found := strings.CutPrefix(urlPath, "/")
	if !found {
		return "", "", false
	}
	name, action, _ = strings.Cut(rest, "/")
	name = normalizeShareName(name)
	if len(name) < shareNameMin || len(name) > shareNameMax || !shareNamePattern.MatchString(name) {
		return "", "", false
	}

	return name, action, true
}

// serveShare reports whether it wrote the response. A request that does not resolve writes nothing, so the caller
// answers it with the ordinary not-found page, and it is counted against the address that sent it.
func (p *Portal) serveShare(w http.ResponseWriter, r *http.Request) bool {
	name, action, ok := splitSharePath(r.URL.Path)
	if !ok {
		return false
	}
	ip := ipKey(r)
	if p.shareMiss.blocked(ip) || !p.shareFlood.reserve(ip) {
		w.Header().Set("Retry-After", "60")
		writeCoded(w, http.StatusTooManyRequests, codeRateLimited, "too many requests, try again later")
		return true
	}
	if !p.answerShare(w, r, name, action) {
		p.shareMiss.fail(ip)
		return false
	}

	return true
}

func (p *Portal) answerShare(w http.ResponseWriter, r *http.Request, name, action string) bool {
	switch action {
	case shareActionPage, shareActionList, shareActionDownload, shareActionPreview, shareActionZip:
	default:
		return false
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return false
	}
	share, found := p.shares.Get(name)
	if !found || share.Expired(p.now()) {
		return false
	}
	files, err := p.openFiles(share.Owner)
	if err != nil {
		return false
	}
	defer closerOf(files)()

	visit := sharedItem{portal: p, share: share, files: files}
	if !visit.targetIsLive() {
		return false
	}
	if action == shareActionPage {
		p.sharePage(w, r)
		return true
	}

	return visit.serve(w, r, action, query.Get("path"))
}

func (p *Portal) sharePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	http.ServeFileFS(w, r, p.pages, "share.html")
}

type sharedItem struct {
	portal *Portal
	share  auth.Share
	files  system.Files
}

// targetIsLive is false when the shared path is gone, is a link, or changed between file and folder since the link was made.
func (s sharedItem) targetIsLive() bool {
	info, err := lstatWithoutLinks(s.files, s.share.Path)

	return err == nil && info.IsDir == s.share.Dir
}

// below turns the visitor's path into a path under the shared item. It is "" for the item itself.
func (s sharedItem) below(requested string) (full string, ok bool) {
	sub, err := system.CleanPath(requested)
	if err != nil || (sub != "" && !s.share.Dir) {
		return "", false
	}
	if sub == "" {
		return s.share.Path, true
	}
	full = s.share.Path + "/" + sub
	if _, err := lstatWithoutLinks(s.files, full); err != nil {
		return "", false
	}

	return full, true
}

func (s sharedItem) serve(w http.ResponseWriter, r *http.Request, action, requested string) bool {
	full, ok := s.below(requested)
	if !ok {
		return false
	}
	switch action {
	case shareActionList:
		return s.list(w, full, requested)
	case shareActionDownload:
		return s.download(w, r, full)
	case shareActionPreview:
		return s.preview(w, r, full)
	case shareActionZip:
		return s.zip(w, r, full)
	}

	return false
}

func (s sharedItem) list(w http.ResponseWriter, full, requested string) bool {
	if !s.share.Dir {
		info, err := s.files.Lstat(full)
		if err != nil {
			return false
		}
		_, previewable := imageKind(info.Name)
		writeJSON(w, http.StatusOK, map[string]any{"kind": "file", "name": info.Name, "size": info.Size, "modified": info.Modified, "preview": previewable})

		return true
	}
	entries, err := s.files.List(full)
	if err != nil {
		return false
	}
	shown := make([]system.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.Symlink {
			continue
		}
		if _, err := system.CleanPath(entry.Name); err != nil {
			continue
		}
		shown = append(shown, entry)
	}
	sub, _ := system.CleanPath(requested)
	writeJSON(w, http.StatusOK, map[string]any{"kind": "folder", "name": path.Base(s.share.Path), "path": sub, "entries": shown})

	return true
}

func (s sharedItem) download(w http.ResponseWriter, r *http.Request, full string) bool {
	file, info, err := s.files.Open(full)
	if err != nil {
		return false
	}
	defer file.Close()

	s.portal.recordDownload(s.share.Name, r)
	w.Header().Set("Content-Security-Policy", downloadPolicy)
	serveAttachment(w, r, file, info)

	return true
}

func (s sharedItem) preview(w http.ResponseWriter, r *http.Request, full string) bool {
	file, info, err := s.files.Open(full)
	if err != nil {
		return false
	}
	defer file.Close()
	kind, isImage := imageKind(info.Name)
	if !isImage {
		return false
	}

	serveViewed(w, r, file, info, kind)

	return true
}

func (s sharedItem) zip(w http.ResponseWriter, r *http.Request, full string) bool {
	if !s.share.Dir {
		return false
	}
	if info, err := s.files.Lstat(full); err != nil || !info.IsDir {
		return false
	}

	w.Header().Set("Content-Security-Policy", downloadPolicy)
	if r.Method == http.MethodHead {
		zipHeaders(w, full)
		return true
	}
	s.portal.recordDownload(s.share.Name, r)
	fileHandlers{files: s.files}.streamZip(w, r, full)

	return true
}

// imageKind is the only inline type a visitor is ever given. It comes from the table, never from the file, and
// has no SVG or HTML in it.
func imageKind(name string) (viewType, bool) {
	ext := path.Ext(name)
	if ext == name {
		return viewType{}, false
	}
	kind, ok := viewTypes[strings.ToLower(ext)]

	return kind, ok && strings.HasPrefix(kind.contentType, "image/")
}

// A download counts once: only the request that starts a file or archive at its beginning. HEAD requests, and the
// Range requests a download manager makes to resume or fetch the rest, are not new downloads.
func (p *Portal) recordDownload(name string, r *http.Request) {
	if r.Method != http.MethodGet || !startsAtBeginning(r.Header.Get("Range")) {
		return
	}
	if err := p.shares.Record(name, p.now()); err != nil {
		log.Printf("portal: count share download: %v", err)
	}
}

func startsAtBeginning(rangeHeader string) bool {
	if rangeHeader == "" {
		return true
	}
	spec, found := strings.CutPrefix(rangeHeader, "bytes=")
	if !found {
		return true
	}
	first, _, _ := strings.Cut(spec, ",")

	return strings.HasPrefix(strings.TrimSpace(first), "0-")
}
