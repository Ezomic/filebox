package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	// Two tool runs at once is what a low-spec box can take without the file transfers stalling.
	thumbConcurrency = 2
	thumbCacheMaxAge = "private, no-cache"
	failureMemory    = 10 * time.Minute
	failureCap       = 1024
	leaderRetries    = 3
)

// Thumbs makes thumbnails for every user. It limits how many tools run at once across all of them, lets
// requests for the same picture share one run, and remembers pictures that failed so a corrupt file is not retried on every scroll.
type Thumbs struct {
	tool system.Thumbnailer
	now  func() time.Time
	sem  chan struct{}

	mu       sync.Mutex
	flights  map[string]*flight
	failures map[string]time.Time
}

type flight struct {
	done chan struct{}
	data []byte
	err  error
}

func NewThumbs(tool system.Thumbnailer) *Thumbs {
	return &Thumbs{
		tool:     tool,
		now:      time.Now,
		sem:      make(chan struct{}, thumbConcurrency),
		flights:  map[string]*flight{},
		failures: map[string]time.Time{},
	}
}

func (t *Thumbs) build(ctx context.Context, key string, render func(context.Context) ([]byte, error)) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		t.mu.Lock()
		if failedAt, failed := t.failures[key]; failed && t.now().Sub(failedAt) < failureMemory {
			t.mu.Unlock()

			return nil, system.ErrBadMedia
		}
		if running, ok := t.flights[key]; ok {
			t.mu.Unlock()
			select {
			case <-running.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if errors.Is(running.err, context.Canceled) && ctx.Err() == nil && attempt < leaderRetries {
				continue
			}

			return running.data, running.err
		}
		mine := &flight{done: make(chan struct{})}
		t.flights[key] = mine
		t.mu.Unlock()

		mine.data, mine.err = t.run(ctx, render)

		t.mu.Lock()
		delete(t.flights, key)
		if errors.Is(mine.err, system.ErrBadMedia) {
			t.rememberFailure(key)
		}
		t.mu.Unlock()
		close(mine.done)

		return mine.data, mine.err
	}
}

func (t *Thumbs) rememberFailure(key string) {
	if len(t.failures) >= failureCap {
		clear(t.failures)
	}
	t.failures[key] = t.now()
}

