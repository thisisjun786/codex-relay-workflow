package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// The wiring of crw.unitCheck (CRW-1463): the lane the shipped binary builds installs the op over the unit check LaneOptions names, and
// over the shipped one (shippedUnitCheck: a Codex review through the review adapter, Bugbot on a Codex quota) when it names none.

// unitCheckPolicy is the role table the wiring tests hand the lane: the placeholder pairs of two roles and the unit
// check's tool pair at its compiled default (CRW-1589).
func unitCheckPolicy() *engine.Policy {
	pair := func(model string) engine.RoleSpec {
		return engine.RoleSpec{Pair: engine.Pair{Model: model, Effort: "high"}}
	}
	return &engine.Policy{Roles: map[string]engine.RoleSpec{"executor": pair("claude-fake-haiku"), "reviewer": pair("gpt-fake-sol")},
		UnitCheck: engine.Pair{Model: role.UnitCheckDefaultModel, Effort: role.UnitCheckDefaultEffort}}
}

// unitCheckLane builds the lane of a run whose script calls a crw.* operation, with the unit check uc (nil: the shipped one).
func unitCheckLane(t *testing.T, w *world, run string, uc *ops.UnitCheck) LaneWorld {
	t.Helper()
	lw, err := buildUnitCheckLane(t, w, run, uc, func(string) (string, bool) { return "", false }, unitCheckPolicy())
	if err != nil {
		t.Fatal(err)
	}
	return lw
}

