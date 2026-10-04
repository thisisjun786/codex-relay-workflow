package skill

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The merged-tree build check (CRW-534): two pull requests that add different files to one Go
// package can declare the same identifier. Each is green on its own base and git merges them
// without a conflict, so only a build of the merge shows it. The check merges the head with the
// base in memory, extracts the merged tree into a scratch directory and builds and vets the Go
// packages the merge changes there. Everything git does happens in the throwaway repository of
// refreshGit, which borrows the checkout's objects: the checkout is only read. The go tool runs
// on the extracted tree, so it compiles the head's code but never executes it.

const (
	mergeBuildRule = "merged_tree_build"

	mergeBuildSafeConflict = "safe side: this head is not accepted and nothing was built. A conflict is not a build finding and this check does not settle it: the candidate goes back to its child for a base refresh, or to the mechanical route its declarations name. It is not a pass until the check has run clean on a head that merges"
	mergeBuildSafeOnBase   = "safe side: nothing was built. A head that is already on the base has landed, or the wrong base was named: read the tip of the base from the forge, and name the tip seen before the landing"
	mergeBuildSafeFinding  = "safe side: the merge of this head with the base is not accepted. Each side may build alone and the merged tree does not. The candidate returns through the needs-changes verdict naming the package and the first error above, and it is not a pass until the check has run clean on the merge with the base tip"
)

func runMergeBuildCheck(args []string, stdout, stderr io.Writer) int {
	line := newCommandLine("merge-build-check", "", mergeBuildCheckSummary)
	repo := line.String("repo", ".", "a checkout of the repository (a working tree, a linked working tree or a bare repository) that holds the head and the base; it is only read")
	head := line.String("head", "", "the pull request head: a commit, or a revision of the checkout that names it (fetch it first; the check never fetches)")
	base := line.String("base", "origin/dev", "the tip of the base the head merges into, as the checkout names it")
	tags := line.String("tags", "dev", "a build tag every changed package is built under as well as without it, since a tag can change which files of a package and of what it imports compile; empty for none")
	parallel := line.Int("parallel", 4, "the -p of every go command: how many build actions run at once")
	timeout := line.Duration("timeout", 15*time.Minute, "the limit for the whole check")
	if _, code := line.parse(args, stdout, stderr); code >= 0 {
		return code
	}
	usage := func(format string, a ...any) int {
		line.usage(stderr)
		fmt.Fprintf(stderr, "%s: error: %s\n", line.Name(), fmt.Sprintf(format, a...))
		return usageExit
	}
	switch {
	case *head == "":
		return usage("--head is required")
	case *parallel < 1:
		return usage("--parallel must be at least 1")
	case *timeout <= 0:
		return usage("--timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	// a check that ran out of its time or was interrupted is a host problem (exit 3) wherever it was;
	// anything else git could not answer is an input it cannot read (exit 2)
	cannot := func(what string, err error) int {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			fmt.Fprintf(stderr, "%s: it ran out of its %s\n", line.Name(), *timeout)
			return 3
		case ctx.Err() != nil:
			fmt.Fprintf(stderr, "%s: it was interrupted\n", line.Name())
			return 3
		}
		fmt.Fprintf(stderr, "%s: cannot %s in %s: %v\n", line.Name(), what, *repo, err)
		return usageExit
	}
	g, err := openRefreshGit(ctx, *repo)
	if err != nil {
		return cannot("read it", err)
	}
	defer g.close()
	var ids [2]string
	for i, rev := range []string{*head, *base} {
		if ids[i], err = g.resolve(ctx, rev); err != nil {
			return cannot("read "+rev, err)
		}
	}
	r := &mergeBuildReport{out: stdout, head: ids[0], base: ids[1], tree: "none"}
	onBase, err := g.isAncestor(ctx, r.head, r.base)
	if err != nil {
		return cannot("read the history of "+r.head, err)
	}
	if onBase {
		return r.refuse("head_on_base", fmt.Sprintf("%s is already on the base (%s)", r.head, r.base), mergeBuildSafeOnBase, "none", "")
	}
	// The head is the first parent, as in the forge's update-branch, which merges the base into the
	// head's branch and is how a head meets a strict base: its attributes and its side decide.
	tree, conflicts, err := g.mergeTree(ctx, r.head, r.base)
	if err != nil {
		return cannot("merge "+r.head+" and "+r.base, err)
	}
	if len(conflicts) > 0 {
		return r.refuse("merge_conflict", fmt.Sprintf("git cannot merge %s and %s without a resolution (%s); a conflict is not a build finding", r.head, r.base, nameList(conflicts)), mergeBuildSafeConflict, "none", "")
	}
	r.tree = tree
	changed, err := g.differingAll(ctx, r.base, tree)
	if err != nil {
		return cannot("compare the merged tree with "+r.base, err)
	}
	dirs, skipped, notes := mergeBuildDirs(changed)
	r.skipped, r.notes = skipped, notes
	if len(dirs) == 0 {
		return r.pass("changes no Go package, so there is nothing to build")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		fmt.Fprintf(stderr, "%s: the go tool is not on PATH, so the merged tree cannot be built: %v\n", line.Name(), err)
		return 3
	}
	work, err := os.MkdirTemp(g.dir, "merge-build-")
	if err == nil {
		for _, step := range [][]string{{"read-tree", tree}, {"checkout-index", "-a", "-f"}} {
			if _, _, err = g.iso(ctx, nil, append([]string{"--work-tree=" + work}, step...)...); err != nil {
				break
			}
		}
	}
	if err != nil {
		return cannot("extract the merged tree "+tree, err)
	}
	env, err := mergeBuildEnv(g.env, filepath.Join(g.dir, "go"))
	if err != nil {
		return cannot("prepare the go environment", err)
	}
	b := &mergeBuild{goTool: goTool, work: work, tags: *tags, parallel: *parallel, env: env}
	code, err := b.run(ctx, r, dirs)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(stderr, "%s: the go tool could not finish: %v\n", line.Name(), err)
			return 3
		}
		return cannot("run the go tool", err)
	}
	return code
}