func (t *Thumbs) run(ctx context.Context, render func(context.Context) ([]byte, error)) ([]byte, error) {
	select {
	case t.sem <- struct{}{}:
		defer func() { <-t.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return render(ctx)
}

type photoJSON struct {
	Path string    `json:"path"`
	Kind string    `json:"kind"`
	Size int64     `json:"size"`
	Date time.Time `json:"date"`
	// Custom tells the page the date was chosen by the user, so it can offer to reset it.
	Custom bool `json:"custom"`
}

func (a fileHandlers) listPhotos(w http.ResponseWriter, r *http.Request) {
	after, ok := queryValue(w, r, "after")
	if !ok {
		return
	}
	limit, ok := photoLimit(w, r)
	if !ok {
		return
	}
	kind, ok := photoType(w, r)
	if !ok {
		return
	}
	page, err := a.files.ListPhotos(after, limit, kind)
	if err != nil {
		fileError(w, "list photos", after, err)
		return
	}
	photos := make([]photoJSON, 0, len(page.Photos))
	for _, photo := range page.Photos {
		photos = append(photos, photoJSON{Path: photo.Path, Kind: photo.Kind, Size: photo.Size, Date: photo.Date.UTC(), Custom: photo.Custom})
	}
	writeJSON(w, http.StatusOK, map[string]any{"photos": photos, "next": page.Next})
}

// photoType is filtered by the server so a page holds the requested number of matching photos and the cursor stays exact.
func photoType(w http.ResponseWriter, r *http.Request) (string, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	values := query["type"]
	if err != nil || len(values) > 1 || (len(values) == 1 && values[0] != "" && values[0] != system.PhotoImage && values[0] != system.PhotoVideo) {
		writeError(w, http.StatusBadRequest, "type must be image or video")
		return "", false
	}
	if len(values) == 0 {
		return "", true
	}

	return values[0], true
}

type photoDateBody struct {
	Path string `json:"path"`
	Date string `json:"date"`
}

func (a fileHandlers) setPhotoDate(w http.ResponseWriter, r *http.Request) {
	var body photoDateBody
	if !decodeJSON(w, r, &body) {
		return
	}
	day, err := system.ParsePhotoDay(body.Date)
	if err != nil {
		writeError(w, http.StatusBadRequest, "date must be a day like 2024-05-17, from 1900 up to tomorrow")
		return
	}
	if err := a.files.SetPhotoDate(body.Path, day); err != nil {
		photoDateError(w, "set photo date", body.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a fileHandlers) resetPhotoDate(w http.ResponseWriter, r *http.Request) {
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	if err := a.files.ResetPhotoDate(name); err != nil {
		photoDateError(w, "reset photo date", name, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func photoDateError(w http.ResponseWriter, action, name string, err error) {
	if errors.Is(err, system.ErrNotMedia) {
		writeError(w, http.StatusUnsupportedMediaType, "only photos and videos have a date")
		return
	}
	if errors.Is(err, system.ErrTooManyDates) {
		writeError(w, http.StatusUnprocessableEntity, "too many photos have a custom date, reset some first")
		return
	}
	fileError(w, action, name, err)
}

func photoLimit(w http.ResponseWriter, r *http.Request) (int, bool) {
	value, ok := queryValue(w, r, "limit")
	if !ok {
		return 0, false
	}
	if value == "" {
		return system.MaxPhotoPage, true
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > system.MaxPhotoPage {
		writeError(w, http.StatusBadRequest, "limit must be a number from 1 to "+strconv.Itoa(system.MaxPhotoPage))
		return 0, false
	}

	return limit, true
}

func (a fileHandlers) thumbFile(w http.ResponseWriter, r *http.Request) {
	a.serveRendered(w, r, false)
}

func (a fileHandlers) previewFile(w http.ResponseWriter, r *http.Request) {
	a.serveRendered(w, r, true)
}

func (a fileHandlers) serveRendered(w http.ResponseWriter, r *http.Request, preview bool) {
	name, ok := queryValue(w, r, "path")
	if !ok {
		return
	}
	if a.thumbs == nil {
		writeError(w, http.StatusServiceUnavailable, "thumbnails are not available on this server")
		return
	}
	source, photo, err := a.files.OpenPhoto(name)
	if err != nil {
		fileError(w, "thumbnail", name, err)
		return
	}
	defer source.Close()
	if preview && !photo.HasPreview() {
		writeError(w, http.StatusUnsupportedMediaType, "this file type needs no converted preview")
		return
	}

	cacheName, side := photo.ThumbName(), system.ThumbSide
	if preview {
		cacheName, side = photo.PreviewName(), system.PreviewSide
	}
	cached, err := a.files.ReadThumb(cacheName)
	if err == nil {
		defer cached.Close()
		serveJPEG(w, r, cacheName, cached)
		return
	}
	if !errors.Is(err, system.ErrNotFound) {
		log.Printf("read cached thumbnail %s: %v", cacheName, err)
	}

	data, err := a.render(r.Context(), source, photo, cacheName, side)
	if err != nil {
		renderError(w, r, name, err)
		return
	}
	serveJPEG(w, r, cacheName, bytes.NewReader(data))
}

func serveJPEG(w http.ResponseWriter, r *http.Request, cacheName string, content io.ReadSeeker) {
	header := w.Header()
	header.Set("Content-Type", "image/jpeg")
	header.Set("Content-Disposition", "inline")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", viewPolicy)
	header.Set("Cache-Control", thumbCacheMaxAge)
	header.Set("ETag", `"`+cacheName+`"`)
	http.ServeContent(w, r, "", time.Time{}, content)
}

func (a fileHandlers) render(ctx context.Context, source system.File, photo system.Photo, cacheName string, side int) ([]byte, error) {
	return a.thumbs.build(ctx, photo.Scope+"/"+cacheName, func(ctx context.Context) ([]byte, error) {
		data, err := a.thumbs.tool.Render(ctx, source, photo.Kind, side)
		if err != nil {
			return nil, err
		}
		if err := a.files.WriteThumb(cacheName, data); err != nil {
			log.Printf("store thumbnail %s: %v", cacheName, err)
		}

		return data, nil
	})
}

func renderError(w http.ResponseWriter, r *http.Request, name string, err error) {
	switch {
	case r.Context().Err() != nil:
		return
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusServiceUnavailable, "thumbnails are busy, try again")
	case errors.Is(err, system.ErrToolMissing):
		log.Printf("thumbnail %q: %v", name, err)
		writeError(w, http.StatusServiceUnavailable, "thumbnails are not available on this server")
	case errors.Is(err, system.ErrBadMedia):
		log.Printf("thumbnail %q: %v", name, err)
		writeError(w, http.StatusUnprocessableEntity, "this file could not be turned into a picture")
	default:
		fileError(w, "thumbnail", name, err)
	}
}

func (s *Server) photosPage(w http.ResponseWriter, r *http.Request) {
	s.protectedPage(w, r, "photos.html")
}
