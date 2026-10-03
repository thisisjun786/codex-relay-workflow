// Package crwdir is the project-local .crw directory and the step that publishes a file into it:
// the Go form of CXC v0.2.40 pabcd-state/src/{codexclaw-dir,atomic-write}.ts under the CRW names of
// contract/schema/cxc/name-substitution.json.
package crwdir

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// DirName is the project-local state directory (".codexclaw" in CXC).
const DirName = ".crw"

// GitignoreText is the only content of .crw/.gitignore: GITIGNORE_TEXT of codexclaw-dir.ts:4 after
// name-substitution rules R25 and R26.
const GitignoreText = "# CRW wrote this when it created .crw; everything here is local runtime state.\n*\n!.gitignore\n!rules/\n!rules/*.md\n"

// EnsureDir creates cwd/.crw and returns its path. Only the process that creates the directory
// publishes its .gitignore (ensureCodexclawDir): an existing directory, or a symlink or file in
// its place, is returned untouched, and so is a .gitignore a concurrent creator wrote first.
func EnsureDir(cwd string) (string, error) { return ensureDir(cwd, writeExclusive) }

func ensureDir(cwd string, writeIgnore func(path, data string) error) (string, error) {
	dir := filepath.Join(cwd, DirName)
	if err := os.Mkdir(dir, 0o777); err != nil {
		if errors.Is(err, os.ErrExist) {
			return dir, nil
		}
		return "", err
	}
	if err := writeIgnore(filepath.Join(dir, ".gitignore"), GitignoreText); err != nil {
		if errors.Is(err, os.ErrExist) {
			return dir, nil
		}
		// rmdir, as rmdirSync does: it removes an empty directory only, so whatever a concurrent
		// writer put there (a file, a full directory) stays and its failure is ignored.
		_ = syscall.Rmdir(dir)
		return "", err
	}
	return dir, nil
}

// writeExclusive is writeFileSync with flag "wx": it creates path or fails if it exists.
func writeExclusive(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	return errors.Join(err, f.Close())
}