// mergeBuildDirs names the Go directories a set of changed paths reaches, and says why it leaves
// any out. A directory Go does not read (testdata, a name that starts with _ or .) is no package.
func mergeBuildDirs(changed []string) (dirs, skipped, notes []string) {
	seen := map[string]bool{}
	for _, p := range changed {
		switch name := path.Base(p); {
		case name == "go.mod" || name == "go.sum" || name == "go.work":
			if note := "note: " + name + " changed: the check covers the packages whose Go files changed, not every package a dependency change reaches"; !seen[note] {
				seen[note] = true
				notes = append(notes, note)
			}
		case strings.HasSuffix(name, ".go"):
			dir := path.Dir(p)
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if mergeBuildIgnoredDir(dir) {
				skipped = append(skipped, "skipped: "+dir+" reason=ignored_dir")
				continue
			}
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, skipped, notes
}

func mergeBuildIgnoredDir(dir string) bool {
	for _, element := range strings.Split(dir, "/") {
		if element == "testdata" || (element != "." && (strings.HasPrefix(element, "_") || strings.HasPrefix(element, "."))) {
			return true
		}
	}
	return false
}

// mergeBuildReport is what the check says: one refusal or one pass, as lines on standard output.
type mergeBuildReport struct {
	out              io.Writer
	head, base, tree string
	skipped, notes   []string
	checked          []*mergeBuildPackage // the packages of both targets
	steps            []string
}

// mergeBuildRow is one package and tag, with the steps that ran on it for either target.
type mergeBuildRow struct {
	path, tags string
	steps      []string
}

// packages counts the packages checked, by import path, however many targets and tags each had.
func (r *mergeBuildReport) packages() int {
	seen := map[string]bool{}
	for _, p := range r.checked {
		seen[p.path] = true
	}
	return len(seen)
}

// rows merges the packages of the host and of darwin into one row for each package and tag.
func (r *mergeBuildReport) rows() []*mergeBuildRow {
	var rows []*mergeBuildRow
	index := map[string]*mergeBuildRow{}
	for _, p := range r.checked {
		key := p.path + "\x00" + p.tags
		if index[key] == nil {
			index[key] = &mergeBuildRow{path: p.path, tags: p.tags}
			rows = append(rows, index[key])
		}
		index[key].steps = append(index[key].steps, p.steps...)
	}
	return rows
}

func (r *mergeBuildReport) facts(step string, packages int) {
	fmt.Fprintf(r.out, "facts: head=%s base=%s merge_tree=%s step=%s packages=%d\n", r.head, r.base, r.tree, step, packages)
}

// refuse prints a finding. output is what the failing go command wrote, cut to its first lines.
func (r *mergeBuildReport) refuse(code, detail, safe, step, output string) int {
	fmt.Fprintf(r.out, "refused: %s: %s\n", code, detail)
	r.facts(step, r.packages())
	for _, row := range r.skipped {
		fmt.Fprintln(r.out, row)
	}
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if output != "" {
		if len(lines) > 40 {
			lines = append(lines[:40], fmt.Sprintf("... and %d more lines", len(lines)-40))
		}
		for _, l := range lines {
			fmt.Fprintln(r.out, "  "+l)
		}
	}
	fmt.Fprintln(r.out, safe)
	return 1
}

func (r *mergeBuildReport) pass(what string) int {
	steps := "none"
	if len(r.steps) > 0 {
		steps = strings.Join(r.steps, ",")
	}
	rows := r.rows()
	if len(rows) > 0 {
		what = fmt.Sprintf("builds, vets and compiles its tests: %d package(s) checked, %d skipped", r.packages(), len(r.skipped))
	}
	fmt.Fprintf(r.out, "ok: %s merged with %s %s\n", r.head[:12], r.base[:12], what)
	fmt.Fprintf(r.out, "evidence: head=%s base=%s merge_tree=%s packages=%d skipped=%d steps=%s rule=%s\n", r.head, r.base, r.tree, r.packages(), len(r.skipped), steps, mergeBuildRule)
	for _, p := range rows {
		tags := ""
		if p.tags != "" {
			tags = " tags=" + p.tags
		}
		fmt.Fprintf(r.out, "checked: %s%s steps=%s\n", p.path, tags, strings.Join(p.steps, ","))
	}
	for _, row := range append(r.skipped, r.notes...) {
		fmt.Fprintln(r.out, row)
	}
	return 0
}
