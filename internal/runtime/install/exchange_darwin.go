//go:build darwin

package install

import (
	"errors"

	"golang.org/x/sys/unix"
)

// exchange swaps the files at a and b in one step (renamex_np RENAME_SWAP). errNoExchange is the
// answer of a filesystem that cannot.
func exchange(a, b string) error {
	err := unix.RenamexNp(a, b, unix.RENAME_SWAP)
	if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		return errNoExchange
	}
	return err
}
