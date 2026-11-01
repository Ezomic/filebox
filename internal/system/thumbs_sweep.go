package system

import (
	"context"
	"time"
)

// SweepAllThumbs deletes thumbnails whose photo is gone, from the data root and from every user's own files folder.
func SweepAllThumbs(base string, now time.Time) (int, error) {
	return sweepEveryRoot(base, func(files Files) (int, error) { return files.SweepThumbs(now) })
}

// RunThumbSweeper sweeps once straight away and then every interval until ctx ends.
func RunThumbSweeper(ctx context.Context, base string, interval time.Duration, now func() time.Time) {
	runSweeper(ctx, interval, "orphaned thumbnails", func() (int, error) { return SweepAllThumbs(base, now()) })
}
