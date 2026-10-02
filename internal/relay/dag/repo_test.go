package dag

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// summary writes a snapshot as the hand-written expectations below do: node@introduced-revision, with its title when set
// and the node it supersedes; edge@introduced-revision.
func summary(s Snapshot) (nodes, edges []string) {
	for _, n := range s.Nodes {
		line := fmt.Sprintf("%s@%d", n.NodeID, n.IntroducedRev)
		if n.Title != "" {
			line += " " + n.Title
		}
		if n.SupersedesNodeID != "" {
			line += " supersedes " + n.SupersedesNodeID
		}
		nodes = append(nodes, line)
	}
	for _, e := range s.Edges {
		edges = append(edges, fmt.Sprintf("%s@%d", e.EdgeID, e.IntroducedRev))
	}
	return nodes, edges
}

func updateNode(id, kind, title string) doc {
	n := nodeDoc(id, kind)
	n["title"] = title
	return doc{"op": OpUpdateNode, "node": n}
}

// revisions are the log the tests of the fold share: the fork/join plan, then an edit of one node, then a
// change to the edges into the join, then the replacement of one branch.
func revisions() []doc {
	return []doc{
		forkJoin("plan", "r1"),
		revDoc("plan", "r2", 1, updateNode("impl-a", NodeImplementation, "Implementation A")),
		revDoc("plan", "r3", 2, change(OpRetireEdge, doc{"edge_id": "e4"}),
			doc{"op": OpAddEdge, "edge": edgeWith(edgeDoc("e6", "impl-b", "join", EdgeIntegrated), "target_base_ref", "main")}),
		revDoc("plan", "r4", 3,
			doc{"op": OpReplaceNode, "supersedes_node_id": "impl-b", "node": nodeDoc("impl-b2", NodeImplementation)},
			change(OpRetireEdge, doc{"edge_id": "e2"}), change(OpRetireEdge, doc{"edge_id": "e6"}),
			addEdge("e7", "design", "impl-b2", EdgeArtifactVerified), addEdge("e8", "impl-b2", "join", EdgeIntegrated)),
	}
}

func putAll(t *testing.T, r *Repo) []Result {
	t.Helper()
	var out []Result
	for _, d := range revisions() {
		out = append(out, mustPut(t, r, d))
	}
	return out
}

func TestPutThenSnapshotRoundTrip(t *testing.T) {
	r, _, _ := newRepo(t)
	res := mustPut(t, r, forkJoin("plan", "r1"))
	if res.Replayed || res.RevisionNo != 1 || res.ParentRevisionNo != 0 || res.ProjectKey != "P-TEST" || len(res.NodeDigests) != 5 {
		t.Fatalf("result %+v", res)
	}
	snap, head, err := r.Snapshot(context.Background(), "plan", 0)
	if err != nil || head != 1 || snap.Revision != 1 {
		t.Fatalf("snapshot: %v head %d rev %d", err, head, snap.Revision)
	}
	nodes, edges := summary(snap)
	wantNodes := []string{"design@1", "impl-a@1", "impl-b@1", "join@1", "ship@1"}
	wantEdges := []string{"e1@1", "e2@1", "e3@1", "e4@1", "e5@1"}
	if !reflect.DeepEqual(nodes, wantNodes) || !reflect.DeepEqual(edges, wantEdges) {
		t.Fatalf("plan read back: %v %v", nodes, edges)
	}
	if snap.ProjectKey != "P-TEST" || snap.PlanID != "plan" || snap.StateDigest != res.StateDigest {
		t.Fatalf("snapshot header %+v", snap)
	}
	// the fields the writer was given come back as they were given
	byID := map[string]SnapNode{}
	for _, n := range snap.Nodes {
		byID[n.NodeID] = n
	}
	if n := byID["impl-a"]; n.IssueKey != "CRW-impl-a" || n.Kind != NodeImplementation || n.CriteriaSetDigest != dig("criteria impl-a") || n.SliceDigest != res.NodeDigests["impl-a"] {
		t.Fatalf("node impl-a read back as %+v", n)
	}
	for _, e := range snap.Edges {
		if e.EdgeID == "e5" && (e.Kind != EdgeDecision || e.DecisionSubject != "merge holds" || !reflect.DeepEqual(e.RequiredAuthority, []string{"owner", "user"}) || e.DecisionDigest != dig("subject e5")) {
			t.Fatalf("decision edge read back as %+v", e)
		}
		if e.EdgeID == "e3" && (e.TargetRepository != "owner/repo" || e.TargetBaseRef != "dev" || e.PinsCodeHead) {
			t.Fatalf("integrated edge read back as %+v", e)
		}
	}
	if err := r.VerifyLog(context.Background(), "plan"); err != nil {
		t.Fatal(err)
	}
}

