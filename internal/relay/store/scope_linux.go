//go:build linux

package store

import (
	"errors"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// pinnedOpen opens every component from "/" with O_NOFOLLOW so no symlink can redirect the
// read (scope.py _pinned_open). O_NONBLOCK keeps a FIFO from blocking before fstat rejects it.
func pinnedOpen(declared string) (int, error) {
	components := strings.FieldsFunc(declared, func(r rune) bool { return r == '/' })
	if len(components) == 0 {
		return -1, refuse(ReasonNotARegularFile, "'/' is not an artifact")
	}
	directory, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, refuse(ReasonScopeEscape, "cannot open '/': %v", err)
	}
	defer func() { _ = syscall.Close(directory) }()
	for _, component := range components[:len(components)-1] {
		next, err := syscall.Openat(directory, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return -1, walkError(err, component, declared)
		}
		_ = syscall.Close(directory)
		directory = next
	}
	last := components[len(components)-1]
	fd, err := syscall.Openat(directory, last, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, walkError(err, last, declared)
	}
	return fd, nil
}

func walkError(err error, component, declared string) error {
	var errno syscall.Errno
	errors.As(err, &errno)
	switch errno {
	case syscall.ELOOP, syscall.ENOTDIR:
		return &RefusedError{Reason: ReasonSymlinkComponent, Detail: "component " + PythonRepr(component) + " of " + PythonRepr(declared) + " is a symlink or not a directory", cause: err}
	case syscall.ENOENT, syscall.ESTALE:
		return &RefusedError{Reason: ReasonPathChanged, Detail: "component " + PythonRepr(component) + " of " + PythonRepr(declared) + " disappeared during resolution", cause: err}
	default:
		return &RefusedError{Reason: ReasonScopeEscape, Detail: "cannot open component " + PythonRepr(component) + " of " + PythonRepr(declared) + ": " + unix.ErrnoName(errno), cause: err}
	}
}

func descriptorPath(fd int) (string, error) {
	actual, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", &RefusedError{Reason: ReasonUnverifiablePathBinding, Detail: "cannot read /proc/self/fd; path binding cannot be verified", cause: err}
	}
	return actual, nil
}

// acquireReadLease is per descriptor. F_GETLEASE identifies which held descriptor the kernel
// actually broke, so an unrelated SIGIO cannot invalidate another artifact read.
var leaseSignalOnce sync.Once

func acquireReadLease(fd int) (bool, string) {
	leaseSignalOnce.Do(func() {
		signals := make(chan os.Signal, 16)
		signal.Notify(signals, syscall.SIGIO)
		go func() {
			for range signals {
			}
		}()
	})
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETLEASE, unix.F_RDLCK); err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			return false, "a writable open already exists, so no lease could be taken"
		}
		var errno syscall.Errno
		errors.As(err, &errno)
		return false, "lease refused: " + unix.ErrnoName(errno)
	}
	return true, "read lease held for the whole read"
}

func leaseStillHeld(fd int) bool {
	lease, err := fcntl(fd, syscall.F_GETLEASE, 0)
	return err == nil && lease == syscall.F_RDLCK
}

func releaseLease(fd int) {
	_, _ = fcntl(fd, syscall.F_SETLEASE, syscall.F_UNLCK)
}

func fcntl(fd, command, argument int) (int, error) {
	value, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(command), uintptr(argument))
	if errno != 0 {
		return -1, errno
	}
	return int(value), nil
}

type statSnapshot struct {
	dev, ino, size, mtime, ctime int64
}

func snapshotOf(fd int) (statSnapshot, uint32, error) {
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil {
		return statSnapshot{}, 0, err
	}
	return statSnapshot{int64(info.Dev), int64(info.Ino), info.Size, info.Mtim.Nano(), info.Ctim.Nano()}, info.Mode, nil
}
