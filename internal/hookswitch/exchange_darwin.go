//go:build darwin

package hookswitch

import "golang.org/x/sys/unix"

func exchangeEntries(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}
