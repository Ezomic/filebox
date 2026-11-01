package server

import (
	"mime"
	"net/http"
	"path"
	"strings"
)

type viewType struct {
	contentType string
	pdf         bool
}

// Content types come from this table and never from the file, so a file can not pick how the browser treats it.
var viewTypes = map[string]viewType{
	".png":  {contentType: "image/png"},
	".jpg":  {contentType: "image/jpeg"},
	".jpeg": {contentType: "image/jpeg"},
	".gif":  {contentType: "image/gif"},
	".webp": {contentType: "image/webp"},
	".avif": {contentType: "image/avif"},
	".mp4":  {contentType: "video/mp4"},
	".webm": {contentType: "video/webm"},
	".mov":  {contentType: "video/quicktime"},
	".m4v":  {contentType: "video/mp4"},
	".mp3":  {contentType: "audio/mpeg"},
	".m4a":  {contentType: "audio/mp4"},
	".wav":  {contentType: "audio/wav"},
	".ogg":  {contentType: "audio/ogg"},
	".flac": {contentType: "audio/flac"},
	".pdf":  {contentType: "application/pdf", pdf: true},
	".txt":  {contentType: "text/plain; charset=utf-8"},
	".md":   {contentType: "text/plain; charset=utf-8"},
	".log":  {contentType: "text/plain; charset=utf-8"},
	".json": {contentType: "text/plain; charset=utf-8"},
	".csv":  {contentType: "text/plain; charset=utf-8"},
}

const (
	viewPolicy    = "sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; media-src 'self'; frame-ancestors 'none'"
	viewPDFPolicy = "default-src 'none'; style-src 'unsafe-inline'; img-src 'self'; media-src 'self'; frame-ancestors 'self'"
)

func (a fileHandlers) viewFile(w http.ResponseWriter, r *http.Request) {
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	file, info, err := a.files.Open(name)
	if err != nil {
		fileError(w, "view", name, err)
		return
	}
	defer file.Close()

	kind, ok := viewType{}, false
	if ext := path.Ext(info.Name); ext != info.Name {
		kind, ok = viewTypes[strings.ToLower(ext)]
	}
	if !ok {
		writeCoded(w, http.StatusUnsupportedMediaType, codePreviewUnsupported, "this file type can not be previewed")
		return
	}

	disposition := mime.FormatMediaType("inline", map[string]string{"filename": info.Name})
	if disposition == "" {
		disposition = "inline"
	}
	policy := viewPolicy
	if kind.pdf {
		policy = viewPDFPolicy
	}
	header := w.Header()
	header.Set("Content-Type", kind.contentType)
	header.Set("Content-Disposition", disposition)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", policy)

	http.ServeContent(&deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r, "", info.Modified, file)
}
