//go:build linux

package system

func UserFilesOpener(base string) func(user string) (Files, error) {
	return func(user string) (Files, error) { return OpenUserFiles(base, user) }
}

func FamilyFilesOpener(base string) func() (Files, error) {
	return func() (Files, error) { return OpenFamilyFiles(base) }
}
