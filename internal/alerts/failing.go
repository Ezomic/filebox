package alerts

import "time"

const (
	failingFile = "alert-failing-disks.json"
	// GoneDiskAfter is how long a failing disk may stay away before its alert ends: it was replaced, or it is dead and
	// the mirror and read error alerts take over.
	GoneDiskAfter = 7 * 24 * time.Hour
)

// FailingDisk is a disk SMART called failing, and when it was last seen.
type FailingDisk struct {
	Device   string    `json:"device"`
	Model    string    `json:"model"`
	Serial   string    `json:"serial"`
	LastSeen time.Time `json:"lastSeen"`
}

// FailingDisks remembers the failing disks across restarts, so one that stops answering keeps its alert instead of
// getting a "resolved" email.
type FailingDisks struct {
	store *keyed[FailingDisk]
}

func OpenFailingDisks(dir string) (*FailingDisks, error) {
	store, err := openKeyed[FailingDisk](dir, failingFile)
	if err != nil {
		return nil, err
	}

	return &FailingDisks{store: store}, nil
}

func (f *FailingDisks) saw(id string, disk FailingDisk) {
	f.store.change(id, func(FailingDisk, bool) (FailingDisk, bool) { return disk, true })
}

// answered keeps a failing disk that is asleep from looking gone.
func (f *FailingDisks) answered(id string, now time.Time) {
	f.store.change(id, func(prev FailingDisk, seen bool) (FailingDisk, bool) {
		prev.LastSeen = now
		return prev, seen
	})
}

func (f *FailingDisks) forget(id string) {
	f.store.change(id, func(FailingDisk, bool) (FailingDisk, bool) { return FailingDisk{}, false })
}

func (f *FailingDisks) all() map[string]FailingDisk {
	return f.store.all()
}
