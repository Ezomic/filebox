//go:build linux

package system

import (
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type linuxHost struct {
	diskPath string
	cpu      *cpuMeter
}

func NewHost(diskPath string) Host {
	return &linuxHost{diskPath: diskPath, cpu: &cpuMeter{read: readCPUSample, now: time.Now, sleep: time.Sleep}}
}

func (h *linuxHost) Metrics() (Metrics, error) {
	var metrics Metrics
	var err error

	if metrics.CPUPercent, err = h.cpu.Percent(); err != nil {
		return Metrics{}, err
	}

	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Metrics{}, err
	}
	if metrics.MemTotal, metrics.MemUsed, err = parseMeminfo(string(meminfo)); err != nil {
		return Metrics{}, err
	}

	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return Metrics{}, err
	}
	if metrics.UptimeSeconds, err = parseUptime(string(uptime)); err != nil {
		return Metrics{}, err
	}

	var fs syscall.Statfs_t
	if err = syscall.Statfs(h.diskPath, &fs); err != nil {
		return Metrics{}, err
	}
	metrics.DiskTotal = fs.Blocks * uint64(fs.Bsize)
	metrics.DiskUsed = (fs.Blocks - fs.Bfree) * uint64(fs.Bsize)

	metrics.TempCelsius = hottestZone()

	return metrics, nil
}

func readCPUSample() (cpuSample, error) {
	content, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}

	return parseCPUStat(string(content))
}

func hottestZone() *float64 {
	paths, _ := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")

	var hottest *float64
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		celsius, err := parseMillidegrees(string(content))
		if err != nil {
			continue
		}
		if hottest == nil || celsius > *hottest {
			hottest = &celsius
		}
	}

	return hottest
}
