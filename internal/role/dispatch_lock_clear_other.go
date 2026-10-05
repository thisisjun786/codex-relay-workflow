//go:build !linux && !darwin

package role

import "errors"

// dispatchProcessStart has no source on this platform: an owner is recorded without a start time, and a lock whose
// pid still exists cannot be told from one that a later process of that pid holds, so dispatch-lock-clear refuses it.
func dispatchProcessStart(int) (string, error) {
	return "", errors.New("process start time is not available on this platform")
}
