package skill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// refreshTimeout bounds one whole check; it is a handful of git commands over local objects.
const refreshTimeout = 2 * time.Minute

// refreshRule names the rule a pass applied, in the merged mark and the merge record. A later rule
// (another kind of resolution) is another name, so the record says which proof was made.
const refreshRule = "tree_identity"

// The last line of every refusal names the safe side. Nothing here converts a refusal into a pass:
// the three texts differ only in what the parent may do before it gives up and returns the
// candidate to its child.
const (
	refreshSafeSide      = "safe side: this head is not accepted. Return the candidate to its child. A hand recheck (git range-diff of the child's commits, the differing paths named above) only decides what the correction says; it never turns this refusal into a pass"
	refreshSafeSideChain = "safe side: this head is not accepted as one update. If it is a chain of updates, prove them one step at a time, each with the head the earlier proof named as --previous; a head that no proof names goes back to its child. It is not a pass until a proof names it"
	refreshSafeSideMoved = "safe side: this head is not accepted against the commit named. If the base moved between reading its tip and the update, read the tip of the base from the forge again and prove against that tip; a commit that no forge reading named as the tip is never one to prove against, and without a pass the candidate goes back to its child"
)

var baseRefresh = family{name: "base-refresh", description: `Check that a pull request head is the previously verified head plus the tip of its base, and nothing else.

crw-run/references/merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved owns the
rule. A parent that updates a pull request branch itself (the forge's update-branch call, which
merges the base into the branch with a merge commit) may merge the result only when nothing but
that update separates the new head from the head that was verified. check answers that from git
alone, as an identity. It passes only when all three hold: the head has exactly two parents, the
previous head first and the base tip second, in that order; git merge-tree --write-tree of those two
commits exits 0; and the tree it writes is the tree of the head. An edit, a hand resolution, a file
of its own, a second update or a reversed parent order cannot ride along in such a merge.

check reads the checkout and writes nothing to it, and it is evidence about the commits it names:
it does not say that a job, a review or a merge happened on them. A pass prints the evidence line the
merged mark and the merge record carry. Run it before the merge. A head that has landed is an
ancestor of its base, so a replay afterwards names the base tip seen before the landing instead of
the branch.`, commands: [][2]string{
	{"check", "report whether the head is the previous head plus the base tip and nothing else; exit 1 when it is not"},
}}

