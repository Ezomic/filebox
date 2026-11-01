//go:build !linux

package system

func NewDiskSpace(_ string) DiskSpace {
	return NewFakeDisk(DiskUsage{Total: 500 << 30, Used: 120 << 30, Free: 380 << 30})
}
