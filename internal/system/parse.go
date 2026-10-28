package system

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const guestFieldIndex = 8

type cpuSample struct {
	idle  uint64
	total uint64
}

func parseCPUStat(content string) (cpuSample, error) {
	line, _, _ := strings.Cut(content, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return cpuSample{}, errors.New("unexpected /proc/stat format")
	}

	var sample cpuSample
	for i, field := range fields[1:] {
		if i >= guestFieldIndex {
			break
		}
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return cpuSample{}, fmt.Errorf("parse /proc/stat: %w", err)
		}
		sample.total += value
		if i == 3 || i == 4 {
			sample.idle += value
		}
	}

	return sample, nil
}

func cpuPercent(previous, current cpuSample) float64 {
	totalDelta := current.total - previous.total
	if current.total < previous.total || totalDelta == 0 {
		return 0
	}
	idleDelta := current.idle - previous.idle
	if idleDelta > totalDelta {
		return 0
	}

	return float64(totalDelta-idleDelta) / float64(totalDelta) * 100
}

func parseMeminfo(content string) (total, used uint64, err error) {
	var available, free, buffers, cached uint64
	var haveTotal, haveAvailable bool

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		name, rest, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		kilobytes, parseErr := strconv.ParseUint(fields[0], 10, 64)
		if parseErr != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total, haveTotal = kilobytes*1024, true
		case "MemAvailable":
			available, haveAvailable = kilobytes*1024, true
		case "MemFree":
			free = kilobytes * 1024
		case "Buffers":
			buffers = kilobytes * 1024
		case "Cached":
			cached = kilobytes * 1024
		}
	}

	if !haveTotal {
		return 0, 0, errors.New("unexpected /proc/meminfo format")
	}
	if !haveAvailable {
		available = free + buffers + cached
	}

	return total, total - min(available, total), nil
}

func parseUptime(content string) (uint64, error) {
	field, _, _ := strings.Cut(strings.TrimSpace(content), " ")
	seconds, err := strconv.ParseFloat(field, 64)
	if err != nil || seconds < 0 {
		return 0, errors.New("unexpected /proc/uptime format")
	}

	return uint64(seconds), nil
}

func parseMillidegrees(content string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(content), 64)
	if err != nil {
		return 0, fmt.Errorf("parse temperature: %w", err)
	}

	return value / 1000, nil
}
