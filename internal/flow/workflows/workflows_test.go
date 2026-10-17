package workflows

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentbackend "github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
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

func TestDesignPhasePinsIntegratedIssueBeforeEveryAudit(t *testing.T) {
	const original = `{"issue":"A-1","node":"n1","size_fit":"executor","size_reason":"","issue_size_decision":"ok","edit_regions":["a.go"],"approach":"original","tests_first":[],"done_condition":"go test ./a","decision_refs":["D1"],"order_after":[],"group":null,"sub_tasks":null}`
	const decision = `{"id":"D1","evidence":[],"change":"kept","alternatives":[],"assumptions":[]}`
	const src = `import { designPhase } from 'crw:lib/project-design'
import { DESIGN } from 'crw:lib/schemas'
export const meta = {name:'integrated-design'}
const ms = {id:'M',plan:'P',nodes:[{id:'n1'},{id:'n2'}]}
crw.explore = async () => ({answers:[], carried:[]})
const saved = [{reused:true,node:'n1',head:'h1',source_run:'old',source_revision:'r0',issue:` + original + `,decisions:[` + decision + `]}]
const issue = ` + original + `
issue.approach += '!'
const design = {decisions:[` + decision + `], issues:[issue,Object.assign({},issue,{node:'n2',issue:'A-2'})],open_questions:[],risks:[],excluded_scope:null,dispositions:null}
design.decisions[0].change += '!'
const first = {status:'completed', callId:'architect', output:design, meta:{}}
const phase = designPhase(ms, [], DESIGN, saved)
const audited = await phase.auditedDesign(first, '', undefined, 'tip')
return audited.arch.output
`
	p, err := script.Load(script.Source{Name: "integrated-design.js", Text: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	fake := engine.NewFake()
	audits := 0
	fake.Handler = func(req engine.FakeRequest) engine.FakeReply {
		audits++
		if !strings.Contains(req.Prompt, `"approach":"original"`) || strings.Contains(req.Prompt, `"change":"kept!"`) {
			t.Errorf("critic saw a rewritten integrated issue or decision: %s", req.Prompt)
		}
		return engine.FakeReply{Text: `{"verdict":"approved","findings":[],"summary":"remaining node approved","root":null,"first_check":null}`}
	}
	lane := ops.NewLane(ops.Deps{})
	res, err := engine.Run(context.Background(), p, engine.Config{Backend: fake, Policy: engine.FakePolicy(), Ops: lane.Ops()})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var d ops.Design
	if err := json.Unmarshal(res.Value, &d); err != nil {
		t.Fatal(err)
	}
	if audits != 1 || d.Issues[0].Approach != "original" || d.Decisions[0].Change != "kept" {
		t.Fatalf("integrated DESIGN drifted: audits=%d design=%s", audits, res.Value)
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

// ccHistoryRelay adds the crosscheck-history seam of CRW-1649 to the stock relay: previous project-lane runs of the plan.
type ccHistoryRelay struct {
	ops.Relay
	history []ops.DesignRun
}

func (r *ccHistoryRelay) CrosscheckHistory(context.Context, string) ([]ops.DesignRun, error) {
	return r.history, nil
}

// TestClose1649PendingCrosscheckIsThePriorOfTheNextRun (CRW-1649): a run that stopped crosscheck_pending leaves its recorded
// verdict as the next run's round-1 prior: the round-1 prompts of the next run of the same plan and milestone carry the prior's
// blocking findings, so a finding the last run already found is judged again instead of silently verifying.
func TestClose1649PendingCrosscheckIsThePriorOfTheNextRun(t *testing.T) {
	const marker = "prior-red-receipt-1649"
	finding := map[string]any{"severity": "P1", "impact": "criterion_unmet", "file": "internal/x.go", "line": 3,
		"text": marker, "kind": "evidence_missing", "evidence_question": map[string]any{"question": "is the receipt read?", "files_needed": []string{}}}
	answer := func(withFinding bool) []byte {
		b, _ := json.Marshal(map[string]any{
			"verdict": map[bool]string{true: "needs_changes", false: "verified"}[withFinding],
			"findings": func() []any {
				if withFinding {
					return []any{finding}
				}
				return []any{}
			}(),
		})
		return b
	}
	runClose := func(runID string, history []ops.DesignRun, capture, alwaysFinding bool) (*engine.MemoryJournal, []string) {
		j := engine.NewMemoryJournal()
		var prompts []string
		var mu sync.Mutex
		msJSON, _ := json.Marshal(ops.Milestone{ID: "M", Plan: "plan-1", Title: "test", Criteria: []string{"c"}})
		j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: "c_ms", Op: ops.OpMilestone, Event: engine.EvCompleted,
			Data: map[string]any{"milestone": "M", "plan": "plan-1", "result": string(msJSON)}})
		adapter := &reviewadapter.Adapter{
			Fingerprint: func(context.Context, string) (string, error) { return "fp-fixed", nil },
			Run: func(_ context.Context, p reviewadapter.Profile, req reviewadapter.Request) ([]byte, []byte, int, error) {
				if capture {
					mu.Lock()
					prompts = append(prompts, req.Prompt)
					mu.Unlock()
				}
				return answer(alwaysFinding || strings.Contains(req.Prompt, marker)), nil, 0, nil
			},
		}
		cc := &ops.Crosscheck{Checker: adapter, Limit: time.Minute,
			Profiles: func(string) ([]reviewadapter.Profile, error) {
				return []reviewadapter.Profile{
					reviewadapter.JSONProfile("engine-a", []string{"a"}, "model-a", "high"),
					reviewadapter.JSONProfile("engine-b", []string{"b"}, "model-b", "high"),
				}, nil
			},
			Checkout: func(context.Context, string, int, int, string, int, string) (string, func() error, error) {
				return t.TempDir(), func() error { return nil }, nil
			}}
		lane := ops.NewLane(ops.Deps{RunID: runID, Journal: j, Relay: &ccHistoryRelay{history: history}, Decisions: &ops.MemoryDecisions{}, Crosscheck: cc, LandRoot: t.TempDir()})
		opset := lane.Ops()
		opset["finalGate"] = researchOp(func(engine.OpRequest) any {
			return map[string]any{"state": "pass", "head": "head-x", "base": "base-x"}
		})
		opset["exploreRoute"] = researchOp(func(engine.OpRequest) any {
			return map[string]any{"routes": []any{map[string]any{"route": "orchestrator", "id": "q1", "digest": "d1", "question": "is the receipt read?"}}}
		})
		opset["exploreRecord"] = researchOp(func(engine.OpRequest) any { return map[string]any{} })
		opset["milestoneClose"] = researchOp(func(engine.OpRequest) any { return map[string]any{"moved": false, "tip": "head-x"} })
		body := `import { install } from 'crw:lib/lane'
import { closeMilestone } from 'crw:lib/close'
export const meta = {name:'cc-prior-1649'}
install(crw)
const ms = {id:'M', title:'test', criteria:['c'], nodes:[]}
const closed = await closeMilestone(ms)
if (closed.state !== 'closed') return crw.milestoneHalt(ms, closed.source, closed.status, closed.reason, closed.call, { ask: closed.ask, evidence: closed.evidence, fix: closed.fix, detail: closed.detail })
return closed`
		prog, err := script.Load(script.Source{Name: "cc-prior-1649.js", Text: []byte(body)})
		if err != nil {
			t.Fatal(err)
		}
		res, err := engine.Run(context.Background(), prog, engine.Config{RunID: runID, Backend: engine.NewFake(), Policy: engine.FakePolicy(), Journal: j, Ops: opset})
		if err != nil {
			t.Fatalf("%s: %v", runID, err)
		}
		if res.Status != engine.RunCompleted {
			t.Fatalf("%s: run status %s value %s", runID, res.Status, res.Value)
		}
		return j, prompts
	}

	j1, _ := runClose("run-1649-a", nil, false, true)
	var esc []engine.Event
	for _, ev := range j1.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == engine.EvEscalation {
			esc = append(esc, ev)
		}
	}
	if len(esc) != 1 {
		t.Fatalf("first run escalations: %+v", esc)
	}
	if got := fmt.Sprint(esc[0].Data["reason"]); got != "crosscheck_pending" {
		t.Fatalf("first run stopped for %q, want crosscheck_pending", got)
	}

	j2, prompts := runClose("run-1649-b", []ops.DesignRun{{Run: "run-1649-a", Events: j1.Events()}}, true, false)
	for _, ev := range j2.Events() {
		if ev.Op == ops.OpMilestoneClose {
			t.Fatalf("the second run closed over a pending verdict it never saw: %+v", ev)
		}
	}
	var esc2 []engine.Event
	for _, ev := range j2.Events() {
		if ev.Kind == engine.KindFlow && ev.Event == engine.EvEscalation {
			esc2 = append(esc2, ev)
		}
	}
	if len(esc2) != 1 || fmt.Sprint(esc2[0].Data["reason"]) != "crosscheck_pending" {
		t.Fatalf("second run did not stay crosscheck_pending on the carried finding: %+v", esc2)
	}
	if len(prompts) != 2 {
		t.Fatalf("the second run's crosscheck got %d prompts, want 2", len(prompts))
	}
	for _, p := range prompts {
		if !strings.Contains(p, marker) {
			t.Errorf("a round-1 prompt of the next run lacks the prior finding text: %s", p)
		}
	}
}

// TestClose1649CorrectionNodeOfARevisionLandsInTheCloseRun (CRW-1649): the route the crosscheck fix packet now names - a new run
// of the same plan reuses the already integrated node, builds the revision's correction node in its ordinary lane, and closes the
// milestone in that same run; the report carries the previous run's pending verdict and its findings' observed results.
func TestClose1649CorrectionNodeOfARevisionLandsInTheCloseRun(t *testing.T) {
	j := engine.NewMemoryJournal()
	lane := ops.NewLane(ops.Deps{RunID: "run-correction", Journal: j, Decisions: &ops.MemoryDecisions{}, LandRoot: t.TempDir()})
	opset := lane.Ops()
	// the lane's own operations of the correction node: plan, check, commit, verify and land
	opset["issuePlan"] = researchOp(func(req engine.OpRequest) any {
		return map[string]any{"size_fit": "executor", "regions": []string{"c.txt"}, "criteria": []string{}, "decisions": []any{}}
	})
	opset["unitCheck"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"state": "ok", "check_id": "check-c", "verdict": "verified", "findings": []any{}}
	})
	opset["commit"] = researchOp(func(engine.OpRequest) any { return map[string]any{"commit": "head-C"} })
	opset["verify"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"ran": true, "pass": true, "digest": "dg-C", "base": "base-x"}
	})
	opset["accept"] = researchOp(func(engine.OpRequest) any { return map[string]any{"acceptance_id": "acc-C"} })
	var integrated []string
	opset["integrate"] = researchOp(func(req engine.OpRequest) any {
		var call []string
		_ = json.Unmarshal(req.Args, &call)
		integrated = append(integrated, call[0])
		return map[string]any{"batch_commit": "batch-C", "tip": "head-C"}
	})
	opset["spawnDecision"] = researchOp(func(engine.OpRequest) any { return map[string]any{"id": "dec-x"} })
	var reuseCalls []string
	opset["reuseIntegrated"] = researchOp(func(req engine.OpRequest) any {
		var call []string
		_ = json.Unmarshal(req.Args, &call)
		reuseCalls = append(reuseCalls, call[1])
		if call[1] == "A" {
			return map[string]any{"reused": true, "source_run": "run-old", "head": "head-A", "source_kind": "land"}
		}
		return map[string]any{"reused": false}
	})
	var releases []string
	opset["release"] = researchOp(func(req engine.OpRequest) any {
		var call []string
		_ = json.Unmarshal(req.Args, &call)
		releases = append(releases, call[1])
		return map[string]any{"released": true, "kind": "executor"}
	})
	var closeArgs []map[string]any
	opset["finalGate"] = researchOp(func(engine.OpRequest) any {
		if len(integrated) != 1 || integrated[0] != "C" {
			t.Errorf("the gate ran before the correction's integration: %v", integrated)
		}
		return map[string]any{"state": "pass", "head": "head-C", "base": "base-x"}
	})
	prior := map[string]any{"verdict": "needs_changes", "round": 2, "run": "run-old", "head": "head-A",
		"findings": []any{map[string]any{"id": "f-old", "severity": "P1", "impact": "core_function", "file": "a.txt", "line": 1, "text": "the old finding"}},
		"carried":  []string{}}
	opset["crosscheck"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"state": "verdict",
			"verdict": map[string]any{"verdict": "verified", "findings": []any{}, "carried": []string{}, "run": "run-correction", "head": "head-C"},
			"prior":   prior}
	})
	opset["milestoneClose"] = researchOp(func(req engine.OpRequest) any {
		var call []any
		_ = json.Unmarshal(req.Args, &call)
		if len(call) == 2 {
			if m, ok := call[1].(map[string]any); ok {
				closeArgs = append(closeArgs, m)
			}
		}
		return map[string]any{"moved": false, "tip": "head-C"}
	})
	opset["audit"] = researchOp(func(engine.OpRequest) any {
		return map[string]any{"ok": 2, "mismatch": 0, "unobserved": 0, "results": []any{}}
	})
	var reportArgs []map[string]any
	opset["reportUp"] = researchOp(func(req engine.OpRequest) any {
		var call []any
		_ = json.Unmarshal(req.Args, &call)
		if len(call) == 2 {
			if m, ok := call[1].(map[string]any); ok {
				reportArgs = append(reportArgs, m)
			}
		}
		return map[string]any{"report_id": "rep-1"}
	})
	body := `import { install } from 'crw:lib/lane'
import { lane } from 'crw:lib/project-build'
import { closeMilestone, reportClose } from 'crw:lib/close'
export const meta = {name:'correction-1649'}
install(crw)
const ms = {id:'M', plan:'plan-1', title:'fix', criteria:['c'], nodes:[
  {id:'A', regions:['a.txt'], after:[]},
  {id:'C', regions:['c.txt'], after:['A']},
]}
const inherited = []
const pending = []
for (const node of ms.nodes) {
  const saved = await crw.reuseIntegrated(ms.id, node.id)
  if (saved.reused) inherited.push({node:node.id, state:'landed', reused:true})
  else pending.push(node)
}
const fresh = await crw.dag(ms.plan, lane, {nodes:pending, milestone:ms.id, cap:1})
const closed = await closeMilestone(ms)
const reported = await reportClose(ms, closed, 'ALIGNED')
return {lanes:inherited.concat(fresh), closed, reported}`
	prog, err := script.Load(script.Source{Name: "correction-1649.js", Text: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	fake := engine.NewFake()
	fake.Handler = func(req engine.FakeRequest) engine.FakeReply {
		return engine.FakeReply{Text: `{"outcome":"ready_for_review","files":["c.txt"],"verification":[],"summary":"correction built","gap":null,"research_question":null,"plan_mismatch":null}`}
	}
	res, err := engine.Run(context.Background(), prog, engine.Config{RunID: "run-correction", Backend: fake, Policy: engine.FakePolicy(), Journal: j, Ops: opset})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("correction run: %v %+v", err, res)
	}
	var out struct {
		Lanes    []map[string]any `json:"lanes"`
		Closed   map[string]any   `json:"closed"`
		Reported map[string]any   `json:"reported"`
	}
	if err := json.Unmarshal(res.Value, &out); err != nil {
		t.Fatalf("result: %v: %s", err, res.Value)
	}
	if len(reuseCalls) != 2 || reuseCalls[0] != "A" || reuseCalls[1] != "C" {
		t.Fatalf("reuseIntegrated calls: %v, want A then C once each", reuseCalls)
	}
	if len(releases) != 1 || releases[0] != "C" || len(integrated) != 1 || integrated[0] != "C" {
		t.Fatalf("only the correction may build: releases %v integrates %v", releases, integrated)
	}
	var built bool
	for _, c := range out.Lanes {
		if c["node"] == "A" && c["reused"] != true {
			t.Fatalf("the integrated node was not reused: %v", c)
		}
		if c["node"] == "C" && c["state"] == "landed" {
			built = true
		}
	}
	if !built {
		t.Fatalf("the correction node did not land: %v", out.Lanes)
	}
	if out.Closed["state"] != "closed" || out.Closed["tip"] != "head-C" {
		t.Fatalf("the milestone did not close on the repaired head: %v", out.Closed)
	}
	var closes, seqClose, seqReport int
	for _, ev := range j.Events() {
		switch ev.Op {
		case ops.OpMilestoneClose:
			if ev.Event == engine.EvCompleted {
				closes++
				seqClose = ev.Seq
			}
		case ops.OpReportUp:
			if ev.Event == engine.EvCompleted {
				seqReport = ev.Seq
			}
		}
	}
	if closes != 1 || seqClose == 0 || seqReport <= seqClose {
		t.Fatalf("closes=%d close seq %d report seq %d: want exactly one close before the report", closes, seqClose, seqReport)
	}
	if len(closeArgs) != 1 {
		t.Fatalf("close calls: %+v", closeArgs)
	}
	// the V recorded and sent to milestoneClose is never annotated; the report's copy carries the previous verdict
	if cc, _ := closeArgs[0]["crosscheck"].(map[string]any); cc["previous_crosscheck"] != nil {
		t.Fatalf("the milestoneClose verdict was annotated: %v", cc)
	}
	if cc, _ := out.Closed["crosscheck"].(map[string]any); cc["previous_crosscheck"] != nil {
		t.Fatalf("closed.crosscheck was annotated: %v", cc)
	}
	if len(reportArgs) != 1 {
		t.Fatalf("reportUp calls: %+v", reportArgs)
	}
	cc, _ := reportArgs[0]["crosscheck"].(map[string]any)
	prev, _ := cc["previous_crosscheck"].(map[string]any)
	if prev == nil || prev["verdict"] == nil {
		t.Fatalf("the report carries no previous crosscheck: %v", cc)
	}
	pv, _ := prev["verdict"].(map[string]any)
	if pv["run"] != "run-old" || pv["verdict"] != "needs_changes" {
		t.Fatalf("the report's previous verdict: %v", pv)
	}
	outcomes, _ := prev["outcomes"].([]any)
	if len(outcomes) != 1 {
		t.Fatalf("previous outcomes: %v", prev)
	}
	if o, _ := outcomes[0].(map[string]any); o["id"] != "f-old" || o["state"] != "not_reported" {
		t.Fatalf("the old finding's outcome: %v", o)
	}
}
