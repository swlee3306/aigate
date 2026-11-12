//go:build darwin

package fsx

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// RenameNoReplace: see rename_linux.go (RENAME_EXCL on macOS).
func RenameNoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		if _, e := os.Lstat(to); e == nil {
			return os.ErrExist
		}
		return os.Rename(from, to)
	}
	return err
}
