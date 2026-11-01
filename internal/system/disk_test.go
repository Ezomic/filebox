package system

import (
	"math"
	"testing"
)

func TestLevelsAtTheExactThresholds(t *testing.T) {
	const total = 10000
	cases := []struct {
		used uint64
		want StorageLevel
	}{
		{0, LevelOK},
		{8499, LevelOK},
		{8500, LevelWarning},
		{9499, LevelWarning},
		{9500, LevelCritical},
		{9799, LevelCritical},
		{9800, LevelFull},
		{10000, LevelFull},
	}
	for _, c := range cases {
		disk := DiskUsage{Total: total, Used: c.used, Free: total - c.used}
		if got := disk.Level(); got != c.want {
			t.Errorf("used %d of %d: got %s, want %s", c.used, total, got, c.want)
		}
		if full := disk.Full(); full != (c.want == LevelFull) {
			t.Errorf("used %d: Full() = %v", c.used, full)
		}
	}
}

func TestLevelOfNeverReturnsFull(t *testing.T) {
	if got := LevelOf(100, 100); got != LevelCritical {
		t.Fatalf("got %s", got)
	}
	if got := LevelOf(500, 100); got != LevelCritical {
		t.Fatalf("over the limit: got %s", got)
	}
}

func TestAZeroSizedDiskIsNeverFullOrWarned(t *testing.T) {
	disk := DiskUsage{}
	if disk.Full() || disk.Level() != LevelOK || LevelOf(5, 0) != LevelOK {
		t.Fatal("a disk with no size must read as ok")
	}
}

func TestThresholdMathDoesNotOverflowOnHugeDisks(t *testing.T) {
	disk := DiskUsage{Total: math.MaxUint64, Used: math.MaxUint64 - 1}
	if !disk.Full() {
		t.Fatal("a nearly full huge disk must be full")
	}
	if (DiskUsage{Total: math.MaxUint64, Used: math.MaxUint64 / 2}).Level() != LevelOK {
		t.Fatal("a half full huge disk must be ok")
	}
}

func TestNormalizeClampsOddFilesystemAnswers(t *testing.T) {
	cases := []struct{ in, want DiskUsage }{
		{DiskUsage{Total: 100, Used: 40, Free: 500}, DiskUsage{Total: 100, Used: 40, Free: 60}},
		{DiskUsage{Total: 100, Used: 400, Free: 5}, DiskUsage{Total: 100, Used: 100, Free: 0}},
		{DiskUsage{Total: 0, Used: 4, Free: 5}, DiskUsage{}},
		{DiskUsage{Total: 100, Used: 40, Free: 10}, DiskUsage{Total: 100, Used: 40, Free: 10}},
	}
	for _, c := range cases {
		if got := c.in.Normalize(); got != c.want {
			t.Errorf("%+v: got %+v, want %+v", c.in, got, c.want)
		}
	}
}
