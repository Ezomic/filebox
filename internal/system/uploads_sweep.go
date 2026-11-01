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

func sweepEveryRoot(base string, sweep func(Files) (int, error)) (int, error) {
	roots := []string{base, filepath.Join(base, FamilyDir)}
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if entry.IsDir() && ValidUsername(entry.Name()) {
			roots = append(roots, filepath.Join(base, entry.Name(), "files"))
		}
	}

	swept := 0
	var errs []error
	for _, root := range roots {
		count, err := sweepRoot(root, sweep)
		swept += count
		if err != nil {
			errs = append(errs, err)
		}
	}

	return swept, errors.Join(errs...)
}

func sweepRoot(root string, sweep func(Files) (int, error)) (int, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return 0, nil
	}
	files, err := NewDirFiles(root)
	if err != nil {
		return 0, err
	}
	defer files.(*dirFiles).Close()

	return sweep(files)
}

// RunUploadSweeper sweeps once straight away and then every interval until ctx ends.
func RunUploadSweeper(ctx context.Context, base string, interval time.Duration, now func() time.Time) {
	runSweeper(ctx, interval, "partial uploads", func() (int, error) { return SweepAllUploads(base, now(), UploadMaxAge) })
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
