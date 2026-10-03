package dagsched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The proof of a base refresh (CRW-430): in a local checkout, whether a head differs from an accepted head only by merges of the base branch. The relay answers it from git objects alone, which are
// content addressed, so any clone that holds the commits gives the same answer; nothing is written to the checkout (every git command that computes runs in a throwaway bare repository that borrows the
// checkout's objects and has no configuration, hooks, attributes, refs or replace objects of its own).

// The closed codes a refused proof carries in the detail of its disposition_conflict.
const (
	RefreshNoUpdate           = "no_update"             // the head is the accepted head: nothing was refreshed
	RefreshNotBuiltOnAccepted = "not_built_on_accepted" // the first-parent history of the head does not lead to the accepted head
	RefreshNotAMerge          = "not_a_merge"           // a commit between the heads is not a merge of exactly two parents: it is work of the child's own
	RefreshNotFromBase        = "not_from_base"         // a merged commit is not on the first-parent line of the base tip
	RefreshTreeDiffers        = "tree_differs"          // a merge holds content that git does not merge from its parents, outside the files git could not merge
	RefreshChainTooLong       = "chain_too_long"        // more merges than MaxRefreshHops lie between the heads
)

// MaxRefreshHops bounds the merges between the accepted head and the refreshed head; MaxBaseLine bounds the first-parent line of the base tip that is read to place a merged commit on it.
const (
	MaxRefreshHops = 32
	MaxBaseLine    = 200000
)

// RefreshResolved is a path git could not merge in one hop and the blob the head holds for it (empty when the resolution deleted the file): what a hand put there is the one thing the relay cannot prove,
// so it is recorded for a reviewer to read.
type RefreshResolved struct{ Path, Blob string }

// RefreshStep is one merge of the chain: the previous head (first parent), the commit of the base that was merged (second parent), the commit and its tree, and the paths that needed a hand resolution.
type RefreshStep struct {
	Previous, BaseParent, Head, Tree string
	Resolved                         []RefreshResolved
}

// refreshRefusal is an answer of the proof and not a failure: the head is something other than the accepted head plus merges of the base.
type refreshRefusal struct{ Code, Detail string }

// refreshProof is a passed proof: the merges from the accepted head to the refreshed head, oldest first.
type refreshProof struct{ Steps []RefreshStep }

