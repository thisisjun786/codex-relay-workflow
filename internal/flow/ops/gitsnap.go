// Package ops holds the git side of the flow journal's effect proofs: the working snapshot of a worktree, the commit of
// exactly that snapshot, the record of the paths a commit changed outside its declared edit region, and (proofs.go) the
// functions that decide from the journal and a worktree whether the effect of a recorded call is still there.
package ops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// GitFunc runs one git command as `git -C dir args...` with env added to the process environment and returns its standard
// output. ExecGit removes only the line feed that ends the output; a runner that trims more (the agent allocator's) is
// accepted because no command of this package reads a leading or trailing space from the output: the directories it works in
// are the ones it is given, and the commands whose output is a list of names are asked in a form that begins with a status
// letter or ends with a NUL. Every command of this package goes through it, so a test can see what was run.
type GitFunc func(ctx context.Context, dir string, env []string, args ...string) (string, error)

// ExecGit is the GitFunc that runs the git binary. The variables that route git to a repository, a work tree or an index
// other than the one -C names are taken out of the environment, so a process started from a git hook cannot make the
// snapshot of another repository.
func ExecGit(binary string) GitFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cleanEnvironment(os.Environ()), env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", &GitError{Dir: dir, Args: args, Err: err, Stderr: strings.TrimSpace(stderr.String())}
		}
		return strings.TrimSuffix(stdout.String(), "\n"), nil
	}
}

// GitError is a git command that failed.
type GitError struct {
	Dir    string
	Args   []string
	Err    error
	Stderr string
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git -C %s %s: %v: %s", e.Dir, strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *GitError) Unwrap() error { return e.Err }

// repositoryVariables are the environment variables that point git at a repository, a work tree, an index or an object
// store (git rev-parse --local-env-vars, and the namespace).
var repositoryVariables = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_CONFIG": true, "GIT_CONFIG_COUNT": true,
	"GIT_CONFIG_PARAMETERS": true, "GIT_DIR": true, "GIT_GRAFT_FILE": true, "GIT_IMPLICIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
	"GIT_NAMESPACE": true, "GIT_NO_REPLACE_OBJECTS": true, "GIT_OBJECT_DIRECTORY": true, "GIT_PREFIX": true,
	"GIT_REPLACE_REF_BASE": true, "GIT_SHALLOW_FILE": true, "GIT_WORK_TREE": true,
}

func cleanEnvironment(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if repositoryVariables[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// identity is the author and committer of the commits the run makes; it is passed through the environment of one command
// and never written to a git configuration.
var identity = []string{
	"GIT_AUTHOR_NAME=crw-flow", "GIT_AUTHOR_EMAIL=crw-flow@localhost",
	"GIT_COMMITTER_NAME=crw-flow", "GIT_COMMITTER_EMAIL=crw-flow@localhost",
}

// Snapshot is the working snapshot of a worktree: the tree object id of the files the worktree holds now. It is made in a
// private index file (GIT_INDEX_FILE) that starts from HEAD (`read-tree HEAD`), takes in every change of the working files
// (`add -A -- .`: modified, deleted, mode-changed and untracked files that no ignore rule hides) and is written out as a
// tree (`write-tree`). The worktree's real index is neither read nor written: it holds only what was staged, and an
// executor leaves nothing staged. A file that is ignored and not tracked is not in the snapshot, so it is not in a commit
// of it either. Two worktrees with the same files have the same snapshot, whatever their HEAD.
func Snapshot(ctx context.Context, git GitFunc, worktree string) (string, error) {
	// The worktree is the directory it is given, exactly: a path git prints is text a runner may have trimmed, and a worktree
	// whose name ends in a space is a worktree.
	root := worktree
	dir, err := os.MkdirTemp("", "crw-snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := git(ctx, root, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil {
		if _, err := git(ctx, root, env, quiet("read-tree", "HEAD")...); err != nil {
			return "", err
		}
	}
	if err := keepSparseEntries(ctx, git, root, env); err != nil {
		return "", err
	}
	if _, err := git(ctx, root, env, quiet("add", "-A", "--sparse", "--", ".")...); err != nil {
		return "", err
	}
	return git(ctx, root, env, quiet("write-tree")...)
}

// keepSparseEntries marks, in the private index, the entries the worktree's real index holds as skip-worktree while their
// file is absent from the working files (a sparse checkout): such an entry is held with its content from HEAD and is not a
// deletion. The real index is only read.
func keepSparseEntries(ctx context.Context, git GitFunc, root string, env []string) error {
	listing, err := git(ctx, root, nil, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range strings.Split(listing, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' || (entry[0] != 'S' && entry[0] != 's') {
			continue
		}
		if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(entry[2:]))); statErr != nil {
			names = append(names, entry[2:])
		}
	}
	for len(names) > 0 {
		n := min(len(names), 500)
		if _, err := git(ctx, root, env, quiet(append([]string{"update-index", "--skip-worktree", "--"}, names[:n]...)...)...); err != nil {
			return err
		}
		names = names[n:]
	}
	return nil
}

