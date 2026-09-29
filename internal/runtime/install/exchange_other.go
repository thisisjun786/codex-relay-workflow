//go:build !linux

package install

// exchange has no atomic two-name swap to use here, so a caller renames instead.
func exchange(string, string) error { return errNoExchange }
