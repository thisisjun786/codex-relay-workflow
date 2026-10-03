package dagsched

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
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
	// Drift is the nodes whose declarations do not cover a conflicting file (CRW-410), as recorded with the observation.
	Drift []DriftMark
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
	if err := s.requireParent(ctx, q, snap, actor); err != nil {
		return out, err
	}
	iso, err := isolate(ctx, in.Repository)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read: %v", in.Repository, err)
	}
	defer iso.close()
	pm, err := iso.measurePair(ctx, leftHead, rightHead)
	if err != nil {
		return out, err
	}
	if pm.Reason != "" {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s in %s", pm.Detail, in.Repository)
	}
	out.BaseSHA, out.Method, out.Files, out.Conflicts = pm.Base, mergeTreeMethod, pm.Files, len(pm.Files)
	// the measurement is a one-member sweep of its own (trigger manual): the observation, its files and drift, and the ledger row that orders it among the measurements of the pair
	member := SweepMember{Kind: MemberPair, LeftNode: left, RightNode: right, LeftHead: leftHead, RightHead: rightHead, LeftSource: HeadExplicit, RightSource: HeadExplicit,
		Status: MemberObserved, Files: pm.Files, Conflicts: len(pm.Files), base: pm.Base}
	res := SweepResult{PlanID: plan, Trigger: TriggerManual, Repository: in.Repository}
	if err := s.recordSweep(ctx, plan, actor, &res, repositoryNames(ctx, in.Repository), []SweepMember{member}); err != nil {
		return out, err
	}
	recorded := res.Members[0]
	out.ObservationID, out.Replayed, out.Drift = recorded.ObservationID, recorded.Status == MemberReplayed, recorded.Drift
	return out, nil
}

// mergeTreeMethod is how a conflict is measured, as every observation records it.
const mergeTreeMethod = "git merge-tree --write-tree"

// pairMeasure is what git says of two commits: their merge base and the files that do not merge, or why it could not be asked (Reason, with Detail; Missing is which commit, 1 or 2, is not in the
// checkout).
type pairMeasure struct {
	Base           string
	Files          []string
	Reason, Detail string
	Missing        int
}

// measurePair asks git whether two commits merge, in the throwaway repository. A commit the checkout lacks, and two commits with no common ancestor, are answers (Reason); a git that cannot be run or a
// merge it cannot compute is an error and never a count.
func (g *isolated) measurePair(ctx context.Context, left, right string) (pairMeasure, error) {
	for i, head := range []string{left, right} {
		code, _, err := g.run(ctx, "cat-file", "-e", head+"^{commit}")
		if code < 0 {
			return pairMeasure{}, err
		}
		if code != 0 {
			return pairMeasure{Reason: ReasonCommitMissing, Missing: i + 1, Detail: fmt.Sprintf("commit %s is not in the checkout: fetch the branch first", head)}, nil
		}
	}
	code, base, err := g.run(ctx, "merge-base", left, right)
	if code < 0 {
		return pairMeasure{}, err
	}
	if code != 0 {
		return pairMeasure{Reason: ReasonNoCommonAncestor, Detail: fmt.Sprintf("the commits %s and %s have no common ancestor", left, right)}, nil
	}
	files, err := g.mergeTree(ctx, left, right)
	if err != nil {
		return pairMeasure{}, err
	}
	return pairMeasure{Base: strings.TrimSpace(base), Files: files}, nil
}

// repositoryNames are the names an observed checkout is known by: its absolute path with links resolved and, when its origin remote names owner/name on the forge the relay talks to, that slug. A
// region is declared with one of them, and a conflict is attributed to a region only under the name the region was declared with. The origin is read here, at the observation, for the evidence; it is never given to
// the isolated repository the merge runs in, and an origin that is unset or names another host or no owner/name leaves the path alone.
func repositoryNames(ctx context.Context, checkout string) []string {
	names := []string{filepath.Clean(checkout)}
	if resolved, err := filepath.EvalSymlinks(checkout); err == nil {
		names[0] = resolved
	}
	if origin, err := runGit(ctx, checkout, nil, "config", "--get", "remote.origin.url"); err == nil {
		if slug := originSlug(origin, forgeHost()); slug != "" {
			names = append(names, slug)
		}
	}
	return names
}

// forgeHost is the host the relay's owner/name repository slugs name: GH_HOST, else github.com, as the merge lane reads a tip.
func forgeHost() string {
	if host := os.Getenv("GH_HOST"); host != "" {
		return host
	}
	return "github.com"
}

// originSlug is owner/name for a remote URL that names a repository on the given forge host: https://host/owner/name[.git], ssh://[user@]host/owner/name[.git] and the scp form [user@]host:owner/name[.git].
// A local path, another host (the same owner/name on another host is another repository), a URL with more or fewer than two path segments or anything else gives "".
func originSlug(origin, host string) string {
	origin = strings.TrimSpace(origin)
	var originHost, repoPath string
	switch {
	case strings.Contains(origin, "://"):
		u, err := url.Parse(origin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git") {
			return ""
		}
		originHost, repoPath = u.Hostname(), u.Path
	case strings.HasPrefix(origin, "/") || strings.HasPrefix(origin, "."):
		return ""
	default:
		before, after, ok := strings.Cut(origin, ":")
		if !ok {
			return ""
		}
		originHost, repoPath = before[strings.LastIndex(before, "@")+1:], after
	}
	if originHost == "" || !strings.EqualFold(originHost, host) {
		return ""
	}
	owner, name, ok := strings.Cut(strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git"), "/")
	slug := owner + "/" + name
	if !ok || strings.Contains(name, "/") || !slugPattern.MatchString(slug) || owner == "." || owner == ".." || name == "." || name == ".." {
		return ""
	}
	return slug
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
	return g.runWith(ctx, nil, args...)
}

func (g *isolated) runWith(ctx context.Context, extraEnv []string, args ...string) (int, string, error) {
	argv := args
	if args[0] != "init" {
		argv = append([]string{"--git-dir=" + g.gitdir}, args...)
	}
	return runGitExit(ctx, "", append(append([]string{}, g.env...), extraEnv...), argv...)
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
	// the attributes are the ones committed in the left head (git 2.40 and newer read them from the commit named in GIT_ATTR_SOURCE; an older git reads none), never a working tree's
	code, out, err := g.runWith(ctx, []string{"GIT_ATTR_SOURCE=" + left}, "merge-tree", "-z", "--write-tree", "--name-only", "--no-messages", left, right)
	if code != 0 && code != 1 {
		return nil, fmt.Errorf("git merge-tree could not merge %s and %s (git 2.38 or newer is needed, and 2.40 or newer to read the attributes committed in the commits): %w", left, right, err)
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
