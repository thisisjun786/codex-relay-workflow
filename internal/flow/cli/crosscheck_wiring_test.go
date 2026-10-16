package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// The wiring of crw.crosscheck in the shipped lane (CRW-1473): BuildLane installs the op over the crosscheck of its options, or the
// shipped one (the review adapter over the astra and grok profiles), with the author pairs of the run's role table.

// laneWorldFor builds the lane of a run of the lab over a role table, as BuildLane builds it for `crw flow start`.
func (l *laneLab) laneWorldFor(run string, policy *engine.Policy, opt LaneOptions) LaneWorld {
	l.t.Helper()
	raw := l.handoffBytes(nil)
	ref, err := PinHandoff(l.w.state, run, raw)
	if err != nil {
		l.t.Fatal(err)
	}
	id := journal.Identity{PlanID: l.plan, PlanRevision: 1, ActorTaskID: labOwner, EpochNonce: run}
	j, err := journal.Open(context.Background(), l.w.st, run, id, journal.Options{Immediate: true})
	if err != nil {
		l.t.Fatal(err)
	}
	script := filepath.Join(l.t.TempDir(), "laneful.js")
	if err := os.WriteFile(script, []byte("export const meta = { name: 'laneful' }\nawait crw.crosscheck('M1')\n"), 0o600); err != nil {
		l.t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"plan": l.plan, "milestone": "M1", "handoff": ref})
	req := LaneRequest{WorldRequest: WorldRequest{Spec: Spec{Schema: SpecSchema, RunID: run, Workflow: "x", Script: script, Args: args, Thread: labOwner, CreatedAt: "2026-10-12T09:00:00Z"}, State: l.w.state},
		Store: l.w.st, Journal: j, Identity: id, Repo: l.repo, Policy: policy}
	opt.Relay = func(r *SchedRelay) ops.Relay { return &labRelay{SchedRelay: r, l: l} }
	if opt.SkillsDigest == "" {
		opt.SkillsDigest = "sha256:lab-skills"
	}
	opt.Verifier = labVerifier{l}
	opt.IntegrationVerify = l.verifyMerged
	world, err := BuildLane(context.Background(), req, opt)
	if err != nil {
		l.t.Fatal(err)
	}
	return world
}

// runCrosscheck runs the world's crw.crosscheck over the gate at head of the lab's milestone M1, as the engine would.
func (l *laneLab) runCrosscheck(world LaneWorld, run, head string) ops.CrosscheckOutcome {
	l.t.Helper()
	op, ok := world.Ops[ops.NameCrosscheck]
	if !ok {
		l.t.Fatalf("the lane installs no %s", ops.NameCrosscheck)
	}
	ms := ops.Milestone{ID: "M1", Plan: l.plan, Title: "milestone M1", Criteria: []string{"M1 is integrated"}}
	for key, node := range l.nodeOf {
		ms.Nodes = append(ms.Nodes, ops.PlanNode{ID: node, Issue: key})
	}
	j := engine.NewMemoryJournal()
	raw, _ := json.Marshal(ms)
	j.Append(engine.Event{RunID: run, Kind: engine.KindCall, CallID: "c_ms", Op: ops.OpMilestone, Event: engine.EvCompleted, Data: map[string]any{"milestone": "M1", "plan": l.plan, "result": string(raw)}})
	gate := map[string]any{"milestone": "M1", "plan": l.plan, "head": head, "base": head, "range": head, "state": ops.GatePass}
	opts := map[string]any{"round": 1, "prompts": map[string]any{"A": "pass A of M1\n", "B": "pass B of M1\n"}}
	argv, _ := json.Marshal([]any{"M1", gate, opts})
	res, err := op.Run(context.Background(), engine.OpRequest{RunID: run, CallID: "c_cc", RequestID: engine.RequestID(run, "c_cc"), Args: argv, Journal: j})
	if err != nil {
		l.t.Fatalf("crw.crosscheck: %v", err)
	}
	var out ops.CrosscheckOutcome
	if err := json.Unmarshal(res.Result, &out); err != nil {
		l.t.Fatal(err)
	}
	return out
}

