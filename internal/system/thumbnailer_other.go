//go:build !linux

package system

func NewThumbnailer() Thumbnailer {
	return NewFakeThumbnailer()
}
