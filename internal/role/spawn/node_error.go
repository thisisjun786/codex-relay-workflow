package spawn

import (
	"errors"
	"os"
	"syscall"
)

// This file ports the Node spelling of a managed dispatch failure. The oracle prints
// `managed dispatch: ${dispatchError.message}` (subagent-config/src/spawn-attach-hook.ts:892-907), and
// that message is whatever Node's fs layer raised: for a missing dispatch record it is
// `ENOENT: no such file or directory, lstat '<path>'`. The Go port raises the same failure as an
// *os.PathError, whose Error() is `lstat <path>: no such file or directory`, so the refusal text
// diverged. spawnParityNodeError spells the PathError the way Node's Error.message does.
//
// internal/pabcd/cli/node_error.go holds the same conversion for the plan and divergence libraries,
// but it is unexported there and this issue may edit internal/role/spawn only, so the conversion is
// copied here under this package's spawnParity prefix. Keep the two tables in step.

// spawnParityNodeError is the oracle's Error.message for err: a filesystem failure whose errno
// spawnParityErrno names becomes `<NAME>: <description>, <op> '<path>'`, with the path left out for
// write and close; anything else keeps err.Error().
//
// Only a direct *os.PathError is converted. errors.As would find one inside a wrapper, but the port
// wraps some failures with text of its own (a dispatch-lock-clear's "(a dispatch-lock-clear of this
// session is running)", a lock abandon's "lock ... left behind") that has no oracle counterpart;
// rewriting the whole message from the inner PathError would drop that text. The oracle-reachable
// failures (dispatchRead's os.Lstat, and the plain errors beside it) arrive here unconverted.
func spawnParityNodeError(err error) string {
	path, ok := err.(*os.PathError)
	if !ok {
		return err.Error()
	}
	var errno syscall.Errno
	if !errors.As(path.Err, &errno) {
		return err.Error()
	}
	name, desc := spawnParityErrno(errno)
	if name == "" {
		return err.Error()
	}
	output := name + ": " + desc + ", " + path.Op
	if path.Op != "write" && path.Op != "close" {
		output += " '" + path.Path + "'"
	}
	return output
}

// spawnParityErrno is libuv's filesystem errno name and description, the pair Node prints in
// Error.message. An errno the table does not name returns an empty name, and the caller keeps Go's
// wording, exactly as nodeErrorMessage does.
func spawnParityErrno(e syscall.Errno) (string, string) {
	switch e {
	case syscall.ENOENT:
		return "ENOENT", "no such file or directory"
	case syscall.EACCES:
		return "EACCES", "permission denied"
	case syscall.EPERM:
		return "EPERM", "operation not permitted"
	case syscall.ENOTDIR:
		return "ENOTDIR", "not a directory"
	case syscall.EEXIST:
		return "EEXIST", "file already exists"
	case syscall.EISDIR:
		return "EISDIR", "illegal operation on a directory"
	case syscall.ENOSPC:
		return "ENOSPC", "no space left on device"
	case syscall.EROFS:
		return "EROFS", "read-only file system"
	case syscall.EIO:
		return "EIO", "i/o error"
	case syscall.EMFILE:
		return "EMFILE", "too many open files"
	case syscall.ENFILE:
		return "ENFILE", "file table overflow"
	case syscall.ENAMETOOLONG:
		return "ENAMETOOLONG", "name too long"
	case syscall.ELOOP:
		return "ELOOP", "too many symbolic links encountered"
	case syscall.EFBIG:
		return "EFBIG", "file too large"
	}
	return "", ""
}
