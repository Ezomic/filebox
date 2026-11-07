package server

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

// The panel's view of the earlier versions: per folder only counts, dates, sizes and errors, never a file name.
type snapshotsView struct {
	// Reported is false until filebox-snapshot has written its first status.
	Reported bool                 `json:"reported"`
	Enabled  bool                 `json:"enabled"`
	Reason   string               `json:"reason"`
	Checked  *time.Time           `json:"checked"`
	Daily    *time.Time           `json:"daily"`
	DailyOK  bool                 `json:"dailyOk"`
	LowSpace *lowSpaceView        `json:"lowSpace"`
	Folders  []snapshotFolderView `json:"folders"`
}

type lowSpaceView struct {
	At        time.Time `json:"at"`
	Deleted   int       `json:"deleted"`
	StillFull bool      `json:"stillFull"`
}

type snapshotFolderView struct {
	Name string `json:"name"`
	// Kind is "family", "user" or "deleted".
	Kind string `json:"kind"`
	// Protection is "yes", "no", "gone", or "unchecked" for an account the helper has not seen yet.
	Protection string     `json:"protection"`
	Count      int        `json:"count"`
	Oldest     *time.Time `json:"oldest"`
	Newest     *time.Time `json:"newest"`
	Held       *uint64    `json:"held"`
	Error      string     `json:"error"`
}

func (s *Server) snapshots(w http.ResponseWriter, _ *http.Request) {
	view := snapshotsView{Folders: []snapshotFolderView{}}
	if s.snapshotStatus == nil {
		writeJSON(w, http.StatusOK, view)
		return
	}
	status, err := s.snapshotStatus()
	switch {
	case errors.Is(err, system.ErrNoSnapshotStatus):
		writeJSON(w, http.StatusOK, view)
		return
	case err != nil:
		log.Printf("read snapshot status: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the status of the earlier versions")
		return
	}
	names, err := s.users.List()
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "could not list users")
		return
	}
	writeJSON(w, http.StatusOK, newSnapshotsView(status, names))
}

// Every current account gets a row, also before the helper has seen it. A deleted account's earlier versions keep
// a row of their own until they expire.
func newSnapshotsView(status system.SnapshotStatus, users []string) snapshotsView {
	view := snapshotsView{
		Reported: true, Enabled: status.Enabled, Reason: status.Reason,
		Checked: optionalTime(status.Checked), Daily: optionalTime(status.Daily), DailyOK: status.DailyOK,
		Folders: []snapshotFolderView{},
	}
	if low := status.LowSpace; low != nil {
		view.LowSpace = &lowSpaceView{At: low.At, Deleted: low.Deleted, StillFull: low.StillFull}
	}
	if !status.Enabled {
		return view
	}
	reported := map[string]system.SnapshotFolder{}
	for _, folder := range status.Folders {
		reported[folder.Name] = folder
	}
	view.Folders = append(view.Folders, folderView(reported, system.FamilyDir, "family"))
	for _, name := range users {
		view.Folders = append(view.Folders, folderView(reported, name, "user"))
	}
	for _, folder := range status.Folders {
		if folder.Protection == "gone" {
			view.Folders = append(view.Folders, folderView(reported, folder.Name, "deleted"))
		}
	}

	return view
}

func folderView(reported map[string]system.SnapshotFolder, name, kind string) snapshotFolderView {
	folder, ok := reported[name]
	if !ok {
		return snapshotFolderView{Name: name, Kind: kind, Protection: "unchecked"}
	}

	return snapshotFolderView{
		Name: name, Kind: kind, Protection: folder.Protection, Count: folder.Count,
		Oldest: optionalTime(folder.Oldest), Newest: optionalTime(folder.Newest), Held: folder.Held, Error: folder.Error,
	}
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}

	return &value
}
