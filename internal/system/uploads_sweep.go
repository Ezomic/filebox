package system

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"
)

const UploadMaxAge = 7 * 24 * time.Hour

// SweepAllUploads clears stale partial uploads from the data root, the family folder and every user's own files folder.
func SweepAllUploads(base string, now time.Time, maxAge time.Duration) (int, error) {
	cutoff := now.Add(-maxAge)

	return sweepEveryRoot(base, func(files Files) (int, error) { return files.SweepUploads(cutoff) })
}

// The earlier versions are never a root of their own (".snapshots" is no username). The data root is swept as a root,
// and its skip keeps the thumbnail sweep's photo walk out of them; the trash and upload sweeps only read their own
// hidden folder at the top of each root, so they never reach them either.
func sweepEveryRoot(base string, sweep func(Files) (int, error)) (int, error) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, err
	}
	swept, err := sweepRoot(base, SnapshotsDir, sweep)
	errs := []error{err}
	roots := []string{filepath.Join(base, FamilyDir)}
	for _, entry := range entries {
		if entry.IsDir() && ValidUsername(entry.Name()) {
			roots = append(roots, filepath.Join(base, entry.Name(), "files"))
		}
	}
	for _, root := range roots {
		count, err := sweepRoot(root, "", sweep)
		swept += count
		errs = append(errs, err)
	}

	return swept, errors.Join(errs...)
}

func sweepRoot(dir, skip string, sweep func(Files) (int, error)) (int, error) {
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return 0, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	files := &dirFiles{root: root, skip: skip}
	defer files.Close()

	return sweep(files)
}

// RunUploadSweeper sweeps once straight away and then every interval until ctx ends.
func RunUploadSweeper(ctx context.Context, base string, interval time.Duration, now func() time.Time) {
	runSweeper(ctx, interval, "partial uploads", func() (int, error) {
		at := now()
		swept, err := SweepAllUploads(base, at, UploadMaxAge)
		visitors, visitorErr := SweepAllVisitorUploads(base, at)

		return swept + visitors, errors.Join(err, visitorErr)
	})
}

// VisitorUploadMaxAge is how long an unfinished upload through an upload link may sit idle.
const VisitorUploadMaxAge = 24 * time.Hour

func SweepAllVisitorUploads(base string, now time.Time) (int, error) {
	cutoff := now.Add(-VisitorUploadMaxAge)

	return sweepEveryRoot(base, func(files Files) (int, error) { return files.SweepVisitorUploads(cutoff) })
}

func runSweeper(ctx context.Context, interval time.Duration, what string, sweep func() (int, error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if swept, err := sweep(); err != nil {
			log.Printf("sweep %s: %v", what, err)
		} else if swept > 0 {
			log.Printf("swept %d stale %s", swept, what)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
