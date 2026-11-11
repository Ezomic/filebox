//go:build linux

package system

func UserFilesOpener(base string) func(user string) (Files, error) {
	return func(user string) (Files, error) { return OpenUserFiles(base, user) }
}

func FamilyFilesOpener(base string) func() (Files, error) {
	return func() (Files, error) { return OpenFamilyFiles(base) }
}

func UserVersionsOpener(base string) func(user string) (Versions, error) {
	return func(user string) (Versions, error) { return OpenUserVersions(base, user) }
}

func FamilyVersionsOpener(base string) func() (Versions, error) {
	return func() (Versions, error) { return OpenFamilyVersions(base), nil }
}
