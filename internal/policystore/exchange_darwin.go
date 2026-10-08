//go:build darwin

package policystore

import "golang.org/x/sys/unix"

// exchangeFiles swaps the files at a and b atomically (renamex_np RENAME_SWAP). A filesystem without
// the exchange reports the error rather than falling back to a plain rename, which would destroy one
// of the two files.
func exchangeFiles(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}
