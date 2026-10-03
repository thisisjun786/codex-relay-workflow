package dagsched

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// observe records a conflict observation of two parallel branches and the files git could not merge in owner/repo, as dag-conflict-observe leaves them.
func (f *fixture) observe(id, left, right string, conflicts int, files ...string) {
	f.t.Helper()
	f.observeIn("owner/repo", id, left, right, conflicts, files...)
}

// observeIn is observe for the repository the files are recorded under.
func (f *fixture) observeIn(repository, id, left, right string, conflicts int, files ...string) {
	f.t.Helper()
	f.exec("INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at)"+
		" VALUES (?, 'g', ?, ?, '/checkout', ?, ?, ?, ?, 'git merge-tree --write-tree', 'parent', ?)", id, left, right, dig(id + "l")[:40], dig(id + "r")[:40], dig(id + "b")[:40], conflicts, f.clock())
	for _, p := range files {
		f.exec("INSERT INTO dag_conflict_observation_files (observation_id, repository, path) VALUES (?, ?, ?)", id, repository, p)
	}
}

// jsonOf parses the document dag-ready prints for a reading.
func jsonOf(t *testing.T, r Reading) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(emitted(t, r)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func nodeJSON(t *testing.T, list any, id string) map[string]any {
	t.Helper()
	for _, n := range list.([]any) {
		if m := n.(map[string]any); m["node_id"] == id {
			return m
		}
	}
	t.Fatalf("no node %s in %v", id, list)
	return nil
}

// Criterion c4: the reading says how each judged node is released and on what basis. q overlaps p on a.go as local against local; the plan's recent observations
// are the evidence on that place: five observations, one of which conflicted on a.go of this repository, one on b.go, one with no file recorded, one clean and one on a.go of another repository.
func TestReleaseRuleAndBasisAreRecorded(t *testing.T) {
	f, _ := gradedFixture(t, map[string][]Region{"p": {gr("a.go", "local", "")}, "q": {gr("a.go", "local", ""), gr("c.go", "local", "")}}, "p", "q")
	f.observe("o1", "n1", "n2", 1, "a.go")
	f.observe("o2", "n1", "n3", 1, "b.go")
	f.observe("o3", "n2", "n3", 2) // a conflict observed before files were recorded: counted, and not attributed to any place
	f.observe("o4", "n1", "n2", 0)
	f.observeIn("owner/other", "o5", "n1", "n2", 1, "a.go") // the same path in another repository is not this place
	reading := f.read("g")
	q := reading.node("q")
	want := []BasisRow{{Holder: "p", Repository: "owner/repo", Path: "a.go", Grade: GradeLocal, CandidateGrade: GradeLocal, HolderGrade: GradeLocal, Observations: 5, Conflicts: 1, Unattributed: 2}}
	if q.Release == nil || q.Release.Rule != RuleLocalOptimistic || !reflect.DeepEqual(q.Release.Basis, want) || q.Release.Omitted != 0 {
		t.Fatalf("q release = %+v, want local-optimistic on the basis %+v", q.Release, want)
	}

	// the printed reading
	doc := jsonOf(t, reading)
	pass := doc["pass"].(map[string]any)
	if pass["overlap_count"] != float64(1) || !reflect.DeepEqual(pass["overlaps"], map[string]any{"mechanical": float64(0), "local": float64(1), "exclusive": float64(0)}) {
		t.Fatalf("pass = %v", pass)
	}
	for _, list := range []any{doc["nodes"], doc["ready"]} {
		release := nodeJSON(t, list, "q")["release"].(map[string]any)
		row := release["basis"].([]any)[0].(map[string]any)
		wantRow := map[string]any{"holder": "p", "repository": "owner/repo", "path": "a.go", "grade": "local", "candidate_grade": "local", "candidate_rule": nil, "holder_grade": "local", "holder_rule": nil,
			"recent_observations": float64(5), "recent_conflicts": float64(1), "unattributed_conflicts": float64(2)}
		if release["rule"] != "local-optimistic" || release["basis_omitted"] != float64(0) || !reflect.DeepEqual(row, wantRow) ||
			!reflect.DeepEqual(release["overlaps"], map[string]any{"mechanical": float64(0), "local": float64(1), "exclusive": float64(0)}) {
			t.Fatalf("q release = %v", release)
		}
	}
	if release := nodeJSON(t, doc["nodes"], "p")["release"].(map[string]any); release["rule"] != "independent" || len(release["basis"].([]any)) != 0 {
		t.Fatalf("p release = %v", release)
	}

	// the pass record keeps the same: one release object per judged node in the pass's node list
	recorded, seq := f.recordPass("g")
	if seq != 1 || recorded.node("q").Release.Rule != RuleLocalOptimistic {
		t.Fatalf("recorded = %d %+v", seq, recorded.node("q").Release)
	}
	var stored []any
	if err := json.Unmarshal([]byte(f.passRows("g")[0].dispositions), &stored); err != nil {
		t.Fatal(err)
	}
	release := nodeJSON(t, stored, "q")["release"].(map[string]any)
	row := release["basis"].([]any)[0].(map[string]any)
	if release["rule"] != "local-optimistic" || row["holder"] != "p" || row["path"] != "a.go" || row["recent_conflicts"] != float64(1) ||
		!reflect.DeepEqual(release["overlaps"], map[string]any{"mechanical": float64(0), "local": float64(1), "exclusive": float64(0)}) {
		t.Fatalf("the pass record holds %v", release)
	}
	if rel := nodeJSON(t, stored, "p")["release"].(map[string]any); rel["rule"] != "independent" {
		t.Fatalf("the pass record holds %v for p", rel)
	}
}

// The reason of a deferral names the rule and the row that decided it, and is still the reason the release path refuses with.
func TestDeferredReasonCarriesTheRuleAndTheDecidingRow(t *testing.T) {
	_, reading := gradedFixture(t, map[string][]Region{"p": {gr("a.go", "local", ""), gr("b.go", "exclusive", "")}, "q": {gr("a.go", "local", ""), gr("b.go", "mechanical", "union")}}, "p", "q")
	q := reading.node("q")
	if q.Reason != DeferEditOverlap || q.Disposition != DispDefer {
		t.Fatalf("q = %+v", q)
	}
	for _, want := range []string{"its edit regions overlap a node that is running or accepted and not yet landed, or a region is undeclared", "release rule defer", "exclusive overlap with p on b.go", "candidate mechanical (union)", "holder exclusive"} {
		if !strings.Contains(q.Detail, want) {
			t.Errorf("detail %q does not say %q", q.Detail, want)
		}
	}
	// the rows are the worst grade first
	if got := q.Release.Basis; len(got) != 2 || got[0].Path != "b.go" || got[0].Grade != GradeExclusive || got[1].Path != "a.go" || got[1].Grade != GradeLocal {
		t.Fatalf("basis = %+v", got)
	}
	if got := q.Release.Basis[0]; got.CandidateGrade != GradeMechanical || got.CandidateRule != "union" || got.HolderGrade != GradeExclusive || got.HolderRule != "" {
		t.Fatalf("the exclusive row = %+v", got)
	}
}

// The basis is the plan's latest twenty observations; an older one is history.
func TestBasisReadsTheLatestObservationsOnly(t *testing.T) {
	f, _ := gradedFixture(t, map[string][]Region{"p": {gr("a.go", "local", "")}, "q": {gr("a.go", "local", "")}}, "p", "q")
	for i := 0; i < 5; i++ {
		f.observe(fmt.Sprintf("old%d", i), "n1", "n2", 1, "a.go")
	}
	for i := 0; i < RecentObservations; i++ {
		f.observe(fmt.Sprintf("new%02d", i), "n1", "n2", 1, "b.go")
	}
	row := f.read("g").node("q").Release.Basis[0]
	if row.Observations != RecentObservations || row.Conflicts != 0 || row.Unattributed != 0 {
		t.Fatalf("row = %+v, want the twenty newest observations, none of which touched a.go", row)
	}
	f.observe("newest", "n1", "n3", 1, "a.go")
	if row := f.read("g").node("q").Release.Basis[0]; row.Observations != RecentObservations || row.Conflicts != 1 {
		t.Fatalf("row = %+v, want the newest observation in and the oldest of the twenty out", row)
	}
}

// A tree region covers the files under it; a file region covers its file; a symbol region its file. The row's place is the deeper of the two paths.
func TestBasisPlaceFollowsTheRegionKinds(t *testing.T) {
	f, _ := gradedFixture(t, map[string][]Region{
		"p": {{Repository: "owner/repo", Path: "internal", Kind: "tree", Change: "edit", Grade: "local"}},
		"q": {gr("internal/x/y.go", "local", "")},
	}, "p", "q")
	f.observe("o1", "n1", "n2", 1, "internal/x/y.go")
	f.observe("o2", "n1", "n3", 1, "internal/other.go")
	f.observe("o3", "n2", "n3", 1, "elsewhere.go")
	row := f.read("g").node("q").Release.Basis[0]
	if row.Path != "internal/x/y.go" || row.Conflicts != 1 || row.Observations != 3 {
		t.Fatalf("row = %+v, want the file's place with the one observation that touched it", row)
	}
}

// No more than MaxBasisRows rows are kept, worst grade first; the count says how many were left out, and the overlap counts still cover every overlap.
func TestBasisIsBounded(t *testing.T) {
	var p, q []Region
	for i := 0; i < MaxBasisRows+4; i++ {
		p = append(p, gr(fmt.Sprintf("f%02d.go", i), "local", ""))
		q = append(q, gr(fmt.Sprintf("f%02d.go", i), "local", ""))
	}
	_, reading := gradedFixture(t, map[string][]Region{"p": p, "q": q}, "p", "q")
	release := reading.node("q").Release
	if len(release.Basis) != MaxBasisRows || release.Omitted != 4 || release.Overlaps != (OverlapCounts{Local: 1}) {
		t.Fatalf("release = %d rows, %d omitted, %+v", len(release.Basis), release.Omitted, release.Overlaps)
	}
}

// Two readings of one store state are equal, and a change of grade that releases the same nodes is still a different reading.
func TestReleaseRuleIsPartOfTheReadingDigest(t *testing.T) {
	_, mechanical := gradedFixture(t, map[string][]Region{"p": {gr("a.go", "mechanical", "union")}, "q": {gr("a.go", "mechanical", "union")}}, "p", "q")
	f, local := gradedFixture(t, map[string][]Region{"p": {gr("a.go", "mechanical", "union")}, "q": {gr("a.go", "mechanical", "renumber")}}, "p", "q")
	if strings.Join(mechanical.readyIDs(), ",") != "p,q" || strings.Join(local.readyIDs(), ",") != "p,q" {
		t.Fatal("both plans release both nodes")
	}
	if mechanical.node("q").Release.Rule == local.node("q").Release.Rule {
		t.Fatalf("the plans were meant to be released under different rules: %+v %+v", mechanical.node("q").Release, local.node("q").Release)
	}
	if mechanical.InputDigest == local.InputDigest {
		t.Fatal("a reading released under another rule shares a digest")
	}
	if first, second := emitted(t, local), emitted(t, f.read("g")); first != second {
		t.Fatalf("two readings of one store state differ:\n%s\n%s", first, second)
	}
}

// A store whose zone predates the two tables (a runtime older than this build opened it last, and a read-only open creates nothing) reads as it always did.
func TestReadingToleratesAZoneWithoutTheGradeTables(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("g", 0, "g-r1", addNode("p", dag.NodeImplementation), addNode("q", dag.NodeImplementation))
	for _, node := range []string{"p", "q"} {
		f.exec("INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at) VALUES ('g', ?, 1, 'owner/repo', 'a.go', 'file', '', 'edit', 0, 'parent', 't')", node)
	}
	f.exec("INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at) VALUES ('o1', 'g', 'n1', 'n2', '/c', ?, ?, ?, 1, 'm', 'parent', 't')", dig("l")[:40], dig("r")[:40], dig("b")[:40])
	f.exec("DROP TABLE dag_conflict_observation_files")
	f.exec("DROP TABLE dag_node_region_grades")
	reading := f.read("g")
	q := reading.node("q")
	if got := strings.Join(reading.readyIDs(), ","); got != "p" || q.Reason != DeferEditOverlap || q.Release.Rule != RuleDefer {
		t.Fatalf("ready = %q q = %+v", got, q)
	}
	if row := q.Release.Basis[0]; row.Observations != 1 || row.Conflicts != 0 || row.Unattributed != 1 {
		t.Fatalf("row = %+v, want the observation counted and its files unrecorded", row)
	}
}

// dag-conflict-observe leaves the conflicted files with the observation, under each name the observed checkout is known by (its path with links resolved and, when its origin remote
// names owner/name, that slug), and a repeat of the observation leaves the rows alone.
func TestObserveConflictsRecordsTheConflictedFiles(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.commit("c.txt", lines(12, nil))
	repo.commit("d.txt", lines(12, nil))
	repo.git("remote", "add", "origin", "https://github.com/owner/repo.git")
	left, right := repo.parallel(
		map[string]string{"c.txt": lines(12, map[int]string{3: "left"}), "d.txt": lines(12, map[int]string{5: "left"})},
		map[string]string{"c.txt": lines(12, map[int]string{3: "right"}), "d.txt": lines(12, map[int]string{5: "right"})})
	in := ConflictInput{Repository: repo.path, LeftNode: "D", RightNode: "I", LeftHead: left, RightHead: right}
	first, err := k.sched.ObserveConflicts(contextBackground(), "g", "parent", in)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(repo.path)
	if err != nil {
		t.Fatal(err)
	}
	files := func() string {
		rows, err := k.s.DB.Query("SELECT repository, path FROM dag_conflict_observation_files WHERE observation_id = ? ORDER BY repository, path", first.ObservationID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var r, p string
			if err := rows.Scan(&r, &p); err != nil {
				t.Fatal(err)
			}
			out = append(out, r+" "+p)
		}
		return strings.Join(out, ",")
	}
	want := strings.Join([]string{resolved + " c.txt", resolved + " d.txt", "owner/repo c.txt", "owner/repo d.txt"}, ",")
	if got := files(); got != want {
		t.Fatalf("files = %q, want %q", got, want)
	}
	// the origin is evidence metadata of the first observation: changing it afterwards changes no row, and the repeat is a replay
	repo.git("remote", "set-url", "origin", "https://github.com/other/name.git")
	again, err := k.sched.ObserveConflicts(contextBackground(), "g", "parent", in)
	if err != nil || !again.Replayed || files() != want {
		t.Fatalf("the repeat = %+v, %v, files %q", again, err, files())
	}
	// a checkout whose origin names no owner/name (a path, a host with one segment, a deeper path) is known by its path alone
	for _, origin := range []string{"", "/srv/git/repo.git", "/srv/owner/repo.git", "https://example.com/repo.git", "ssh://git@example.com/a/b/c/d.git", "file:///srv/owner/repo.git", "not a url"} {
		if slug := originSlug(origin, "github.com"); slug != "" {
			t.Errorf("originSlug(%q) = %q, want none", origin, slug)
		}
	}
	// only the forge the relay talks to names a repository: the same owner/name on another host is another repository
	for _, origin := range []string{"https://gitlab.com/owner/repo.git", "git@gitlab.com:owner/repo.git", "ssh://git@gitlab.com/owner/repo.git", "https://github.com.evil.example/owner/repo.git"} {
		if slug := originSlug(origin, "github.com"); slug != "" {
			t.Errorf("originSlug(%q) = %q on github.com, want none", origin, slug)
		}
		if slug := originSlug(origin, "gitlab.com"); origin != "https://github.com.evil.example/owner/repo.git" && slug != "owner/repo" {
			t.Errorf("originSlug(%q) = %q on gitlab.com, want owner/repo", origin, slug)
		}
	}
	for origin, want := range map[string]string{
		"https://github.com/owner/repo.git":     "owner/repo",
		"https://github.com/owner/repo":         "owner/repo",
		"git@github.com:owner/repo.git":         "owner/repo",
		"ssh://git@github.com/owner/repo.git":   "owner/repo",
		"https://user:token@github.com/o/n.git": "o/n",
		"https://github.com/owner/repo.git/":    "owner/repo",
	} {
		if got := originSlug(origin, "github.com"); got != want {
			t.Errorf("originSlug(%q) = %q, want %q", origin, got, want)
		}
	}
}

// The scheduler's page describes the grades, the rules, the two tables and the counts as built.
func TestSchedulerPageDescribesTheGrades(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"`independent`", "`mechanical`", "`local`", "`exclusive`", "`union`", "`renumber`", "`regenerate:<command>`", "`local-optimistic`", "`defer`",
		"`overlap_count`", "`overlaps`", "`release`", "`basis`", "dag_node_region_grades", "dag_conflict_observation_files"} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/relay/dag-scheduler.md does not mention %s", want)
		}
	}
	plans, err := os.ReadFile("../../../docs/relay/dag-plans.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"dag_node_region_grades", "dag_conflict_observation_files"} {
		if !strings.Contains(string(plans), want) {
			t.Errorf("docs/relay/dag-plans.md does not list %s", want)
		}
	}
}

