//go:build !linux

package system

func NewSmart() Smart {
	return NewFakeSmart()
}
