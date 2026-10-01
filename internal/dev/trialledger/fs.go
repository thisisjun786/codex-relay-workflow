//go:build dev

package trialledger

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/pyload"
)

// The four states a reading of a record ends in: absent is a
// clean answer, unreadable means something is there whose shape cannot be read, an access error
// means the question could not be asked, and present means it was read.
const (
	absent      = "ABSENT"
	present     = "PRESENT"
	unreadable  = "UNREADABLE"
	accessError = "ACCESS_ERROR"
)

func kindOf(mode os.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeNamedPipe != 0:
		return "named pipe"
	case mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0:
		return "block device"
	case mode&os.ModeCharDevice != 0:
		return "character device"
	}
	return "not a regular file"
}

// failure is an error classified: one from the system established nothing (an access error), a
// decodeError read something and could not make sense of it.
func failure(err error, detail string) (string, string) {
	var decode *decodeError
	if errors.As(err, &decode) {
		return unreadable, detail + " (" + decode.text + ")"
	}
	return accessError, detail + " (" + err.Error() + ")"
}

// decodeError is bytes a reading could not decode: not UTF-8 text, or not JSON.
type decodeError struct{ text string }

func (e *decodeError) Error() string { return e.text }

// observe is what is at a path before it is read: a settled state and detail, or "" when a
// regular file is there to read.
func observe(path string) (string, string) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return absent, "nothing exists at " + path
		}
		return failure(err, "whether anything exists at this path could not be established")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
		switch {
		case errors.Is(err, syscall.ENOENT):
			return unreadable, "a symbolic link whose target does not exist"
		case errors.Is(err, syscall.ELOOP):
			return unreadable, "a symbolic link that loops"
		case err != nil:
			return failure(err, "a symbolic link whose target could not be resolved")
		}
	}
	if !info.Mode().IsRegular() {
		return unreadable, "this path is a " + kindOf(info.Mode()) + ", not a regular file"
	}
	return "", ""
}

// readRegularText is the file's text: read through one descriptor judged a regular file, strict
// UTF-8, "\r\n" and "\r" read as "\n".
func readRegularText(path, what string) (string, string, string) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		state, detail := failure(err, "could not read "+what)
		return state, detail, ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		return accessError, "could not read " + what + " (this path became a " + kindOf(info.Mode()) + " after it was looked at, and it is not read: " + path + ")", ""
	}
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(f)
	}
	if err != nil {
		state, detail := failure(err, "could not read "+what)
		return state, detail, ""
	}
	if !utf8.Valid(raw) {
		state, detail := failure(&decodeError{"not UTF-8 text"}, "could not read "+what)
		return state, detail, ""
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return present, "", strings.ReplaceAll(text, "\r", "\n")
}

func readText(path string) (string, string, string) {
	if state, detail := observe(path); state != "" {
		return state, detail, ""
	}
	return readRegularText(path, "the intervention ledger")
}

func readJSON(path, what string) (string, string, any) {
	if state, detail := observe(path); state != "" {
		return state, detail, nil
	}
	state, detail, text := readRegularText(path, what)
	if state != present {
		return state, detail, nil
	}
	value, err := pyload.Loads([]byte(text))
	if err != nil {
		state, detail := failure(&decodeError{err.Error()}, "could not read "+what)
		return state, detail, nil
	}
	return present, "", value
}

// pathlibForm is str(Path(p)): repeated separators and "." components collapse, ".." stays, a
// trailing separator goes, and exactly two leading separators are kept.
func pathlibForm(p string) string {
	prefix := ""
	switch {
	case strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///"):
		prefix = "//"
	case strings.HasPrefix(p, "/"):
		prefix = "/"
	}
	var kept []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	if joined := prefix + strings.Join(kept, "/"); joined != "" {
		return joined
	}
	return "."
}

// join is str(Path(root) / name).
func join(root, name string) string {
	if strings.HasSuffix(root, "/") {
		return root + name
	}
	return root + "/" + name
}

// realpath is os.path.realpath(path, strict=False) as Python 3.14 writes it: every symbolic link
// on the way is followed, a component that cannot be examined is kept as it is spelled, and a
// loop stops at the link that closes it.
func realpath(filename string) string {
	var rest []*string
	for _, part := range strings.Split(filename, "/") {
		rest = append([]*string{ptr(part)}, rest...)
	}
	partCount := len(rest)
	path := "/"
	if !strings.HasPrefix(filename, "/") {
		path, _ = os.Getwd()
	}
	seen := map[string]*string{}
	pop := func() *string {
		last := rest[len(rest)-1]
		rest = rest[:len(rest)-1]
		return last
	}
	for partCount > 0 {
		name := pop()
		if name == nil {
			seen[*pop()] = ptr(path)
			continue
		}
		partCount--
		if *name == "" || *name == "." {
			continue
		}
		if *name == ".." {
			if i := strings.LastIndex(path, "/"); i > 0 {
				path = path[:i]
			} else {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + *name
		if path == "/" {
			newpath = path + *name
		}
		info, err := os.Lstat(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if resolved, ok := seen[newpath]; ok {
			if resolved != nil {
				path = *resolved
				continue
			}
			path = newpath // a loop
			continue
		}
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		rest = append(rest, ptr(newpath), nil)
		parts := strings.Split(target, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			rest = append(rest, ptr(parts[i]))
		}
		partCount += len(parts)
	}
	return path
}

func ptr(s string) *string { return &s }

// within is containment of places, not spellings: an absolute child whose resolved path is the
// resolved parent or under it.
func within(child, parent string) bool {
	if !strings.HasPrefix(child, "/") {
		return false
	}
	here, root := pathlibForm(realpath(child)), pathlibForm(realpath(parent))
	return here == root || strings.HasPrefix(here, strings.TrimSuffix(root, "/")+"/")
}

// gitWorktreeOf is the nearest directory at or above the resolved path holding a .git, or "".
func gitWorktreeOf(path string) string {
	here := pathlibForm(realpath(path))
	for {
		if _, err := os.Stat(join(here, ".git")); err == nil {
			return here
		}
		if here == "/" {
			return ""
		}
		if i := strings.LastIndex(here, "/"); i > 0 {
			here = here[:i]
		} else {
			here = "/"
		}
	}
}

// pySplitLines is str.splitlines(): every line boundary Python knows, the boundary dropped.
func pySplitLines(s string) []string {
	var lines []string
	start := 0
	for i, r := range s {
		switch r {
		case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		default:
			continue
		}
		if r == '\n' && i > 0 && s[i-1] == '\r' && start == i {
			start = i + 1 // the \n of a \r\n
			continue
		}
		lines = append(lines, s[start:i])
		start = i + len(string(r))
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
