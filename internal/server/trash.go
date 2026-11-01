package server

import (
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

type trashJSON struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Folder  string    `json:"folder"`
	Deleted time.Time `json:"deleted"`
	Expires time.Time `json:"expires"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"isDir"`
}

func toTrashJSON(item system.TrashItem) trashJSON {
	return trashJSON{
		ID: item.ID, Name: item.Name(), Path: item.Path, Folder: item.Folder(),
		Deleted: item.DeletedAt.UTC(), Expires: item.DeletedAt.Add(system.TrashMaxAge).UTC(),
		Size: item.Size, IsDir: item.IsDir,
	}
}

func (a fileHandlers) listTrash(w http.ResponseWriter, _ *http.Request) {
	items, err := a.files.ListTrash()
	if err != nil {
		fileError(w, "list trash", "", err)
		return
	}
	entries := make([]trashJSON, 0, len(items))
	var total int64
	for _, item := range items {
		entries = append(entries, toTrashJSON(item))
		total += item.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": entries, "totalSize": total})
}

func (a fileHandlers) restoreTrash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	restored, err := a.files.RestoreTrash(id)
	if err != nil {
		fileError(w, "restore", id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": restored})
}

func (a fileHandlers) purgeTrash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := a.files.PurgeTrash(id); err != nil {
		fileError(w, "delete forever", id, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a fileHandlers) emptyTrash(w http.ResponseWriter, _ *http.Request) {
	removed, err := a.files.EmptyTrash()
	if err != nil {
		fileError(w, "empty trash", "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"removed": removed})
}

// The panel browses the data root, so its own trash holds what was deleted there. An admin
// reaches one user's trash with ?user=, which is opened the same way the portal opens it.
func (s *Server) trashRoute(call func(fileHandlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		user, ok := queryValue(w, r, "user")
		if !ok {
			return
		}
		if user == "" {
			call(s.fh, w, r)
			return
		}
		if s.openUserFiles == nil {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		files, err := s.openUserFiles(user)
		switch {
		case errors.Is(err, system.ErrInvalidUsername):
			writeError(w, http.StatusBadRequest, "invalid user")
			return
		case errors.Is(err, fs.ErrNotExist):
			writeError(w, http.StatusNotFound, "not found")
			return
		case err != nil:
			log.Printf("open files for %q: %v", user, err)
			writeError(w, http.StatusInternalServerError, "could not open the files")
			return
		}
		if closer, ok := files.(io.Closer); ok {
			defer closer.Close()
		}
		call(fileHandlers{files: files, maxUpload: s.maxUpload}, w, r)
	})
}
