//go:build linux

package system

func NewFiles(root string) (Files, error) {
	return NewDirFiles(root)
}
