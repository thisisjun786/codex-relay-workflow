package doctor

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// doctorRootedReadSeam is a test seam: called with the path about to be opened, after the caller's
// containment verdict and before the open. A test swaps a link there to prove the read cannot
// leave the plugin root. Production leaves it nil.
var doctorRootedReadSeam func(path string)

// errDoctorRootEscape is the error doctorRootedRead answers when the file, or a link on the way to
// it, leaves the plugin root at the moment it is opened; errDoctorRootMissing when the root cannot
// reach the file at all. Callers map the first to their "escapes the plugin root" finding and the
// second to their "missing" finding; any other error is the read's own and is reported as it is.
var (
	errDoctorRootEscape  = errors.New("path escapes the plugin root")
	errDoctorRootMissing = errors.New("path is missing under the plugin root")
)

// doctorRootedError carries one of the two sentinels beside the error that caused it.
type doctorRootedError struct {
	kind  error
	cause error
}

func (e *doctorRootedError) Error() string { return e.cause.Error() }
func (e *doctorRootedError) Unwrap() []error {
	return []error{e.kind, e.cause}
}

// doctorRootedRead reads the file at path through a handle bound to the plugin root (CRW-1015).
// The caller has already judged path inside the root by name (targetEscapesRoot); a verdict on a
// name is a verdict on that moment only, and a read that opens the same name again follows
// whatever link stands there by then. Here the name is made relative to the root, the root is
// opened once (os.OpenRoot on its real path, as hookTrustEntriesReadContained does), and the file
// is statted and opened through that root: a link that stays inside is followed, one that leaves
// it -- however it is swapped around the caller's check -- is refused at the open, and the bytes
// come from the handle that open returned. The Root refuses an absolute link even when it points
// inside the root, a link that leaves the root and comes back, and a chain of more than 8 links;
// those are answered as escapes, the fail-closed direction.
//
// The path is the file-system form of the name (targetNodeText or hookTrustEntriesFSPath already
// applied); it is the root's own spelling plus the reference, or an absolute reference. A trailing
// separator is kept, so a regular file named with one is refused as it is by a stat.
func doctorRootedRead(pluginRoot, path string) ([]byte, error) {
	root, within, err := doctorRootedWithin(pluginRoot, path)
	if err != nil {
		return nil, err
	}
	rooted, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	if doctorRootedReadSeam != nil {
		doctorRootedReadSeam(path)
	}
	if _, err := rooted.Stat(within); err != nil {
		return nil, doctorRootedClassify(path, err)
	}
	file, err := rooted.Open(within)
	if err != nil {
		if doctorRootedOpenIsRead(err) {
			return nil, err
		}
		return nil, doctorRootedClassify(path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &fs.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	}
	return io.ReadAll(file)
}

// doctorRootedClassify turns the Root's answer into the sentinel the callers act on. A name the
// root's stat cannot reach for any reason but an escape (absent, not a directory on the way, a link
// loop) is "missing", the answer the stat these callers made before gave. An open that fails after
// that stat succeeded is "missing" only when it found nothing to open (doctorRootedOpenIsRead);
// any other open failure is returned as the read's own error. An escape stays an escape,
// with one exception: a link that leaves the root toward a file that does not exist is missing, as
// the paired walk of targetEscapesRoot judges it (CRW-652); the stat that decides it reads no
// content, so a swap around it can change the finding but never what is read.
func doctorRootedClassify(path string, err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Err.Error() == hookTrustEntriesRootEscape {
		if _, statErr := os.Stat(path); statErr == nil {
			return &doctorRootedError{kind: errDoctorRootEscape, cause: err}
		}
	}
	return &doctorRootedError{kind: errDoctorRootMissing, cause: err}
}

// doctorRootedOpenIsRead tells an open that failed on the file itself -- the name was reachable, the
// stat before it succeeded, and the open was refused (EACCES, EIO, EMFILE, ...) -- from one that
// found nothing to open. The first does not prove absence: it stays the read's own error, as the
// ReadFile after a stat gave it. An escape, and a name that vanished or turned into a link loop
// between the stat and the open, go to doctorRootedClassify.
func doctorRootedOpenIsRead(err error) bool {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) && pathErr.Err.Error() == hookTrustEntriesRootEscape {
		return false
	}
	return !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) && !errors.Is(err, syscall.ELOOP)
}

// doctorRootedWithin is the real path of the plugin root and the name of path under it. The name
// is taken from whichever spelling of the root path starts with -- the root as the caller gave it
// made absolute, or its real path -- and kept as spelled below it, a '..' component included, for
// the Root to resolve in kernel order. A path under neither is outside the root.
func doctorRootedWithin(pluginRoot, path string) (root, within string, err error) {
	absolute, err := filepath.Abs(pluginRoot)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", err
	}
	for _, base := range []string{absolute, root} {
		trimmed := strings.TrimRight(base, string(filepath.Separator))
		switch {
		case path == trimmed || path == trimmed+string(filepath.Separator):
			return root, ".", nil
		case strings.HasPrefix(path, trimmed+string(filepath.Separator)):
			return root, path[len(trimmed)+1:], nil
		}
	}
	return "", "", &doctorRootedError{kind: errDoctorRootEscape, cause: errors.New("path is not under the plugin root: " + path)}
}
