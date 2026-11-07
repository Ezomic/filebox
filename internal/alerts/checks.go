package alerts

import (
	"errors"
	"fmt"
	"log"
	"math/bits"
	"strings"
	"time"

	"github.com/Ezomic/filebox/internal/system"
)

const (
	// A disk alert opens at system.CriticalPercent and only ends below this, so a disk that the hourly snapshot
	// pruning keeps just under 95% does not mail every hour.
	diskEasePercent = 90
	certWarning     = 14 * 24 * time.Hour
	pruneWindow     = 24 * time.Hour
	restartWindow   = 24 * time.Hour
	keepNewest      = 7
)

// What each check looks at, as the dashboard and an alert say it: "Could not check the SMART disk health".
const (
	diskName      = "how full the data disk is"
	smartName     = "the SMART disk health"
	btrfsName     = "the data disk and its mirror"
	snapshotsName = "the earlier versions"
	certName      = "the portal certificate"
	restartName   = "how filebox last shut down"
)

// DiskCheck watches how full the data disk is.
func DiskCheck(space system.DiskSpace) Check {
	return func(time.Time) Result {
		usage, err := space.Usage()
		if err != nil {
			log.Printf("alerts: read disk usage: %v", err)
			return Result{Source: "disk", Name: diskName}
		}

		return Result{Source: "disk", Name: diskName, Known: true, Problems: diskProblems(usage.Normalize())}
	}
}

func diskProblems(usage system.DiskUsage) []Problem {
	percent := wholePercent(usage.Used, usage.Total)
	level := usage.Level()
	critical := level == system.LevelCritical || level == system.LevelFull
	if !critical && percent < diskEasePercent {
		return nil
	}

	return []Problem{{
		Key:     "disk-full",
		Subject: fmt.Sprintf("The data disk is %d%% full", percent),
		Detail: fmt.Sprintf("The data disk is %d%% full, with %s free of %s. At %d%% the web portal stops accepting uploads. "+
			"Delete files or empty the Trash, or add a disk.", percent, size(usage.Free), size(usage.Total), system.FullPercent),
		Cleared: fmt.Sprintf("The data disk is below %d%% full again.", diskEasePercent),
		Easing:  !critical,
	}}
}

// wholePercent rounds down without float error, so it never says 94% of a disk that system.LevelOf calls 95% full.
func wholePercent(used, total uint64) uint64 {
	if total == 0 {
		return 0
	}
	hi, lo := bits.Mul64(min(used, total), 100)
	percent, _ := bits.Div64(hi, lo, total)

	return percent
}

// SmartCheck watches what the disks say about themselves: failing, or more reallocated sectors than before.
func SmartCheck(smart system.Smart, counters *Counters, failing *FailingDisks) Check {
	return func(now time.Time) Result {
		disks, err := smart.Disks()
		if err != nil {
			log.Printf("alerts: read disk health: %v", err)
			return Result{Source: "smart", Name: smartName}
		}
		var problems []Problem
		listed := map[string]bool{}
		for _, disk := range disks {
			listed[diskID(disk)] = true
			problems = append(problems, failingProblems(disk, failing, now)...)
			problems = append(problems, reallocatedProblems(disk, counters, now)...)
		}
		problems = append(problems, vanishedProblems(listed, failing, now)...)

		return Result{Source: "smart", Name: smartName, Known: true, Problems: problems}
	}
}

// The serial number follows a disk when its device name changes between boots.
func diskID(disk system.DiskHealth) string {
	if disk.Serial != "" {
		return disk.Serial
	}

	return disk.Device
}

func describe(disk system.DiskHealth) string {
	var about []string
	if disk.Model != "" {
		about = append(about, disk.Model)
	}
	if disk.Serial != "" {
		about = append(about, "serial "+disk.Serial)
	}
	if len(about) == 0 {
		return disk.Device
	}

	return disk.Device + " (" + strings.Join(about, ", ") + ")"
}

