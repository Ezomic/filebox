//go:build linux

package system

import "time"

func NewHealth(path string) *Health {
	return &Health{read: func() (HealthStatus, error) { return ReadHealthStatus(path) }, now: time.Now}
}