// Editing one node changes the digest of that node and of no other; changing the edges into a node changes that
// node alone; there is no digest of the whole plan for an edit anywhere to reach.
func TestSliceDigestsMoveOnlyWhereTheEditIs(t *testing.T) {
	r, _, _ := newRepo(t)
	res := putAll(t, r)
	changed := func(a, b map[string]string) []string {
		var out []string
		for id, d := range a {
			if b[id] != d {
				out = append(out, id)
			}
		}
		for id := range b {
			if _, ok := a[id]; !ok {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out
	}
	for i, want := range [][]string{
		{"impl-a"},                    // r2: the spec of impl-a
		{"join"},                      // r3: the edges into join
		{"impl-b", "impl-b2", "join"}, // r4: impl-b leaves, impl-b2 arrives, the edges into join change
	} {
		if got := changed(res[i].NodeDigests, res[i+1].NodeDigests); !reflect.DeepEqual(got, want) {
			t.Errorf("revision %d: digests moved for %v, want exactly %v", i+2, got, want)
		}
	}
	// design is untouched by every edit downstream of it
	for i := 1; i < len(res); i++ {
		if res[i].NodeDigests["design"] != res[0].NodeDigests["design"] {
			t.Errorf("revision %d moved the digest of design", i+1)
		}
	}
	// the slice is the node and its incoming edges and nothing else: the same node with the same incoming
	// edge in a different plan has the same digest
	other := revDoc("other", "o1", 0, addNode("design", NodeNonPR), addNode("impl-a", NodeImplementation), addNode("unrelated", NodeNonPR),
		addEdge("e1", "design", "impl-a", EdgeArtifactVerified))
	res2 := mustPut(t, r, other)
	if res2.NodeDigests["impl-a"] != res[0].NodeDigests["impl-a"] || res2.NodeDigests["design"] != res[0].NodeDigests["design"] {
		t.Fatalf("a node's digest depends on the plan around it: %v vs %v", res2.NodeDigests, res[0].NodeDigests)
	}
}

// The hand-written plan at each revision: the fold read back is what the typed changes mean.
func TestEachRevisionFoldsToWhatItsChangesMean(t *testing.T) {
	r, _, _ := newRepo(t)
	putAll(t, r)
	want := []struct{ nodes, edges []string }{
		{[]string{"design@1", "impl-a@1", "impl-b@1", "join@1", "ship@1"}, []string{"e1@1", "e2@1", "e3@1", "e4@1", "e5@1"}},
		{[]string{"design@1", "impl-a@2 Implementation A", "impl-b@1", "join@1", "ship@1"}, []string{"e1@1", "e2@1", "e3@1", "e4@1", "e5@1"}},
		{[]string{"design@1", "impl-a@2 Implementation A", "impl-b@1", "join@3", "ship@1"}, []string{"e1@1", "e2@1", "e3@1", "e5@1", "e6@3"}},
		{[]string{"design@1", "impl-a@2 Implementation A", "impl-b2@4 supersedes impl-b", "join@4", "ship@1"}, []string{"e1@1", "e3@1", "e5@1", "e7@4", "e8@4"}},
	}
	for i, w := range want {
		snap, head, err := r.Snapshot(context.Background(), "plan", int64(i+1))
		if err != nil || head != 4 || snap.Revision != int64(i+1) {
			t.Fatalf("revision %d: %v (head %d)", i+1, err, head)
		}
		nodes, edges := summary(snap)
		if !reflect.DeepEqual(nodes, w.nodes) || !reflect.DeepEqual(edges, w.edges) {
			t.Errorf("revision %d:\n nodes %v\n want  %v\n edges %v\n want  %v", i+1, nodes, w.nodes, edges, w.edges)
		}
	}
}

// The revision log has no UPDATE path: the table refuses one whoever writes it, and the product's code holds none.
func TestAppendOnlyRevisionLog(t *testing.T) {
	r, s, _ := newRepo(t)
	putAll(t, r)
	for _, statement := range []string{
		"UPDATE dag_plan_revisions SET change_json = '[]' WHERE revision_no = 2",
		"UPDATE dag_plan_revisions SET state_digest = 'x'",
		"DELETE FROM dag_plan_revisions WHERE revision_no = 4",
		"DELETE FROM dag_plan_revisions",
		"UPDATE dag_plans SET project_key = 'X'",
	} {
		if _, err := s.DB.Exec(statement); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: %v", statement, err)
		}
	}
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM dag_plan_revisions").Scan(&n); err != nil || n != 4 {
		t.Fatalf("revisions: %d (%v)", n, err)
	}
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"UPDATE dag_plan_revisions", "DELETE FROM dag_plan_revisions", "UPDATE dag_plans", "DELETE FROM dag_plans", "REPLACE INTO dag_plan"} {
			if strings.Contains(string(source), banned) {
				t.Errorf("%s holds %q: the log has no UPDATE path", name, banned)
			}
		}
	}
}