// A disk that is asleep keeps an open alert open, and counts as seen.
func failingProblems(disk system.DiskHealth, failing *FailingDisks, now time.Time) []Problem {
	id := diskID(disk)
	key := "smart-failing:" + id
	switch {
	case disk.Healthy == nil:
		failing.answered(id, now)
		return []Problem{{Key: key, Easing: true}}
	case *disk.Healthy:
		failing.forget(id)
		return nil
	}
	failing.saw(id, FailingDisk{Device: disk.Device, Model: disk.Model, Serial: disk.Serial, LastSeen: now})

	return []Problem{{
		Key:     key,
		Subject: "Disk " + disk.Device + " is failing",
		Detail:  "SMART on " + describe(disk) + " says the disk is failing. Make sure the files on it are backed up and replace it soon.",
		Cleared: "SMART no longer reports " + disk.Device + " as failing.",
	}}
}

// A failing disk that drops off the list keeps its alert, because a dead disk is no better than a failing one. After
// GoneDiskAfter it is taken as replaced.
func vanishedProblems(listed map[string]bool, failing *FailingDisks, now time.Time) []Problem {
	var problems []Problem
	for id, disk := range failing.all() {
		if listed[id] {
			continue
		}
		if now.Sub(disk.LastSeen) >= GoneDiskAfter {
			failing.forget(id)
			continue
		}
		about := system.DiskHealth{Device: disk.Device, Model: disk.Model, Serial: disk.Serial}
		problems = append(problems, Problem{
			Key:     "smart-failing:" + id,
			Subject: "Disk " + disk.Device + " is failing",
			Detail: "SMART said " + describe(about) + " is failing, and the disk no longer answers since " + when(disk.LastSeen) +
				". It may have died or come loose. Make sure the files on it are backed up and replace it soon.",
			Cleared: "Disk " + disk.Device + " has not been seen for 7 days, so if it was replaced there is nothing more to do.",
		})
	}

	return problems
}

func reallocatedProblems(disk system.DiskHealth, counters *Counters, now time.Time) []Problem {
	key := "reallocated:" + diskID(disk)
	var counter Counter
	if disk.Reallocated != nil {
		counter = counters.Observe(key, *disk.Reallocated, now)
	} else if stored, found := counters.Get(key); found {
		counter = stored
	}
	if !counter.Rising(now) {
		return nil
	}

	return []Problem{{
		Key:     key,
		Subject: "Disk " + disk.Device + " has more reallocated sectors",
		Detail: fmt.Sprintf("The reallocated sector count of %s went from %d to %d, last on %s. The disk is setting worn-out spots "+
			"aside; when that keeps happening it is wearing out. Make sure the files on it are backed up and plan to replace it.",
			describe(disk), counter.From, counter.Count, when(counter.RoseAt)),
		Cleared: "The reallocated sector count of " + disk.Device + " has not gone up for 7 days. A new rise starts a new alert.",
	}}
}

// MirrorCheck watches the btrfs data disk: whether it is mounted, disks missing from the mirror, read and write
// errors, and what the monthly scrub found.
func MirrorCheck(read func() (system.BtrfsHealth, error), counters *Counters) Check {
	return func(now time.Time) Result {
		health, err := read()
		if err != nil {
			log.Printf("alerts: read data disk health: %v", err)
			return Result{Source: "btrfs", Name: btrfsName}
		}

		return Result{Source: "btrfs", Name: btrfsName, Known: true, Problems: btrfsProblems(health, counters, now)}
	}
}

func btrfsProblems(health system.BtrfsHealth, counters *Counters, now time.Time) []Problem {
	if health.State == system.BtrfsOff {
		return nil
	}
	var problems []Problem
	if health.State == system.BtrfsUnmounted {
		problems = append(problems, Problem{
			Key:     "data-unmounted",
			Subject: "The data disk is not mounted",
			Detail: "/srv/sftp is set up as a btrfs data disk but is not mounted, so new files land on the system disk. With a disk " +
				"mirror this happens when one of its disks is missing; see \"If a mirror disk dies\" in the filebox guide.",
			Cleared: "The data disk is mounted again.",
		})
	}
	problems = append(problems, missingProblems(health.Devices)...)
	problems = append(problems, ioErrorProblems(health.Devices, counters, now)...)

	return append(problems, scrubProblems(health.Scrub)...)
}

