//go:build !linux

package store

import "errors"

// Without /proc/self/fd a descriptor cannot be bound to its path, so every artifact read is
// refused, as scope.py AuthorizedFile.__enter__ refuses it.
func pinnedOpen(declared string) (int, error) {
	return -1, refuse(ReasonUnverifiablePathBinding, "/proc/self/fd is unavailable, so %q cannot be bound to its path", declared)
}

func descriptorPath(int) (string, error) {
	return "", refuse(ReasonUnverifiablePathBinding, "/proc/self/fd is unavailable")
}

func acquireReadLease(int) (bool, string) { return false, "F_SETLEASE unavailable on this platform" }

func leaseStillHeld(int) bool { return false }

func releaseLease(int) {}

type statSnapshot struct{}

func snapshotOf(int) (statSnapshot, uint32, error) {
	return statSnapshot{}, 0, errors.New("fstat is not used on this platform")
}