// resolvedPaths is every path the chain needed a hand resolution for, sorted and without repeats.
func (p refreshProof) resolvedPaths() []string {
	seen := map[string]bool{}
	for _, s := range p.Steps {
		for _, r := range s.Resolved {
			seen[r.Path] = true
		}
	}
	out := make([]string, 0, len(seen))
	for path := range seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

var refreshCommitPattern = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

// refreshRepo is a throwaway bare repository that reads a checkout's objects through an alternates file.
type refreshRepo struct {
	dir, gitdir string
	env         []string
}

func refreshGitEnv(extra ...string) []string {
	return append(append(cleanGitEnv(), "GIT_NO_REPLACE_OBJECTS=1"), extra...)
}

// refreshGit runs git and returns the exit code, the standard output and, for a failure, git's own words. A non-zero exit is an answer for the callers that expect one.
func refreshGit(ctx context.Context, env []string, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return -1, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

func openRefreshRepo(ctx context.Context, checkout string) (*refreshRepo, error) {
	env := refreshGitEnv()
	_, objects, err := refreshGit(ctx, env, "-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	_, format, err := refreshGit(ctx, env, "-C", checkout, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	if format = strings.TrimSpace(format); format != "sha1" && format != "sha256" {
		return nil, fmt.Errorf("git answered %q to --show-object-format, which is not an object format this proof knows", refreshFirstLine(format))
	}
	dir, err := os.MkdirTemp("", "dag-base-refresh-")
	if err != nil {
		return nil, err
	}
	g := &refreshRepo{dir: dir, gitdir: filepath.Join(dir, "g.git")}
	g.env = refreshGitEnv("GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TEMPLATE_DIR=", "HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if _, _, err := refreshGit(ctx, g.env, "init", "--bare", "-q", "--object-format="+format, g.gitdir); err != nil {
		g.close()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(g.gitdir, "objects", "info"), 0o700); err != nil {
		g.close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(g.gitdir, "objects", "info", "alternates"), []byte(strings.TrimSpace(objects)+"\n"), 0o600); err != nil {
		g.close()
		return nil, err
	}
	return g, nil
}

func (g *refreshRepo) close() { _ = os.RemoveAll(g.dir) }

func (g *refreshRepo) run(ctx context.Context, extraEnv []string, args ...string) (int, string, error) {
	return refreshGit(ctx, append(append([]string{}, g.env...), extraEnv...), append([]string{"--git-dir=" + g.gitdir}, args...)...)
}

// hasCommit is whether the checkout holds the commit.
func (g *refreshRepo) hasCommit(ctx context.Context, id string) bool {
	code, _, _ := g.run(ctx, nil, "cat-file", "-e", id+"^{commit}")
	return code == 0
}

func (g *refreshRepo) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	code, _, err := g.run(ctx, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

func (g *refreshRepo) parents(ctx context.Context, commit string) ([]string, error) {
	_, out, err := g.run(ctx, nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != commit {
		return nil, fmt.Errorf("git rev-list answered %q for %s", refreshFirstLine(out), commit)
	}
	return fields[1:], nil
}

// firstParentLine is the commits on the first-parent history of a tip: the base branch's own line (the merges of its pull requests and its direct commits), which a merge of the base takes its second parent
// from. A commit that only lies in the ancestry of the tip, on a branch the base merged, is not on it.
func (g *refreshRepo) firstParentLine(ctx context.Context, tip string) (map[string]bool, error) {
	_, out, err := g.run(ctx, nil, "rev-list", "--first-parent", fmt.Sprintf("--max-count=%d", MaxBaseLine), tip)
	if err != nil {
		return nil, err
	}
	line := map[string]bool{}
	for _, id := range strings.Fields(out) {
		line[id] = true
	}
	return line, nil
}

func (g *refreshRepo) treeOf(ctx context.Context, commit string) (string, error) {
	_, out, err := g.run(ctx, nil, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if !refreshCommitPattern.MatchString(id) {
		return "", fmt.Errorf("git answered %q for the tree of %s", refreshFirstLine(out), commit)
	}
	return id, nil
}

// mergeTree merges two commits in memory: the tree git writes and the sorted names of the files it cannot merge (none for a clean merge). The attributes are the ones committed in the first commit, never a
// working tree's. Anything but a clean merge or a conflict is a failure to compute it.
func (g *refreshRepo) mergeTree(ctx context.Context, first, second string) (string, []string, error) {
	code, out, err := g.run(ctx, []string{"GIT_ATTR_SOURCE=" + first}, "merge-tree", "-z", "--write-tree", "--name-only", "--no-messages", first, second)
	if code != 0 && code != 1 {
		return "", nil, fmt.Errorf("git merge-tree could not merge %s and %s (git 2.40 or newer is needed): %w", first, second, err)
	}
	records := strings.Split(out, "\x00")
	if len(records) == 0 || !refreshCommitPattern.MatchString(records[0]) {
		return "", nil, fmt.Errorf("git merge-tree answered %q for %s and %s, which is not a merge result", refreshFirstLine(out), first, second)
	}
	if code == 0 {
		return records[0], nil, nil
	}
	seen := map[string]bool{}
	for _, name := range records[1:] {
		if name == "" {
			break
		}
		seen[name] = true
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	if len(files) == 0 {
		return "", nil, fmt.Errorf("git merge-tree reported a conflict for %s and %s and named no file", first, second)
	}
	return records[0], files, nil
}

// differing is the sorted paths in which two trees differ.
func (g *refreshRepo) differing(ctx context.Context, a, b string) ([]string, error) {
	_, out, err := g.run(ctx, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", a, b)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// blobAt is the blob id a commit holds for a path, empty when the commit has no such file.
func (g *refreshRepo) blobAt(ctx context.Context, commit, path string) (string, error) {
	_, out, err := g.run(ctx, nil, "ls-tree", "-z", commit, "--", path)
	if err != nil {
		return "", err
	}
	entry, _, _ := strings.Cut(out, "\x00")
	meta, name, ok := strings.Cut(entry, "\t")
	if !ok || name != path {
		return "", nil
	}
	if fields := strings.Fields(meta); len(fields) == 3 {
		return fields[2], nil
	}
	return "", nil
}

var (
	conflictStartPattern = regexp.MustCompile("(?m)^<{7}( |$)")
	conflictEndPattern   = regexp.MustCompile("(?m)^>{7}( |$)")
)

// maxMarkerScan bounds the blob read for conflict markers; a larger file is recorded by its blob id and read by the parent.
const maxMarkerScan = 8 << 20

// hasConflictMarkers is whether a resolved file still holds the start and the end line of a conflict (git's own labels differ from the working tree's, so the tree comparison alone cannot tell a file that
// was committed with its markers from one that was edited).
func (g *refreshRepo) hasConflictMarkers(ctx context.Context, blob string) (bool, error) {
	if blob == "" {
		return false, nil
	}
	_, size, err := g.run(ctx, nil, "cat-file", "-s", blob)
	if err != nil {
		return false, err
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(size), "%d", &n); err != nil || n > maxMarkerScan {
		return false, nil
	}
	_, body, err := g.run(ctx, nil, "cat-file", "blob", blob)
	if err != nil {
		return false, err
	}
	return conflictStartPattern.MatchString(body) && conflictEndPattern.MatchString(body), nil
}

// refreshPathsText names up to ten paths for a refusal.
func refreshPathsText(paths []string) string {
	if len(paths) > 10 {
		return strings.Join(paths[:10], ", ") + fmt.Sprintf(" and %d more", len(paths)-10)
	}
	return strings.Join(paths, ", ")
}

// proveBaseRefresh walks the first parents from head back to accepted. Every commit on the way must be a merge of exactly two parents, the previous commit first and a commit on the first-parent line of baseTip
// second, and must hold the tree git merges from those two parents. A merge git cannot do cleanly is accepted only when its tree differs from the tree git writes in the files git could not merge and nowhere
// else; those files are returned as resolved by hand. A refusal is an answer; an error means git could not answer (a commit the checkout does not hold is the caller's to name before this).
func proveBaseRefresh(ctx context.Context, g *refreshRepo, accepted, head, baseTip string) (*refreshProof, *refreshRefusal, error) {
	refuse := func(code, format string, args ...any) (*refreshProof, *refreshRefusal, error) {
		return nil, &refreshRefusal{Code: code, Detail: fmt.Sprintf(format, args...)}, nil
	}
	if head == accepted {
		return refuse(RefreshNoUpdate, "the head %s is the accepted head: nothing was refreshed", head)
	}
	built, err := g.isAncestor(ctx, accepted, head)
	if err != nil {
		return nil, nil, err
	}
	if !built {
		return refuse(RefreshNotBuiltOnAccepted, "the accepted head %s is not in the history of %s", accepted, head)
	}
	line, err := g.firstParentLine(ctx, baseTip)
	if err != nil {
		return nil, nil, err
	}
	var steps []RefreshStep
	for cur := head; cur != accepted; {
		if len(steps) >= MaxRefreshHops {
			return refuse(RefreshChainTooLong, "more than %d merges lie between the accepted head %s and %s", MaxRefreshHops, accepted, head)
		}
		parents, err := g.parents(ctx, cur)
		if err != nil {
			return nil, nil, err
		}
		if len(parents) != 2 {
			return refuse(RefreshNotAMerge, "%s has %d parent(s): a refresh is a chain of merges of exactly two parents, and this commit is work of the child's own", cur, len(parents))
		}
		previous, merged := parents[0], parents[1]
		if previous != accepted {
			under, err := g.isAncestor(ctx, accepted, previous)
			if err != nil {
				return nil, nil, err
			}
			if !under {
				return refuse(RefreshNotBuiltOnAccepted, "the first parent %s of %s does not contain the accepted head %s (parents swapped, or a branch that is not the accepted one was merged in)", previous, cur, accepted)
			}
		}
		if !line[merged] {
			return refuse(RefreshNotFromBase, "%s merges %s, which is not on the first-parent line of the base tip %s: a branch that is not the base, or a base that moved after the tip was read", cur, merged, baseTip)
		}
		tree, conflicts, err := g.mergeTree(ctx, previous, merged)
		if err != nil {
			return nil, nil, err
		}
		headTree, err := g.treeOf(ctx, cur)
		if err != nil {
			return nil, nil, err
		}
		step := RefreshStep{Previous: previous, BaseParent: merged, Head: cur, Tree: headTree}
		differs := map[string]bool{}
		var outside []string
		if headTree != tree {
			names, err := g.differing(ctx, tree, headTree)
			if err != nil {
				return nil, nil, err
			}
			conflicted := map[string]bool{}
			for _, c := range conflicts {
				conflicted[c] = true
			}
			for _, d := range names {
				differs[d] = true
				if !conflicted[d] {
					outside = append(outside, d)
				}
			}
		}
		if len(outside) > 0 {
			where := ""
			if len(conflicts) > 0 {
				where = " outside the files git could not merge"
			}
			return refuse(RefreshTreeDiffers, "the tree of %s is not what git merges from %s and %s; paths that differ%s: %s", cur, previous, merged, where, refreshPathsText(outside))
		}
		// a file git could not merge that the commit holds exactly as git wrote it (with its conflict markers) was never resolved
		var unresolved []string
		for _, c := range conflicts {
			if !differs[c] {
				unresolved = append(unresolved, c)
			}
		}
		if len(unresolved) > 0 {
			return refuse(RefreshTreeDiffers, "%s commits git's own conflict markers for %s, which no one resolved", cur, refreshPathsText(unresolved))
		}
		for _, c := range conflicts {
			blob, err := g.blobAt(ctx, cur, c)
			if err != nil {
				return nil, nil, err
			}
			if marked, err := g.hasConflictMarkers(ctx, blob); err != nil {
				return nil, nil, err
			} else if marked {
				return refuse(RefreshTreeDiffers, "%s commits %s with conflict markers in it, which no one resolved", cur, c)
			}
			step.Resolved = append(step.Resolved, RefreshResolved{Path: c, Blob: blob})
		}
		steps = append(steps, step)
		cur = previous
	}
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}
	return &refreshProof{Steps: steps}, nil, nil
}

// refreshFirstLine is the text up to the first line break or NUL.
func refreshFirstLine(s string) string {
	if i := strings.IndexAny(s, "\n\x00"); i >= 0 {
		return s[:i]
	}
	return s
}
