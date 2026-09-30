//go:build dev

package ci

import (
	"bufio"
	"bytes"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// allowList is where the repository names the Python files it may still track (todo 44).
const allowList = "scripts/dev/ALLOWED_PYTHON.txt"

// pythonAllowListErrors compares the Python files root's Git index tracks with the allow-list,
// both ways, and names every other tracked file whose first line is a python shebang. The
// allow-list's own directory and the skill fixtures (data a skill's replay reads) are not
// compared; the plan's acceptance is `git ls-files '*.py' | grep -vxF -f <(grep -v '^#' list)`.
func pythonAllowListErrors(root string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, allowList))
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			allowed[line] = true
		}
	}
	out, err := runGit(root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var errs []string
	tracked := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if name == "" || strings.HasPrefix(name, "scripts/dev/") {
			continue
		}
		if fixture, _ := path.Match("plugins/crw/skills/*/scripts/fixtures/*", firstComponents(name, 7)); fixture {
			continue
		}
		tracked[name] = true
		switch {
		case strings.HasSuffix(name, ".py"):
			if !allowed[name] {
				errs = append(errs, name+": a tracked Python file the allow-list does not name")
			}
		case !allowed[name] && pythonShebang(filepath.Join(root, name)):
			errs = append(errs, name+": a tracked file with a python shebang the allow-list does not name")
		}
	}
	for name := range allowed {
		if !tracked[name] {
			errs = append(errs, name+": listed, but not a tracked file")
		}
	}
	slices.Sort(errs)
	return errs, nil
}

// firstComponents is the first n slash-separated components of name.
func firstComponents(name string, n int) string {
	parts := strings.SplitN(name, "/", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "/")
}

// pythonShebang reports whether path is a regular file whose first line starts with #! and names
// python (a symbolic link is judged as what it is, never followed).
func pythonShebang(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	line, _ := bufio.NewReader(file).ReadBytes('\n')
	return bytes.HasPrefix(line, []byte("#!")) && bytes.Contains(line, []byte("python"))
}

// Todo 44's acceptance: this repository tracks only the Python files the allow-list names, and
// no other file that a python shebang would run.
func TestPythonFilesAreAllowListed(t *testing.T) {
	errs, err := pythonAllowListErrors(repoRoot())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range errs {
		t.Error(e)
	}
}

// The failure drill: a Python file the list does not name, a shebang script without the .py
// suffix and a listed path nothing tracks are each refused; the skill fixtures and scripts/dev
// are not compared.
func TestPythonAllowListRefusesWhatItDoesNotName(t *testing.T) {
	r := newRepo(t)
	r.write(allowList, "# comment\nscripts/tool.py\nscripts/gone.py\n")
	r.write("scripts/tool.py", "print()\n")
	r.write("packages/x.py", "print()\n")
	r.write("scripts/fake-gh", "#!/usr/bin/env python3\nprint()\n")
	r.write("scripts/fake-git", "#!/bin/sh\nexit 0\n")
	r.write("scripts/dev/helper.py", "print()\n")
	r.write("plugins/crw/skills/crw-run/scripts/fixtures/case/input.py", "data\n")
	r.commit()
	errs, err := pythonAllowListErrors(r.root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"packages/x.py: a tracked Python file the allow-list does not name",
		"scripts/fake-gh: a tracked file with a python shebang the allow-list does not name",
		"scripts/gone.py: listed, but not a tracked file",
	}
	expectEqual(t, "allow-list errors", errs, want)
}
