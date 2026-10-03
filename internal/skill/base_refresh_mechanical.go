package skill

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// The mechanical-resolution check of a conflicted base refresh (CRW-412). check proves a head that is
// a pure merge of the base tip; a head that carries a resolution is not one. This proves a resolution
// by the rules the plan declared for the places that conflict: it passes only when every conflict lies
// in a mechanical region, the head differs from the clean three-way result only inside such regions,
// and inside them is exactly what the region's rule makes: a union keeps every line of both sides, and
// a regeneration command, run twice, leaves the head's files as they are. A conflict anywhere else, a
// difference anywhere else and a rule it cannot check (renumber) are refused, and the candidate goes
// back to its child.

// mechanicalResolution names the rule a pass applied, in the merged mark and the merge record.
const mechanicalResolution = "mechanical_resolution"

const mechanicalSafeSide = "safe side: this head is not accepted. Return the candidate to its child, naming the code and the paths above. Nothing here turns a refusal into a pass: a hand recheck only decides what the correction says"

const defaultRegenerateTimeout = 10 * time.Minute

// stringList is a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runBaseRefreshMechanical(args []string, stdout, stderr io.Writer) int {
	line := newCommandLine("base-refresh", "mechanical", "Check that the head is the previous head merged with the base tip, every conflict settled by the rule the plan declared for its place, and nothing else.\n\nExit 0: it is. Exit 1: it is not (the first line names why). Exit 2: git or a regeneration command could not answer, or the arguments or the declarations cannot be read.")
	repo := line.String("repo", ".", "a checkout of the repository (a working tree, a linked working tree or a bare repository) that holds the three commits")
	previous := line.String("previous", "", "the head that was verified, or the head an earlier proof of this chain named")
	head := line.String("head", "", "the head after the update, with the conflicts resolved")
	base := line.String("base", "", "the tip of the base the update merged, as check reads it")
	repository := line.String("repository", "", "the forge repository, owner/name, whose regions count")
	var regionFiles stringList
	line.Var(&regionFiles, "regions", "a node's declared regions, the JSON list dag-region-declare reads; give it once for the candidate and once for each node whose landing the conflict comes from")
	timeout := line.Duration("regenerate-timeout", defaultRegenerateTimeout, "how long one run of a regeneration command may take")
	if _, code := line.parse(args, stdout, stderr); code >= 0 {
		return code
	}
	for _, f := range []struct{ name, value string }{{"previous", *previous}, {"head", *head}, {"base", *base}, {"repository", *repository}} {
		if f.value == "" {
			line.usage(stderr)
			fmt.Fprintf(stderr, "%s: error: --%s is required\n", line.Name(), f.name)
			return usageExit
		}
	}
	if len(regionFiles) == 0 || *timeout <= 0 {
		line.usage(stderr)
		if len(regionFiles) == 0 {
			fmt.Fprintf(stderr, "%s: error: --regions is required\n", line.Name())
		} else {
			fmt.Fprintf(stderr, "%s: error: --regenerate-timeout is a positive duration\n", line.Name())
		}
		return usageExit
	}
	cov, err := readRegionSets(regionFiles, *repository)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot read the regions: %v\n", line.Name(), err)
		return usageExit
	}
	// git gets the usual bound; each regeneration command gets its own, twice
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout+time.Duration(2*len(cov.regenerateCommands()))*(*timeout))
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
	proof, why, err := g.proveMechanical(ctx, ids[0], ids[1], ids[2], cov, *timeout)
	if err != nil {
		fmt.Fprintf(stderr, "%s: cannot settle %s against %s and %s in %s: %v\n", line.Name(), ids[1], ids[0], ids[2], *repo, err)
		return usageExit
	}
	if why != nil {
		fmt.Fprintf(stdout, "refused: %s: %s\n", why.code, why.detail)
		fmt.Fprintf(stdout, "facts: previous=%s dev_tip=%s head=%s parents=%s merge_tree=%s head_tree=%s\n", ids[0], ids[2], ids[1], orNone(strings.Join(why.facts.parents, ",")), orNone(why.facts.mergeTree), orNone(why.facts.headTree))
		fmt.Fprintln(stdout, why.safe)
		return 1
	}
	fmt.Fprintf(stdout, "ok: %s is %s plus the tip of %s (%s) with every conflict settled by a declared mechanical rule and nothing else\n", proof.head, proof.previous, *base, proof.devTip)
	fmt.Fprintf(stdout, "evidence: previous=%s dev_tip=%s head=%s tree=%s rule=%s\n", proof.previous, proof.devTip, proof.head, proof.tree, mechanicalResolution)
	fmt.Fprintf(stdout, "parents: (%s, %s) in that order\n", proof.previous, proof.devTip)
	for _, applied := range proof.applied {
		fmt.Fprintln(stdout, applied.line)
	}
	return 0
}