// buildUnitCheckLane is unitCheckLane with the environment getenv, the role table and BuildLane's error answered.
func buildUnitCheckLane(t *testing.T, w *world, run string, uc *ops.UnitCheck, getenv func(string) (string, bool), policy *engine.Policy) (LaneWorld, error) {
	t.Helper()
	j, err := journal.Open(context.Background(), w.st, run, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	laneful := filepath.Join(t.TempDir(), "laneful.js")
	if err := os.WriteFile(laneful, []byte("export const meta = { name: 'laneful' }\nawait crw.unitCheck('A', null, {})\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := LaneRequest{WorldRequest: WorldRequest{Spec: Spec{Schema: SpecSchema, RunID: run, Workflow: "x", Script: laneful, Thread: labOwner}, State: w.state},
		Store: w.st, Journal: j, Policy: policy}
	return BuildLane(context.Background(), req, LaneOptions{Verifier: &labNoVerifier{}, IntegrationVerify: func(context.Context, string, []string) error { return nil },
		SkillsDigest: "sha256:lab-skills", Getenv: getenv, UnitCheck: uc})
}

// labNoVerifier is a verifier the wiring test never asks.
type labNoVerifier struct{}

func (labNoVerifier) Verify(context.Context, ops.VerifyRequest) (ops.VerifyOutcome, error) {
	return ops.VerifyOutcome{}, nil
}

// placedFake is the fake backend with a placer: an executor's first call runs in one fixed worktree, and a snapshot is the working
// snapshot of git (ops.Snapshot), the pending tree the engine records.
type placedFake struct {
	*engine.Fake
	wt, start string
}

func (p *placedFake) Place(req engine.PlaceRequest) (engine.Placement, error) {
	if req.Family != "executor" || req.Continues != nil {
		return engine.Placement{Kind: engine.PlaceContinued}, nil
	}
	return engine.Placement{Kind: engine.PlaceFresh, Attempt: 1, Path: p.wt, Start: p.start}, nil
}
func (p *placedFake) Establish(context.Context, engine.PlaceRequest, engine.Placement) error {
	return nil
}
func (p *placedFake) Recheck(context.Context, engine.Placement) error { return nil }
func (p *placedFake) Snapshot(ctx context.Context, path string) (string, error) {
	return ops.Snapshot(ctx, ops.ExecGit("git"), path)
}

// TestBuildLaneWiresTheUnitCheck (CRW-1463): BuildLane installs crw.unitCheck in the run's operations; a script that builds a node in a
// worktree and calls crw.unitCheck on the call reaches the unit check LaneOptions names (the lab's checker), which is given the node's
// worktree in the uncommitted mode, and the run's journal keeps the row of role unit-check. With no unit check named the shipped one is
// wired: the op takes the shipped committed profile (CRW-1468) and stops at the journal that records no worktree, not that no checker is
// wired.
func TestBuildLaneWiresTheUnitCheck(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	fake := newLabUnitChecker()
	lw := unitCheckLane(t, w, "run-ucwire0001", fake.seam())
	if lw.Ops[ops.NameUnitCheck] == nil || ops.NameUnitCheck != "unitCheck" {
		t.Fatalf("the lane installs no crw.unitCheck (%q): %d operations", ops.NameUnitCheck, len(lw.Ops))
	}

	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	l := &laneLab{t: t}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.git(repo, "init", "-q", "-b", "main")
	l.git(repo, "add", "-A")
	l.git(repo, "commit", "-q", "-m", "base")
	start := l.git(repo, "rev-parse", "HEAD")
	wt := filepath.Join(t.TempDir(), "wt")
	l.git(repo, "worktree", "add", "--detach", "-q", wt, start)

	backend := &placedFake{Fake: engine.NewFake(), wt: wt, start: start}
	backend.Handler = func(r engine.FakeRequest) engine.FakeReply {
		if err := os.WriteFile(filepath.Join(wt, "node-A.txt"), []byte("built\n"), 0o644); err != nil {
			t.Error(err)
		}
		return engine.FakeReply{Text: "built"}
	}
	src := "export const meta = { name: 'unit-check-wiring' }\n" +
		"const ex = await agent.call('build A', { agentType: 'executor', node: 'A', label: 'build:A' })\n" +
		"return await crw.unitCheck('A', ex, { round: 1 })\n"
	prog, err := script.Load(script.Source{Name: "unit-check-wiring.js", Text: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	j := engine.NewMemoryJournal()
	rctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := engine.Run(rctx, prog, engine.Config{Backend: backend, Policy: engine.FakePolicy(), Journal: j, RunID: "run-ucwire0001", Ops: lw.Ops})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("the run: %v %+v", err, res)
	}
	var out ops.UnitCheckOutcome
	if err := json.Unmarshal(res.Value, &out); err != nil || out.State != reviewadapter.StateOK || out.Verdict != reviewadapter.VerdictVerified || out.Model != labCheckModel {
		t.Errorf("the script's answer %s (%v)", res.Value, err)
	}
	calls := fake.requests()
	if len(calls) != 1 || calls[0].Node != "A" || calls[0].Mode != "uncommitted" || calls[0].Request.Worktree != wt {
		t.Fatalf("the lab's checker was given %+v", calls)
	}
	found := false
	for _, ev := range j.Events() {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpUnitCheck && ev.Event == engine.EvCompleted {
			found = found || (ev.Data["role"] == "unit-check" && ev.Data["node"] == "A")
		}
	}
	if !found {
		t.Error("the run's journal has no completed crw.unitCheck row of role unit-check for A")
	}

	// no unit check named: the shipped one, never none
	shipped := unitCheckLane(t, w, "run-ucwire0002", nil)
	op := shipped.Ops[ops.NameUnitCheck]
	if op == nil {
		t.Fatal("the shipped lane installs no crw.unitCheck")
	}
	_, err = op.Run(context.Background(), engine.OpRequest{RunID: "run-ucwire0002", CallID: "c1", RequestID: "req-c1", Args: json.RawMessage(`["A", null, {"commit": "abc", "base": "def"}]`),
		Journal: engine.NewMemoryJournal()})
	if err == nil || !strings.Contains(err.Error(), "no worktree is recorded") || strings.Contains(err.Error(), "no unit checker is wired") || strings.Contains(err.Error(), "profile") {
		t.Errorf("the shipped unit check in the committed mode: %v", err)
	}
}

// TestShippedUnitCheckIsCodexInBothModes (CRW-1463, CRW-1517): the shipped unit check is the review adapter over the tool runner and
// ops.Snapshot as its fingerprint, so the tree a check saw is the pending tree crw.commit commits; both modes run a Codex review
// (codex-review, codex-review-committed: gpt-6-astra at high effort by default) and fall back to Bugbot (bugbot, bugbot-committed) on a
// Codex quota; CRW_UNIT_CHECK_MODEL and CRW_UNIT_CHECK_EFFORT change the Codex profiles (model and review_model, and the family the
// model names) and neither Bugbot one, a setting codex cannot be given or whose family cannot be told is refused when the lane is built, and the call is limited to ten minutes.
func TestShippedUnitCheckIsCodexInBothModes(t *testing.T) {
	isolateGit(t)
	uc, err := shippedUnitCheck(unitCheckPolicy())
	if err != nil || uc == nil || uc.Limit != shippedUnitCheckLimit || shippedUnitCheckLimit != 10*time.Minute {
		t.Fatalf("shipped unit check %+v %v", uc, err)
	}
	a, ok := uc.Checker.(reviewadapter.Adapter)
	if !ok || a.Run == nil || a.Fingerprint == nil {
		t.Fatalf("the shipped checker %T is not the review adapter with its seams", uc.Checker)
	}
	if uc.Profile == nil || uc.Fallback == nil {
		t.Fatalf("the shipped unit check names no profile or no fallback: %+v", uc)
	}
	argvHas := func(p reviewadapter.Profile, elem string) bool {
		for _, a := range p.Argv {
			if a == elem {
				return true
			}
		}
		return false
	}
	codex := func(uc *ops.UnitCheck, mode, name, model, effort, family string) {
		t.Helper()
		p, err := uc.Profile(mode)
		if err != nil || p.Name != name || p.Model != model || p.Effort != effort || p.Family != family ||
			!argvHas(p, `model="`+model+`"`) || !argvHas(p, `review_model="`+model+`"`) || !argvHas(p, `model_reasoning_effort="`+effort+`"`) || p.Args == nil || p.Answer == nil {
			t.Errorf("the %s profile %+v %v, want %s with %s at %s, family %s", mode, p, err, name, model, effort, family)
		}
	}
	bugbot := func(uc *ops.UnitCheck, mode string, want reviewadapter.Profile) {
		t.Helper()
		p, err := uc.Fallback(mode)
		if err != nil || p.Name != want.Name || p.Model != want.Model || p.Effort != want.Effort || !slices.Equal(p.Argv, want.Argv) {
			t.Errorf("the %s fallback %+v %v, want %s", mode, p, err, want.Name)
		}
	}
	codex(uc, ops.UnitCheckUncommitted, "codex-review", "gpt-6-astra", "high", "gpt")
	codex(uc, ops.UnitCheckCommitted, "codex-review-committed", "gpt-6-astra", "high", "gpt")
	bugbot(uc, ops.UnitCheckUncommitted, reviewadapter.BugbotProfile())
	bugbot(uc, ops.UnitCheckCommitted, reviewadapter.BugbotCommittedProfile())
	if _, err := uc.Profile("other"); err == nil {
		t.Error("an unknown mode has a profile")
	}
	if _, err := uc.Fallback("other"); err == nil {
		t.Error("an unknown mode has a fallback")
	}

	// the snapshot's unit-check pair decides both Codex profiles and neither Bugbot one (CRW-1589)
	tuned, err := shippedUnitCheck(&engine.Policy{UnitCheck: engine.Pair{Model: "o9", Effort: "medium"}})
	if err != nil || tuned == nil {
		t.Fatalf("the shipped unit check with a configured pair %+v %v", tuned, err)
	}
	codex(tuned, ops.UnitCheckUncommitted, "codex-review", "o9", "medium", "o9")
	codex(tuned, ops.UnitCheckCommitted, "codex-review-committed", "o9", "medium", "o9")
	bugbot(tuned, ops.UnitCheckUncommitted, reviewadapter.BugbotProfile())
	bugbot(tuned, ops.UnitCheckCommitted, reviewadapter.BugbotCommittedProfile())

	// a pair codex cannot be given is refused — naming the variable when the value is an environment override, the field
	// otherwise — and stops the lane when it is built; a lane that names its own unit check does not read it
	for i, tc := range []struct {
		unit      engine.Pair
		overrides map[string]string
		want      string
	}{
		{engine.Pair{Model: `a"b`, Effort: "high"}, map[string]string{role.UnitCheckModelEnv: `a"b`}, role.UnitCheckModelEnv},
		{engine.Pair{Model: "gpt-6-astra", Effort: "High!"}, map[string]string{role.UnitCheckEffortEnv: "High!"}, role.UnitCheckEffortEnv},
		{engine.Pair{Model: "-astra", Effort: "high"}, nil, "unit-check model"},
		{engine.Pair{Model: "gpt-6-astra"}, nil, "resolved model and effort"},
	} {
		policy := unitCheckPolicy()
		policy.UnitCheck, policy.UnitCheckOverrides = tc.unit, tc.overrides
		if uc, err := shippedUnitCheck(policy); err == nil || uc != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: shipped unit check %+v %v, want an error naming %q", tc.unit, uc, err, tc.want)
		}
		if _, err := buildUnitCheckLane(t, newWorld(t), "run-ucbadpol01", nil, func(string) (string, bool) { return "", false }, policy); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: BuildLane %v, want the refused pair naming %q", tc.unit, err, tc.want)
		}
		if _, err := buildUnitCheckLane(t, newWorld(t), fmt.Sprintf("run-ucbadpol0%d", i), newLabUnitChecker().seam(), func(string) (string, bool) { return "", false }, policy); err != nil {
			t.Errorf("%+v with a named unit check: BuildLane %v", tc.unit, err)
		}
	}
	// a Claude model records the claude family
	claude := &engine.Policy{UnitCheck: engine.Pair{Model: "anthropic/claude-sonnet-5", Effort: "high"}}
	if c, err := shippedUnitCheck(claude); err != nil {
		t.Errorf("a Claude model: %v", err)
	} else {
		codex(c, ops.UnitCheckUncommitted, "codex-review", "anthropic/claude-sonnet-5", "high", "claude")
		codex(c, ops.UnitCheckCommitted, "codex-review-committed", "anthropic/claude-sonnet-5", "high", "claude")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	l := &laneLab{t: t}
	l.git(repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want, err := ops.Snapshot(context.Background(), ops.ExecGit("git"), repo)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.Fingerprint(context.Background(), repo); err != nil || got != want {
		t.Errorf("the shipped fingerprint %q %v, want ops.Snapshot %q", got, err, want)
	}
}

// TestCRW1589_ShippedUnitCheckUsesTheRoleSnapshot (CRW-1589): the shipped unit check's Codex profiles run the pair of
// the role store's unit-check entry — the snapshot the run's role table is built from — and never read the environment
// for it at lane build.
func TestCRW1589_ShippedUnitCheckUsesTheRoleSnapshot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, role.StoreFile), []byte(`{"roles": {"unit-check": {"mode": "model", "model": "gpt-7-sol", "effort": "medium"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", dir)
	t.Setenv(execution.EnvPolicy, "")
	table, _, err := engine.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	uc, err := shippedUnitCheck(table)
	if err != nil || uc == nil {
		t.Fatalf("shipped unit check %+v %v", uc, err)
	}
	for _, mode := range []string{ops.UnitCheckUncommitted, ops.UnitCheckCommitted} {
		p, err := uc.Profile(mode)
		argv := strings.Join(p.Argv, " ")
		if err != nil || p.Model != "gpt-7-sol" || p.Effort != "medium" ||
			!strings.Contains(argv, `model="gpt-7-sol"`) || !strings.Contains(argv, `review_model="gpt-7-sol"`) || !strings.Contains(argv, `model_reasoning_effort="medium"`) {
			t.Errorf("the %s profile %+v %v, want the store's gpt-7-sol at medium", mode, p, err)
		}
	}
	// environment overrides apply per field through the snapshot; an override that codex cannot take is refused, naming the variable
	t.Setenv("CRW_UNIT_CHECK_MODEL", "o9")
	table, _, err = engine.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if table.UnitCheck != (engine.Pair{Model: "o9", Effort: "medium"}) || table.UnitCheckOverrides[role.UnitCheckModelEnv] != "o9" {
		t.Fatalf("the overridden table %+v %+v", table.UnitCheck, table.UnitCheckOverrides)
	}
	tuned, err := shippedUnitCheck(table)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := tuned.Profile(ops.UnitCheckUncommitted); p.Model != "o9" || p.Effort != "medium" {
		t.Errorf("the overridden profile %+v", p)
	}
	// the pair is snapshotted with the table: changing the environment afterwards changes nothing the tool runs
	t.Setenv("CRW_UNIT_CHECK_MODEL", "changed-later")
	if p, _ := tuned.Profile(ops.UnitCheckCommitted); p.Model != "o9" {
		t.Errorf("the profile reread the environment: %+v", p)
	}
	t.Setenv("CRW_UNIT_CHECK_MODEL", `a"b`)
	bad, _, err := engine.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if uc, err := shippedUnitCheck(bad); err == nil || uc != nil || !strings.Contains(err.Error(), "CRW_UNIT_CHECK_MODEL") {
		t.Errorf("a bad override: %+v %v, want an error naming CRW_UNIT_CHECK_MODEL", uc, err)
	}
}
