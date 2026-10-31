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

// SweepAllUploads clears stale partial uploads from the admin root and from every user's own files folder.
func SweepAllUploads(base string, now time.Time, maxAge time.Duration) (int, error) {
	cutoff := now.Add(-maxAge)
	roots := []string{base}
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
		count, err := sweepRoot(root, cutoff)
		swept += count
		if err != nil {
			errs = append(errs, err)
		}
	}

	return swept, errors.Join(errs...)
}

func sweepRoot(root string, cutoff time.Time) (int, error) {
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return 0, nil
	}
	files, err := NewDirFiles(root)
	if err != nil {
		return 0, err
	}
	defer files.(*dirFiles).Close()

	return files.SweepUploads(cutoff)
}

// RunUploadSweeper sweeps once straight away and then every interval until ctx ends.
func RunUploadSweeper(ctx context.Context, base string, interval time.Duration, now func() time.Time) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if swept, err := SweepAllUploads(base, now(), UploadMaxAge); err != nil {
			log.Printf("sweep partial uploads: %v", err)
		} else if swept > 0 {
			log.Printf("swept %d stale partial uploads", swept)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