// mechanicalProof is what a pass proves; applied has one line for each resolved path, by path.
type mechanicalProof struct {
	previous, devTip, head, tree string
	applied                      []appliedRule
}

type appliedRule struct{ path, line string }

// stageEntry is one stage of a conflicted file in the index git merge-tree describes: 1 is the merge
// base's, 2 the first commit's and 3 the second's.
type stageEntry struct{ mode, oid string }

type conflictStages [4][]stageEntry

// mergeOutcome is what git merges from two commits: the tree it writes (clean for a path that merged,
// with conflict markers for one that did not) and the conflicted paths with their stages.
type mergeOutcome struct {
	tree      string
	conflicts map[string]*conflictStages
}

func (g *refreshGit) mergeConflicts(ctx context.Context, first, second string) (*mergeOutcome, error) {
	// the attributes are the first commit's, as in mergeTree
	code, out, err := g.iso(ctx, nil, "--attr-source="+first, "merge-tree", "-z", "--write-tree", "--no-messages", first, second)
	if code != 0 && code != 1 {
		return nil, fmt.Errorf("git merge-tree could not merge %s and %s: %w", first, second, err)
	}
	records := strings.Split(out, "\x00")
	if len(records) == 0 || !treeIDPattern.MatchString(records[0]) {
		return nil, fmt.Errorf("git merge-tree answered %q for %s and %s, which is not a merge result", firstLine(out), first, second)
	}
	merged := &mergeOutcome{tree: records[0], conflicts: map[string]*conflictStages{}}
	if code == 0 {
		return merged, nil
	}
	for _, rec := range records[1:] {
		if rec == "" {
			break
		}
		meta, name, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 || name == "" || !commitIDPattern.MatchString(fields[1]) {
			return nil, fmt.Errorf("git merge-tree wrote %q, which is not a conflicted file entry", rec)
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 1 || stage > 3 {
			return nil, fmt.Errorf("git merge-tree wrote the stage %q for %s", fields[2], name)
		}
		stages := merged.conflicts[name]
		if stages == nil {
			stages = &conflictStages{}
			merged.conflicts[name] = stages
		}
		stages[stage] = append(stages[stage], stageEntry{mode: fields[0], oid: fields[1]})
	}
	if len(merged.conflicts) == 0 {
		return nil, fmt.Errorf("git merge-tree reported a conflict for %s and %s and named no file", first, second)
	}
	return merged, nil
}

// differingAll names every path in which two trees differ, by content, mode or type.
func (g *refreshGit) differingAll(ctx context.Context, computed, actual string) ([]string, error) {
	_, out, err := g.iso(ctx, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", computed, actual)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range strings.Split(out, "\x00") {
		if n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

func (g *refreshGit) mergeBases(ctx context.Context, a, b string) ([]string, error) {
	code, out, err := g.iso(ctx, nil, "merge-base", "--all", a, b)
	switch code {
	case 0:
		return strings.Fields(out), nil
	case 1:
		return nil, nil
	}
	return nil, err
}

func (g *refreshGit) blob(ctx context.Context, oid string) (string, error) {
	_, out, err := g.iso(ctx, nil, "cat-file", "blob", oid)
	return out, err
}

// sideChanges is what one side did to its files since the merge base, read without rename detection: a
// path's status (A, M, D, T) and whether the side deleted anything.
type sideChanges struct {
	status  map[string]string
	deleted bool
}

func (g *refreshGit) sideChanges(ctx context.Context, base, tip string) (*sideChanges, error) {
	_, out, err := g.iso(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", "--name-status", base, tip)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	changes := &sideChanges{status: map[string]string{}}
	for i := 0; i+1 < len(fields) && fields[i] != ""; i += 2 {
		changes.status[fields[i+1]] = fields[i]
		if fields[i] == "D" {
			changes.deleted = true
		}
	}
	return changes, nil
}

// plainTextConflict is why a conflicted path is not a conflict of text that both sides have, or "".
// git reports other conflicts (a deleted file, a rename, a change of mode, a link) with other stages.
func plainTextConflict(stages *conflictStages) (mode, why string) {
	if len(stages[2]) != 1 || len(stages[3]) != 1 || len(stages[1]) > 1 {
		return "", "its stages are not those of one file that both sides changed or added"
	}
	mode = stages[2][0].mode
	for _, stage := range stages[1:] {
		for _, entry := range stage {
			if entry.mode != mode || (mode != "100644" && mode != "100755") {
				return "", fmt.Sprintf("it is not a regular file of one mode in every stage (%s and %s)", mode, entry.mode)
			}
		}
	}
	return mode, ""
}

// proveMechanical decides whether head is previous merged with tip, every conflict settled by the rule
// declared for its place and nothing else. It returns the proof, or the reason the head is something
// else; an error means git or a command could not answer. The reasons come in an order that gives the
// most specific one: what the head is, then which places are outside the declarations, then what each
// rule finds.
func (g *refreshGit) proveMechanical(ctx context.Context, previous, head, tip string, cov coverage, timeout time.Duration) (*mechanicalProof, *refreshRefusal, error) {
	facts, why, err := g.shape(ctx, previous, head, tip)
	if err != nil || why != nil {
		return nil, why, err
	}
	refuse := func(code, format string, args ...any) (*mechanicalProof, *refreshRefusal, error) {
		return nil, &refreshRefusal{code: code, detail: fmt.Sprintf(format, args...), safe: mechanicalSafeSide, facts: facts}, nil
	}
	merged, err := g.mergeConflicts(ctx, previous, tip)
	if err != nil {
		return nil, nil, err
	}
	facts.mergeTree = merged.tree
	_, treeOut, err := g.iso(ctx, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return nil, nil, err
	}
	headTree := strings.TrimSpace(treeOut)
	facts.headTree = headTree
	differing, err := g.differingAll(ctx, merged.tree, headTree)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var resolved []string
	for _, p := range differing {
		seen[p] = true
		resolved = append(resolved, p)
	}
	for p := range merged.conflicts {
		if !seen[p] {
			resolved = append(resolved, p)
		}
	}
	sort.Strings(resolved)
	if len(resolved) == 0 {
		return refuse("nothing_resolved", "the tree of %s is what git merges from %s and %s, with no conflict: nothing was settled by a rule, and the proof of such a head is check", head, previous, tip)
	}
	rules := map[string]string{}
	var openConflicts, openDifferences []string
	for _, p := range resolved {
		if rule, ok := cov.ruleFor(p); ok {
			rules[p] = rule
		} else if _, conflicted := merged.conflicts[p]; conflicted {
			openConflicts = append(openConflicts, p)
		} else {
			openDifferences = append(openDifferences, p)
		}
	}
	if len(openConflicts) > 0 {
		return refuse("conflict_outside_mechanical", "git cannot merge %s and %s without a resolution in %s, which no declared mechanical region covers with one rule agreed by every declaration given (a file or tree region of the mechanical grade, no symbol or other grade touching the path, no shared contract surface)", previous, tip, nameList(openConflicts))
	}
	if len(openDifferences) > 0 {
		return refuse("differs_outside_mechanical", "the tree of %s differs from the clean three-way result of %s and %s in %s, which no declared mechanical region covers with one rule", head, previous, tip, nameList(openDifferences))
	}
	conflicted := make([]string, 0, len(merged.conflicts))
	for p := range merged.conflicts {
		conflicted = append(conflicted, p)
	}
	sort.Strings(conflicted)
	for _, p := range conflicted {
		if _, problem := plainTextConflict(merged.conflicts[p]); problem != "" {
			return refuse("conflict_not_content", "the conflict in %s is not one a rule settles: %s", p, problem)
		}
	}
	var bases []string
	if len(conflicted) > 0 {
		if bases, err = g.mergeBases(ctx, previous, tip); err != nil {
			return nil, nil, err
		}
		if len(bases) != 1 {
			return refuse("ambiguous_base", "%s and %s have %d merge bases, so the base git merged from is a virtual one and the stages of a conflict are not those of any commit", previous, tip, len(bases))
		}
	}
	var proof mechanicalProof
	var unionPaths, renumberPaths []string
	regenerate := map[string][]string{}
	for _, p := range resolved {
		switch rule := rules[p]; {
		case rule == dagsched.RuleUnion:
			unionPaths = append(unionPaths, p)
		case rule == dagsched.RuleRenumber:
			renumberPaths = append(renumberPaths, p)
		default:
			command := strings.TrimPrefix(rule, dagsched.RuleRegeneratePref)
			regenerate[command] = append(regenerate[command], p)
		}
	}
	if len(renumberPaths) > 0 {
		return refuse("rule_unchecked", "the rule of %s is renumber, which names no id and so has no check here: a renumbered clash is settled by the child", nameList(renumberPaths))
	}
	headEntries, err := g.treeEntries(ctx, head)
	if err != nil {
		return nil, nil, err
	}
	if len(unionPaths) > 0 {
		var previousSide, devSide *sideChanges
		if len(conflicted) > 0 {
			if previousSide, err = g.sideChanges(ctx, bases[0], previous); err != nil {
				return nil, nil, err
			}
			if devSide, err = g.sideChanges(ctx, bases[0], tip); err != nil {
				return nil, nil, err
			}
		}
		for _, p := range unionPaths {
			applied, code, detail, err := g.checkUnionPath(ctx, p, merged.conflicts[p], headEntries[p], previousSide, devSide)
			if err != nil {
				return nil, nil, err
			}
			if code != "" {
				return refuse(code, "%s", detail)
			}
			proof.applied = append(proof.applied, appliedRule{p, applied})
		}
	}
	if len(regenerate) > 0 {
		devEntries, err := g.treeEntries(ctx, tip)
		if err != nil {
			return nil, nil, err
		}
		commands := make([]string, 0, len(regenerate))
		for command := range regenerate {
			commands = append(commands, command)
		}
		sort.Strings(commands)
		for _, command := range commands {
			rule := dagsched.RuleRegeneratePref + command
			scope := func(p string) bool { r, ok := cov.ruleFor(p); return ok && r == rule }
			result, err := g.regenerate(ctx, head, command, regenerate[command], scope, headEntries, devEntries, timeout)
			if err != nil {
				return nil, nil, err
			}
			if result != nil {
				return refuse(result.code, "%s", result.detail)
			}
			for _, p := range regenerate[command] {
				proof.applied = append(proof.applied, appliedRule{p, fmt.Sprintf("applied: regenerate path=%s command=%q runs=2 identical=yes matches_head=yes", p, command)})
			}
		}
	}
	sort.Slice(proof.applied, func(i, j int) bool { return proof.applied[i].path < proof.applied[j].path })
	proof.previous, proof.devTip, proof.head, proof.tree = previous, tip, head, headTree
	return &proof, nil, nil
}

// checkUnionPath applies the union rule to one resolved path: it answers the pass line, or the code
// and detail of a refusal. stages is nil for a path that merged cleanly, which a union does not settle.
func (g *refreshGit) checkUnionPath(ctx context.Context, p string, stages *conflictStages, result treeEntry, previousSide, devSide *sideChanges) (applied, code, detail string, err error) {
	if stages == nil {
		return "", "union_not_conflicted", fmt.Sprintf("%s merged cleanly and the head changed it: a union rule settles a conflict, and a clean merge is kept as git made it", p), nil
	}
	mode, _ := plainTextConflict(stages)
	// git follows a rename that a plain diff may not (its detection stops at a limit), so a path that a
	// side added is a possible rename target whenever that side deleted anything; a path a side did not
	// touch under its own name was moved there by directory-rename detection
	for _, side := range []struct {
		name    string
		changes *sideChanges
	}{{"previous head", previousSide}, {"dev tip", devSide}} {
		if status := side.changes.status[p]; status != "A" && status != "M" {
			return "", "conflict_not_content", fmt.Sprintf("the %s did not add or modify %s under its own name (status %q): a rename, a delete or a moved file is not a list that both sides extend", side.name, p, status), nil
		}
	}
	if previousSide.status[p] == "A" && devSide.status[p] == "A" && (previousSide.deleted || devSide.deleted) {
		return "", "conflict_not_content", fmt.Sprintf("both sides added %s and one of them also deleted files: git may have followed a rename, which a union does not settle", p), nil
	}
	if result.kind != "blob" || result.mode != mode {
		return "", "union_result_not_a_file", fmt.Sprintf("the head does not hold %s as a regular file of mode %s (it has %q, mode %q)", p, mode, result.kind, result.mode), nil
	}
	var base, ours, theirs, got string
	if len(stages[1]) == 1 { // no base stage: both sides added the file
		if base, err = g.blob(ctx, stages[1][0].oid); err != nil {
			return "", "", "", err
		}
	}
	if ours, err = g.blob(ctx, stages[2][0].oid); err != nil {
		return "", "", "", err
	}
	if theirs, err = g.blob(ctx, stages[3][0].oid); err != nil {
		return "", "", "", err
	}
	if got, err = g.blob(ctx, result.oid); err != nil {
		return "", "", "", err
	}
	for _, text := range []string{base, ours, theirs, got} {
		if strings.IndexByte(text, 0) >= 0 {
			return "", "conflict_not_content", fmt.Sprintf("%s holds a NUL byte in a stage or in the head: it is not text a union can read line by line", p), nil
		}
	}
	stats, failure := checkUnion([]byte(base), []byte(ours), []byte(theirs), []byte(got))
	if failure != nil {
		return "", failure.code, fmt.Sprintf("%s: %s", p, failure.detail), nil
	}
	return fmt.Sprintf("applied: union path=%s base_lines=%d previous_added=%d dev_added=%d result_lines=%d", p, stats.base, stats.previousAdded, stats.devAdded, stats.result), "", "", nil
}
