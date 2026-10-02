package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// forkJoin is the real Go bridge adapter and managed engine talking to a fake App Server that creates a thread per child, over the real store, with a real git repository as the base branch,
// the real merge lane and the real ancestry check. Only the forge (the pull requests and their checks) and the children's reports are scripted.
type forkJoin struct {
	*realKit
	repo    *gitRepo
	heads   map[string]string
	prs     map[string]PullRequest
	service *mergeturn.Service
	started int
	mu      sync.Mutex
	perThr  map[string]int
}

func newForkJoin(t *testing.T) *forkJoin {
	t.Helper()
	f := &forkJoin{realKit: newRealKit(t), repo: newGitRepo(t), heads: map[string]string{}, prs: map[string]PullRequest{}, perThr: map[string]int{}}
	f.sched.Tips, f.sched.Ancestry = mergeturn.TargetReader{}, GitAncestry{}.Ancestry
	f.service = &mergeturn.Service{Store: f.s, Registry: &registry.Registry{Store: f.s}, Now: f.sched.now, Delivery: mergeturn.StoreDelivery{Store: f.s}}
	settings := f.settings()
	environments := settings["environments"]
	f.host.Handle("thread/start", func(json.RawMessage) fakehost.Reply {
		f.mu.Lock()
		f.started++
		id := fmt.Sprintf("child-%d", f.started)
		f.mu.Unlock()
		r := map[string]any{}
		for key, v := range settings {
			if key != "environments" {
				r[key] = v
			}
		}
		r["thread"] = map[string]any{"id": id, "environments": environments}
		return fakehost.Reply{Result: r}
	})
	f.host.Handle("turn/start", func(raw json.RawMessage) fakehost.Reply {
		var params struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(raw, &params)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.perThr[params.ThreadID]++
		id := "standby"
		if f.perThr[params.ThreadID] > 1 {
			id = "business"
		}
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": id}}}
	})
	f.host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		f.mu.Lock()
		defer f.mu.Unlock()
		data := []any{}
		if params["archived"] != true {
			for i := 1; i <= f.started; i++ {
				data = append(data, map[string]any{"id": fmt.Sprintf("child-%d", i)})
			}
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	return f
}

func (f *forkJoin) children() int { return f.host.Count("thread/start") }

// release gives a node to a child and returns its relationship.
func (f *forkJoin) release(plan, node string) ReleaseResult {
	f.t.Helper()
	req := f.request()
	req.Base = &BaseSpec{Repository: f.repo.path, Ref: "dev"}
	res, err := f.sched.Release(context.Background(), plan, node, "parent", req)
	if err != nil || !res.Bound {
		f.t.Fatalf("release %s = %v %+v", node, err, res)
	}
	return res
}

// reported is the child finishing: its branch has a commit and the relay holds the verified report (the rows the intake, acknowledgement and ruling leave).
func (f *forkJoin) report(plan, node string, res ReleaseResult, number int64) {
	f.t.Helper()
	f.seedReport(res.RelationshipID, node, plan)
	pr := PullRequest{Repository: "owner/repo", Number: number, State: "open", HeadSHA: f.heads[node], BaseRef: "dev", BaseSHA: f.repo.git("rev-parse", "dev"), Verdict: "ready",
		RequiredDeclared: []string{"A", "B"}, RequiredReadable: true,
		Checks: []Check{{Name: "A", RunID: "1", Attempt: 1, Conclusion: "success", HeadSHA: f.heads[node]}, {Name: "B", RunID: "2", Attempt: 1, Conclusion: "success", HeadSHA: f.heads[node]}}}
	f.prs[node] = pr
	f.forge.by[fmt.Sprintf("owner/repo#%d", number)] = pr
}

func (f *forkJoin) accept(plan, node string, number int64) AcceptResult {
	f.t.Helper()
	res, err := f.sched.Accept(context.Background(), plan, node, "parent", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: number}, RuleVersion: verifier})
	if err != nil {
		f.t.Fatalf("accept %s: %v", node, err)
	}
	return res
}

