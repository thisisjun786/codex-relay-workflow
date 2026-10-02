package dagsched

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// ConflictInput names two implementation nodes of a plan and the heads of their parallel branches in a local checkout of one repository.
type ConflictInput struct{ Repository, LeftNode, RightNode, LeftHead, RightHead string }

// ConflictResult is the answer of ObserveConflicts. Left and right are the nodes in sorted order, with their heads.
type ConflictResult struct {
	ObservationID, PlanID, LeftNode, RightNode, LeftHead, RightHead, BaseSHA, Method string
	Conflicts                                                                        int
	Files                                                                            []string
	Replayed                                                                         bool
}

var commitPattern = regexp.MustCompile("^[0-9a-f]{40}$")

// ObserveConflicts records how many files git cannot merge between the heads of two parallel branches (git merge-tree --write-tree, which merges in memory and never touches a
// working tree or a ref), as the measurement criterion c7 asks for: the number of merge-tree conflicts between parallel child branches. It is a record, not a gate: nothing in the
// reading waits for it. The merge writes its trees into a throwaway object directory that borrows the checkout's objects, so the observed checkout is not changed. The pair is
// stored once for a base and two heads: a repeat is a replay, and the nodes are stored in sorted order so the question asked either way round is one row.
func (s *Scheduler) ObserveConflicts(ctx context.Context, plan, actor string, in ConflictInput) (ConflictResult, error) {
	out := ConflictResult{PlanID: plan}
	left, right, leftHead, rightHead := in.LeftNode, in.RightNode, in.LeftHead, in.RightHead
	if left == right {
		return out, refuse(contract.RefusalMalformedReceipt, "two different nodes are compared, not %s with itself", left)
	}
	if left > right {
		left, right, leftHead, rightHead = right, left, rightHead, leftHead
	}
	out.LeftNode, out.RightNode, out.LeftHead, out.RightHead = left, right, leftHead, rightHead
	if !commitPattern.MatchString(leftHead) || !commitPattern.MatchString(rightHead) {
		return out, refuse(contract.RefusalMalformedReceipt, "a head is a full 40 hexadecimal digit commit id")
	}
	if !filepath.IsAbs(in.Repository) {
		return out, refuse(contract.RefusalMalformedReceipt, "the repository is a local checkout (an absolute path), because the merge is computed from its objects")
	}
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	for _, id := range []string{left, right} {
		n, ok := nodeOf(snap, id)
		if !ok {
			return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, id)
		}
		if n.Kind != dag.NodeImplementation {
			return out, refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it has no branch to merge", id, n.Kind)
		}
	}
	parents, err := projectParents(ctx, q, snap.ProjectKey)
	if err != nil {
		return out, err
	}
	if len(parents) != 1 || parents[0] != actor {
		return out, refuse(contract.RefusalScopeRoleMismatch, "task %s is not the registered parent of project %s", actor, snap.ProjectKey)
	}
	iso, err := isolate(ctx, in.Repository)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read: %v", in.Repository, err)
	}
	defer iso.close()
	for _, head := range []string{leftHead, rightHead} {
		if code, _, err := iso.run(ctx, "cat-file", "-e", head+"^{commit}"); code != 0 {
			return out, refuse(contract.RefusalMergeTargetUnreadable, "commit %s is not in %s: fetch the branch first (%v)", head, in.Repository, err)
		}
	}
	code, base, err := iso.run(ctx, "merge-base", leftHead, rightHead)
	if code != 0 {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "the heads %s and %s have no common ancestor in %s: %v", leftHead, rightHead, in.Repository, err)
	}
	out.BaseSHA = strings.TrimSpace(base)
	files, err := iso.mergeTree(ctx, leftHead, rightHead)
	if err != nil {
		return out, err
	}
	out.Method, out.Files, out.Conflicts = "git merge-tree --write-tree", files, len(files)
	sum := shaOf([]byte(strings.Join([]string{plan, left, right, leftHead, rightHead, out.BaseSHA}, "|")))
	out.ObservationID = "dco-" + sum[:32]
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		current, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		for _, id := range []string{left, right} {
			if _, ok := nodeOf(current, id); !ok {
				return refuse(contract.RefusalUnregisteredScope, "plan %s no longer has the live node %s", plan, id)
			}
		}
		var count int64
		var method string
		var existing string
		found, err := queryOne(txCtx, tx, "SELECT observation_id, conflict_count, method FROM dag_conflict_observations WHERE plan_id = ? AND left_node_id = ? AND right_node_id = ? AND left_head = ? AND right_head = ? AND base_sha = ?",
			[]any{plan, left, right, leftHead, rightHead, out.BaseSHA}, &existing, &count, &method)
		if err != nil {
			return err
		}
		if found {
			// the same branches and base merge the same way; a different answer would mean the repository changed under the heads, which cannot happen to a commit id
			if int(count) != out.Conflicts {
				return refuse(contract.RefusalDispositionConflict, "the pair was recorded with %d conflicts and merges with %d now", count, out.Conflicts)
			}
			out.ObservationID, out.Replayed = existing, true
			return nil
		}
		_, err = tx.ExecContext(txCtx, "INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			out.ObservationID, plan, left, right, in.Repository, leftHead, rightHead, out.BaseSHA, out.Conflicts, out.Method, actor, s.now())
		return err
	})
	return out, err
}

