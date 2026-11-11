//go:build !linux && !darwin

package clone

import "os"

// renameNoReplace: Lstat check then rename (no atomic no-replace rename here).
func renameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return os.ErrExist
	}
	return os.Rename(from, to)
}
