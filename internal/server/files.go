package server

import (
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

// The server-wide timeouts stay at 30s. Transfers instead get this long to make progress
// between two chunks, so a big file can take hours while a stalled client is still dropped.
const transferIdle = 2 * time.Minute

type pathBody struct {
	Path string `json:"path"`
}

type renameBody struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) filesPage(w http.ResponseWriter, r *http.Request) {
	s.protectedPage(w, r, "files.html")
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	dir, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	entries, err := s.files.List(dir)
	if err != nil {
		fileError(w, "list", dir, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries})
}

func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	file, info, err := s.files.Open(name)
	if err != nil {
		fileError(w, "download", name, err)
		return
	}
	defer file.Close()

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": info.Name})
	if disposition == "" {
		disposition = "attachment"
	}
	header := w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", disposition)
	header.Set("X-Content-Type-Options", "nosniff")

	http.ServeContent(&deadlineWriter{ResponseWriter: w, rc: http.NewResponseController(w)}, r, "", info.Modified, file)
}

func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/octet-stream" {
		writeError(w, http.StatusUnsupportedMediaType, "expected application/octet-stream")
		return
	}
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	overwrite, ok := overwriteFlag(w, r)
	if !ok {
		return
	}
	if r.ContentLength > s.maxUpload {
		writeError(w, http.StatusRequestEntityTooLarge, "the file is larger than the upload limit")
		return
	}

	rc := http.NewResponseController(w)
	body := &trackedReader{r: &deadlineReader{r: http.MaxBytesReader(w, r.Body, s.maxUpload), rc: rc}}
	err := s.files.Write(name, body, overwrite)
	_ = rc.SetWriteDeadline(time.Now().Add(transferIdle))

	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "the file is larger than the upload limit")
	case body.err != nil:
		log.Printf("upload %q interrupted: %v", name, body.err)
		writeError(w, http.StatusBadRequest, "the upload was interrupted")
	case errors.Is(err, system.ErrIsDir):
		writeError(w, http.StatusConflict, "a folder with that name already exists")
	default:
		fileError(w, "upload", name, err)
	}
}

func (s *Server) createFolder(w http.ResponseWriter, r *http.Request) {
	var body pathBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.files.Mkdir(body.Path); err != nil {
		fileError(w, "create folder", body.Path, err)
		return
	}
	writeJSON(w, http.StatusCreated, body)
}

func (s *Server) renameFile(w http.ResponseWriter, r *http.Request) {
	var body renameBody
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.files.Rename(body.From, body.To); err != nil {
		fileError(w, "rename", body.From, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	if err := s.files.Remove(name); err != nil {
		fileError(w, "delete", name, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// A malformed query is refused so a broken escape can never turn into an empty path, which means the root.
func queryValue(w http.ResponseWriter, r *http.Request, key string) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid query string")
		return "", false
	}

	return query.Get(key), true
}

func overwriteFlag(w http.ResponseWriter, r *http.Request) (overwrite, ok bool) {
	value, ok := queryValue(w, r, "overwrite")
	if !ok {
		return false, false
	}
	switch value {
	case "", "0":
		return false, true
	case "1":
		return true, true
	}
	writeError(w, http.StatusBadRequest, "overwrite must be 1 or omitted")

	return false, false
}

func fileError(w http.ResponseWriter, action, name string, err error) {
	switch {
	case errors.Is(err, system.ErrInvalidPath):
		writeError(w, http.StatusBadRequest, "invalid path")
	case errors.Is(err, system.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, system.ErrNotEmpty):
		writeError(w, http.StatusConflict, "the folder is not empty, delete what is inside first")
	case errors.Is(err, system.ErrExists):
		writeError(w, http.StatusConflict, "something with that name already exists")
	case errors.Is(err, system.ErrNotDir):
		writeError(w, http.StatusUnprocessableEntity, "part of that path is not a folder")
	case errors.Is(err, system.ErrIsDir):
		writeError(w, http.StatusUnprocessableEntity, "that path is a folder")
	case errors.Is(err, system.ErrIntoSelf):
		writeError(w, http.StatusUnprocessableEntity, "a folder cannot be moved into itself")
	default:
		log.Printf("%s %q: %v", action, name, err)
		writeError(w, http.StatusInternalServerError, "the file operation failed")
	}
}

type trackedReader struct {
	r   io.Reader
	err error
}

func (t *trackedReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}

	return n, err
}

type deadlineReader struct {
	r  io.Reader
	rc *http.ResponseController
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	_ = d.rc.SetReadDeadline(time.Now().Add(transferIdle))

	return d.r.Read(p)
}

type deadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(transferIdle))

	return d.ResponseWriter.Write(p)
}
