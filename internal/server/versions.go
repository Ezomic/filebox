package server

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	// A big folder is copied file by file, which can take longer than the server's write timeout.
	restoreTimeout = 15 * time.Minute

	restoreOverQuotaMessage = "restoring this would take you over your storage quota: empty the trash, delete files, or ask an admin for more space"
	restoreDiskFullMessage  = "The disk is full, so nothing can be restored now. Delete files or empty the trash to free space."
)

// versionHandlers serve the earlier versions of one Files root: the read-only snapshots, and the live files a restore
// writes back into.
type versionHandlers struct {
	live     system.Files
	versions system.Versions
	space    *storageProbe
	// quota is the signed-in user's quota; the family folder has none.
	quota func() (system.Quota, bool)
}

// versionScope picks the folder a version route works on. Like fileAPI, the route decides it, never the request.
type versionScope struct {
	files    func(http.ResponseWriter, string) (system.Files, func(), bool)
	versions func(user string) (system.Versions, error)
	quota    func(user string) (system.Quota, bool)
}

type versionJSON struct {
	ID    string    `json:"id"`
	Taken time.Time `json:"taken"`
}

func (p *Portal) versionRoutes() {
	p.versionAPI("GET /api/versions", versionHandlers.list)
	p.versionAPI("GET /api/versions/{id}/files", versionHandlers.listFiles)
	p.versionAPI("GET /api/versions/{id}/files/download", versionHandlers.download)
	p.versionAPI("POST /api/versions/{id}/restore", versionHandlers.restore)
}

// versionAPI serves one version route for the signed-in user's own folder, and under /api/family for the shared folder.
func (p *Portal) versionAPI(pattern string, call func(versionHandlers, http.ResponseWriter, *http.Request)) {
	own := versionScope{files: p.openUserFiles, versions: p.openVersions, quota: p.space.quota}
	p.mux.HandleFunc(pattern, p.versionRoute(own, call))
	if p.openFamily == nil || p.openFamilyVersions == nil {
		return
	}
	family := versionScope{
		files:    p.openFamilyFiles,
		versions: func(string) (system.Versions, error) { return p.openFamilyVersions() },
		quota:    func(string) (system.Quota, bool) { return system.Quota{}, false },
	}
	method, path, _ := strings.Cut(pattern, " ")
	p.mux.HandleFunc(method+" /api/family"+strings.TrimPrefix(path, "/api"), p.versionRoute(family, call))
}

func (p *Portal) versionRoute(scope versionScope, call func(versionHandlers, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return p.requireSession(func(w http.ResponseWriter, r *http.Request, user string) {
		versions, err := scope.versions(user)
		if err != nil {
			log.Printf("portal: open earlier versions: %v", err)
			writeCoded(w, http.StatusInternalServerError, codeVersionsUnavailable, "could not open the earlier versions")
			return
		}
		live, closeLive, ok := scope.files(w, user)
		if !ok {
			return
		}
		defer closeLive()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			defer p.space.invalidate()
		}
		quota := func() (system.Quota, bool) { return scope.quota(user) }
		call(versionHandlers{live: live, versions: versions, space: p.space, quota: quota}, w, r)
	})
}

func (h versionHandlers) list(w http.ResponseWriter, _ *http.Request) {
	versions, err := h.versions.List()
	if err != nil {
		log.Printf("portal: list earlier versions: %v", logCause(err))
		writeCoded(w, http.StatusInternalServerError, codeVersionsUnavailable, "could not open the earlier versions")
		return
	}
	listed := make([]versionJSON, 0, len(versions))
	for _, version := range versions {
		listed = append(listed, versionJSON{ID: version.ID, Taken: version.Taken})
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": listed})
}

// open opens the version the URL names and answers the request itself when it cannot.
func (h versionHandlers) open(w http.ResponseWriter, r *http.Request) (system.Files, func(), bool) {
	earlier, err := h.versions.Open(r.PathValue("id"))
	if err != nil {
		fileError(w, "open earlier version", "", err)
		return nil, nil, false
	}

	return earlier, closerOf(earlier), true
}

func (h versionHandlers) listFiles(w http.ResponseWriter, r *http.Request) {
	earlier, closeEarlier, ok := h.open(w, r)
	if !ok {
		return
	}
	defer closeEarlier()
	fileHandlers{files: earlier}.listFiles(w, r)
}

func (h versionHandlers) download(w http.ResponseWriter, r *http.Request) {
	earlier, closeEarlier, ok := h.open(w, r)
	if !ok {
		return
	}
	defer closeEarlier()
	fileHandlers{files: earlier}.downloadFile(w, r)
}

func (h versionHandlers) restore(w http.ResponseWriter, r *http.Request) {
	var body pathBody
	if !decodeJSON(w, r, &body) {
		return
	}
	earlier, closeEarlier, ok := h.open(w, r)
	if !ok {
		return
	}
	defer closeEarlier()
	size, err := system.VersionSize(earlier, body.Path)
	if err != nil {
		fileError(w, "restore", "", err)
		return
	}
	if h.refuseWhenFull(w) || h.refuseOverQuota(w, size) {
		return
	}

	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(restoreTimeout))
	restored, err := system.RestoreVersion(h.live, earlier, body.Path)
	if err != nil {
		fileError(w, "restore", "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": restored.Path, "replaced": restored.TrashID != ""})
}

// Restores are refused on a full disk, like uploads.
func (h versionHandlers) refuseWhenFull(w http.ResponseWriter) bool {
	if !h.space.full() {
		return false
	}
	writeCoded(w, http.StatusInsufficientStorage, codeRestoreDiskFull, restoreDiskFullMessage)

	return true
}

// The item that is in the way goes to the trash, which still counts, so the whole restored size must fit.
func (h versionHandlers) refuseOverQuota(w http.ResponseWriter, size int64) bool {
	quota, ok := h.quota()
	if !ok || quota.Limit == 0 || uint64(size) <= quota.Limit-min(quota.Used, quota.Limit) {
		return false
	}
	writeCoded(w, http.StatusInsufficientStorage, codeRestoreOverQuota, restoreOverQuotaMessage)

	return true
}
