package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

type CollectionError struct {
	Code      string
	Retryable bool
	Detail    string
}

func (e *CollectionError) Error() string { return e.Code + ": " + e.Detail }
func collectionError(code string, retry bool, format string, args ...any) error {
	return &CollectionError{code, retry, fmt.Sprintf(format, args...)}
}

func candidateCommand(ctx context.Context, dir string, args ...string) (string, error) {
	argv := []string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "protocol.https.allow=always", "-c", "protocol.ssh.allow=always", "-c", "credential.interactive=false", "-C", dir}
	// Keep controller GitHub authentication while excluding global URL rewrites,
	// filters and hooks. The helper is fixed product code, never result input.
	argv = append([]string{"-c", "credential.https://github.com.helper=!gh auth git-credential"}, argv...)
	cmd := exec.CommandContext(ctx, "git", append(argv, args...)...)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_LFS_SKIP_SMUDGE=1")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSuffix(string(raw), "\n"), nil
}

// CollectCandidate creates an isolated checkout with no inherited config or
// object alternates. Fetching a branch proves publication; an arbitrary object
// available elsewhere in a local object database is not publication evidence.
func CollectCandidate(ctx context.Context, a Admission, want Candidate, repo RegisteredRepository, root string) (out *CollectedCandidate, err error) {
	if err = ValidateAdmission(a); err != nil {
		return nil, err
	}
	if repo.ID != a.Task.Repository.ID || repo.Forge != a.Task.Repository.Forge || repo.Source == "" {
		return nil, collectionError("repository_mismatch", false, "unregistered source")
	}
	if !validOID(want.Head) || !validOID(want.Tree) || want.Branch != a.Task.Repository.TaskBranch {
		return nil, collectionError("candidate_invalid", false, "candidate must name assigned branch and full object IDs")
	}
	if root == "" {
		root, err = tmploc.Dir()
		if err != nil {
			return nil, err
		}
	}
	dir, err := tmploc.MkdirTempIn(root, "crw-candidate-")
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	git := func(args ...string) (string, error) { return candidateCommand(ctx, dir, args...) }
	if _, err = git("init", "--template=", "."); err != nil {
		return nil, err
	}
	if _, err = git("fetch", "--no-tags", "--no-recurse-submodules", "--", repo.Source, want.Branch+":refs/remotes/assigned/candidate"); err != nil {
		return nil, collectionError("fetch_failed", true, "%v", err)
	}
	published, err := git("rev-parse", "--verify", "refs/remotes/assigned/candidate^{commit}")
	if err != nil {
		return nil, err
	}
	if _, err = git("cat-file", "-e", want.Head+"^{commit}"); err != nil {
		return nil, collectionError("object_missing", false, "advertised commit is absent from the fetched branch")
	}
	if _, err = git("merge-base", "--is-ancestor", want.Head, published); err != nil {
		return nil, collectionError("unpublished", false, "candidate is not reachable from the assigned published branch")
	}
	if _, err = git("checkout", "--detach", "--force", want.Head); err != nil {
		return nil, collectionError("checkout_failed", true, "%v", err)
	}
	out = &CollectedCandidate{Repository: repo.ID, Checkout: dir, PublishedHead: published}
	if err = InspectCandidate(ctx, a, want, out); err != nil {
		return nil, err
	}
	return out, nil
}

// InspectCandidate re-reads actual objects and dirty state; no envelope tree or
// remote absolute path is used as local checkout evidence.
func InspectCandidate(ctx context.Context, a Admission, want Candidate, c *CollectedCandidate) error {
	git := func(args ...string) (string, error) { return candidateCommand(ctx, c.Checkout, args...) }
	head, err := git("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if head != want.Head {
		return collectionError("head_mismatch", false, "checkout HEAD differs from the result")
	}
	tree, err := git("rev-parse", "--verify", "HEAD^{tree}")
	if err != nil {
		return err
	}
	if tree != want.Tree {
		return collectionError("tree_mismatch", false, "actual tree %s differs from %s", tree, want.Tree)
	}
	dirty, err := git("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return err
	}
	if dirty != "" {
		return collectionError("dirty_checkout", false, "collected checkout has unfinished changes")
	}
	if _, err = git("merge-base", "--is-ancestor", a.Task.Repository.BaseCommit, head); err != nil {
		return collectionError("base_mismatch", false, "candidate does not descend from the frozen base")
	}
	// No rename detection: both deleted and added paths must be in scope.
	raw, err := git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--name-only", "-z", a.Task.Repository.BaseCommit, head, "--")
	if err != nil {
		return err
	}
	paths := []string{}
	if raw != "" {
		paths = strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	}
	for _, p := range paths {
		covered := false
		for _, r := range a.Task.Regions {
			if r.Repository != a.Task.Repository.ID {
				continue
			}
			if r.Kind == "tree" && (r.Path == "." || p == r.Path || strings.HasPrefix(p, strings.TrimSuffix(r.Path, "/")+"/")) || r.Kind == "file" && p == r.Path {
				covered = true
				break
			}
		}
		if !covered {
			return collectionError("scope_decision_required", false, "changed path %q is outside file/tree declarations; symbol edits require an explicit scope decision", p)
		}
	}
	c.Head, c.Tree, c.Base, c.ChangedPaths = head, tree, a.Task.Repository.BaseCommit, paths
	return nil
}
