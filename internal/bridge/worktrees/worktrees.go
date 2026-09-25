// Package worktrees manages retained, locked Git worktrees without adopting existing paths.
package worktrees

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

var commitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var filterName = regexp.MustCompile(`^filter\..*\.(?:clean|smudge|process|required)$`)

// AfterGit is a test seam for interrupting a subprocess after its filesystem effect.
var AfterGit func(string)

type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

type Worktree struct{ Source, Destination, Revision, CommonDir string }
type Inspection struct {
	Checkout           string `json:"checkout"`
	InitialRevision    string `json:"initialRevision"`
	GitCommonDirectory string `json:"gitCommonDirectory"`
	Detached           bool   `json:"detached"`
	Clean              bool   `json:"clean"`
}

func git(ctx context.Context, cwd string, args ...string) (string, error) {
	bound, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prefix := []string{"--no-optional-locks", "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "submodule.recurse=false", "-c", "core.fsmonitor=false", "-C", cwd}
	cmd := exec.CommandContext(bound, "git", append(prefix, args...)...)
	cmd.WaitDelay = time.Second
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1"}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	cmd.Env = env
	output, err := cmd.Output()
	command := args
	for len(command) >= 2 && command[0] == "-c" {
		command = command[2:]
	}
	if err == nil && AfterGit != nil && ((len(command) > 1 && command[0] == "worktree" && command[1] == "add") || (len(command) > 0 && command[0] == "checkout-index")) {
		AfterGit(command[0])
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", &Error{strings.TrimSpace(string(exit.Stderr))}
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSuffix(string(output), "\n"), nil
}

// canonical is worktrees.py canonical_path: absolute, and equal to its own resolution.
func canonical(path, name string) error {
	if !filepath.IsAbs(path) || resolve(path) != path {
		return &Error{name + " must be a canonical absolute path without symlinks"}
	}
	return nil
}

// Validate is worktrees.py Worktree.validate, check for check and in the same order.
func Validate(ctx context.Context, source, revision, destination string) (Worktree, error) {
	for _, pair := range [][2]string{{source, "source_repository"}, {destination, "destination"}} {
		if err := canonical(pair[0], pair[1]); err != nil {
			return Worktree{}, err
		}
	}
	if !isDir(source) {
		return Worktree{}, &Error{"source_repository must be an existing Git checkout root"}
	}
	if !commitID.MatchString(revision) {
		return Worktree{}, &Error{"starting_revision must be a full lowercase commit object ID"}
	}
	root, err := git(ctx, source, "rev-parse", "--show-toplevel")
	if err != nil {
		return Worktree{}, err
	}
	if root != source {
		return Worktree{}, &Error{"source_repository must be the Git checkout root"}
	}
	if kind, err := git(ctx, source, "cat-file", "-t", revision); err != nil {
		return Worktree{}, err
	} else if kind != "commit" {
		return Worktree{}, &Error{"starting_revision must name a commit object"}
	}
	common, err := git(ctx, source, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Worktree{}, err
	}
	present, err := exists(destination)
	if err != nil {
		return Worktree{}, fmt.Errorf("destination: %w", err)
	}
	if present || isSymlink(destination) || !isDir(filepath.Dir(destination)) {
		return Worktree{}, &Error{"destination must be absent with an existing parent directory"}
	}
	for parent := filepath.Dir(destination); ; parent = filepath.Dir(parent) {
		dotGit, err := exists(filepath.Join(parent, ".git"))
		if err != nil {
			return Worktree{}, fmt.Errorf("destination parent: %w", err)
		}
		head, err := os.Stat(filepath.Join(parent, "HEAD"))
		bare := err == nil && head.Mode().IsRegular() && isDir(filepath.Join(parent, "objects"))
		if dotGit || bare {
			return Worktree{}, &Error{"destination must be outside existing repositories"}
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	list, err := git(ctx, source, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return Worktree{}, err
	}
	roots := []string{common}
	for _, entry := range strings.Split(list, "\x00") {
		if strings.HasPrefix(entry, "worktree ") {
			roots = append(roots, strings.TrimPrefix(entry, "worktree "))
		}
	}
	for _, r := range roots {
		// Resolved without requiring it to exist: a registered worktree whose directory was
		// deleted still owns its path, and reusing that path is what this refusal prevents.
		r = resolve(r)
		if destination == r || strings.HasPrefix(destination, strings.TrimSuffix(r, "/")+"/") {
			return Worktree{}, &Error{"destination must be outside existing worktrees and Git metadata"}
		}
	}
	return Worktree{source, destination, revision, resolve(common)}, nil
}
func (w Worktree) Receipt() map[string]any {
	return map[string]any{"sourceRepository": w.Source, "checkout": w.Destination, "requestedRevision": w.Revision, "gitCommonDirectory": w.CommonDir, "ownership": "bridge-managed", "lifecycle": "retained-until-manual-cleanup", "state": "planned"}
}
func (w Worktree) Reserve() error {
	if err := os.Mkdir(w.Destination, 0700); err != nil {
		return fmt.Errorf("reserve worktree: %w", err)
	}
	return nil
}
func (w Worktree) Create(ctx context.Context) error {
	_, err := git(ctx, w.Source, "worktree", "add", "--detach", "--no-checkout", "--lock", "--reason", "codex-thread-bridge: retained until manual cleanup", "--", w.Destination, w.Revision)
	return err
}
func filters(ctx context.Context, cwd string) ([]string, error) {
	out, err := git(ctx, cwd, "config", "--null", "--list")
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, v := range strings.Split(out, "\x00") {
		key, _, _ := strings.Cut(v, "\n")
		if filterName.MatchString(key) {
			names = append(names, key[:strings.LastIndexByte(key, '.')])
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	result := []string{}
	for _, name := range names {
		for _, suffix := range []string{"clean=", "smudge=", "process=", "required=false"} {
			result = append(result, "-c", name+"."+suffix)
		}
	}
	return result, nil
}
func (w Worktree) Checkout(ctx context.Context) error {
	config, err := filters(ctx, w.Destination)
	if err != nil {
		return err
	}
	if _, err = git(ctx, w.Destination, "read-tree", w.Revision); err != nil {
		return err
	}
	_, err = git(ctx, w.Destination, append(config, "checkout-index", "--all")...)
	return err
}
func (w Worktree) Inspect(ctx context.Context) (Inspection, error) {
	config, err := filters(ctx, w.Destination)
	if err != nil {
		return Inspection{}, err
	}
	fields := [][]string{{"rev-parse", "--show-toplevel"}, {"rev-parse", "HEAD"}, {"rev-parse", "--path-format=absolute", "--git-common-dir"}, {"rev-parse", "--abbrev-ref", "HEAD"}}
	values := []string{}
	for _, args := range fields {
		value, err := git(ctx, w.Destination, args...)
		if err != nil {
			return Inspection{}, err
		}
		values = append(values, value)
	}
	status, err := git(ctx, w.Destination, append(config, "status", "--porcelain=v1", "--untracked-files=all")...)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{values[0], values[1], values[2], values[3] == "HEAD", status == ""}, nil
}
func (w Worktree) Matches(ctx context.Context, actual Inspection) bool {
	return resolve(w.Destination) == w.Destination && actual.Checkout == w.Destination && actual.InitialRevision == w.Revision && actual.GitCommonDirectory == w.CommonDir && actual.Detached && actual.Clean
}