// land is the parent's order after an acceptance: judge and ask the lane for a turn, check, merge, land, mark merged, observe. It returns the observation.
func (f *forkJoin) land(plan, node string, explicit ...Target) IntegrationResult {
	f.t.Helper()
	ctx := context.Background()
	judged, turn, err := f.sched.RequestMergeTurn(ctx, plan, node, "parent", MergeRequestInput{Host: "host"})
	if err != nil || !judged.Eligible() {
		f.t.Fatalf("merge turn for %s = %v %+v", node, err, judged)
	}
	id := turn["turnId"].(string)
	if grant, _ := turn["grant"].(map[string]any); grant != nil {
		if _, err := f.service.Acknowledge(ctx, id, "parent", grant["grantId"].(string), "re-read the store before merging"); err != nil {
			f.t.Fatalf("acknowledge %s: %v", node, err)
		}
	}
	pr := f.prs[node]
	var list []any
	for _, c := range pr.Checks {
		list = append(list, contract.OrderedObject{{Key: "runId", Value: c.RunID}, {Key: "name", Value: c.Name}, {Key: "headSha", Value: c.HeadSHA}, {Key: "conclusion", Value: c.Conclusion}, {Key: "attempt", Value: json.Number("1")}})
	}
	green := contract.OrderedObject{{Key: "hasNextPage", Value: false}, {Key: "pagesRead", Value: json.Number("1")}, {Key: "totalCount", Value: json.Number("0")}, {Key: "threadsSeen", Value: []any{}}, {Key: "unresolved", Value: json.Number("0")}}
	if _, err := f.service.Check(ctx, id, "parent", f.heads[node], f.repo.git("rev-parse", "dev"), list, green, []string{"A", "B"}, mergeturn.TargetReader{}); err != nil {
		f.t.Fatalf("lane check of %s: %v", node, err)
	}
	f.repo.git("merge", "-q", "--no-ff", "-m", "merge "+node, "branch-"+node)
	if _, err := f.service.Land(ctx, id, "parent", f.repo.git("rev-parse", "dev"), "", "merged by the forge", mergeturn.TargetReader{}); err != nil {
		f.t.Fatalf("landing %s: %v", node, err)
	}
	a, found, err := loadActiveAcceptance(ctx, f.s.Q(ctx), plan, node)
	if err != nil || !found {
		f.t.Fatalf("no acceptance of %s: %v", node, err)
	}
	f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', ?, ?, ?, 'merged', 'parent', ?)",
		a.RelationshipID, a.EventID, a.ExecutionGeneration, a.RevisionHash, f.clock())
	obs, err := f.sched.ObserveIntegration(ctx, plan, node, "parent", explicit)
	if err != nil || !obs.Integrated {
		f.t.Fatalf("observe %s = %v %+v", node, err, obs)
	}
	return obs
}

func (f *forkJoin) branch(node string, files ...string) {
	f.t.Helper()
	f.repo.git("checkout", "-q", "-b", "branch-"+node, "dev")
	for _, file := range files {
		f.heads[node] = f.repo.commit(file, "work of "+node)
	}
	f.repo.git("checkout", "-q", "dev")
}

