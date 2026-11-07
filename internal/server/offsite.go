package server

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

// The panel's view of the offsite backup: where it goes (the host only), times, sizes and results. It never holds a
// file name or a secret, because the status it comes from holds none.
type offsiteView struct {
	// Installed is false without the backup add-on; the dashboard then shows nothing about backups.
	Installed bool `json:"installed"`
	// Reported is false until filebox-backup has written a status, which "filebox-backup setup" does at its end.
	Reported   bool       `json:"reported"`
	Configured bool       `json:"configured"`
	Checked    *time.Time `json:"checked"`
	// Kind is "sftp", "s3", "b2", "local" and so on; Host is "" for a kind without one.
	Kind string          `json:"kind"`
	Host string          `json:"host"`
	Run  *offsiteRunView `json:"run"`
	// Latest is when the newest complete backup was started, null before the first one has finished.
	Latest *time.Time        `json:"latest"`
	Size   *offsiteSizeView  `json:"size"`
	Check  *offsiteCheckView `json:"check"`
}

type offsiteRunView struct {
	Started time.Time `json:"started"`
	Seconds int64     `json:"seconds"`
	Result  string    `json:"result"`
	Added   *uint64   `json:"added"`
}

type offsiteSizeView struct {
	Measured time.Time `json:"measured"`
	Bytes    uint64    `json:"bytes"`
}

type offsiteCheckView struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
}

func (s *Server) offsite(w http.ResponseWriter, _ *http.Request) {
	if s.offsiteStatus == nil {
		writeJSON(w, http.StatusOK, offsiteView{})
		return
	}
	status, err := s.offsiteStatus()
	switch {
	case errors.Is(err, system.ErrNoBackupStatus):
		writeJSON(w, http.StatusOK, offsiteView{Installed: true})
	case err != nil:
		log.Printf("read backup status: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the status of the offsite backup")
	default:
		writeJSON(w, http.StatusOK, newBackupView(status))
	}
}

func newBackupView(status system.BackupStatus) offsiteView {
	view := offsiteView{
		Installed: true, Reported: true, Configured: status.Configured, Checked: optionalTime(status.Checked),
		Kind: status.Kind, Host: status.Host, Latest: optionalTime(status.Latest),
	}
	if run := status.Run; run != nil {
		view.Run = &offsiteRunView{Started: run.Started, Seconds: int64(run.Duration / time.Second), Result: run.Result, Added: run.Added}
	}
	if size := status.Size; size != nil {
		view.Size = &offsiteSizeView{Measured: size.Measured, Bytes: size.Bytes}
	}
	if check := status.Check; check != nil {
		view.Check = &offsiteCheckView{At: check.At, OK: check.OK}
	}

	return view
}
