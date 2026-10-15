package cli

import (
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
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// The wiring of crw.unitCheck (CRW-1463): the lane the shipped binary builds installs the op over the unit check LaneOptions names, and
// over the shipped one (shippedUnitCheck: Bugbot through the review adapter) when it names none.

// unitCheckLane builds the lane of a run whose script calls a crw.* operation, with the unit check uc (nil: the shipped one).
func unitCheckLane(t *testing.T, w *world, run string, uc *ops.UnitCheck) LaneWorld {
	t.Helper()
	j, err := journal.Open(context.Background(), w.st, run, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	laneful := filepath.Join(t.TempDir(), "laneful.js")
	if err := os.WriteFile(laneful, []byte("export const meta = { name: 'laneful' }\nawait crw.unitCheck('A', null, {})\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pair := func(model string) engine.RoleSpec {
		return engine.RoleSpec{Pair: engine.Pair{Model: model, Effort: "high"}}
	}
	req := LaneRequest{WorldRequest: WorldRequest{Spec: Spec{Schema: SpecSchema, RunID: run, Workflow: "x", Script: laneful, Thread: labOwner}, State: w.state},
		Store: w.st, Journal: j, Policy: &engine.Policy{Roles: map[string]engine.RoleSpec{"executor": pair("claude-fake-haiku"), "reviewer": pair("gpt-fake-sol")}}}
	lw, err := BuildLane(context.Background(), req, LaneOptions{Verifier: &labNoVerifier{}, IntegrationVerify: func(context.Context, string, []string) error { return nil },
		SkillsDigest: "sha256:lab-skills", Getenv: func(string) (string, bool) { return "", false }, UnitCheck: uc})
	if err != nil {
		t.Fatal(err)
	}
	return lw
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

// TestShippedUnitCheckIsTheBugbotAdapter (CRW-1463): the shipped unit check is the review adapter over the tool runner and ops.Snapshot
// as its fingerprint, so the tree a check saw is the pending tree crw.commit commits; the uncommitted mode runs Bugbot, the committed mode
// runs Bugbot with the range's scope reader (CRW-1468), and the call is limited to ten minutes.
func TestShippedUnitCheckIsTheBugbotAdapter(t *testing.T) {
	isolateGit(t)
	uc := shippedUnitCheck()
	if uc == nil || uc.Limit != shippedUnitCheckLimit || shippedUnitCheckLimit != 10*time.Minute {
		t.Fatalf("shipped unit check %+v", uc)
	}
	a, ok := uc.Checker.(reviewadapter.Adapter)
	if !ok || a.Run == nil || a.Fingerprint == nil {
		t.Fatalf("the shipped checker %T is not the review adapter with its seams", uc.Checker)
	}
	p, err := uc.Profile("uncommitted")
	if err != nil || p.Name != reviewadapter.BugbotProfile().Name || p.Model != reviewadapter.BugbotProfile().Model {
		t.Errorf("the uncommitted profile %+v %v", p, err)
	}
	if p, err := uc.Profile("committed"); err != nil || p.Name != reviewadapter.BugbotCommittedProfile().Name || p.Model != reviewadapter.BugbotCommittedProfile().Model || p.Answer == nil {
		t.Errorf("the committed profile %+v %v", p, err)
	}
	if _, err := uc.Profile("other"); err == nil {
		t.Error("an unknown mode has a profile")
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