func runBaseRefresh(args []string, stdout, stderr io.Writer) int {
	name, code, ok := baseRefresh.command(args, stdout, stderr)
	if !ok {
		return code
	}
	line := newCommandLine("base-refresh", name, "Check that the head is the previous head merged with the base tip and nothing else.\n\nExit 0: it is. Exit 1: it is not (the first line names why). Exit 2: git could not answer.")
	repo := line.String("repo", ".", "a checkout of the repository (a working tree, a linked working tree or a bare repository) that holds the three commits")
	previous := line.String("previous", "", "the head that was verified, or the head an earlier proof of this chain named")
	head := line.String("head", "", "the head after the update")
	base := line.String("base", "", "the tip of the base the update merged: the commit read from the forge right before the update, or the base branch as the checkout names it, such as origin/dev")
	if _, code := line.parse(args[1:], stdout, stderr); code >= 0 {
		return code
	}
	for _, f := range []struct{ name, value string }{{"previous", *previous}, {"head", *head}, {"base", *base}} {
		if f.value == "" {
			line.usage(stderr)
			fmt.Fprintf(stderr, "%s: error: --%s is required\n", line.Name(), f.name)
			return usageExit
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	g, err := openRefreshGit(ctx, *repo)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read %s: %v\n", line.Name(), *repo, err)
		return usageExit
	}
	defer g.close()
	var ids [3]string
	for i, rev := range []string{*previous, *head, *base} {
		if ids[i], err = g.resolve(ctx, rev); err != nil {
			fmt.Fprintf(stderr, "%s: cannot read %s in %s: %v\n", line.Name(), rev, *repo, err)
			return usageExit
		}
	}
	proof, why, err := g.prove(ctx, ids[0], ids[1], ids[2])
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read the history between %s and %s in %s: %v\n", line.Name(), ids[0], ids[1], *repo, err)
		return usageExit
	}
	if why != nil {
		fmt.Fprintf(stdout, "refused: %s: %s\n", why.code, why.detail)
		fmt.Fprintf(stdout, "facts: previous=%s dev_tip=%s head=%s parents=%s merge_tree=%s head_tree=%s\n", ids[0], ids[2], ids[1], orNone(strings.Join(why.facts.parents, ",")), orNone(why.facts.mergeTree), orNone(why.facts.headTree))
		fmt.Fprintln(stdout, why.safe)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %s is %s plus the tip of %s (%s) and nothing else\n", proof.head, proof.previous, *base, proof.devTip)
	fmt.Fprintf(stdout, "evidence: previous=%s dev_tip=%s head=%s tree=%s rule=%s\n", proof.previous, proof.devTip, proof.head, proof.tree, refreshRule)
	fmt.Fprintf(stdout, "parents: (%s, %s) in that order\n", proof.previous, proof.devTip)
	fmt.Fprintf(stdout, "tree identity: git merge-tree --write-tree %s %s exits 0 and its tree %s is the tree of %s\n", proof.previous, proof.devTip, proof.tree, proof.head)
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// refreshRefusal is an answer, not a failure: the head is something other than the previous head
// merged with the base tip. facts is what was read on the way to it.
type refreshRefusal struct {
	code, detail string
	safe         string
	facts        refreshFacts
}

// refreshFacts are the values a refusal quotes for the correction: the head's parents, the tree git
// merges from the previous head and the tip, and the head's own tree. A value not reached is empty.
type refreshFacts struct {
	parents             []string
	mergeTree, headTree string
}

// refreshProof is what a pass proves; its four commits and trees are the fields of the evidence line.
type refreshProof struct{ previous, devTip, head, tree string }

var (
	commitIDPattern = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")
	treeIDPattern   = commitIDPattern
)

// refreshGit asks git about a checkout. The three revisions are resolved in the checkout itself,
// which reads nothing but its refs and objects. Everything else runs in a throwaway bare repository
// that borrows the checkout's objects and has no configuration, hooks, attributes, refs or replace
// objects of its own: a merge driver the checkout configures, an uncommitted .gitattributes or a
// replace ref can then neither run code nor change the answer, and nothing is written to the
// checkout (the merged trees go into the throwaway repository).
type refreshGit struct {
	checkout string
	dir      string
	gitdir   string
	env      []string // for the checkout: the caller's environment without GIT_*, replace objects off
	isoEnv   []string // for the throwaway repository: no configuration of any kind
}

func refreshEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "LC_ALL=C")
}

