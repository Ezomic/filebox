//go:build !linux

package system

func UserFilesOpener(string) func(user string) (Files, error) {
	files := NewSampleFiles()

	return func(string) (Files, error) { return files, nil }
}