// Disks that could not be listed keep an open alert open.
func missingProblems(devices []system.BtrfsDevice) []Problem {
	const key = "mirror-missing"
	if devices == nil {
		return []Problem{{Key: key, Easing: true}}
	}
	var missing []string
	for _, device := range devices {
		if device.Missing {
			missing = append(missing, fmt.Sprint(device.ID))
		}
	}
	if len(missing) == 0 {
		return nil
	}

	return []Problem{{
		Key:     key,
		Subject: "The disk mirror is missing a disk",
		Detail: fmt.Sprintf("The data disk mirror is running without btrfs device %s (%d of its %d disks), so there is no second "+
			"copy of the files. Replace the disk as described under \"If a mirror disk dies\" in the filebox guide.",
			strings.Join(missing, " and "), len(missing), len(devices)),
		Cleared: "The disk mirror has all its disks again.",
	}}
}

// All disks share one counter: a replaced disk lowers the total and so becomes the new baseline.
func ioErrorProblems(devices []system.BtrfsDevice, counters *Counters, now time.Time) []Problem {
	const key = "io-errors"
	var counter Counter
	var perDevice []string
	if devices != nil {
		var total uint64
		for _, device := range devices {
			total += device.Errors
			perDevice = append(perDevice, fmt.Sprintf("device %d: %d", device.ID, device.Errors))
		}
		counter = counters.Observe(key, total, now)
	} else if stored, found := counters.Get(key); found {
		counter = stored
	}
	if !counter.Rising(now) {
		return nil
	}
	detail := fmt.Sprintf("btrfs counted %d new read, write or flush errors on the data disk, the last ones on %s, %d in all",
		counter.Count-counter.From, when(counter.RoseAt), counter.Count)
	if len(perDevice) > 0 {
		detail += " (" + strings.Join(perDevice, ", ") + ")"
	}

	return []Problem{{
		Key:     key,
		Subject: "Read or write errors on the data disk",
		Detail: detail + ". A disk that keeps failing to read or write is failing or losing its connection; in a mirror it is as " +
			"good as missing. Check the disk health on the dashboard and run: sudo btrfs device stats /srv/sftp",
		Cleared: "No new read or write errors on the data disk for 7 days.",
	}}
}

func scrubProblems(scrub *system.BtrfsScrub) []Problem {
	const key = "scrub-errors"
	switch {
	case scrub == nil:
		return []Problem{{Key: key, Easing: true}}
	case scrub.Errors == 0:
		return nil
	}
	started := "The last scrub"
	if !scrub.Started.IsZero() {
		started = "The scrub of " + when(scrub.Started)
	}

	return []Problem{{
		Key:     key,
		Subject: fmt.Sprintf("The disk check (scrub) found %s", plural(scrub.Errors, "error")),
		Detail: fmt.Sprintf("%s found %s on the data disk, and %d could not be repaired. An error that cannot be repaired is a "+
			"damaged file. Check the disk health on the dashboard; the kernel log has the details: sudo journalctl -k -g BTRFS",
			started, plural(scrub.Errors, "error"), scrub.Uncorrectable),
		Cleared: "The last scrub found no errors.",
	}}
}

// SnapshotCheck watches the earlier versions: a failed nightly run, and old ones deleted to make room on a full disk.
func SnapshotCheck(read func() (system.SnapshotStatus, error)) Check {
	return func(now time.Time) Result {
		if read == nil {
			return Result{Source: "snapshots", Name: snapshotsName, Known: true}
		}
		status, err := read()
		switch {
		case errors.Is(err, system.ErrNoSnapshotStatus):
			return Result{Source: "snapshots", Name: snapshotsName, Known: true}
		case err != nil:
			log.Printf("alerts: read snapshot status: %v", err)
			return Result{Source: "snapshots", Name: snapshotsName}
		}

		return Result{Source: "snapshots", Name: snapshotsName, Known: true, Problems: snapshotProblems(status, now)}
	}
}

func snapshotProblems(status system.SnapshotStatus, now time.Time) []Problem {
	if !status.Enabled {
		return nil
	}
	var problems []Problem
	var failed []string
	for _, folder := range status.Folders {
		if folder.Error != "" {
			failed = append(failed, folderLabel(folder)+" ("+folderError(folder.Error)+")")
		}
	}
	if len(failed) > 0 || (!status.Daily.IsZero() && !status.DailyOK) {
		detail := "The nightly snapshot run of " + when(status.Daily) + " failed."
		if len(failed) > 0 {
			detail = "The nightly snapshots failed for " + strings.Join(failed, ", ") + "."
		}
		problems = append(problems, Problem{
			Key:     "snapshots-failed",
			Subject: "Earlier versions (snapshots) failed",
			Detail:  detail + " See: sudo journalctl -u filebox-snapshot",
			Cleared: "The nightly snapshots work again.",
		})
	}
	if low := status.LowSpace; low != nil && now.Sub(low.At) < pruneWindow {
		problems = append(problems, prunedProblem(*low))
	}

	return problems
}

