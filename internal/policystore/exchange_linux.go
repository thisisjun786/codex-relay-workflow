//go:build linux

package policystore

import "golang.org/x/sys/unix"

// exchangeFiles swaps the files at a and b atomically (renameat2 RENAME_EXCHANGE): afterwards a
// holds what b held and b holds what a held, with no window in which either path is missing. A
// filesystem without the exchange reports the error rather than falling back to a plain rename,
// which would destroy one of the two files.
func exchangeFiles(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
