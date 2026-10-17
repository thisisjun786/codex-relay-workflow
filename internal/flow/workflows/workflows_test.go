package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	agentbackend "github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
)

// Every built-in workflow loads: its header parses, its helpers resolve inside the embedded library and the strict-schema check of
// crw:lib/schemas passes.
func TestBuiltinWorkflowsLoad(t *testing.T) {
	names := Names()
	if !slices.Contains(names, "project-lane") {
		t.Fatalf("built-in workflows %v", names)
	}
	for _, name := range names {
		prog, err := Load(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if prog.Meta["name"] != name {
			t.Errorf("%s: meta.name is %v", name, prog.Meta["name"])
		}
	}
	if _, err := Load("no-such-workflow"); err == nil {
		t.Errorf("a workflow that does not exist loaded")
	}
}

// Keep actual engine placement/snapshots and research operations; only agent answers
// and unrelated review/relay operations are fake.
type researchBackend struct {
	*engine.Fake
	*agentbackend.Allocator
}
type researchOp func(engine.OpRequest) any

func (f researchOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	b, err := json.Marshal(f(req))
	return engine.OpResult{Result: b}, err
}
func (researchOp) Proven(context.Context, []engine.Event, engine.Event) (bool, string, error) {
	return true, "", nil
}

func TestResearchExplorerReadsExecutorPendingTree(t *testing.T) {
	for _, path := range []string{"build", "close"} {
		t.Run(path, func(t *testing.T) { runResearchLane(t, path, true) })
	}
}

func TestScopeFixDoesNotStartResearch(t *testing.T) {
	for _, path := range []string{"build", "close"} {
		t.Run(path, func(t *testing.T) { runResearchLane(t, path, false) })
	}
}

func runResearchLane(t *testing.T, path string, research bool) {
	t.Helper()
	ctx := context.Background()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(dir string, argv ...string) string {
		t.Helper()
		out, err := ops.ExecGit("git")(ctx, dir, nil, argv...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	git(repo, "init", "-q", "-b", "main")
	writeFile := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(repo, "tracked.txt", "base\n")
	git(repo, "add", "tracked.txt")
	git(repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	base := git(repo, "rev-parse", "HEAD")
	allocator, err := agentbackend.NewAllocator(agentbackend.AllocatorConfig{Repo: repo, StateDir: t.TempDir(), RunID: "research-test"})
	if err != nil {
		t.Fatal(err)
	}
	fake := engine.NewFake()
	var mu sync.Mutex
	specs := map[string]engine.ThreadSpec{}
	var executorCwd, executorThread string
	executorTurns, explorers := 0, 0
	fake.StartHook = func(spec engine.ThreadSpec) (engine.ThreadInfo, error) {
		mu.Lock()
		defer mu.Unlock()
		specs["thr_"+spec.CallID] = spec
		return engine.ThreadInfo{Pair: spec.Pair, Cwd: spec.Cwd}, nil
	}
	fake.Handler = func(req engine.FakeRequest) engine.FakeReply {
		mu.Lock()
		defer mu.Unlock()
		spec := specs[req.ThreadID]
		if spec.Role == "explorer" {
			explorers++
			if spec.Sandbox != "read-only" || spec.Cwd == executorCwd {
				t.Errorf("explorer placement: %+v", spec)
			}
			for file, want := range map[string]string{"tracked.txt": "new git query\n", "uncommitted.txt": "question-only file\n"} {
				b, err := os.ReadFile(filepath.Join(spec.Cwd, file))
				if err != nil || string(b) != want {
					t.Errorf("explorer cannot read executor %s: %q %v", file, b, err)
				}
			}
			if git(spec.Cwd, "rev-parse", "HEAD") == base {
				t.Error("explorer still reads integration tip")
			}
			for _, want := range []string{"f_scope", "internal/other.go:17", "the new git query starts from context.Background()", "tracked.txt", "uncommitted.txt", "READ AT:", "uncommitted changes"} {
				if !strings.Contains(req.Prompt, want) {
					t.Errorf("research packet lacks %q: %s", want, req.Prompt)
				}
			}
			return engine.FakeReply{Text: `{"answer":"the new query uses Background","evidence":["tracked.txt"],"verified":["read current files"],"inferred":[],"unknown":[]}`}
		}
		executorTurns++
		if executorThread == "" {
			executorThread, executorCwd = req.ThreadID, spec.Cwd
		}
		if req.ThreadID != executorThread {
			t.Error("research restarted executor thread")
		}
		writeFile(spec.Cwd, "tracked.txt", "new git query\n")
		writeFile(spec.Cwd, "uncommitted.txt", "question-only file\n")
		outcome := "blocked_scope"
		if path == "build" && executorTurns == 1 {
			outcome = "ready_for_review"
		} else if research && explorers == 0 {
			outcome = "needs_research"
		}
		b, _ := json.Marshal(map[string]any{"outcome": outcome, "files": []string{}, "verification": []any{}, "summary": "test turn", "gap": "internal/other.go: required for the fix", "research_question": "Which context starts this new git query?", "plan_mismatch": nil})
		return engine.FakeReply{Text: string(b)}
	}
	j := engine.NewMemoryJournal()
	lane := ops.NewLane(ops.Deps{Repo: repo, RunID: "research-test", Journal: j, Decisions: &ops.MemoryDecisions{}})
	opset := lane.Ops()
	finding := map[string]any{"id": "f_scope", "severity": "P2", "kind": "code", "file": "internal/other.go", "line": 17, "text": "the new git query starts from context.Background()"}
	opset["issuePlan"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"size_fit": "executor", "regions": []string{"tracked.txt", "uncommitted.txt"}, "criteria": []string{}, "decisions": []any{}}
	})
	opset["unitCheck"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"state": "ok", "check_id": "check", "verdict": "needs_changes", "findings": []any{finding}}
	})
	opset["finalGate"] = researchOp(func(engine.OpRequest) any { return map[string]any{"state": "pass", "head": base, "base": base} })
	opset["crosscheck"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"state": "verdict", "verdict": map[string]any{"verdict": "needs_changes", "findings": []any{finding}}}
	})
	var escalation string
	opset["escalate"] = researchOp(func(req engine.OpRequest) any {
		escalation = string(req.Args)
		return map[string]any{"id": "escalation"}
	})
	body := `import { lane } from 'crw:lib/project-build'
export const meta = {name:'research-build'}
install(crw)
return await lane({id:'A'}, {}, {receipt:()=>{}})`
	if path == "close" {
		body = `import { closeMilestone } from 'crw:lib/close'
export const meta = {name:'research-close'}
install(crw)
return await closeMilestone({id:'M', title:'test', criteria:[], nodes:[{id:'A', regions:['tracked.txt','uncommitted.txt']}]})`
	}
	body = "import { install } from 'crw:lib/lane'\n" + body
	prog, err := script.Load(script.Source{Name: "research.js", Text: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(ctx, prog, engine.Config{RunID: "research-test", Backend: &researchBackend{fake, allocator}, Policy: engine.FakePolicy(), Journal: j, Ops: opset})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("lane: %v %+v", err, res)
	}
	want := 0
	if research {
		want = 1
	}
	if explorers != want {
		t.Errorf("explorers=%d, want %d; result=%s", explorers, want, res.Value)
	}
	if path == "build" && (!strings.Contains(string(res.Value), "blocked_scope") || !strings.Contains(escalation, "internal/other.go: required for the fix")) {
		t.Errorf("scope gap lost: %s %s", res.Value, escalation)
	}
	if path == "close" && !strings.Contains(string(res.Value), "close_fix_scope") {
		t.Errorf("close scope result: %s", res.Value)
	}
	if !research {
		for _, ev := range j.Events() {
			if ev.Op == ops.OpExploreRoute {
				t.Error("scope request entered research")
			}
		}
	}
	if git(repo, "rev-parse", "HEAD") != base || git(executorCwd, "rev-parse", "HEAD") != base {
		t.Fatal("original HEAD moved")
	}
	if _, err := os.Stat(filepath.Join(repo, "uncommitted.txt")); !os.IsNotExist(err) {
		t.Errorf("pending file leaked into integration repo: %v", err)
	}
	t.Logf("%s: %d explorer; executor turns=%d; result=%s", path, explorers, executorTurns, res.Value)
}

