//go:build unix && !linux && !darwin

package hookswitch

import (
	"errors"
	"syscall"
)

func exchangeEntries(a, b string) error {
	return errors.Join(errors.New("this platform has no atomic swap of two entries"), syscall.ENOTSUP)
}
