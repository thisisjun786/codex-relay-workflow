package bound

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Reasons a directory or file under the flow-bound directory cannot be trusted or read. UnsafeDir is the detail of the
// refusal the hook gives for the directory itself (`crw doctor` shows the same check).
const (
	ReasonUnsafeDir  = "unsafe_dir"
	ReasonUnsafeFile = "unsafe_file"
	ReasonUnreadable = "unreadable"
	ReasonTruncated  = "truncated"
	ReasonInvalid    = "unparseable"
	ReasonLedger     = "ledger"
	ReasonClaim      = "claim"
)

// UID is the user id the files must belong to: the current user's. A test replaces it to stand for a file of another owner.
var UID = os.Getuid

// UnavailableError says that the binding state of a thread, or a record a bound thread's spawn needs, cannot be known or kept: a
// lookup that cannot read the binding leaves the spawn to the ordinary path with a warning (CRW-1348), and a claim or record of a
// bound engine thread that cannot be kept refuses that spawn.
type UnavailableError struct {
	Reason string
	Detail string
}

func (e *UnavailableError) Error() string {
	if e.Detail == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Detail
}

func unavailable(reason, format string, args ...any) *UnavailableError {
	return &UnavailableError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// CheckDir lstats dir and judges it: a directory that does not exist is (false, nil); a directory that is a real directory
// owned by the current user with mode 0700 is (true, nil); anything else (a symlink, a file, another owner, other
// permission bits, an error other than "not there") is an *UnavailableError with ReasonUnsafeDir.
func CheckDir(dir string) (bool, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		if errorsIsAbsent(err) {
			return false, nil
		}
		return false, unavailable(ReasonUnsafeDir, "%v", err)
	}
	if err := trusted(info, dir, true, 0o700); err != nil {
		return true, err
	}
	return true, nil
}

// errorsIsAbsent is whether err says that the path (or a parent of it) is not there.
func errorsIsAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// trusted judges what a path or descriptor stat shows: a real directory (dir) or regular file, the current user's, with exactly
// the permission bits wanted and no setuid, setgid or sticky bit.
func trusted(info fs.FileInfo, name string, dir bool, perm fs.FileMode) *UnavailableError {
	reason := ReasonUnsafeFile
	if dir {
		reason = ReasonUnsafeDir
	}
	switch {
	case dir && !info.IsDir(), !dir && !info.Mode().IsRegular():
		return unavailable(reason, "%s is not a %s (mode %v)", name, kind(dir), info.Mode())
	case info.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 || info.Mode().Perm() != perm:
		return unavailable(reason, "%s has mode %v, want %v", name, info.Mode(), perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != UID() {
		return unavailable(reason, "%s is not owned by the current user", name)
	}
	return nil
}

func kind(dir bool) string {
	if dir {
		return "directory"
	}
	return "regular file"
}