func reasonOf(err error) string {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}

// The same request id returns the revision it produced, however the plan has moved on; a request id is one request.
func TestRepeatedRequestReturnsTheStoredResult(t *testing.T) {
	r, _, _ := newRepo(t)
	ctx := context.Background()
	first := mustPut(t, r, forkJoin("plan", "r1"))
	before := countingRows(t, r)
	again := mustPut(t, r, forkJoin("plan", "r1"))
	if !again.Replayed {
		t.Fatal("a repeated request was written again")
	}
	again.Replayed = false
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("the repeated request answered differently:\n first %+v\n again %+v", first, again)
	}
	if !reflect.DeepEqual(before, countingRows(t, r)) {
		t.Fatal("a repeated request changed a row")
	}
	// the plan moves on; the old request still answers with its own revision
	mustPut(t, r, revisions()[1])
	later := mustPut(t, r, forkJoin("plan", "r1"))
	later.Replayed = false
	if !reflect.DeepEqual(first, later) {
		t.Fatalf("after revision 2 the repeated request 1 answered %+v, want %+v", later, first)
	}
	// byte-different, semantically equal documents are the same request
	reordered := forkJoin("plan", "r1")
	reordered["author_task_id"], reordered["schema"] = "task-test", SchemaRevision
	if res := mustPut(t, r, reordered); !res.Replayed {
		t.Fatal("an equal request in another spelling was not recognised")
	}
	// a request id reused for a different request is a conflict, and changes nothing
	counts := countingRows(t, r)
	_, err := r.Put(ctx, decode(t, revDoc("plan", "r1", 0, addNode("elsewhere", NodeNonPR))))
	if reasonOf(err) != "plan_revision_conflict" || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("a reused request id: %v", err)
	}
	// a stale parent is the same conflict, with its own explanation
	_, err = r.Put(ctx, decode(t, revDoc("plan", "stale", 1, addNode("late", NodeNonPR))))
	if reasonOf(err) != "plan_revision_conflict" || !strings.Contains(err.Error(), "expects parent revision 1, but plan plan is at revision 2") {
		t.Fatalf("a stale parent: %v", err)
	}
	// a parent that does not exist yet is the same
	_, err = r.Put(ctx, decode(t, revDoc("fresh", "x", 3, addNode("a", NodeNonPR))))
	if reasonOf(err) != "plan_revision_conflict" {
		t.Fatalf("a first revision claiming a parent: %v", err)
	}
	if !reflect.DeepEqual(counts, countingRows(t, r)) {
		t.Fatal("a conflicting request changed a row")
	}
}