// The command line: grade and rule in the declaration document, in its answer, and in what dag-ready prints.
func TestCLIRegionGrades(t *testing.T) {
	state, _ := cliState(t)
	regions := `[{"repository":"owner/repo","path":"internal/x.go","kind":"file","grade":"mechanical","rule":"union"},{"repository":"owner/repo","path":"b.go","kind":"file","grade":"local"},{"repository":"owner/repo","path":"c.go","kind":"file"}]`
	out, code := crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", regions)
	m := parseOut(t, out)
	if code != 0 || m["declaration_seq"] != float64(1) {
		t.Fatalf("declare: exit %d\n%s", code, out)
	}
	got := m["regions"].([]any)
	row := func(i int) map[string]any { return got[i].(map[string]any) }
	if len(got) != 3 || row(0)["path"] != "b.go" || row(0)["grade"] != "local" || row(0)["rule"] != nil || row(1)["grade"] != "independent" ||
		row(2)["path"] != "internal/x.go" || row(2)["grade"] != "mechanical" || row(2)["rule"] != "union" || row(2)["exclusive"] != false {
		t.Fatalf("regions = %v", got)
	}
	if out, code := crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", regions); code != 0 || parseOut(t, out)["replayed"] != true {
		t.Fatalf("replay: exit %d\n%s", code, out)
	}
	for _, c := range []struct {
		name, regions string
		code          int
		reason        string
	}{
		{"a mechanical grade without a rule", `[{"repository":"owner/repo","path":"a.go","kind":"file","grade":"mechanical"}]`, 2, "malformed_receipt"},
		{"a rule that is not one", `[{"repository":"owner/repo","path":"a.go","kind":"file","grade":"mechanical","rule":"merge"}]`, 2, "malformed_receipt"},
		{"a grade that is not one", `[{"repository":"owner/repo","path":"a.go","kind":"file","grade":"shared"}]`, 2, "malformed_receipt"},
		{"an unknown field", `[{"repository":"owner/repo","path":"a.go","kind":"file","colour":"red"}]`, 4, ""},
	} {
		out, code := crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", c.regions)
		if code != c.code || (c.reason != "" && parseOut(t, out)["reason"] != c.reason) {
			t.Errorf("%s: exit %d\n%s", c.name, code, out)
		}
	}
	out, code = crw(t, state, "dag-ready", "--plan", "p1")
	pass, _ := parseOut(t, out)["pass"].(map[string]any)
	if code != 0 || pass["overlap_count"] != float64(0) || !reflect.DeepEqual(pass["overlaps"], map[string]any{"mechanical": float64(0), "local": float64(0), "exclusive": float64(0)}) {
		t.Fatalf("dag-ready: exit %d pass %v\n%s", code, pass, out)
	}
}
