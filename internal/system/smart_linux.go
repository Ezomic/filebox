//go:build linux

package system

import "time"

// Must match the sudoers rule written by deploy/setup.sh.
var smartHelper = []string{"sudo", "-n", "/usr/local/sbin/filebox-smart"}

func NewSmart() Smart {
	return &cachedSmart{read: func() ([]DiskHealth, error) { return readSmartHelper(smartHelper) }, now: time.Now}
}
