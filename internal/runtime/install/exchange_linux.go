package install

import (
	"errors"

	"golang.org/x/sys/unix"
)

// exchange swaps the files at a and b in one step (renameat2 RENAME_EXCHANGE). errNoExchange
// is the answer of a filesystem that cannot.
func exchange(a, b string) error {
	err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
		return errNoExchange
	}
	return err
}