// The issue-lane workflow of a `child` session is retired (CRW-1415): a node sized for a child escalates as a size split from the
// project lane, so no built-in workflow is named issue-lane and none loads under that name.
func TestBuiltinWorkflowsHaveNoIssueLane(t *testing.T) {
	if names := Names(); slices.Contains(names, "issue-lane") {
		t.Errorf("built-in workflows %v still name issue-lane", names)
	}
	if _, err := Load("issue-lane"); err == nil {
		t.Errorf("the issue-lane workflow still loads")
	}
}

// TestLegacyLaneWorkflowsAreTheLaneBuiltins (CRW-1429): the lane scope of the legacy refusal (engine.LegacyLaneWorkflows) is the built-in
// workflows that ran the lane library: each name is a built-in, and every built-in whose source imports crw:lib/lane or crw:lib/close,
// or nests the project lane, is in the list.
func TestLegacyLaneWorkflowsAreTheLaneBuiltins(t *testing.T) {
	names := Names()
	for _, name := range engine.LegacyLaneWorkflows {
		if !slices.Contains(names, name) {
			t.Errorf("engine.LegacyLaneWorkflows names %q, which is no built-in workflow (%v)", name, names)
		}
	}
	lane := regexp.MustCompile(`from 'crw:lib/(lane|close)'|workflow\('project-lane'`)
	for _, name := range names {
		src, err := Source(name)
		if err != nil {
			t.Fatal(err)
		}
		if lane.Match(src) && !slices.Contains(engine.LegacyLaneWorkflows, name) {
			t.Errorf("%s runs the lane library and is not in engine.LegacyLaneWorkflows %v", name, engine.LegacyLaneWorkflows)
		}
	}
}

