//go:build linux

package fsx

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// RenameNoReplace moves from to to and fails with an error matching
// os.ErrExist if anything (a symlink included) is at to. Where the file system
// lacks RENAME_NOREPLACE it falls back to an Lstat check before the rename.
func RenameNoReplace(from, to string) error {
	err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return renameChecked(from, to)
	}
	return err
}

func renameChecked(from, to string) error {
	if _, err := os.Lstat(to); err == nil {
		return os.ErrExist
	}
	return os.Rename(from, to)
}
