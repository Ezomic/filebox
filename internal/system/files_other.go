//go:build !linux

package system

func NewFiles(string) (Files, error) {
	return NewSampleFiles(), nil
}
