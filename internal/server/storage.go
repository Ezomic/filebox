package server

import (
	"errors"
	"log"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	storageTTL = 3 * time.Second

	diskFullMessage = "The disk is full, so new uploads are paused. Delete files or empty the trash to free space."
)

// storageProbe answers "how full is the disk" for many pages at once, so the answers are kept for a few seconds.
// Any write through the portal drops them, so the bar a user sees right after an upload or a delete is current.
type storageProbe struct {
	disk  system.DiskSpace
	users system.Users
	now   func() time.Time

	diskMu sync.Mutex
	diskAt time.Time
	diskV  system.DiskUsage

	quotaMu sync.Mutex
	quotaAt time.Time
	quotaV  map[string]system.Quota
}

func newStorageProbe(disk system.DiskSpace, users system.Users) *storageProbe {
	return &storageProbe{disk: disk, users: users, now: time.Now}
}

func (p *storageProbe) usage() (system.DiskUsage, error) {
	if p == nil || p.disk == nil {
		return system.DiskUsage{}, system.ErrNoDisk
	}
	p.diskMu.Lock()
	defer p.diskMu.Unlock()

	if !p.diskAt.IsZero() && p.now().Sub(p.diskAt) < storageTTL {
		return p.diskV, nil
	}
	usage, err := p.disk.Usage()
	if err != nil {
		return system.DiskUsage{}, err
	}
	p.diskV, p.diskAt = usage.Normalize(), p.now()

	return p.diskV, nil
}

// quota returns the user's btrfs usage and limit; ok is false when quotas are off or the user has none yet.
func (p *storageProbe) quota(user string) (system.Quota, bool) {
	if p == nil || p.users == nil {
		return system.Quota{}, false
	}
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()

	if p.quotaAt.IsZero() || p.now().Sub(p.quotaAt) >= storageTTL {
		quotas, _ := loadQuotas(p.users)
		p.quotaV, p.quotaAt = quotas, p.now()
	}
	quota, ok := p.quotaV[user]

	return quota, ok
}

func (p *storageProbe) invalidate() {
	if p == nil {
		return
	}
	p.diskMu.Lock()
	p.diskAt = time.Time{}
	p.diskMu.Unlock()
	p.quotaMu.Lock()
	p.quotaAt = time.Time{}
	p.quotaMu.Unlock()
}

type storageView struct {
	Used       uint64              `json:"used"`
	Total      uint64              `json:"total"`
	Free       uint64              `json:"free"`
	Quota      *uint64             `json:"quota"`
	QuotaUsed  *uint64             `json:"quotaUsed"`
	TrashBytes uint64              `json:"trashBytes"`
	Percent    float64             `json:"percent"`
	Measure    string              `json:"measure"`
	Level      system.StorageLevel `json:"level"`
}

func percentOf(used, total uint64) float64 {
	if total == 0 {
		return 0
	}

	return math.Min(100, float64(used)/float64(total)*100)
}

func newStorageView(disk system.DiskUsage, quota system.Quota, hasQuota bool, trash uint64) storageView {
	view := storageView{Used: disk.Used, Total: disk.Total, Free: disk.Free, TrashBytes: trash, Measure: "disk", Level: disk.Level()}
	used, total := disk.Used, disk.Total
	if hasQuota && quota.Limit > 0 {
		limit, quotaUsed := quota.Limit, quota.Used
		view.Quota, view.QuotaUsed, view.Measure = &limit, &quotaUsed, "quota"
		used, total = quotaUsed, limit
		if view.Level != system.LevelFull {
			view.Level = system.LevelOf(used, total)
		}
	}
	view.Percent = percentOf(used, total)

	return view
}

func (p *Portal) storage(w http.ResponseWriter, _ *http.Request, user string) {
	disk, err := p.space.usage()
	if err != nil {
		log.Printf("portal: read disk usage: %v", err)
		writeCoded(w, http.StatusServiceUnavailable, codeStorageUnavailable, "storage information is not available")
		return
	}
	files, closeFiles, ok := p.openUserFiles(w, user)
	if !ok {
		return
	}
	defer closeFiles()

	quota, hasQuota := p.space.quota(user)
	writeJSON(w, http.StatusOK, newStorageView(disk, quota, hasQuota, trashBytes(files)))
}

// The family folder is in nobody's quota, so its bar always measures the whole disk.
func (p *Portal) familyStorage(w http.ResponseWriter, _ *http.Request, user string) {
	disk, err := p.space.usage()
	if err != nil {
		log.Printf("portal: read disk usage: %v", err)
		writeCoded(w, http.StatusServiceUnavailable, codeStorageUnavailable, "storage information is not available")
		return
	}
	files, closeFiles, ok := p.openFamilyFiles(w, user)
	if !ok {
		return
	}
	defer closeFiles()

	writeJSON(w, http.StatusOK, newStorageView(disk, system.Quota{}, false, trashBytes(files)))
}

func trashBytes(files system.Files) uint64 {
	items, err := files.ListTrash()
	if err != nil {
		log.Printf("portal: read trash size: %v", logCause(err))
		return 0
	}
	var total uint64
	for _, item := range items {
		if item.Size > 0 && total <= math.MaxUint64-uint64(item.Size) {
			total += uint64(item.Size)
		}
	}

	return total
}

type diskView struct {
	Used    uint64              `json:"used"`
	Total   uint64              `json:"total"`
	Free    uint64              `json:"free"`
	Percent float64             `json:"percent"`
	Level   system.StorageLevel `json:"level"`
}

func (s *Server) storage(w http.ResponseWriter, _ *http.Request) {
	disk, err := s.space.usage()
	if err != nil {
		log.Printf("read disk usage: %v", err)
		writeCoded(w, http.StatusServiceUnavailable, codeStorageUnavailable, "storage information is not available")
		return
	}
	writeJSON(w, http.StatusOK, diskView{Used: disk.Used, Total: disk.Total, Free: disk.Free, Percent: percentOf(disk.Used, disk.Total), Level: disk.Level()})
}

// A full disk refuses new uploads only. Reads, deletes and the trash keep working so space can be freed.
func (a fileHandlers) refuseWhenFull(w http.ResponseWriter) bool {
	disk, err := a.space.usage()
	if err != nil {
		if !errors.Is(err, system.ErrNoDisk) {
			log.Printf("upload: read disk usage: %v", err)
		}
		return false
	}
	if !disk.Full() {
		return false
	}
	writeJSON(w, http.StatusInsufficientStorage, map[string]any{"error": diskFullMessage, "code": codeDiskFull, "diskFull": true})

	return true
}
