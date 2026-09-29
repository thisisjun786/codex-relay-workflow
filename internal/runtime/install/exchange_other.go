//go:build !linux && !darwin

package install

// exchange has no atomic two-name swap here, so a caller renames nothing over the active path.
func exchange(string, string) error { return errNoExchange }
