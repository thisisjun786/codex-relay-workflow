//go:build linux

package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

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

// afterPinnedComponent is a test seam (nil in production): it runs after each directory component of the walk was opened, with the path walked so far.
var afterPinnedComponent func(path string)

var errPinnedAbsent = errors.New("a component of the path is not there")

// UnlinkPinned removes a regular file from <root>/<directory> without resolving an ancestor twice, in a tree a child may be changing. root is an absolute, normalized path; directory and name are single
// path components. Every component of root and then directory is opened from "/" with openat(O_NOFOLLOW|O_DIRECTORY), each relative to the descriptor of the one before (the walk pinnedOpen makes for an artifact
// read), so a link in any component, or a component swapped after it was opened, cannot redirect it. The file is opened relative to the directory's descriptor with O_NOFOLLOW|O_NONBLOCK (a FIFO opens
// at once and is refused), must be a regular file, and at most limit bytes of it are read and given to decide. When decide says yes, the name is checked against the file that was read (same device and
// inode) and unlinked with unlinkat on the directory's descriptor. A root, directory or file that is not there is PinnedAbsent and not an error. A link, a file that is not regular, a file over the limit and a name
// that changed are refusals or errors and nothing is removed. The file name itself is looked up three times (the open, the identity check and the unlink), so it is covered by the inode comparison and not by
// the descriptor; a mount point on the path is not stopped by O_NOFOLLOW.
func UnlinkPinned(root, directory, name string, limit int, decide func(raw []byte) (bool, error)) (PinnedRemoval, error) {
	declared, err := NormalizeDeclaredPath(root)
	if err != nil {
		return PinnedAbsent, err
	}
	for _, component := range []string{directory, name} {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "/\x00") {
			return PinnedAbsent, refuse(ReasonScopeEscape, "%s is not a single path component", pyvalue.StrRepr(component))
		}
	}
	components := strings.FieldsFunc(declared, func(r rune) bool { return r == '/' })
	components = append(components, directory)
	dirfd, err := openPinnedDirectory(components, declared)
	if errors.Is(err, errPinnedAbsent) {
		return PinnedAbsent, nil
	}
	if err != nil {
		return PinnedAbsent, err
	}
	defer func() { _ = syscall.Close(dirfd) }()
	fd, err := syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, syscall.ENOENT):
		return PinnedAbsent, nil
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENXIO):
		return PinnedAbsent, refuse(ReasonNotARegularFile, "%s is a link or a special file, not a regular file", pyvalue.StrRepr(name))
	case err != nil:
		return PinnedAbsent, fmt.Errorf("open %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	var read unix.Stat_t
	if err := unix.Fstat(fd, &read); err != nil {
		return PinnedAbsent, fmt.Errorf("stat %s: %w", name, err)
	}
	if read.Mode&unix.S_IFMT != unix.S_IFREG {
		return PinnedAbsent, refuse(ReasonNotARegularFile, "%s is not a regular file", pyvalue.StrRepr(name))
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return PinnedAbsent, fmt.Errorf("read %s: %w", name, err)
	}
	if len(raw) > limit {
		return PinnedAbsent, ErrPinnedTooLarge
	}
	remove, err := decide(raw)
	if err != nil {
		return PinnedKept, err
	}
	if !remove {
		return PinnedKept, nil
	}
	var now unix.Stat_t
	switch err := unix.Fstatat(dirfd, name, &now, unix.AT_SYMLINK_NOFOLLOW); {
	case errors.Is(err, unix.ENOENT):
		return PinnedAbsent, nil
	case err != nil:
		return PinnedKept, fmt.Errorf("stat %s: %w", name, err)
	case uint64(now.Dev) != uint64(read.Dev) || uint64(now.Ino) != uint64(read.Ino):
		return PinnedKept, ErrPinnedChanged
	}
	if err := unix.Unlinkat(dirfd, name, 0); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return PinnedAbsent, nil
		}
		return PinnedKept, fmt.Errorf("remove %s: %w", name, err)
	}
	return PinnedRemoved, nil
}

// openPinnedDirectory opens components from "/" one at a time, each with O_NOFOLLOW|O_DIRECTORY relative to the descriptor before it, and returns the descriptor of the last. A component that is not there is
// errPinnedAbsent; a link or a component that is not a directory is a symlink_component refusal.
func openPinnedDirectory(components []string, declared string) (int, error) {
	current, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, refuse(ReasonScopeEscape, "cannot open '/': %v", err)
	}
	walked := ""
	for _, component := range components {
		walked += "/" + component
		next, err := syscall.Openat(current, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(current)
		switch {
		case errors.Is(err, syscall.ENOENT):
			return -1, errPinnedAbsent
		case err != nil:
			return -1, walkError(err, component, declared)
		}
		current = next
		if afterPinnedComponent != nil {
			afterPinnedComponent(walked)
		}
	}
	return current, nil
}
