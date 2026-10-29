//go:build linux

package system

func UserFilesOpener(base string) func(user string) (Files, error) {
	return func(user string) (Files, error) { return OpenUserFiles(base, user) }
}
