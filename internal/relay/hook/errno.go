package hook

import (
	"errors"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func prescanErrno(err error) bool {
	var number syscall.Errno
	if !errors.As(err, &number) {
		return false
	}
	switch number {
	case syscall.ENOENT, syscall.ECONNREFUSED, syscall.EACCES, syscall.EAGAIN, syscall.ENOTDIR:
		return true
	}
	return false
}

// Python's errno.errorcode chooses these aliases when two names share a value;
// x/sys provides the OS-specific table, but chooses EWOULDBLOCK on some targets.
func pythonErrnoName(e syscall.Errno) string {
	switch e {
	case syscall.EAGAIN:
		return "EAGAIN"
	case syscall.EDEADLK:
		if runtime.GOOS == "linux" {
			return "EDEADLOCK"
		}
	case syscall.EOPNOTSUPP:
		return "ENOTSUP"
	}
	name := unix.ErrnoName(e)
	// Linux's newer filesystem aliases postdate Python's errno table.
	if runtime.GOOS == "linux" {
		switch name {
		case "EFSCORRUPTED":
			return "EUCLEAN"
		case "EFSBADCRC":
			return "EBADMSG"
		}
	}
	return name
}
