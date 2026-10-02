//go:build !linux

package store

import "errors"

// PinnedRemoval says what UnlinkPinned did.
type PinnedRemoval int

const (
	// PinnedAbsent: the root, the directory or the file is not there, so there is nothing to remove.
	PinnedAbsent PinnedRemoval = iota
	// PinnedKept: decide answered that the file stays.
	PinnedKept
	// PinnedRemoved: the file was unlinked.
	PinnedRemoved
)

// ErrPinnedTooLarge is returned when the file is longer than the limit given to UnlinkPinned.
var ErrPinnedTooLarge = errors.New("the file is longer than the limit")

// ErrPinnedChanged is returned when the name no longer is the file that was read.
var ErrPinnedChanged = errors.New("the name no longer is the file that was read")

// UnlinkPinned needs /proc/self/fd and descriptor-relative opens, as every artifact read does: elsewhere it refuses.
func UnlinkPinned(root, directory, name string, limit int, decide func(raw []byte) (bool, error)) (PinnedRemoval, error) {
	return PinnedAbsent, refuse(ReasonUnverifiablePathBinding, "/proc/self/fd is unavailable, so %q cannot be removed without resolving its path twice", root)
}