// TestNoWorkflowCallsTheNodeReviewerAnyMore (CRW-1447): the node reviewer's write path is retired. No built-in workflow and no helper of
// crw:lib calls the node reviewer (crw.review and its composites crw.reviewHalt and crw.exploreEvidence, an agent call of role reviewer,
// the spawn decisions REV_1, REV_2 and EXPL_3), and the close archives no thread (crw.archive, crw.settleReviews): it audits the
// acceptances and reports. The lane registers no review op; the rows old runs wrote stay readable under ops.OpReview.
func TestNoWorkflowCallsTheNodeReviewerAnyMore(t *testing.T) {
	retired := []string{"crw.review(", "crw.reviewHalt(", "crw.exploreEvidence(", "crw.settleReviews(", "crw.archive(", "agentType: 'reviewer'", "'REV_1'", "'REV_2'", "'EXPL_3'"}
	sources := builtinSources(t)
	for name, src := range sources {
		for _, word := range retired {
			if bytes.Contains(src, []byte(word)) {
				t.Errorf("%s holds %q of the retired node reviewer path", name, word)
			}
		}
	}
	if _, ok := ops.NewLane(ops.Deps{}).Ops()["review"]; ok {
		t.Errorf("the lane still registers crw.review")
	}
}

// TestNoWorkflowCallsReviewerFinal (CRW-1450): the reviewer-final write path is retired. No built-in workflow and no helper of crw:lib
// calls reviewer-final (crw.reviewerFinal, an agent call of role reviewer-final, the spawn decision REVF_1) or hands its schema
// VERDICT_FINAL to a call, and the lane registers no reviewerFinal op; the rows old runs wrote stay readable under ops.OpReviewerFinal.
func TestNoWorkflowCallsReviewerFinal(t *testing.T) {
	retired := []string{"crw.reviewerFinal(", "agentType: 'reviewer-final'", "'REVF_1'", "VERDICT_FINAL"}
	for name, src := range builtinSources(t) {
		for _, word := range retired {
			if bytes.Contains(src, []byte(word)) {
				t.Errorf("%s holds %q of the retired reviewer-final path", name, word)
			}
		}
	}
	if _, ok := ops.NewLane(ops.Deps{}).Ops()["reviewerFinal"]; ok {
		t.Errorf("the lane still registers crw.reviewerFinal")
	}
}

// builtinSources are the texts of every built-in workflow (by <name>.js) and of every helper of crw:lib (by lib/<name>.js).
func builtinSources(t *testing.T) map[string][]byte {
	t.Helper()
	sources := map[string][]byte{}
	for _, name := range Names() {
		src, err := Source(name)
		if err != nil {
			t.Fatal(err)
		}
		sources[name+".js"] = src
	}
	helpers, err := fs.Glob(lib.FS, "*.js")
	if err != nil || len(helpers) == 0 {
		t.Fatalf("the helper library: %v %v", helpers, err)
	}
	for _, name := range helpers {
		src, err := fs.ReadFile(lib.FS, name)
		if err != nil {
			t.Fatal(err)
		}
		sources["lib/"+name] = src
	}
	return sources
}
