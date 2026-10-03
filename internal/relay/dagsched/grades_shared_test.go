package dagsched

import (
	"strings"
	"testing"
)

// Criterion c3 (exclusive list), the places a list of files could be slipped past: the overlap of two regions is judged on the place they share. A symbol key means nothing on a
// listed path, and a tree that holds a listed path hands it to whoever else may edit under it.
func TestSharedContractOverlapIsJudgedOnTheCommonPlace(t *testing.T) {
	const specs = "internal/relay/argparse/specs.json"
	tree := func(path, grade string) Region {
		return Region{Repository: "owner/repo", Path: path, Kind: "tree", Change: "edit", Grade: grade}
	}
	sym := func(path, key, grade string) Region {
		return Region{Repository: "owner/repo", Path: path, Kind: "symbol", Key: key, Change: "edit", Grade: grade}
	}
	cases := []struct {
		name      string
		p, q      Region
		wantReady string
		wantRule  string
	}{
		{"two commands of the CLI spec", sym(specs, "dag-ready", "local"), sym(specs, "dag-release", "local"), "p", RuleDefer},
		{"a command of the CLI spec against the whole file", sym(specs, "dag-ready", "local"), gr(specs, "local", ""), "p", RuleDefer},
		{"the whole file against a command of the CLI spec", gr(specs, "local", ""), sym(specs, "dag-ready", "local"), "p", RuleDefer},
		{"two symbols of a file that is not on the list", sym("internal/x.go", "A", "local"), sym("internal/x.go", "B", "local"), "p,q", RuleIndependent},
		{"two trees that both hold the CLI spec", tree("internal", "local"), tree("internal/relay", "local"), "p", RuleDefer},
		{"the same two trees the other way round", tree("internal/relay", "local"), tree("internal", "local"), "p", RuleDefer},
		{"a tree that holds the CLI spec against a file elsewhere under it", tree("internal", "local"), gr("internal/x.go", "local", ""), "p,q", RuleLocalOptimistic},
		{"a tree that holds the CLI spec against the file", tree("internal", "local"), gr(specs, "local", ""), "p", RuleDefer},
		{"two trees whose common tree holds nothing on the list", tree("internal/relay/dagsched", "local"), tree("internal/relay/dagsched/x", "local"), "p,q", RuleLocalOptimistic},
		{"two trees that both hold the contract directories", tree("contract", "local"), tree("contract", "local"), "p", RuleDefer},
		{"a tree that holds the contract directories against a listed one", tree("contract", "local"), tree("contract/golden", "local"), "p", RuleDefer},
		{"a tree that holds the contract directories against a file beside them", tree("contract", "local"), gr("contract/README.md", "local", ""), "p,q", RuleLocalOptimistic},
		{"a tree that holds the contract directories against an unlisted directory", tree("contract", "local"), tree("contract/notes", "local"), "p,q", RuleLocalOptimistic},
		{"two ordinary trees, one inside the other", tree("internal/app", "local"), tree("internal/app/x", "local"), "p,q", RuleLocalOptimistic},
		{"a golden directory of a package", tree("internal/relay/store/testdata", "local"), tree("internal/relay/store/testdata/golden", "local"), "p", RuleDefer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, reading := gradedFixture(t, map[string][]Region{"p": {c.p}, "q": {c.q}}, "p", "q")
			q := reading.node("q")
			if got := strings.Join(reading.readyIDs(), ","); got != c.wantReady || q.Release == nil || q.Release.Rule != c.wantRule {
				t.Fatalf("ready = %q q = %+v: %s", got, q.Release, reading.brief())
			}
		})
	}
}

// Regions of two repositories never overlap, a shared contract path included.
func TestSharedContractPathsOfTwoRepositoriesDoNotOverlap(t *testing.T) {
	other := Region{Repository: "owner/other", Path: "internal", Kind: "tree", Change: "edit", Grade: "local"}
	_, reading := gradedFixture(t, map[string][]Region{"p": {{Repository: "owner/repo", Path: "internal", Kind: "tree", Change: "edit", Grade: "local"}}, "q": {other}}, "p", "q")
	if got := strings.Join(reading.readyIDs(), ","); got != "p,q" || reading.node("q").Release.Rule != RuleIndependent {
		t.Fatalf("ready = %q: %s", got, reading.brief())
	}
}
