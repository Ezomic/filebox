package system

import (
	"context"
	"time"
)

// SweepAllTrash deletes trash items older than maxAge from the data root, the family folder and every user's own files folder.
func SweepAllTrash(base string, now time.Time, maxAge time.Duration) (int, error) {
	cutoff := now.Add(-maxAge)

	return sweepEveryRoot(base, func(files Files) (int, error) { return files.SweepTrash(cutoff) })
}

// RunTrashSweeper sweeps once straight away and then every interval until ctx ends.
func RunTrashSweeper(ctx context.Context, base string, interval time.Duration, now func() time.Time) {
	runSweeper(ctx, interval, "trash items", func() (int, error) { return SweepAllTrash(base, now(), TrashMaxAge) })
}
