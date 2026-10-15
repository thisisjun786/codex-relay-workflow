// Package crwdir is the project-local .crw directory and the step that publishes a file into it:
// the Go form of CXC v0.2.40 pabcd-state/src/{codexclaw-dir,atomic-write}.ts under the CRW names of
// contract/schema/cxc/name-substitution.json.
package crwdir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// DirName is the project-local state directory (".codexclaw" in CXC).
const DirName = ".crw"

// GitignoreText is the only content of .crw/.gitignore: GITIGNORE_TEXT of codexclaw-dir.ts:4 after
// name-substitution rules R25 and R26.
const GitignoreText = "# CRW wrote this when it created .crw; everything here is local runtime state.\n*\n!.gitignore\n!rules/\n!rules/*.md\n"

// ErrLink is what the refusal of a symbolic link in the .crw tree wraps: a link where a directory or a file of the tree belongs
// is an input error, and nothing is read, created or written through it (CRW-475).
var ErrLink = errors.New("symbolic link")

// linkError names path as the refused link.
func linkError(path string) error {
	return fmt.Errorf("%s is a symlink (symbolic link); CRW does not write through a link in its state tree: %w", path, ErrLink)
}

// RequireDir is nil when path is a directory itself, judged by Lstat, which does not follow a link. A symbolic link (dangling or
// not) is refused with an error that names path and wraps ErrLink, anything else that is not a directory with one that wraps
// ENOTDIR; a missing path is the Lstat error (fs.ErrNotExist).
func RequireDir(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return err
	case info.Mode()&fs.ModeSymlink != 0:
		return linkError(path)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory: %w", path, syscall.ENOTDIR)
	}
	return nil
}

// EnsureDir creates cwd/.crw and returns its path. Only the process that creates the directory publishes its .gitignore
// (ensureCodexclawDir); an existing directory, and a .gitignore a concurrent creator wrote first, are returned untouched. A
// symbolic link or any other non-directory at .crw, there before the call or put there while it runs, is refused (RequireDir)
// and nothing is written through it: the .gitignore is created through a descriptor of the directory opened without following
// a link. The oracle returns such an entry untouched and writes through it (port: fixed, CRW-475).
func EnsureDir(cwd string) (string, error) { return ensureDir(cwd, writeExclusive) }

func ensureDir(cwd string, writeIgnore func(path, data string) error) (string, error) {
	dir := filepath.Join(cwd, DirName)
	checked := func() (string, error) {
		if err := RequireDir(dir); err != nil {
			return "", err
		}
		return dir, nil
	}
	if err := os.Mkdir(dir, 0o777); err != nil {
		if errors.Is(err, os.ErrExist) {
			return checked()
		}
		return "", err
	}
	if err := writeIgnore(filepath.Join(dir, ".gitignore"), GitignoreText); err != nil {
		if errors.Is(err, os.ErrExist) {
			return checked()
		}
		// rmdir, as rmdirSync does: it removes an empty directory only, so whatever a concurrent
		// writer put there (a file, a full directory, a link) stays and its failure is ignored.
		_ = syscall.Rmdir(dir)
		return "", err
	}
	return checked()
}

// WriteExclusive creates the file path with data (mode 0666 under the umask), or fails with an error that wraps fs.ErrExist when
// any entry is there, a link included. The directory that holds it is opened with O_DIRECTORY|O_NOFOLLOW and the file is created
// relative to that descriptor with O_EXCL|O_NOFOLLOW, so a symbolic link in the directory's place is refused (an error that wraps
// ErrLink) and nothing is created or written through it. It is how EnsureDir writes .crw/.gitignore, for a writer of another
// ignore file in the tree.
func WriteExclusive(path, data string) error { return writeExclusive(path, data) }

// writeExclusive is writeFileSync with flag "wx": it creates path or fails if it exists. The directory that holds it is opened
// without following a link and path is created relative to that descriptor, so a link put in the directory's place is refused.
func writeExclusive(path, data string) error {
	dir := filepath.Dir(path)
	dfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if derr := RequireDir(dir); derr != nil {
			return derr
		}
		return &fs.PathError{Op: "open", Path: dir, Err: err}
	}
	defer unix.Close(dfd)
	fd, err := unix.Openat(dfd, filepath.Base(path), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o666)
	if err != nil {
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	_, err = f.WriteString(data)
	return errors.Join(err, f.Close())
}

// EnsureSubdir is EnsureDir followed by the directories below .crw that parts name, one component at a time: each is created
// when missing (mode 0777 under the umask) and must then be a directory itself (RequireDir), so no writer of the tree creates
// or opens anything through a link planted at .crw or below it. It returns the deepest directory.
func EnsureSubdir(cwd string, parts ...string) (string, error) {
	dir, err := EnsureDir(cwd)
	if err != nil {
		return "", err
	}
	for _, part := range parts {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := RequireDir(dir); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// CheckTree is the check of EnsureSubdir without creating anything: .crw and each directory below it that parts name must be a
// directory itself as far as they exist; the first missing one ends the check with nil.
func CheckTree(cwd string, parts ...string) error {
	dir := filepath.Join(cwd, DirName)
	for i := 0; ; i++ {
		if err := RequireDir(dir); errors.Is(err, fs.ErrNotExist) {
			return nil
		} else if err != nil || i == len(parts) {
			return err
		}
		dir = filepath.Join(dir, parts[i])
	}
}

// NotLink is nil unless path is a symbolic link, which it refuses as RequireDir does. A path that cannot be looked at is left
// to the caller's own open, which reports it.
func NotLink(path string) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return linkError(path)
	}
	return nil
}

// OpenFile is os.OpenFile with O_NOFOLLOW: a symbolic link at path is refused with an error that names it and wraps ErrLink,
// never opened or created through.
func OpenFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
	if err != nil && errors.Is(err, syscall.ELOOP) {
		if lerr := NotLink(path); lerr != nil {
			return nil, lerr
		}
	}
	return f, err
}
