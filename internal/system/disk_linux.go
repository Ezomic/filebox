//go:build linux

package system

import "syscall"

type statfsDisk struct{ path string }

func NewDiskSpace(path string) DiskSpace {
	return statfsDisk{path: path}
}

func (s statfsDisk) Usage() (DiskUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(s.path, &stat); err != nil {
		return DiskUsage{}, err
	}
	size := uint64(stat.Bsize)

	return DiskUsage{
		Total: stat.Blocks * size,
		Used:  (stat.Blocks - min(stat.Bfree, stat.Blocks)) * size,
		Free:  stat.Bavail * size,
	}.Normalize(), nil
}