// gitAt runs git with an environment and returns its exit code and standard output; a non-zero
// exit carries git's own words as the error.
func gitAt(ctx context.Context, env []string, args ...string) (int, string, error) {
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

// gitVersionPattern reads the first line of `git --version`: "git version 2.53.0", and the longer
// spellings some builds print after the number.
var gitVersionPattern = regexp.MustCompile(`^git version (\d+)\.(\d+)`)

// requireGit refuses a git that cannot answer. The check merges in memory with
// `git merge-tree --write-tree` (2.38) and has it read the attributes committed in the commits
// through `--attr-source` instead of a working tree's (2.41): an older git would merge the same
// commits under other rules, accept a hand resolution as automatic or refuse an honest update, and
// say nothing about it.
func requireGit(version string) error {
	m := gitVersionPattern.FindStringSubmatch(strings.TrimSpace(firstLine(version)))
	if m == nil {
		return fmt.Errorf("git answered %q to --version, which is not a version this check can read", firstLine(version))
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || (major == 2 && minor < 41) {
		return fmt.Errorf("git 2.41 or newer is needed (merge-tree reads the attributes committed in the commits through --attr-source); this is %q", strings.TrimSpace(firstLine(version)))
	}
	return nil
}

func openRefreshGit(ctx context.Context, checkout string) (*refreshGit, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, err
	}
	env := refreshEnv()
	if _, version, err := gitAt(ctx, env, "--version"); err != nil {
		return nil, err
	} else if err := requireGit(version); err != nil {
		return nil, err
	}
	_, objects, err := gitAt(ctx, env, "-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	_, format, err := gitAt(ctx, env, "-C", checkout, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	// the throwaway repository has to hash like the checkout, or it cannot read the objects it borrows
	if format = strings.TrimSpace(format); format != "sha1" && format != "sha256" {
		return nil, fmt.Errorf("git answered %q to --show-object-format, which is not an object format this check knows", firstLine(format))
	}
	dir, err := os.MkdirTemp("", "base-refresh-")
	if err != nil {
		return nil, err
	}
	g := &refreshGit{checkout: checkout, dir: dir, gitdir: filepath.Join(dir, "g.git"), env: env}
	g.isoEnv = append(refreshEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TEMPLATE_DIR=", "HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if _, _, err := gitAt(ctx, g.isoEnv, "init", "--bare", "-q", "--object-format="+format, g.gitdir); err != nil {
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

func (g *refreshGit) close() { _ = os.RemoveAll(g.dir) }

func (g *refreshGit) iso(ctx context.Context, extraEnv []string, args ...string) (int, string, error) {
	return gitAt(ctx, append(append([]string{}, g.isoEnv...), extraEnv...), append([]string{"--git-dir=" + g.gitdir}, args...)...)
}

// resolve reads a revision as a revision, never as an option, and returns the full commit it names.
func (g *refreshGit) resolve(ctx context.Context, rev string) (string, error) {
	_, out, err := gitAt(ctx, g.env, "-C", g.checkout, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if !commitIDPattern.MatchString(id) {
		return "", fmt.Errorf("git answered %q, which is not a commit id", firstLine(out))
	}
	return id, nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\n\x00"); i >= 0 {
		return s[:i]
	}
	return s
}

func (g *refreshGit) isAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	code, _, err := g.iso(ctx, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

func (g *refreshGit) parents(ctx context.Context, commit string) ([]string, error) {
	_, out, err := g.iso(ctx, nil, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != commit {
		return nil, fmt.Errorf("git rev-list answered %q for %s", firstLine(out), commit)
	}
	return fields[1:], nil
}

// mergeTree merges two commits in memory and returns the resulting tree, or the sorted names of the
// files that conflict. Anything but a clean merge or a conflict is a failure to compute it.
func (g *refreshGit) mergeTree(ctx context.Context, first, second string) (string, []string, error) {
	// the attributes are the ones committed in the first parent, never a working tree's (the
	// throwaway repository has none): an option rather than an environment variable, so that a git
	// which does not know it fails instead of merging under other rules (requireGit checks the version)
	code, out, err := g.iso(ctx, nil, "--attr-source="+first, "merge-tree", "-z", "--write-tree", "--name-only", "--no-messages", first, second)
	if code != 0 && code != 1 {
		return "", nil, fmt.Errorf("git merge-tree could not merge %s and %s: %w", first, second, err)
	}
	records := strings.Split(out, "\x00")
	if len(records) == 0 || !treeIDPattern.MatchString(records[0]) {
		return "", nil, fmt.Errorf("git merge-tree answered %q for %s and %s, which is not a merge result", firstLine(out), first, second)
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

// differing names up to ten paths in which two trees differ.
func (g *refreshGit) differing(ctx context.Context, computed, actual string) (string, error) {
	_, out, err := g.iso(ctx, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", computed, actual)
	if err != nil {
		return "", err
	}
	var names []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 10 {
		return strings.Join(names[:10], ", ") + fmt.Sprintf(" and %d more", len(names)-10), nil
	}
	return strings.Join(names, ", "), nil
}

// prove decides whether head is previous merged with tip and nothing else: one merge commit whose
// parents are exactly (previous, tip) in that order, whose tree is the tree git writes for merging
// them. It returns the proof, or the reason the head is something else; an error means git could not
// answer. The checks run in an order that gives the most specific reason: a reversed merge writes the
// same tree as the right one under merge-ort, so the parent order is read before the tree.
func (g *refreshGit) prove(ctx context.Context, previous, head, tip string) (*refreshProof, *refreshRefusal, error) {
	if head == previous {
		return nil, &refreshRefusal{code: "no_update", detail: "the head is the previous head: nothing was updated", safe: refreshSafeSide}, nil
	}
	onBase, err := g.isAncestor(ctx, head, tip)
	if err != nil {
		return nil, nil, err
	}
	if onBase {
		return nil, &refreshRefusal{code: "not_built_on_previous", detail: fmt.Sprintf("%s is already on the base (%s): a head that has landed is checked by naming the base tip seen before the landing", head, tip), safe: refreshSafeSide}, nil
	}
	parents, err := g.parents(ctx, head)
	if err != nil {
		return nil, nil, err
	}
	facts := refreshFacts{parents: parents}
	refuse := func(safe, code, format string, args ...any) (*refreshProof, *refreshRefusal, error) {
		return nil, &refreshRefusal{code: code, detail: fmt.Sprintf(format, args...), safe: safe, facts: facts}, nil
	}
	if len(parents) != 2 {
		return refuse(refreshSafeSide, "not_a_merge", "%s has %d parent(s); an update is a merge of exactly two", head, len(parents))
	}
	first, second := parents[0], parents[1]
	if first != previous {
		if second == previous {
			return refuse(refreshSafeSide, "parents_swapped", "the parents of %s are (%s, %s): the previous head is the second parent. An update has the parents (previous head, dev tip) in that order, and the reverse merges the branch into the base", head, first, second)
		}
		contains, err := g.isAncestor(ctx, previous, first)
		if err != nil {
			return nil, nil, err
		}
		if contains {
			return refuse(refreshSafeSideChain, "not_built_on_previous", "the first parent %s of %s is not %s, but already contains it: an earlier update or a commit of its own lies between them. Prove an update one step at a time, with the head the earlier proof named as --previous", first, head, previous)
		}
		return refuse(refreshSafeSide, "not_built_on_previous", "the first parent %s of %s does not contain %s", first, head, previous)
	}
	if second != tip {
		fromBase, err := g.isAncestor(ctx, second, tip)
		if err != nil {
			return nil, nil, err
		}
		if !fromBase {
			return refuse(refreshSafeSideMoved, "not_from_base", "%s merges %s, which is not an ancestor of the commit named as the base tip (%s): a branch that is not the base, or a base that moved between reading its tip and the update", head, second, tip)
		}
		return refuse(refreshSafeSideMoved, "not_the_dev_tip", "%s merges %s, which is on the base but is not the commit named as its tip (%s): the base moved after the update, or the wrong tip was named", head, second, tip)
	}
	tree, conflicts, err := g.mergeTree(ctx, previous, tip)
	if err != nil {
		return nil, nil, err
	}
	facts.mergeTree = tree
	if len(conflicts) > 0 {
		return refuse(refreshSafeSide, "merge_conflicts", "git cannot merge %s and %s without a resolution (%s), so %s carries one", previous, tip, strings.Join(conflicts, ", "), head)
	}
	_, actualOut, err := g.iso(ctx, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return nil, nil, err
	}
	actual := strings.TrimSpace(actualOut)
	facts.headTree = actual
	if actual != tree {
		paths, err := g.differing(ctx, tree, actual)
		if err != nil {
			return nil, nil, err
		}
		return refuse(refreshSafeSide, "tree_differs", "the tree of %s is not what git merges from %s and %s; paths that differ: %s", head, previous, tip, paths)
	}
	return &refreshProof{previous: previous, devTip: tip, head: head, tree: tree}, nil, nil
}