// snapshot is one line of the table the capstone checks: how many children exist, which nodes the scheduler would release now and how many slots are held.
func (f *forkJoin) snapshot(plan string) string {
	f.t.Helper()
	reading := f.read(plan)
	ready := make([]string, 0)
	for _, n := range reading.Ready {
		ready = append(ready, n.NodeID)
	}
	return fmt.Sprintf("children=%d ready=[%s] held=%d", f.children(), strings.Join(ready, ","), f.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND state = 'held'"))
}

// Criterion c11, c2, c3, c5, c7, c8 end to end: a fork and a join through the real bridge, store, delivery and merge lane. Before an acceptance nothing downstream is ready; an acceptance
// alone opens no integrated edge; landing opens the fork; the join waits for both branches; every node has exactly one child however often it is woken; the slots return as nodes integrate.
func TestForkJoinEndToEnd(t *testing.T) {
	f := newForkJoin(t)
	repo := f.repo
	edge := func(id, from, to string) doc {
		return addEdge(id, from, to, dag.EdgeIntegrated, doc{"target_repository": repo.path, "target_base_ref": "dev"})
	}
	f.putPlan("fj", 0, "fj-r1", addRelNode("A", dag.NodeImplementation), addRelNode("B", dag.NodeImplementation), addRelNode("C", dag.NodeImplementation), addRelNode("D", dag.NodeImplementation),
		addRelNode("E", dag.NodeNonPR), edge("ab", "A", "B"), edge("ac", "A", "C"), edge("bd", "B", "D"), edge("cd", "C", "D"), edge("de", "D", "E"))
	for _, n := range []string{"A", "B", "C", "D"} {
		f.declare("fj", n, strings.ToLower(n)+".txt")
	}
	var table []string
	step := func(name string) {
		table = append(table, name+": "+f.snapshot("fj"))
	}
	step("plan stored")

	// A runs alone
	a := f.release("fj", "A")
	step("A released")
	f.branch("A", "a.txt")
	f.report("fj", "A", a, 5)
	step("A reported")
	f.accept("fj", "A", 5)
	step("A accepted, not yet landed")
	if n := f.read("fj").node("B"); n.Disposition != DispWait || !strings.HasPrefix(n.Reason, WaitEdgePrefix) {
		t.Fatalf("B right after A's acceptance = %+v: an acceptance alone opens no integrated edge", n)
	}
	f.land("fj", "A")
	step("A landed")

	// the fork: B and C are released together and work in parallel on branches cut from the same base
	b, c := f.release("fj", "B"), f.release("fj", "C")
	step("B and C released")
	f.branch("B", "b.txt")
	f.branch("C", "c.txt")
	if res, err := f.sched.ObserveConflicts(context.Background(), "fj", "parent", ConflictInput{Repository: repo.path, LeftNode: "B", RightNode: "C", LeftHead: f.heads["B"], RightHead: f.heads["C"]}); err != nil || res.Conflicts != 0 {
		t.Fatalf("conflicts between the parallel branches = %v %+v", err, res)
	}
	// a wrong release: D is not ready, and asking for it creates nothing
	req := f.request()
	req.Base = &BaseSpec{Repository: repo.path, Ref: "dev"}
	if _, err := f.sched.Release(context.Background(), "fj", "D", "parent", req); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release of D before its predecessors = %v", err)
	}
	f.report("fj", "B", b, 6)
	f.accept("fj", "B", 6)
	f.land("fj", "B")
	step("B landed")
	// C updates its branch to the new base before it reports (the strict gate: checks run on a tree that contains the base)
	repo.git("checkout", "-q", "branch-C")
	repo.git("merge", "-q", "--no-ff", "-m", "update C", "dev")
	f.heads["C"] = repo.git("rev-parse", "HEAD")
	repo.git("checkout", "-q", "dev")
	f.report("fj", "C", c, 7)
	f.accept("fj", "C", 7)
	step("C accepted, not yet landed")
	f.land("fj", "C")
	step("C landed")

	// the join (D has a successor, so its acceptance is judged against the base branch of that edge)
	d := f.release("fj", "D")
	step("D released")
	f.branch("D", "d.txt")
	f.report("fj", "D", d, 8)
	f.accept("fj", "D", 8)
	f.land("fj", "D")
	step("D landed")

	// duplicate wakes of every node create no child and release nothing again
	for _, node := range []string{"A", "B", "C", "D"} {
		for i := 0; i < 2; i++ {
			req := f.request()
			req.Base = &BaseSpec{Repository: repo.path, Ref: "dev"}
			again, err := f.sched.Release(context.Background(), "fj", node, "parent", req)
			if err != nil && refusalReason(err) != "disposition_conflict" {
				t.Fatalf("wake %d of %s = %v", i, node, err)
			}
			if err == nil && !again.Replayed {
				t.Fatalf("wake %d of %s released again: %+v", i, node, again)
			}
		}
	}
	step("after duplicate wakes")

	want := []string{
		"plan stored: children=0 ready=[A] held=0",
		"A released: children=1 ready=[] held=1",
		"A reported: children=1 ready=[] held=1",
		"A accepted, not yet landed: children=1 ready=[] held=1",
		"A landed: children=1 ready=[B,C] held=0",
		"B and C released: children=3 ready=[] held=2",
		"B landed: children=3 ready=[] held=1",
		"C accepted, not yet landed: children=3 ready=[] held=1",
		"C landed: children=3 ready=[D] held=0",
		"D released: children=4 ready=[] held=1",
		"D landed: children=4 ready=[E] held=0",
		"after duplicate wakes: children=4 ready=[E] held=0",
	}
	if strings.Join(table, "\n") != strings.Join(want, "\n") {
		t.Fatalf("assignment counts before and after each step:\n%s\nwant\n%s", strings.Join(table, "\n"), strings.Join(want, "\n"))
	}
	if f.count("SELECT COUNT(*) FROM relationships") != 4 || f.count("SELECT COUNT(DISTINCT child_task_id) FROM relationships") != 4 {
		t.Fatal("a node has more than one child, or two nodes share one")
	}
	if f.count("SELECT COUNT(*) FROM dag_releases") != 4 || f.count("SELECT COUNT(*) FROM dag_node_executions") != 4 {
		t.Fatal("a node has more than one release or execution")
	}
	for _, node := range []string{"A", "B", "C", "D"} {
		if n := f.read("fj").node(node); n.State != StateIntegrated {
			t.Fatalf("%s = %+v, want integrated", node, n)
		}
	}
}
