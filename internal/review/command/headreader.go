package command

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
)

// gitHead reads files of one commit from its Git objects, for the pipeline's location checks and verification context. It reads committed objects only,
// never the working tree, with the environment hygiene of the bundle (internal/review/bundle/gitdiff.go): no system or global Git configuration, no
// replace objects, no lazy fetch, literal paths, and plumbing commands that run no external diff, textconv or filter.
type gitHead struct {
	ctx        context.Context
	repo, head string // head is a full commit id
	files      map[string][]string
}

func (g *gitHead) git(args ...string) ([]byte, error) {
	c := exec.CommandContext(g.ctx, "git", append([]string{"-c", "core.attributesFile=" + os.DevNull, "-C", g.repo}, args...)...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			c.Env = append(c.Env, v)
		}
	}
	c.Env = append(c.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_LITERAL_PATHSPECS=1", "LC_ALL=C")
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// lines returns the lines of the blob at path, read once. A path with no blob at head (absent, a directory, a submodule) wraps fs.ErrNotExist, as
// review.HeadReader requires.
func (g *gitHead) lines(path string) ([]string, error) {
	if l, ok := g.files[path]; ok {
		return l, nil
	}
	notExist := fmt.Errorf("%s at %s: %w", path, g.head, fs.ErrNotExist)
	if path == "" || strings.ContainsRune(path, 0) {
		return nil, notExist
	}
	entry, err := g.git("ls-tree", "-z", g.head, "--", path)
	if err != nil {
		return nil, err
	}
	meta, _, _ := strings.Cut(string(entry), "\t") // "<mode> <type> <oid>\t<path>\x00"
	f := strings.Fields(meta)
	if len(f) != 3 || f[1] != "blob" {
		return nil, notExist
	}
	blob, err := g.git("cat-file", "blob", f[2])
	if err != nil {
		return nil, err
	}
	var lines []string
	if len(blob) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(blob), "\n"), "\n")
	}
	if g.files == nil {
		g.files = map[string][]string{}
	}
	g.files[path] = lines
	return lines, nil
}

// Lines is the number of lines of the file: the newlines, plus one when the last byte is not a newline, so an empty file has none.
func (g *gitHead) Lines(path string) (int, error) {
	l, err := g.lines(path)
	return len(l), err
}

// ReadLines returns lines start to end of the file, 1-based and inclusive.
func (g *gitHead) ReadLines(path string, start, end int) ([]string, error) {
	l, err := g.lines(path)
	if err != nil {
		return nil, err
	}
	if start < 1 || end < start || end > len(l) {
		return nil, fmt.Errorf("%s has %d lines at %s, not lines %d to %d", path, len(l), g.head, start, end)
	}
	return append([]string(nil), l[start-1:end]...), nil
}
