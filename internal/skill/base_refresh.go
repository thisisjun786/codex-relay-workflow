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

// maxRefreshMerges bounds the walk from the head down to the previous head: more merges than this
// are refused rather than followed. A variable so that a test can lower it.
var maxRefreshMerges = 100

// refreshTimeout bounds one whole check; it is a handful of git commands over local objects.
const refreshTimeout = 2 * time.Minute

var baseRefresh = family{name: "base-refresh", description: `Check that a pull request head is only the previously verified head plus merges of its base.

crw-run/references/merge-readiness.md#refresh-the-base-yourself-when-only-the-base-moved owns the
rule. A parent that updates a pull request branch itself (the forge's update-branch call, which
merges the base into the branch with a merge commit) may merge the result only when nothing but
that update separates the new head from the head that was verified. check answers that from git
alone: walking down from the new head, every commit must be a merge of exactly two parents whose
second parent is already on the base and whose tree is exactly what git merges from its two parents,
and the walk must end at the previous head. An edit, a hand resolution or a file of its own cannot
ride along in such a merge.

check reads the checkout and writes nothing to it, and it is evidence about the commits it names:
it does not say that a job, a review or a merge happened on them. Run it before the merge. A head
that has landed is an ancestor of its base, so a replay afterwards names the base tip seen before
the landing instead of the branch.`, commands: [][2]string{
	{"check", "report whether the head is the previous head plus merges of the base only; exit 1 when it is not"},
}}

func runBaseRefresh(args []string, stdout, stderr io.Writer) int {
	name, code, ok := baseRefresh.command(args, stdout, stderr)
	if !ok {
		return code
	}
	line := newCommandLine("base-refresh", name, "Check that the head is the previous head plus merges of the base and nothing else.\n\nExit 0: it is. Exit 1: it is not (the first line names why). Exit 2: git could not answer.")
	repo := line.String("repo", ".", "a checkout of the repository (a working tree, a linked working tree or a bare repository) that holds the three commits")
	previous := line.String("previous", "", "the head that was verified")
	head := line.String("head", "", "the head after the update")
	base := line.String("base", "", "the base branch as the checkout names it, such as origin/dev, or the tip commit seen before the head landed")
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
	merges, why, err := g.walk(ctx, ids[0], ids[1], ids[2])
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read the history between %s and %s in %s: %v\n", line.Name(), ids[0], ids[1], *repo, err)
		return usageExit
	}
	if why != nil {
		fmt.Fprintf(stdout, "refused: %s: %s\n", why.code, why.detail)
		return 1
	}
	tipInHead, err := g.isAncestor(ctx, ids[2], ids[1])
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read the history of %s in %s: %v\n", line.Name(), ids[1], *repo, err)
		return usageExit
	}
	noun := "merges"
	if len(merges) == 1 {
		noun = "merge"
	}
	fmt.Fprintf(stdout, "ok: %s is %s plus %d %s of %s\n", ids[1], ids[0], len(merges), noun, *base)
	for i := len(merges) - 1; i >= 0; i-- {
		m := merges[i]
		fmt.Fprintf(stdout, "merge %s: first parent %s, second parent %s (on %s), tree %s is what git merges from the two\n", m.commit, m.first, m.second, *base, m.tree)
	}
	if tipInHead {
		fmt.Fprintf(stdout, "head contains the tip of %s: yes\n", *base)
	} else {
		fmt.Fprintf(stdout, "note: head does not contain the tip of %s (%s): the base moved again, so the forge will still call the head behind\n", *base, ids[2])
	}
	return 0
}

// refreshRefusal is an answer, not a failure: the head is something other than the previous head
// plus merges of the base.
type refreshRefusal struct{ code, detail string }

type refreshMerge struct{ commit, first, second, tree string }

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
	dir, err := os.MkdirTemp("", "base-refresh-")
	if err != nil {
		return nil, err
	}
	g := &refreshGit{checkout: checkout, dir: dir, gitdir: filepath.Join(dir, "g.git"), env: env}
	g.isoEnv = append(refreshEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TEMPLATE_DIR=", "HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if _, _, err := gitAt(ctx, g.isoEnv, "init", "--bare", "-q", g.gitdir); err != nil {
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

// walk follows the first parents from head down to previous. It returns the merges it passed, or
// the reason the head is something else; an error means git could not answer.
func (g *refreshGit) walk(ctx context.Context, previous, head, base string) ([]refreshMerge, *refreshRefusal, error) {
	if head == previous {
		return nil, &refreshRefusal{"no_update", "the head is the previous head: nothing was updated"}, nil
	}
	var merges []refreshMerge
	for cur := head; cur != previous; {
		if len(merges) >= maxRefreshMerges {
			return nil, &refreshRefusal{"not_built_on_previous", fmt.Sprintf("more than %d merges lie between the head and %s", maxRefreshMerges, previous)}, nil
		}
		onBase, err := g.isAncestor(ctx, cur, base)
		if err != nil {
			return nil, nil, err
		}
		if onBase {
			return nil, &refreshRefusal{"not_built_on_previous", fmt.Sprintf("%s is already on the base, and the first parents from the head reached it without passing %s", cur, previous)}, nil
		}
		parents, err := g.parents(ctx, cur)
		if err != nil {
			return nil, nil, err
		}
		if len(parents) != 2 {
			return nil, &refreshRefusal{"not_a_merge", fmt.Sprintf("%s has %d parent(s); an update is a merge of exactly two", cur, len(parents))}, nil
		}
		first, second := parents[0], parents[1]
		fromBase, err := g.isAncestor(ctx, second, base)
		if err != nil {
			return nil, nil, err
		}
		if !fromBase {
			return nil, &refreshRefusal{"not_from_base", fmt.Sprintf("%s merges %s, which is not on the base", cur, second)}, nil
		}
		tree, conflicts, err := g.mergeTree(ctx, first, second)
		if err != nil {
			return nil, nil, err
		}
		if len(conflicts) > 0 {
			return nil, &refreshRefusal{"merge_conflicts", fmt.Sprintf("git cannot merge %s and %s without a resolution (%s), so %s carries one", first, second, strings.Join(conflicts, ", "), cur)}, nil
		}
		_, actualOut, err := g.iso(ctx, nil, "rev-parse", "--verify", cur+"^{tree}")
		if err != nil {
			return nil, nil, err
		}
		actual := strings.TrimSpace(actualOut)
		if actual != tree {
			paths, err := g.differing(ctx, tree, actual)
			if err != nil {
				return nil, nil, err
			}
			return nil, &refreshRefusal{"tree_differs", fmt.Sprintf("the tree of %s is not what git merges from %s and %s; paths that differ: %s", cur, first, second, paths)}, nil
		}
		merges = append(merges, refreshMerge{commit: cur, first: first, second: second, tree: tree})
		cur = first
	}
	return merges, nil, nil
}
