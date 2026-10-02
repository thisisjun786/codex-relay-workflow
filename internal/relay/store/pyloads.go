package store

import (
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// LoadsJSON is json.loads into values contract.Emit renders as Python would: objects keep
// their key order (a repeated key keeps its first position and last value), integers stay
// exact as json.Number, NaN and the infinities are float64, and every other number becomes a
// float64. A document json.loads refuses answers its JSONDecodeError text (pyjson.Error).
func LoadsJSON(data []byte) (any, error) {
	return pyjson.Loads(string(data), pyjson.LoadOptions{Python: true, Constants: true})
}

// PathlibSpelling is str(Path(value)): empty and "." components collapse, ".." stays, and exactly
// two leading slashes stay a root of their own where three or more fold to one.
func PathlibSpelling(value string) string { return pathlibSpelling(value) }

// Normpath is posixpath.normpath (ownership.Normpath): folded lexically, exactly two leading
// slashes kept.
func Normpath(value string) string { return ownership.Normpath(value) }

// Absolute is str(Path(value).absolute()) for a path already expanded: the kernel's working
// directory (os.getcwd, never $PWD's spelling of it) prefixed to a relative path, then the
// pathlib spelling, nothing resolved and no ".." folded.
func Absolute(value string) (string, error) { return absolute(value) }

// Abspath is os.path.abspath: a relative path joined to the working directory the kernel names
// (os.getcwd, never $PWD's spelling of it), then posixpath.normpath, which folds "." and ".."
// lexically and keeps exactly two leading slashes.
func Abspath(value string) (string, error) {
	if !strings.HasPrefix(value, "/") {
		cwd, err := unix.Getwd()
		if err != nil {
			return "", err
		}
		value = ownership.JoinCwd(cwd, value)
	}
	return Normpath(value), nil
}

// Dirname is posixpath.dirname: everything before the last slash, trailing slashes dropped unless
// that is all there is ("/" and "//" stay themselves).
func Dirname(value string) string {
	head := value[:strings.LastIndex(value, "/")+1]
	if strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// StoreDirectory is assignment.store_directory, os.path.dirname(os.path.abspath(path)): the
// directory that selects a store with --state, as every recovery line names it.
func StoreDirectory(path string) (string, error) {
	absolute, err := Abspath(path)
	return Dirname(absolute), err
}

// Home is Path.home(): HOME when it is set at all (an empty HOME is the root, trailing slashes
// are dropped), else this user's passwd entry; ErrNoHome where neither answers.
func Home() (string, error) { return homeDir() }

// AbsoluteExpanded is str(Path(value).expanduser().absolute()): home expanded (ErrNoHome for an
// unknown ~user), the working directory prefixed to a relative path, nothing resolved and no
// ".." folded, so a symlink the caller named stays the path they named.
func AbsoluteExpanded(value string) (string, error) { return absoluteExpanded(value) }
