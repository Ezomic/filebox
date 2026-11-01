package system

import "path/filepath"

// FamilyDir is the folder every portal user shares. It sits next to the homes at the data root, and the leading
// underscore keeps it from ever being a valid username.
const FamilyDir = "_family"

// OpenUserFiles confines a Files to one SFTP user's own files folder. The name is checked
// again here so a bad value can never turn into a path outside that user's home.
func OpenUserFiles(base, user string) (Files, error) {
	if !ValidUsername(user) {
		return nil, ErrInvalidUsername
	}

	return NewDirFiles(filepath.Join(base, user, "files"))
}

// OpenFamilyFiles confines a Files to the shared family folder. It never creates the folder: setup.sh does, because
// the panel cannot write to the data root.
func OpenFamilyFiles(base string) (Files, error) {
	return NewDirFiles(filepath.Join(base, FamilyDir))
}
