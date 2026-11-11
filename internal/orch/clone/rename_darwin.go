//go:build darwin

package clone

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace: see rename_linux.go (RENAME_EXCL on macOS).
func renameNoReplace(from, to string) error {
	err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EINVAL) {
		if _, e := os.Lstat(to); e == nil {
			return os.ErrExist
		}
		return os.Rename(from, to)
	}
	return err
}
