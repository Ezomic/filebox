package system

import "path/filepath"

// OpenUserFiles confines a Files to one SFTP user's own files folder. The name is checked
// again here so a bad value can never turn into a path outside that user's home.
func OpenUserFiles(base, user string) (Files, error) {
	if !ValidUsername(user) {
		return nil, ErrInvalidUsername
	}

	return NewDirFiles(filepath.Join(base, user, "files"))
}
