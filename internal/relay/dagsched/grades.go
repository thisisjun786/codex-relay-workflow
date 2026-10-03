package dagsched

import (
	"path"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// The grades of an edit region and the rules a release is judged by (CRW-409). The grade says how an overlap on a place is settled when two nodes edit it in parallel; the judgement
// of a candidate against the nodes that hold regions turns the grades of every overlapping pair into one release rule (releaserule.go).
const (
	GradeIndependent = "independent" // nobody else edits this place; an overlap contradicts the claim and is judged exclusive
	GradeMechanical  = "mechanical"  // the result of an overlap is fixed by a rule: union, renumber or regenerate:<command>
	GradeLocal       = "local"       // the same file, another clause, symbol or a small hunk
	GradeExclusive   = "exclusive"   // the overlap cannot be settled at merge time
)

// The release rules: how a candidate is released (or why it is not) given what it overlaps.
const (
	RuleIndependent     = "independent"      // it overlaps nothing that is held
	RuleMechanical      = "mechanical"       // every overlap is settled by a rule both sides name
	RuleLocalOptimistic = "local-optimistic" // an overlap is released and settled by the later child at its base refresh
	RuleDefer           = "defer"            // an overlap is exclusive: defer:edit_overlap
)

// The rules a mechanical region names. A regenerate rule carries the command that rebuilds the file.
const (
	RuleUnion          = "union"
	RuleRenumber       = "renumber"
	RuleRegeneratePref = "regenerate:"
)

// MaxRuleBytes bounds the rule of a mechanical region; MaxBasisRows the basis rows a judgement keeps; RecentObservations the conflict observations of a plan that count as recent.
const (
	MaxRuleBytes       = 200
	MaxBasisRows       = 16
	RecentObservations = 20
)

// The shared contract surfaces (contract schema, CLI spec, contract goldens): exclusive whatever grade is declared, because two branches cannot both change a contract and be merged by a rule. They are
// places, not repositories: the list is matched by path in any repository. Contract/schema is also a hotspot (a schema directory, Classify), exclusive at its own place like the rest of the list.
var (
	sharedTrees = []string{"contract/schema", "contract/golden", "contract/fixtures"}
	sharedFiles = []string{"internal/relay/argparse/specs.json"}
)

// SharedSurface is whether a repository-relative path is on the list of shared contract surfaces: a listed directory or anything under it, a listed file, or anything under a testdata/golden directory.
func SharedSurface(p string) bool {
	p = path.Clean(p)
	for _, tree := range sharedTrees {
		if within(p, tree) {
			return true
		}
	}
	for _, file := range sharedFiles {
		if p == file {
			return true
		}
	}
	segments := strings.Split(p, "/")
	for i := 0; i+1 < len(segments); i++ {
		if segments[i] == "testdata" && segments[i+1] == "golden" {
			return true
		}
	}
	return false
}

// holdsSharedSurface is SharedSurface for a tree region: the directory is on the list, a listed directory or file lies under it, or it is a testdata directory (which holds testdata/golden). The judgement
// reads only declarations, so a tree further above (a package directory) is not looked into: it claims everything under it, and a golden directory it may hold is not guessed at.
func holdsSharedSurface(tree string) bool {
	tree = path.Clean(tree)
	if SharedSurface(tree) || path.Base(tree) == "testdata" {
		return true
	}
	for _, listed := range append(append([]string(nil), sharedTrees...), sharedFiles...) {
		if within(listed, tree) {
			return true
		}
	}
	return false
}

// validRule is whether a rule is one a mechanical region may name: union, renumber, or regenerate: followed by a command (not blank, no whitespace at its ends, no control character).
func validRule(rule string) bool {
	if len(rule) > MaxRuleBytes {
		return false
	}
	if rule == RuleUnion || rule == RuleRenumber {
		return true
	}
	command, ok := strings.CutPrefix(rule, RuleRegeneratePref)
	return ok && command != "" && command == strings.TrimSpace(command) && !hasControl(command)
}

// checkGrade validates a declared grade and rule: the grade is one of the four (empty is independent), a mechanical grade names a valid rule and no other grade names one.
func checkGrade(r Region) error {
	switch r.Grade {
	case "", GradeIndependent, GradeLocal, GradeExclusive:
		if r.Rule != "" {
			return refuse(contract.RefusalMalformedReceipt, "region %s %s names the rule %q but is graded %q: only a mechanical region names a rule", r.Repository, r.Path, r.Rule, gradeName(r.Grade))
		}
	case GradeMechanical:
		if !validRule(r.Rule) {
			return refuse(contract.RefusalMalformedReceipt, "a mechanical region (%s %s) names its rule: %s, %s or %s<command> (at most %d bytes), not %q", r.Repository, r.Path, RuleUnion, RuleRenumber, RuleRegeneratePref, MaxRuleBytes, r.Rule)
		}
	default:
		return refuse(contract.RefusalMalformedReceipt, "region grade %q is %s, %s, %s or %s", r.Grade, GradeIndependent, GradeMechanical, GradeLocal, GradeExclusive)
	}
	return nil
}

func gradeName(g string) string {
	if g == "" {
		return GradeIndependent
	}
	return g
}

// foldedGrade is the grade and rule a region is judged at: a whole-repository hold (the Exclusive flag, the declarer's word), a shared contract surface and a delete, a rename or a hotspot (Classify; CRW-431) are
// exclusive whatever was declared, the last two at their own place only, an absent grade is independent, and a rule survives only on a mechanical grade. Declaring, reading back and judging all go through it,
// so a row stored before the list grew, or by another build, cannot sit below it.
func foldedGrade(r Region) (grade, rule string) {
	switch {
	case r.Exclusive, SharedSurface(r.Path), placeHold(r):
		return GradeExclusive, ""
	case r.Grade == "":
		return GradeIndependent, ""
	case r.Grade != GradeMechanical:
		return r.Grade, ""
	}
	return r.Grade, r.Rule
}

// EffectiveGrade is the grade a region is judged at.
func EffectiveGrade(r Region) string {
	g, _ := foldedGrade(r)
	return g
}

// wholeFile is whether a region makes its file one place, whatever symbol it names: a shared contract file, or a delete, a rename or a hotspot (CRW-431). A symbol key is a smaller place only for the ordinary edit;
// a hotspot file is one place by what it is, and a symbol that is deleted or renamed moves a name the rest of the file refers to.
func wholeFile(r Region) bool { return SharedSurface(r.Path) || placeHold(r) }

// commonPlace is the place two regions of one repository share by the place rules: a tree contains what lies under it, a path is itself, and two symbols of one file overlap only when they
// are the same symbol, unless either makes the file one place (wholeFile). tree says whether the shared place is a directory (the deeper side is a tree).
func commonPlace(a, b Region) (place string, tree, ok bool) {
	ap, bp := path.Clean(a.Path), path.Clean(b.Path)
	switch {
	case a.Kind == "tree" && within(bp, ap):
		return bp, b.Kind == "tree", true
	case b.Kind == "tree" && within(ap, bp):
		return ap, a.Kind == "tree", true
	case ap != bp:
		return "", false, false
	case a.Kind == "symbol" && b.Kind == "symbol" && a.Key != b.Key && !wholeFile(a) && !wholeFile(b):
		return "", false, false
	}
	return ap, false, true
}

// PairGrade is the grade of the overlap of two regions, or "" when they do not overlap. A whole-repository hold (the declarer's word) overlaps everything of its repository and is exclusive. Otherwise the overlap is judged on the
// place the regions share: exclusive when that place is, or for a directory holds, a shared contract surface (and a symbol key means nothing on one: two symbols of a listed file share the file), or when
// either side is exclusive (a delete, a rename and a hotspot are, at their place), or when either side claims independence that the overlap contradicts; mechanical when both sides name one rule; local for every other mix.
func PairGrade(a, b Region) string {
	if a.Repository != b.Repository {
		return ""
	}
	if a.Exclusive || b.Exclusive {
		return GradeExclusive
	}
	place, tree, ok := commonPlace(a, b)
	if !ok {
		return ""
	}
	if (tree && holdsSharedSurface(place)) || (!tree && SharedSurface(place)) {
		return GradeExclusive
	}
	ga, ra := foldedGrade(a)
	gb, rb := foldedGrade(b)
	switch {
	case ga == GradeExclusive || gb == GradeExclusive:
		return GradeExclusive
	case ga == GradeIndependent || gb == GradeIndependent:
		return GradeExclusive
	case ga == GradeMechanical && gb == GradeMechanical && ra == rb:
		return GradeMechanical
	}
	return GradeLocal
}

// worse is the more serious of two overlap grades ("" is none).
func worse(a, b string) string {
	if gradeWeight(b) > gradeWeight(a) {
		return b
	}
	return a
}

func gradeWeight(g string) int {
	switch g {
	case GradeMechanical:
		return 1
	case GradeLocal:
		return 2
	case GradeExclusive:
		return 3
	}
	return 0
}