func folderLabel(folder system.SnapshotFolder) string {
	switch {
	case folder.Name == system.FamilyDir:
		return "Family"
	case folder.Deleted():
		name, _, _ := strings.Cut(folder.Name, ".deleted-")
		return "the deleted account " + name
	}

	return folder.Name
}

func folderError(code string) string {
	if code == "prune_failed" {
		return "old snapshots could not be deleted"
	}

	return "no new snapshot was taken"
}

// A run that found nothing left to delete keeps the alert open while the disk stays full, but does not start one.
func prunedProblem(low system.SnapshotLowSpace) Problem {
	detail := fmt.Sprintf("On %s the data disk was %d%% full, so filebox deleted the %s to make room.",
		when(low.At), system.CriticalPercent, plural(uint64(low.Deleted), "oldest earlier version"))
	if low.Deleted == 0 {
		detail = fmt.Sprintf("On %s the data disk was %d%% full, but no earlier versions were left to delete.", when(low.At), system.CriticalPercent)
	}
	if low.StillFull {
		detail += fmt.Sprintf(" Every folder is down to its newest %d and the disk is still full: delete files or add a disk.", keepNewest)
	}

	return Problem{
		Key:     "snapshots-pruned",
		Subject: "Earlier versions were deleted to free space",
		Detail:  detail,
		Cleared: "No earlier versions have been deleted to free space for 24 hours.",
		Easing:  low.Deleted == 0,
	}
}

// CertCheck watches the certificate of the public portal; expiry is nil when the portal is off.
func CertCheck(expiry func() time.Time) Check {
	return func(now time.Time) Result {
		if expiry == nil {
			return Result{Source: "cert", Name: certName, Known: true}
		}
		expires := expiry()
		left := expires.Sub(now)
		if left >= certWarning {
			return Result{Source: "cert", Name: certName, Known: true}
		}
		subject := "The portal certificate has expired"
		switch {
		case left >= 24*time.Hour:
			subject = "The portal certificate expires in " + plural(uint64(left/(24*time.Hour)), "day")
		case left > 0:
			subject = "The portal certificate expires within a day"
		}

		return Result{Source: "cert", Name: certName, Known: true, Problems: []Problem{{
			Key:     "portal-cert",
			Subject: subject,
			Detail: "The certificate of the web portal expires on " + when(expires) + ". It renews itself when 30 days are left, " +
				"so the renewal is failing. See: sudo journalctl -u filebox-portal-cert",
			Cleared: "The portal certificate has been renewed.",
		}}}
	}
}

// RestartCheck reports a run that began without the one before it shutting down cleanly, for a day after the start.
func RestartCheck(read func() (system.Boot, error)) Check {
	return func(now time.Time) Result {
		boot, err := read()
		if err != nil {
			log.Printf("alerts: read boot: %v", err)
			return Result{Source: "restart", Name: restartName}
		}
		if boot.CleanBefore == nil || *boot.CleanBefore || now.Sub(boot.Started) >= restartWindow {
			return Result{Source: "restart", Name: restartName, Known: true}
		}

		return Result{Source: "restart", Name: restartName, Known: true, Problems: []Problem{{
			Key:     "unclean-restart:" + boot.ID,
			Subject: "Filebox restarted without a clean shutdown",
			Detail: "Filebox started on " + when(boot.Started) + " after it stopped without shutting down, for example after a power " +
				"cut, a crash or the reset button. Check the disk health on the dashboard. To switch it off, use Shut down in the " +
				"admin panel or press the power button once.",
			Cleared: "A day has passed since the restart without a clean shutdown.",
		}}}
	}
}

func plural(count uint64, noun string) string {
	if count == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", count, noun)
}

func size(bytes uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value, unit := float64(bytes), 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", bytes)
	}

	return fmt.Sprintf("%.1f %s", value, units[unit])
}
