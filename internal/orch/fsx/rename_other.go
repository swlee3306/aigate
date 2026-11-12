//go:build !linux && !darwin

package fsx

import "os"

// RenameNoReplace: Lstat check then rename (no atomic no-replace rename here).
func RenameNoReplace(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return os.ErrExist
	}
	return os.Rename(from, to)
}