// TestBuildLaneWiresTheCrosscheck: the lane's crw.crosscheck reaches the crosscheck of the options (the lab's fake) in a checkout under the
// run's directory, on the author pairs of the run's role table; with no option the crosscheck is the shipped one, astra then grok over the
// review adapter, and an undecided architect pair is an undecided author.
func TestBuildLaneWiresTheCrosscheck(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	head := l.git(l.repo, "rev-parse", "HEAD")
	const run = "run-ccwire00001"
	world := l.laneWorldFor(run, engine.FakePolicy(), LaneOptions{Crosscheck: l.cc.seam()})
	out := l.runCrosscheck(world, run, head)
	if out.State != ops.CrosscheckStateVerdict || out.Verdict == nil || out.Verdict.EngineFamily != "gpt" {
		t.Fatalf("outcome %+v", out)
	}
	want := []ops.CrosscheckAuthor{{Source: "role", Name: "architect", Model: "claude-fake-opus", Family: "claude"}, {Source: "role", Name: "executor", Model: "claude-fake-haiku", Family: "claude"},
		{Source: "role", Name: "executor-t1", Model: "claude-fake-sonnet", Family: "claude"}}
	if len(out.Verdict.Authors) != len(want) {
		t.Fatalf("authors %+v", out.Verdict.Authors)
	}
	for i := range want {
		if out.Verdict.Authors[i] != want[i] {
			t.Errorf("author %d: %+v, want %+v", i, out.Verdict.Authors[i], want[i])
		}
	}
	calls := l.cc.requests()
	if len(calls) != 2 {
		t.Fatalf("%d checks reached the lab's crosschecker", len(calls))
	}
	root := filepath.Join(RunDir(l.w.state, run), "crosscheck-wt") + string(filepath.Separator)
	for _, c := range calls {
		if !c.Existed || !strings.HasPrefix(c.Request.Worktree, root) || c.Request.Profile.Name != "astra" || c.Request.GuideRev != ops.CrosscheckGuideRev {
			t.Errorf("check %s: worktree %q (existed %v), profile %q", c.Key, c.Request.Worktree, c.Existed, c.Request.Profile.Name)
		}
	}
	if out.Verdict.ReceiptsFile != filepath.Join(RunDir(l.w.state, run), "crosscheck", "M1-r1-receipts.md") {
		t.Errorf("receipts file %q", out.Verdict.ReceiptsFile)
	}

	// an undecided architect pair is an undecided author
	undecided := engine.FakePolicy()
	spec := undecided.Roles["architect"]
	spec.Pair = engine.Pair{}
	undecided.Roles["architect"] = spec
	const run2 = "run-ccwire00002"
	out = l.runCrosscheck(l.laneWorldFor(run2, undecided, LaneOptions{Crosscheck: l.cc.seam()}), run2, head)
	if out.State != ops.CrosscheckStateRefused || out.Reason != ops.ReasonCrosscheckAuthorUndecided || !strings.Contains(out.Detail, "architect") {
		t.Errorf("an undecided architect: %+v", out)
	}
	if len(l.cc.requests()) != 2 {
		t.Error("a pass ran for an undecided author")
	}

	// the shipped crosscheck: astra then grok over the review adapter, 40 minutes a pass, the table's author pairs
	cc := laneCrosscheck(nil, engine.FakePolicy())
	if _, ok := cc.Checker.(reviewadapter.Adapter); !ok || cc.Limit != 40*time.Minute || cc.Checkout != nil {
		t.Errorf("the shipped crosscheck: checker %T limit %v checkout set %v", cc.Checker, cc.Limit, cc.Checkout != nil)
	}
	for role, model := range map[string]string{"architect": "claude-fake-opus", "executor": "claude-fake-haiku", "executor-t1": "claude-fake-sonnet"} {
		if cc.Authors[role] != model {
			t.Errorf("shipped author %s = %q, want %q", role, cc.Authors[role], model)
		}
	}
	if got := laneCrosscheck(nil, undecided).Authors; got["architect"] != "" || len(got) != 3 {
		t.Errorf("an undecided architect gives the authors %v", got)
	}
	dir := t.TempDir()
	profiles, err := cc.Profiles(dir)
	if err != nil || len(profiles) != 2 || profiles[0].Name != "astra" || profiles[1].Name != "grok" {
		t.Fatalf("the shipped profiles %+v %v", profiles, err)
	}
	// the floors of a pass that read the repository (CRW-1504) reach the shipped profiles: astra 3 commands and 45 s, grok 100000 cached
	// tokens and 60 s, each with its usage reader, so a shallow pass of either is not_read and crw.crosscheck reruns or voids it
	if a := profiles[0]; a.MinCommands != 3 || a.MinElapsed != 45*time.Second || a.MinCacheRead != 0 || a.Usage == nil {
		t.Errorf("the shipped astra floors: %d commands, %v, %d tokens, usage reader %v", a.MinCommands, a.MinElapsed, a.MinCacheRead, a.Usage != nil)
	}
	if g := profiles[1]; g.MinCacheRead != 100000 || g.MinElapsed != 60*time.Second || g.MinCommands != 0 || g.Usage == nil {
		t.Errorf("the shipped grok floors: %d tokens, %v, %d commands, usage reader %v", g.MinCacheRead, g.MinElapsed, g.MinCommands, g.Usage != nil)
	}
	schemaFile := filepath.Join(dir, "crosscheck", "answer.strict.json")
	strict, err := reviewadapter.StrictSchema(reviewadapter.AnswerSchema)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(schemaFile); err != nil || !bytes.Equal(b, strict) {
		t.Errorf("the strict schema file %s: %v", schemaFile, err)
	}
	if !strings.Contains(strings.Join(profiles[0].Argv, " "), "--output-schema "+schemaFile) {
		t.Errorf("astra argv %v", profiles[0].Argv)
	}
	// a crosscheck option with its own authors keeps them
	own := &ops.Crosscheck{Authors: map[string]string{"architect": "x-model"}}
	if got := laneCrosscheck(own, engine.FakePolicy()); got.Authors["architect"] != "x-model" || len(got.Authors) != 1 {
		t.Errorf("the option's authors were replaced: %v", got.Authors)
	}
}
