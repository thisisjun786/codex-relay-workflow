// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/git/diff.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package bundle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type gitRepo struct {
	ctx        context.Context
	path, head string
}

func (g gitRepo) run(input []byte, args ...string) ([]byte, error) {
	c := exec.CommandContext(g.ctx, "git", append([]string{"-c", "core.attributesFile=" + os.DevNull, "-c", "core.quotePath=true", "-c", "diff.renameLimit=0", "-C", g.path}, args...)...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			c.Env = append(c.Env, v)
		}
	}
	c.Env = append(c.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_LITERAL_PATHSPECS=1", "LC_ALL=C")
	if g.head != "" {
		c.Env = append(c.Env, "GIT_ATTR_SOURCE="+g.head)
	}
	c.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (g gitRepo) resolve(ref string) (string, error) {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsRune(ref, 0) {
		return "", fmt.Errorf("invalid commit ref %q", ref)
	}
	out, err := g.run(nil, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

func (g gitRepo) diff(base, head string, context int, format string) ([]byte, error) {
	return g.run(nil, "diff", "-z", "--no-abbrev", "--full-index", "--find-renames=50%", "--no-ext-diff", "--no-textconv", "--no-color", "--no-relative",
		"--diff-algorithm=myers", "--no-indent-heuristic", "--inter-hunk-context=0", "--src-prefix=a/", "--dst-prefix=b/", "--submodule=short", "--ignore-submodules=none", "-O"+os.DevNull,
		"--output-indicator-new=+", "--output-indicator-old=-", "--output-indicator-context= ", "-U"+strconv.Itoa(context), "--no-patch", format, base, head, "--")
}

func (g gitRepo) changes(base, head string, context int) ([]file, error) {
	raw, err := g.diff(base, head, context, "--raw")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	var files []file
	for i := 0; len(raw) > 0 && i < len(fields); {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		i++
		if len(meta) != 5 || i >= len(fields) {
			return nil, fmt.Errorf("malformed raw diff")
		}
		f := file{FileMetadata: FileMetadata{Path: fields[i], Status: meta[4], Mode: meta[1]}}
		i++
		if strings.HasPrefix(f.Status, "R") || strings.HasPrefix(f.Status, "C") {
			if i >= len(fields) {
				return nil, fmt.Errorf("missing rename destination")
			}
			f.OldPath, f.Path = f.Path, fields[i]
			i++
		}
		files = append(files, f)
	}
	patch, err := g.diff(base, head, context, "-p")
	if err != nil {
		return nil, err
	}
	sections := splitPatch(string(patch))
	num, err := g.diff(base, head, context, "--numstat")
	if err != nil {
		return nil, err
	}
	stats := strings.Split(strings.TrimSuffix(string(num), "\x00"), "\x00")
	pi, si := 0, 0
	for i := range files {
		f := &files[i]
		count := 1
		if f.Status == "T" {
			count = 2
		}
		if pi+count > len(sections) || si >= len(stats) {
			return nil, fmt.Errorf("diff record/section count mismatch")
		}
		f.patch = strings.Join(sections[pi:pi+count], "")
		pi += count
		f.DiffBytes = len(f.patch)
		v := strings.SplitN(stats[si], "\t", 3)
		si++
		if len(v) != 3 {
			return nil, fmt.Errorf("malformed numstat")
		}
		path := v[2]
		if path == "" {
			if si+1 >= len(stats) || stats[si] != f.OldPath {
				return nil, fmt.Errorf("numstat rename mismatch")
			}
			path = stats[si+1]
			si += 2
		}
		if path != f.Path {
			return nil, fmt.Errorf("numstat path mismatch")
		}
		f.Binary = v[0] == "-" || v[1] == "-"
		if !f.Binary {
			if f.Additions, err = strconv.Atoi(v[0]); err != nil {
				return nil, err
			}
			if f.Deletions, err = strconv.Atoi(v[1]); err != nil {
				return nil, err
			}
		}
	}
	if pi != len(sections) || len(files) > 0 && si != len(stats) {
		return nil, fmt.Errorf("unpaired diff output")
	}
	return files, nil
}

func splitPatch(patch string) []string {
	if patch == "" {
		return nil
	}
	parts := strings.Split(patch, "\ndiff --git ")
	for i := 1; i < len(parts); i++ {
		parts[i] = "diff --git " + parts[i]
		parts[i-1] += "\n"
	}
	return parts
}
