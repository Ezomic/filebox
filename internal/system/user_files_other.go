//go:build !linux

package system

import "time"

func UserFilesOpener(string) func(user string) (Files, error) {
	files := NewSampleFiles()

	return func(string) (Files, error) { return files, nil }
}

func FamilyFilesOpener(string) func() (Files, error) {
	files := NewSampleFiles()

	return func() (Files, error) { return files, nil }
}

func UserVersionsOpener(string) func(user string) (Versions, error) {
	versions := NewSampleVersions(time.Now())

	return func(string) (Versions, error) { return versions, nil }
}

func FamilyVersionsOpener(string) func() (Versions, error) {
	versions := NewSampleVersions(time.Now())

	return func() (Versions, error) { return versions, nil }
}
