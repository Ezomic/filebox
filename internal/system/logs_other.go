//go:build !linux

package system

func NewLogs() Logs {
	return NewFakeLogs()
}