// runGit runs one git command in a checkout (a working tree, a linked working tree or a bare repository: git is asked, not guessed) and returns its standard output. A failure carries git's own words.
func runGit(ctx context.Context, repository string, extraEnv []string, args ...string) (string, error) {
	_, out, err := runGitExit(ctx, repository, extraEnv, args...)
	return out, err
}

// runGitExit is runGit that also reports git's exit code: merge-tree uses 1 for "merged with conflicts", which is an answer and not a failure.
func runGitExit(ctx context.Context, repository string, extraEnv []string, args ...string) (int, string, error) {
	argv := args
	if repository != "" {
		argv = append([]string{"-C", repository}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = append(cleanGitEnv(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), stdout.String(), fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return -1, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// isolated is a throwaway bare repository that reads a checkout's objects through an alternates file and has nothing else of the checkout: no configuration, no hooks, no attributes, no
// index, no refs. Every git command of a conflict observation runs against it, so what the checkout's own configuration says (a merge driver is a command it would run) or the state of its
// working tree (an uncommitted .gitattributes) can neither run code nor change the answer, and the two commits are merged the same way wherever they are asked about.
type isolated struct {
	dir    string
	gitdir string
	env    []string
}

func isolate(ctx context.Context, checkout string) (*isolated, error) {
	// where the checkout keeps its objects is git's to say (a linked working tree shares the main one); only this question is put to the checkout itself
	objects, err := runGit(ctx, checkout, nil, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "dag-merge-tree-")
	if err != nil {
		return nil, err
	}
	g := &isolated{dir: dir, gitdir: filepath.Join(dir, "g.git")}
	g.env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=", "HOME=" + dir, "XDG_CONFIG_HOME=" + dir}
	if _, _, err := g.run(ctx, "init", "--bare", "-q", g.gitdir); err != nil {
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

func (g *isolated) close() { _ = os.RemoveAll(g.dir) }

// run runs git in the throwaway repository (or, for init, on the path it is given) and returns the exit code, the standard output and, for a failure, git's own words.
func (g *isolated) run(ctx context.Context, args ...string) (int, string, error) {
	argv := args
	if args[0] != "init" {
		argv = append([]string{"--git-dir=" + g.gitdir}, args...)
	}
	return runGitExit(ctx, "", g.env, argv...)
}

var treeIDPattern = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

// mergeTreeConflicts is the conflicts between two commits of a checkout, asked in a throwaway isolated repository.
func mergeTreeConflicts(ctx context.Context, checkout, left, right string) ([]string, error) {
	g, err := isolate(ctx, checkout)
	if err != nil {
		return nil, err
	}
	defer g.close()
	return g.mergeTree(ctx, left, right)
}

// mergeTree merges two commits in memory and returns the sorted names of the files that conflict (none when the merge is clean). The answer is read from git's NUL separated output (a file
// name may hold any character but NUL) and trusted only when it has the shape git gives a merge: a tree id first. Anything else, exit 1 included, is a failure to compute the merge and never
// a count.
func (g *isolated) mergeTree(ctx context.Context, left, right string) ([]string, error) {
	code, out, err := g.run(ctx, "merge-tree", "-z", "--write-tree", "--name-only", "--no-messages", left, right)
	if code != 0 && code != 1 {
		return nil, fmt.Errorf("git merge-tree could not merge %s and %s (git 2.38 or newer is needed): %w", left, right, err)
	}
	records := strings.Split(out, "\x00")
	if len(records) == 0 || !treeIDPattern.MatchString(records[0]) {
		return nil, fmt.Errorf("git merge-tree answered %q for %s and %s, which is not a merge result: %w", firstLine(out), left, right, err)
	}
	if code == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	// the tree id is followed by the conflicted file names up to the first empty record
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
		return nil, fmt.Errorf("git merge-tree reported a conflict for %s and %s and named no file", left, right)
	}
	return files, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\n\x00"); i >= 0 {
		return s[:i]
	}
	return s
}