// quiet is a git command that runs against the private index: it never writes a split index or consults a file-system
// monitor, so the index file is written whole and nothing is added to the worktree's git directory.
func quiet(args ...string) []string {
	return append([]string{"-c", "core.splitIndex=false", "-c", "core.fsmonitor=false"}, args...)
}

// Head is the commit a worktree's HEAD is at.
func Head(ctx context.Context, git GitFunc, worktree string) (string, error) {
	return git(ctx, worktree, nil, "rev-parse", "--verify", "HEAD^{commit}")
}

// TreeOf is the tree a commit holds.
func TreeOf(ctx context.Context, git GitFunc, dir, commit string) (string, error) {
	return git(ctx, dir, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
}

// IsAncestor reports whether ancestor is reachable from descendant (a commit is its own ancestor). A commit that is not in
// the repository is not an ancestor of anything.
func IsAncestor(ctx context.Context, git GitFunc, dir, ancestor, descendant string) (bool, error) {
	if ancestor == "" || descendant == "" {
		return false, nil
	}
	for _, c := range []string{ancestor, descendant} {
		if _, err := git(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", c+"^{commit}"); err != nil {
			return false, nil
		}
	}
	_, err := git(ctx, dir, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var ge *GitError
	if errors.As(err, &ge) {
		var exit *exec.ExitError
		if errors.As(ge.Err, &exit) && exit.ExitCode() == 1 {
			return false, nil
		}
	}
	return false, err
}

// ErrHeadMoved is the refusal of a commit whose parent is no longer the worktree's HEAD.
var ErrHeadMoved = errors.New("ops: the worktree's HEAD is not the parent the commit was made on")

// CommitRequest asks for the commit of one snapshot.
type CommitRequest struct {
	// Tree is the snapshot to commit, as recorded (the pending tree P of the call that produced it).
	Tree string
	// Parent is the commit the worktree's HEAD must be at; the new commit is its child.
	Parent  string
	Message string
}

// CommitResult is the commit that was made.
type CommitResult struct {
	Commit string `json:"commit"`
	Parent string `json:"parent"`
	Tree   string `json:"tree"`
}

// Commit commits exactly the snapshot Tree: commit-tree makes a commit of that tree on Parent, update-ref moves HEAD to it
// only if HEAD is still Parent (a compare-and-swap, so a commit someone else made in between is not overwritten), and then
// the real index alone is set to the new HEAD (`reset --mixed`; the working files are not touched, so whatever differs from
// the snapshot stays visible as a change). Nothing is added, committed or staged from the working files: the tree that is
// committed is the tree that was recorded.
func Commit(ctx context.Context, git GitFunc, worktree string, req CommitRequest) (CommitResult, error) {
	if req.Tree == "" || req.Parent == "" || req.Message == "" {
		return CommitResult{}, errors.New("ops: a commit needs a tree, a parent and a message")
	}
	if kind, err := git(ctx, worktree, nil, "cat-file", "-t", req.Tree); err != nil || kind != "tree" {
		return CommitResult{}, fmt.Errorf("ops: %s is not a tree of the repository", req.Tree)
	}
	head, err := Head(ctx, git, worktree)
	if err != nil {
		return CommitResult{}, err
	}
	if head != req.Parent {
		return CommitResult{}, fmt.Errorf("%w: HEAD is %s, the parent is %s", ErrHeadMoved, head, req.Parent)
	}
	commit, err := git(ctx, worktree, identity, "-c", "commit.gpgsign=false", "commit-tree", req.Tree, "-p", req.Parent, "-m", req.Message)
	if err != nil {
		return CommitResult{}, err
	}
	if _, err := git(ctx, worktree, nil, "update-ref", "-m", "crw.commit", "HEAD", commit, req.Parent); err != nil {
		return CommitResult{}, fmt.Errorf("%w: %v", ErrHeadMoved, err)
	}
	if _, err := git(ctx, worktree, nil, "reset", "-q", "--mixed", "HEAD", "--"); err != nil {
		return CommitResult{}, fmt.Errorf("ops: the commit %s was made but the index could not be set to it: %w", commit, err)
	}
	return CommitResult{Commit: commit, Parent: req.Parent, Tree: req.Tree}, nil
}

// ScopeReport says which paths a snapshot changes against its base and which of them lie outside the declared edit
// regions. Scope is recorded, never refused: the report goes on the commit row and into the reviewer packet.
type ScopeReport struct {
	Changed    []string `json:"changed"`
	OutOfScope []string `json:"out_of_scope"`
}

// Scope compares tree with the tree of base (a commit or a tree) and sorts the changed paths by the edit regions. A region
// is a directory prefix ending in "/", a file path, or a pattern with `*`, `?` or `[` (path.Match, on the whole path). With
// no region declared nothing is out of scope.
func Scope(ctx context.Context, git GitFunc, dir, base, tree string, regions []string) (ScopeReport, error) {
	// --name-status -z: every record is a status letter and then the path, each ended by a NUL. The output begins with a
	// letter, so a runner that trims the text cannot take a leading space off the first path.
	out, err := git(ctx, dir, nil, "diff-tree", "-r", "-z", "--name-status", "--no-renames", "--end-of-options", base, tree)
	if err != nil {
		return ScopeReport{}, err
	}
	rep := ScopeReport{Changed: []string{}, OutOfScope: []string{}}
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		p := fields[i+1]
		if p == "" {
			continue
		}
		rep.Changed = append(rep.Changed, p)
		if len(regions) > 0 && !slices.ContainsFunc(regions, func(r string) bool { return inRegion(r, p) }) {
			rep.OutOfScope = append(rep.OutOfScope, p)
		}
	}
	slices.Sort(rep.Changed)
	slices.Sort(rep.OutOfScope)
	return rep, nil
}

func inRegion(region, p string) bool {
	switch {
	case strings.HasSuffix(region, "/"):
		return strings.HasPrefix(p, region)
	case strings.ContainsAny(region, "*?["):
		ok, _ := path.Match(region, p)
		return ok
	}
	return p == region
}

// Restore rebuilds a boundary: a detached worktree at the commit base (`git worktree add --detach`), and when the boundary
// ends with a pending tree, that tree on top of it. The files are made equal to the pending tree with a two-tree merge in a
// temporary index (`read-tree -m -u base pending`); the worktree's own index stays at base, so whatever the pending tree
// changes shows as an uncommitted change and the commit that follows commits exactly that tree. The working snapshot is
// compared with the boundary (the tree of base, or the pending tree) before anything runs, and returned: it is the digest the
// worktree is verified with from then on.
func Restore(ctx context.Context, git GitFunc, repo, path, base, pending string) (string, error) {
	if base == "" {
		return "", errors.New("ops: a restore needs a base commit")
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("ops: the worktree path %s already exists: a path belongs to one allocation", path)
	}
	want, err := TreeOf(ctx, git, repo, base)
	if err != nil {
		return "", err
	}
	if pending != "" {
		if kind, err := git(ctx, repo, nil, "cat-file", "-t", pending); err != nil || kind != "tree" {
			return "", fmt.Errorf("ops: the pending tree %s is not a tree of the repository", pending)
		}
		want = pending
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := git(ctx, repo, nil, "worktree", "add", "--detach", "--quiet", path, base); err != nil {
		return "", err
	}
	if pending != "" {
		if err := materialize(ctx, git, path, base, pending); err != nil {
			return "", err
		}
	}
	got, err := Snapshot(ctx, git, path)
	if err != nil {
		return "", err
	}
	if got != want {
		return "", fmt.Errorf("ops: the restored worktree %s holds %s, the boundary is %s", path, got, want)
	}
	return got, nil
}

// materialize makes the files of a worktree that holds the tree of base equal to the tree pending, in a temporary index so
// that the real index stays at base.
func materialize(ctx context.Context, git GitFunc, worktree, base, pending string) error {
	dir, err := os.MkdirTemp("", "crw-restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := git(ctx, worktree, env, quiet("read-tree", base)...); err != nil {
		return err
	}
	// the temporary index is new: its entries get the stat data of the files they describe, or the merge refuses to touch them
	if _, err := git(ctx, worktree, env, quiet("update-index", "-q", "--refresh")...); err != nil {
		return err
	}
	_, err = git(ctx, worktree, env, quiet("read-tree", "-m", "-u", base, pending)...)
	return err
}

// Preserved is the commit that keeps the state of a worktree.
type Preserved struct {
	Commit, Head, Snapshot string
}

// Preserve keeps a worktree's HEAD and working snapshot as a commit on ref (the snapshot's tree on the HEAD commit), made
// without touching the worktree: its files, its index and its HEAD are read, never written. A ref that already exists is
// moved to the new commit.
func Preserve(ctx context.Context, git GitFunc, repo, worktree, ref, message string) (Preserved, error) {
	head, err := Head(ctx, git, worktree)
	if err != nil {
		return Preserved{}, err
	}
	tree, err := Snapshot(ctx, git, worktree)
	if err != nil {
		return Preserved{}, err
	}
	commit, err := PreserveTree(ctx, git, repo, tree, head, ref, message)
	if err != nil {
		return Preserved{}, err
	}
	return Preserved{Commit: commit, Head: head, Snapshot: tree}, nil
}

// PreserveTree keeps a tree as a commit on ref, on the parent commit.
func PreserveTree(ctx context.Context, git GitFunc, repo, tree, parent, ref, message string) (string, error) {
	commit, err := git(ctx, repo, identity, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", parent, "-m", message)
	if err != nil {
		return "", err
	}
	if _, err := git(ctx, repo, nil, "update-ref", ref, commit); err != nil {
		return "", err
	}
	return commit, nil
}
