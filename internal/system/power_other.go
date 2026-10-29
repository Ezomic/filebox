//go:build !linux

package system

func NewPower() Power {
	return &FakePower{}
}