func openMany(t *testing.T, path string, n int) []*Repo {
	t.Helper()
	repos := make([]*Repo, n)
	for i := range repos {
		repos[i] = &Repo{Store: openStore(t, path)}
	}
	return repos
}

func zoneCount(t *testing.T, r *Repo, query string) int {
	t.Helper()
	var n int
	if err := r.Store.DB.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Writers on separate connections that expect the same parent: exactly one wins each round, the rest conflict, and
// the log never branches or repeats a row.
func TestRaceWritersOnOneParent(t *testing.T) {
	_, _, path := newRepo(t)
	const writers, rounds = 6, 3
	repos := openMany(t, path, writers)
	ctx := context.Background()
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		errs := make([]error, writers)
		start := make(chan struct{})
		for i := range repos {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				var d doc
				if round == 0 {
					d = revDoc("plan", fmt.Sprintf("w%d-r%d", i, round), 0, addNode(fmt.Sprintf("n-%d-%d", round, i), NodeNonPR))
				} else {
					d = revDoc("plan", fmt.Sprintf("w%d-r%d", i, round), round, addNode(fmt.Sprintf("n-%d-%d", round, i), NodeNonPR))
				}
				<-start
				_, errs[i] = repos[i].Put(ctx, decode(t, d))
			}(i)
		}
		close(start)
		wg.Wait()
		wins, conflicts := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case reasonOf(err) == "plan_revision_conflict":
				conflicts++
			default:
				t.Fatalf("round %d: a writer failed with %v", round, err)
			}
		}
		if wins != 1 || conflicts != writers-1 {
			t.Fatalf("round %d: %d winners and %d conflicts of %d writers", round, wins, conflicts, writers)
		}
	}
	r := repos[0]
	if n := zoneCount(t, r, "SELECT count(*) FROM dag_plan_revisions"); n != rounds {
		t.Fatalf("revisions: %d, want %d", n, rounds)
	}
	if n := zoneCount(t, r, "SELECT count(DISTINCT parent_revision_no) FROM dag_plan_revisions"); n != rounds {
		t.Fatalf("distinct parents: %d: the log branched", n)
	}
	if n := zoneCount(t, r, "SELECT count(*) FROM dag_nodes"); n != rounds {
		t.Fatalf("node rows: %d, want %d (one per winning revision, no duplicates)", n, rounds)
	}
	if n := zoneCount(t, r, "SELECT count(*) FROM dag_plans"); n != 1 {
		t.Fatalf("plans: %d", n)
	}
	if err := r.VerifyLog(ctx, "plan"); err != nil {
		t.Fatal(err)
	}
}

// The same request sent by several writers at once is one revision and every writer gets it.
func TestRaceTheSameRequest(t *testing.T) {
	_, _, path := newRepo(t)
	repos := openMany(t, path, 6)
	var wg sync.WaitGroup
	results := make([]Result, len(repos))
	errs := make([]error, len(repos))
	start := make(chan struct{})
	for i := range repos {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = repos[i].Put(context.Background(), decode(t, forkJoin("plan", "same")))
		}(i)
	}
	close(start)
	wg.Wait()
	fresh := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if !results[i].Replayed {
			fresh++
		}
		if results[i].RevisionNo != 1 || results[i].StateDigest != results[0].StateDigest {
			t.Fatalf("writer %d got %+v", i, results[i])
		}
	}
	if fresh != 1 {
		t.Fatalf("%d writers wrote the revision, want exactly 1", fresh)
	}
	if n := zoneCount(t, repos[0], "SELECT count(*) FROM dag_plan_revisions"); n != 1 {
		t.Fatalf("revisions: %d", n)
	}
	if n := zoneCount(t, repos[0], "SELECT count(*) FROM dag_nodes"); n != 5 {
		t.Fatalf("node rows: %d, want 5", n)
	}
}
