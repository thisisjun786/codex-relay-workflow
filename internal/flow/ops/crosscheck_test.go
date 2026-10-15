package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// The tests of crw.crosscheck (CRW-1473): the review of a milestone's integrated range by two passes of an engine of another model
// family than every author, run in Go through the review adapter's seam. A fake checker answers the passes; the checkout is a fake that
// makes a directory under the test's LandRoot, or the real one (Lane.crosscheckWorktree) over a temporary repository.

const ccHead = "abcdef0123456789abcdef0123456789abcdef01"

// ccAnswer is one scripted answer of the fake crosschecker.
type ccAnswer struct {
	State    reviewadapter.State
	Verdict  string
	Findings []reviewadapter.Finding
}

// ccCall is one Check the fake crosschecker was given, with whether its worktree existed during the call and the HEAD it was at.
type ccCall struct {
	Pass    string
	Request reviewadapter.Request
	Existed bool
	HeadAt  string
}

// fakeCrosschecker answers the passes of a crosscheck: by pass (the prompt starts with "pass A" or "pass B"), the scripted answers in
// order, then a verified answer with no finding.
type fakeCrosschecker struct {
	mu    sync.Mutex
	queue map[string][]ccAnswer
	calls []ccCall
}

func newFakeCrosschecker() *fakeCrosschecker {
	return &fakeCrosschecker{queue: map[string][]ccAnswer{}}
}

func (f *fakeCrosschecker) script(pass string, answers ...ccAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[pass] = append(f.queue[pass], answers...)
}

func (f *fakeCrosschecker) requests() []ccCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeCrosschecker) Check(ctx context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	pass := "?"
	switch {
	case strings.HasPrefix(req.Prompt, "pass A"):
		pass = "A"
	case strings.HasPrefix(req.Prompt, "pass B"):
		pass = "B"
	}
	call := ccCall{Pass: pass, Request: req}
	if _, err := os.Stat(req.Worktree); err == nil {
		call.Existed = true
		if at, err := Head(ctx, ExecGit("git"), req.Worktree); err == nil {
			call.HeadAt = at
		}
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	a := ccAnswer{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified}
	if q := f.queue[pass]; len(q) > 0 {
		a, f.queue[pass] = q[0], q[1:]
	}
	f.mu.Unlock()
	if a.State == "" {
		a.State = reviewadapter.StateOK
	}
	findings := []reviewadapter.Finding{}
	verdict := ""
	if a.State == reviewadapter.StateOK {
		verdict = a.Verdict
		if verdict == "" {
			verdict = reviewadapter.VerdictVerified
		}
		for _, x := range a.Findings {
			if x.ID == "" {
				x.ID = reviewadapter.FindingID(x.File, x.Text)
			}
			findings = append(findings, x)
		}
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	return reviewadapter.Result{State: a.State, Verdict: verdict, Findings: findings, EngineFamily: profileFamily(req.Profile), Model: req.Profile.Model, Effort: req.Profile.Effort,
		GuideRev: req.GuideRev, PromptDigest: hex.EncodeToString(sum[:]), ElapsedMS: 7}, nil
}

// ccCheckout is the fake checkout of the harnesses that close without the cli: a fresh directory under LandRoot per pass.
type ccCheckout struct {
	mu    sync.Mutex
	root  string
	made  []string
	freed []string
}

func (c *ccCheckout) checkout(_ context.Context, ms string, round, evidenceRound int, pass string, attempt int, head string) (string, func() error, error) {
	dir := filepath.Join(c.root, "crosscheck-wt", fmt.Sprintf("%s-r%d-e%d-%s%d", ms, round, evidenceRound, pass, attempt))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	c.mu.Lock()
	c.made = append(c.made, dir)
	c.mu.Unlock()
	return dir, func() error {
		c.mu.Lock()
		c.freed = append(c.freed, dir)
		c.mu.Unlock()
		return os.RemoveAll(dir)
	}, nil
}

func (c *ccCheckout) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.made)
}

// ccWorld is a lane with a crosschecker, a journal holding the milestone M1 of plan-1 and the test's LandRoot.
type ccWorld struct {
	*world
	checker  *fakeCrosschecker
	checkout *ccCheckout
	landRoot string
	ms       Milestone
	authors  map[string]string
	profiles []string // the run directories Profiles was asked for
	gitArgs  [][]string
	gitMu    sync.Mutex
	repo     string
	realGit  bool
}

func claudeAuthors() map[string]string {
	return map[string]string{"architect": "claude-fake-opus", "executor": "claude-fake-haiku", "executor-t1": "claude-fake-sonnet"}
}

func newCCWorld(t *testing.T, nodes ...string) *ccWorld {
	t.Helper()
	if len(nodes) == 0 {
		nodes = []string{"n1"}
	}
	w := &ccWorld{world: newWorld(t), checker: newFakeCrosschecker(), landRoot: t.TempDir(), authors: claudeAuthors()}
	w.checkout = &ccCheckout{root: w.landRoot}
	w.ms = Milestone{ID: "M1", Plan: "plan-1", Title: "milestone one", Criteria: []string{"the milestone is done"}, CriteriaDigest: "crit-1"}
	for _, n := range nodes {
		w.ms.Nodes = append(w.ms.Nodes, PlanNode{ID: n, Issue: "CRW-" + n, Criteria: []string{"the work of " + n}})
	}
	raw, _ := json.Marshal(w.ms)
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_milestone", Op: OpMilestone, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": "M1", "plan": "plan-1", "result": string(raw)}})
	w.build()
	return w
}

// build makes the lane again over the world's settings.
func (w *ccWorld) build() {
	cc := &Crosscheck{Checker: w.checker, Limit: time.Minute, Authors: w.authors,
		Profiles: func(runDir string) ([]reviewadapter.Profile, error) {
			w.profiles = append(w.profiles, runDir)
			return []reviewadapter.Profile{reviewadapter.AstraProfile(filepath.Join(runDir, "crosscheck", "answer.strict.json")), reviewadapter.GrokProfile(reviewadapter.AnswerSchema)}, nil
		}}
	d := Deps{RunID: "run-1", Journal: w.j, Relay: w.relay, Decisions: &MemoryDecisions{}, Actor: "parent", LandRoot: w.landRoot, Crosscheck: cc,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }}
	if w.realGit {
		d.Repo = w.repo
		d.Git = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
			w.gitMu.Lock()
			w.gitArgs = append(w.gitArgs, slices.Clone(args))
			w.gitMu.Unlock()
			return ExecGit("git")(ctx, dir, env, args...)
		}
	} else {
		cc.Checkout = w.checkout.checkout
		d.Repo = "/repo"
	}
	w.lane = NewLane(d)
}

// worktreeAdds are the `git worktree add` calls the lane made, by the directory they made.
func (w *ccWorld) worktreeAdds() []string {
	w.gitMu.Lock()
	defer w.gitMu.Unlock()
	var out []string
	for _, a := range w.gitArgs {
		if len(a) >= 2 && a[0] == "worktree" && a[1] == "add" {
			out = append(out, a[len(a)-2])
		}
	}
	return out
}

func (w *ccWorld) gate(head string) map[string]any {
	return map[string]any{"milestone": "M1", "plan": "plan-1", "head": head, "base": "base000", "range": "base000.." + head, "state": GatePass}
}

func ccOpts(extra map[string]any) map[string]any {
	o := map[string]any{"round": 1, "prompts": map[string]any{"A": "pass A: coverage of M1\n", "B": "pass B: bug hunt of M1\n"}}
	for k, v := range extra {
		o[k] = v
	}
	return o
}

// crosscheck runs crw.crosscheck over the gate at head and answers its outcome and its row.
func (w *ccWorld) crosscheck(head string, opts map[string]any) (CrosscheckOutcome, engine.Event) {
	w.t.Helper()
	raw := w.must(NameCrosscheck, "M1", w.gate(head), ccOpts(opts))
	var out CrosscheckOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		w.t.Fatalf("the crosscheck's answer: %v: %s", err, raw)
	}
	evs := w.j.Events()
	return out, evs[len(evs)-1]
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	norm := func(v any) string {
		var raw []byte
		switch x := v.(type) {
		case json.RawMessage:
			raw = x
		case []byte:
			raw = x
		default:
			var err error
			if raw, err = json.Marshal(v); err != nil {
				t.Fatal(err)
			}
		}
		var any_ any
		if err := json.Unmarshal(raw, &any_); err != nil {
			t.Fatalf("not JSON: %v: %s", err, raw)
		}
		out, _ := json.Marshal(any_)
		return string(out)
	}
	return norm(a) == norm(b)
}

// TestCrosscheckVerdictCarriesGuideRev: a clean round 1 answers state verdict with V, which names the guide revision, the engine, the
// round, the milestone, the plan, the gate's head and base, the run and the authors; the row carries the same V.
func TestCrosscheckVerdictCarriesGuideRev(t *testing.T) {
	w := newCCWorld(t)
	out, row := w.crosscheck(ccHead, nil)
	if out.State != CrosscheckStateVerdict || out.Verdict == nil {
		t.Fatalf("outcome %+v", out)
	}
	v := out.Verdict
	if v.Verdict != reviewadapter.VerdictVerified || len(v.Findings) != 0 || v.Findings == nil || v.Carried == nil || v.FollowUps == nil {
		t.Errorf("verdict %q findings %v carried %v follow-ups %v", v.Verdict, v.Findings, v.Carried, v.FollowUps)
	}
	if v.GuideRev != CrosscheckGuideRev || CrosscheckGuideRev != "crw-crosscheck/1" {
		t.Errorf("guide rev %q", v.GuideRev)
	}
	if v.EngineFamily != "gpt" || !slices.Equal(v.EngineFamilies, []string{"gpt"}) || v.Model != "gpt-6-astra" {
		t.Errorf("engine %q %v %q", v.EngineFamily, v.EngineFamilies, v.Model)
	}
	if v.Round != 1 || v.EvidenceRound != 0 || v.Milestone != "M1" || v.Plan != "plan-1" || v.Head != ccHead || v.Base != "base000" || v.Run != "run-1" {
		t.Errorf("round %d/%d milestone %q plan %q head %q base %q run %q", v.Round, v.EvidenceRound, v.Milestone, v.Plan, v.Head, v.Base, v.Run)
	}
	if !slices.Equal(v.AuthorFamilies, []string{"claude"}) {
		t.Errorf("author families %v", v.AuthorFamilies)
	}
	want := []CrosscheckAuthor{{Source: "role", Name: "architect", Model: "claude-fake-opus", Family: "claude"}, {Source: "role", Name: "executor", Model: "claude-fake-haiku", Family: "claude"},
		{Source: "role", Name: "executor-t1", Model: "claude-fake-sonnet", Family: "claude"}}
	if !slices.Equal(v.Authors, want) {
		t.Errorf("authors %+v", v.Authors)
	}
	if v.PriorHead != "" || v.PriorRun != "" || v.PriorDigest != "" {
		t.Errorf("round 1 names a prior: %q %q %q", v.PriorHead, v.PriorRun, v.PriorDigest)
	}
	if len(v.Passes) != 2 || v.Passes[0].Pass != "A" || v.Passes[1].Pass != "B" || v.Passes[0].Attempt != 1 || v.Passes[0].State != "ok" || v.Passes[0].Model != "gpt-6-astra" || v.Passes[0].PromptDigest == "" {
		t.Errorf("passes %+v", v.Passes)
	}
	if row.Op != OpCrosscheck || OpCrosscheck != "crw.crosscheck" || dataStr(row, "milestone") != "M1" || dataStr(row, "head") != ccHead || intOf(row.Data["round"]) != 1 ||
		intOf(row.Data["evidence_round"]) != 0 || dataStr(row, "state") != CrosscheckStateVerdict || dataStr(row, "hold") != "" {
		t.Errorf("row %+v", row.Data)
	}
	if !jsonEqual(t, row.Data["verdict"], v) {
		t.Errorf("the row's verdict is not V:\n%v\n%+v", row.Data["verdict"], v)
	}
	for _, c := range w.checker.requests() {
		if c.Request.GuideRev != CrosscheckGuideRev || !bytes.Equal(c.Request.Schema, reviewadapter.AnswerSchema) || c.Request.Profile.Name != "astra" || c.Request.Limit != time.Minute {
			t.Errorf("request %+v", c.Request)
		}
	}
	if n := len(w.checker.requests()); n != 2 {
		t.Errorf("%d checks, want 2", n)
	}
}

// TestCrosscheckMergesThePassesKeepingTheHigherSeverity: the same finding of both passes is one finding of the higher severity and the
// blocking impact, whichever pass held which copy; a finding that does not block is carried as a follow-up.
func TestCrosscheckMergesThePassesKeepingTheHigherSeverity(t *testing.T) {
	minor := reviewadapter.Finding{ID: "f1", Severity: "P3", Impact: "minor_separable", File: "a.go", Line: 3, Text: "the name is long"}
	major := reviewadapter.Finding{ID: "f1", Severity: "P1", Impact: "core_function", File: "a.go", Line: 9, Text: "the name is wrong"}
	for _, c := range []struct {
		name string
		a, b reviewadapter.Finding
		line int
	}{{"A minor, B major", minor, major, 3}, {"A major, B minor", major, minor, 9}} {
		t.Run(c.name, func(t *testing.T) {
			w := newCCWorld(t)
			w.checker.script("A", ccAnswer{Verdict: verdictFor(c.a), Findings: []reviewadapter.Finding{c.a}})
			w.checker.script("B", ccAnswer{Verdict: verdictFor(c.b), Findings: []reviewadapter.Finding{c.b}})
			out, _ := w.crosscheck(ccHead, nil)
			if out.State != CrosscheckStateVerdict || out.Verdict == nil {
				t.Fatalf("outcome %+v", out)
			}
			v := out.Verdict
			if v.Verdict != reviewadapter.VerdictNeedsChanges || len(v.Findings) != 1 {
				t.Fatalf("verdict %q findings %+v", v.Verdict, v.Findings)
			}
			f := v.Findings[0]
			if f.Severity != "P1" || f.Impact != "core_function" || f.Line != c.line || len(v.Carried) != 0 || len(v.FollowUps) != 0 {
				t.Errorf("finding %+v carried %v follow-ups %v", f, v.Carried, v.FollowUps)
			}
		})
	}
	t.Run("a finding that does not block is carried", func(t *testing.T) {
		w := newCCWorld(t)
		w.checker.script("B", ccAnswer{Verdict: reviewadapter.VerdictVerified, Findings: []reviewadapter.Finding{minor}})
		out, _ := w.crosscheck(ccHead, nil)
		v := out.Verdict
		if v == nil || v.Verdict != reviewadapter.VerdictVerified || !slices.Equal(v.Carried, []string{"f1"}) || !slices.Equal(v.FollowUps, []string{"f1 [P3/minor_separable] a.go:3 the name is long"}) {
			t.Errorf("outcome %+v", out)
		}
	})
}

func verdictFor(f reviewadapter.Finding) string {
	if blocks(f) {
		return reviewadapter.VerdictNeedsChanges
	}
	return reviewadapter.VerdictVerified
}

func blocks(f reviewadapter.Finding) bool {
	v, _ := CrosscheckVerdictOf([]reviewadapter.Finding{f})
	return v == reviewadapter.VerdictNeedsChanges
}

// TestCrosscheckProofNeedsTheSameHead: a crosscheck row proves the crosscheck of its milestone, head, round and evidence round; another
// head or round is not proven by it, and a held crosscheck proves nothing.
func TestCrosscheckProofNeedsTheSameHead(t *testing.T) {
	var events []engine.Event
	add := func(d map[string]any) engine.Event {
		ev := engine.Event{Seq: len(events) + 1, Kind: engine.KindCall, CallID: fmt.Sprintf("c%d", len(events)+1), Op: OpCrosscheck, Event: engine.EvCompleted, Data: d}
		events = append(events, ev)
		return ev
	}
	add(map[string]any{"milestone": "M1", "head": "h1", "round": float64(1), "evidence_round": float64(0), "state": CrosscheckStateVerdict})
	held := add(map[string]any{"milestone": "M1", "head": "h2", "round": float64(1), "evidence_round": float64(0), "state": CrosscheckStateHeld, "hold": "crosscheck_quota"})
	if !CrosscheckProof(events, "M1", "h1", 1, 0).Proven {
		t.Error("the same milestone, head, round and evidence round")
	}
	for name, v := range map[string]Verdict{
		"another head":           CrosscheckProof(events, "M1", "h9", 1, 0),
		"another round":          CrosscheckProof(events, "M1", "h1", 2, 0),
		"another evidence round": CrosscheckProof(events, "M1", "h1", 1, 1),
		"another milestone":      CrosscheckProof(events, "M2", "h1", 1, 0),
		"held":                   CrosscheckProof(events, "M1", "h2", 1, 0),
	} {
		if v.Proven {
			t.Errorf("%s: %+v", name, v)
		}
	}
	for _, st := range []string{CrosscheckStateRefused, CrosscheckStateFailed, CrosscheckStateNoVerdict, CrosscheckStateEvidence} {
		add(map[string]any{"milestone": "M1", "head": "h-" + st, "round": float64(1), "evidence_round": float64(0), "state": st})
		if !CrosscheckProof(events, "M1", "h-"+st, 1, 0).Proven {
			t.Errorf("state %s is a recorded crosscheck", st)
		}
	}
	w := newCCWorld(t)
	op := w.lane.Ops()[NameCrosscheck]
	if ok, _, err := op.Proven(context.Background(), events, held); ok || err != nil {
		t.Errorf("a held crosscheck is proven: %v %v", ok, err)
	}
	if ok, _, err := op.Proven(context.Background(), events, events[0]); !ok || err != nil {
		t.Errorf("a recorded verdict is not proven: %v %v", ok, err)
	}
}

// ccRepo makes the world's lane run the real checkout over a temporary repository of two commits; it answers the two commits.
func (w *ccWorld) ccRepo() (first, second string) {
	w.t.Helper()
	repo, first := newRepo(w.t)
	write(w.t, repo, "a.txt", "alpha two\n", 0o644)
	gitIn(w.t, repo, "add", "-A")
	gitIn(w.t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "second")
	second = gitIn(w.t, repo, "rev-parse", "HEAD")
	w.repo, w.realGit = repo, true
	w.build()
	return first, second
}

func (w *ccWorld) passDir(pass, head string) string {
	return filepath.Join(w.landRoot, "crosscheck-wt", "M1-r1-e0-"+pass+"1-"+head[:12])
}

// TestCrosscheckChecksOutEachPassAndRemovesTheCheckout: each pass runs in a checkout of its own at the gate head under LandRoot, which
// exists while the pass runs and is removed after it; a LandRoot that is empty or relative refuses before any file or checkout is made.
func TestCrosscheckChecksOutEachPassAndRemovesTheCheckout(t *testing.T) {
	w := newCCWorld(t)
	_, head := w.ccRepo()
	out, _ := w.crosscheck(head, nil)
	if out.State != CrosscheckStateVerdict {
		t.Fatalf("outcome %+v", out)
	}
	calls := w.checker.requests()
	if len(calls) != 2 {
		t.Fatalf("%d checks", len(calls))
	}
	seen := map[string]bool{}
	for _, c := range calls {
		want := w.passDir(c.Pass, head)
		if c.Request.Worktree != want || !c.Existed || c.HeadAt != head {
			t.Errorf("pass %s ran in %q (existed %v, at %q), want %q at %s", c.Pass, c.Request.Worktree, c.Existed, c.HeadAt, want, head)
		}
		seen[c.Request.Worktree] = true
		if _, err := os.Stat(c.Request.Worktree); !os.IsNotExist(err) {
			t.Errorf("the checkout %s is still there: %v", c.Request.Worktree, err)
		}
		if strings.Contains(gitIn(t, w.repo, "worktree", "list", "--porcelain"), c.Request.Worktree) {
			t.Errorf("the repository still lists the worktree %s", c.Request.Worktree)
		}
	}
	if len(seen) != 2 {
		t.Errorf("the passes shared a checkout: %v", seen)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{"", "land"} {
		t.Run("LandRoot "+root, func(t *testing.T) {
			v := newCCWorld(t)
			v.landRoot = root
			v.build()
			out, row := v.crosscheck(ccHead, nil)
			if out.State != CrosscheckStateFailed || out.Reason != ReasonCrosscheckNoRunDir || out.Verdict != nil || dataStr(row, "reason") != ReasonCrosscheckNoRunDir {
				t.Errorf("outcome %+v", out)
			}
			if n := len(v.checker.requests()); n != 0 || v.checkout.count() != 0 || len(v.profiles) != 0 {
				t.Errorf("%d checks, %d checkouts, profiles asked for %v", n, v.checkout.count(), v.profiles)
			}
			for _, name := range []string{"land", "crosscheck", "crosscheck-wt"} {
				if _, err := os.Stat(filepath.Join(wd, name)); !os.IsNotExist(err) {
					t.Errorf("%s was made in the test's working directory: %v", name, err)
				}
			}
		})
	}
}

// TestCrosscheckReusesACheckoutOnlyWhenItsTreeIsTheHeadTree: a resume that finds a pass's checkout uses it again only when it is at the
// gate head and its working files are the head's tree. A checkout with a modified or untracked file is failed dirty and kept as it is,
// one at another commit failed moved and kept; neither runs a pass. A clean one is used again without a second `worktree add`.
func TestCrosscheckReusesACheckoutOnlyWhenItsTreeIsTheHeadTree(t *testing.T) {
	w := newCCWorld(t)
	first, head := w.ccRepo()
	dirA := w.passDir("A", head)
	if err := os.MkdirAll(filepath.Dir(dirA), 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w.repo, "worktree", "add", "--detach", "-q", dirA, head)
	write(t, dirA, "a.txt", "edited by an interrupted pass\n", 0o644)
	write(t, dirA, "left.txt", "untracked\n", 0o644)

	out, _ := w.crosscheck(head, nil)
	if out.State != CrosscheckStateFailed || out.Reason != ReasonCrosscheckWorktreeDirty || !strings.Contains(out.Detail, dirA) || out.Verdict != nil {
		t.Fatalf("a dirty checkout: %+v", out)
	}
	if n := len(w.checker.requests()); n != 0 {
		t.Errorf("%d checks ran over a dirty checkout", n)
	}
	if b, err := os.ReadFile(filepath.Join(dirA, "a.txt")); err != nil || string(b) != "edited by an interrupted pass\n" {
		t.Errorf("the dirty checkout was not kept as it was: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dirA, "left.txt")); err != nil {
		t.Errorf("the untracked file is gone: %v", err)
	}

	// the same checkout made clean is used again
	gitIn(t, dirA, "checkout", "--", "a.txt")
	if err := os.Remove(filepath.Join(dirA, "left.txt")); err != nil {
		t.Fatal(err)
	}
	addsBefore := len(w.worktreeAdds())
	out, _ = w.crosscheck(head, nil)
	if out.State != CrosscheckStateVerdict {
		t.Fatalf("a clean checkout: %+v", out)
	}
	var ranInA bool
	for _, c := range w.checker.requests() {
		if c.Pass == "A" && c.Request.Worktree == dirA && c.Existed && c.HeadAt == head {
			ranInA = true
		}
	}
	if !ranInA {
		t.Errorf("pass A did not run in the checkout it found: %+v", w.checker.requests())
	}
	adds := w.worktreeAdds()[addsBefore:]
	if slices.Contains(adds, dirA) || len(adds) != 1 || adds[0] != w.passDir("B", head) {
		t.Errorf("worktree adds %v: pass A's checkout was made again, or B's was not made", adds)
	}

	// a checkout at another commit is not used
	gitIn(t, w.repo, "worktree", "add", "--detach", "-q", dirA, first)
	n := len(w.checker.requests())
	out, _ = w.crosscheck(head, nil)
	if out.State != CrosscheckStateFailed || out.Reason != ReasonCrosscheckWorktreeMoved || !strings.Contains(out.Detail, dirA) {
		t.Fatalf("a checkout at another commit: %+v", out)
	}
	if len(w.checker.requests()) != n {
		t.Errorf("a pass ran over a checkout at another commit")
	}
	if at := gitIn(t, dirA, "rev-parse", "HEAD"); at != first {
		t.Errorf("the checkout at another commit was not kept: at %s", at)
	}
}

// receiptRow writes a completed unit-check row of a node with the receipt it recorded.
func (w *ccWorld) receiptRow(node string, receipt map[string]any) {
	w.calls++
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: fmt.Sprintf("c_uc%d", w.calls), Op: OpUnitCheck, Event: engine.EvCompleted,
		Data: map[string]any{"role": "unit-check", "node": node, "state": "ok", "receipt": receipt}})
}

// TestCrosscheckCarriesTheReceiptsAndRedRecords (C12): the newest unit-check receipt of each node of the milestone goes to the receipts
// file under LandRoot, V names the file, its digest and each node's receipt, and both passes are told to read it and check the red
// records of each criterion against it.
func TestCrosscheckCarriesTheReceiptsAndRedRecords(t *testing.T) {
	w := newCCWorld(t, "n1", "n2", "n3")
	oldCopy, oldSum := receiptCopy(t, "an older receipt\n")
	w.receiptRow("n1", map[string]any{"state": "accepted", "sha256": oldSum, "copy": oldCopy})
	body := "red-first: TestThing FAIL before the change\nmutation-red: TestThing FAIL with the guard removed\n"
	cp, sum := receiptCopy(t, body)
	w.receiptRow("n1", map[string]any{"state": "accepted", "sha256": sum, "copy": cp})
	w.receiptRow("n2", map[string]any{"state": ReceiptNotAssigned})
	out, row := w.crosscheck(ccHead, nil)
	if out.State != CrosscheckStateVerdict || out.Verdict == nil {
		t.Fatalf("outcome %+v", out)
	}
	v := out.Verdict
	want := []CrosscheckReceipt{{Node: "n1", State: "accepted", SHA256: sum}, {Node: "n2", State: ReceiptNotAssigned}, {Node: "n3", State: ReceiptNoExecutor}}
	if !slices.Equal(v.Receipts, want) {
		t.Errorf("receipts %+v", v.Receipts)
	}
	file := filepath.Join(w.landRoot, "crosscheck", "M1-r1-receipts.md")
	if v.ReceiptsFile != file {
		t.Errorf("receipts file %q, want %q", v.ReceiptsFile, file)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	fileSum := sha256.Sum256(got)
	if v.ReceiptsSHA256 != hex.EncodeToString(fileSum[:]) {
		t.Errorf("receipts sha256 %q", v.ReceiptsSHA256)
	}
	if !strings.Contains(string(got), "## node n1 (sha256 "+sum+")\n") || !strings.Contains(string(got), body) || strings.Contains(string(got), "an older receipt") {
		t.Errorf("the receipts file:\n%s", got)
	}
	calls := w.checker.requests()
	if len(calls) != 2 {
		t.Fatalf("%d checks", len(calls))
	}
	for _, c := range calls {
		p := c.Request.Prompt
		for _, s := range []string{"EXECUTOR RECEIPTS", file, v.ReceiptsSHA256, "node n1: accepted, sha256 " + sum, "node n2: " + ReceiptNotAssigned, "node n3: " + ReceiptNoExecutor,
			"red-first", "mutation-red"} {
			if !strings.Contains(p, s) {
				t.Errorf("pass %s's prompt lacks %q:\n%s", c.Pass, s, p)
			}
		}
		if !strings.HasPrefix(p, "pass "+c.Pass) {
			t.Errorf("pass %s's prompt does not start with the script's text:\n%s", c.Pass, p)
		}
	}
	if !jsonEqual(t, row.Data["verdict"], v) {
		t.Error("the row's V differs")
	}

	// the same round again with the same receipts uses the file as it is; with other receipts it never overwrites the earlier V's file
	again, _ := w.crosscheck(ccHead, nil)
	if again.Verdict == nil || again.Verdict.ReceiptsFile != file || again.Verdict.ReceiptsSHA256 != v.ReceiptsSHA256 {
		t.Errorf("the same receipts again: %+v", again.Verdict)
	}
	cp2, sum2 := receiptCopy(t, "red-first: TestThing FAIL, a later check\n")
	w.receiptRow("n1", map[string]any{"state": "accepted", "sha256": sum2, "copy": cp2})
	later, _ := w.crosscheck("1111111111111111111111111111111111111111", nil)
	if later.Verdict == nil || later.Verdict.ReceiptsFile == file || !strings.HasPrefix(later.Verdict.ReceiptsFile, filepath.Join(w.landRoot, "crosscheck")+string(filepath.Separator)) {
		t.Fatalf("other receipts: %+v", later.Verdict)
	}
	for _, rv := range []*CrosscheckVerdictRecord{v, later.Verdict} {
		b, err := os.ReadFile(rv.ReceiptsFile)
		s := sha256.Sum256(b)
		if err != nil || hex.EncodeToString(s[:]) != rv.ReceiptsSHA256 {
			t.Errorf("the receipts file %s of a V no longer hashes to its receipts_sha256: %v", rv.ReceiptsFile, err)
		}
	}
}

// TestCrosscheckHoldsWhenAReceiptCannotBeRead: a receipt that is missing, unverified, not copied or does not hash to its digest holds the
// crosscheck before any checkout or pass; the row names the node, and a held crosscheck is not proven.
func TestCrosscheckHoldsWhenAReceiptCannotBeRead(t *testing.T) {
	cp, sum := receiptCopy(t, "the receipt\n")
	for _, c := range []struct {
		hold    string
		receipt map[string]any
	}{
		{"crosscheck_receipt_missing", map[string]any{"state": "missing"}},
		{"crosscheck_receipt_unverified", map[string]any{"state": "unverified"}},
		{"crosscheck_receipt_not_copied", map[string]any{"state": "accepted", "sha256": sum, "copy": filepath.Join(t.TempDir(), "gone.md")}},
		{"crosscheck_receipt_mismatch", map[string]any{"state": "accepted", "sha256": strings.Repeat("0", 64), "copy": cp}},
		{"crosscheck_receipt_missing", nil}, // the newest check of n2 was not run for its receipt (receipt_held, no role)
	} {
		t.Run(c.hold, func(t *testing.T) {
			w := newCCWorld(t, "n1", "n2")
			if c.receipt == nil {
				w.receiptRow("n2", map[string]any{"state": "accepted", "sha256": sum, "copy": cp})
				w.calls++
				w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: fmt.Sprintf("c_uc%d", w.calls), Op: OpUnitCheck, Event: engine.EvCompleted,
					Data: map[string]any{"node": "n2", "state": StateReceiptHeld, "hold": "receipt_missing", "detail": "the executor's evidence receipt is missing"}})
			} else {
				w.receiptRow("n2", c.receipt)
			}
			out, row := w.crosscheck(ccHead, nil)
			if out.State != CrosscheckStateHeld || out.Hold != c.hold || out.Verdict != nil || !strings.Contains(out.Detail, "n2") {
				t.Errorf("outcome %+v", out)
			}
			if dataStr(row, "hold") != c.hold || dataStr(row, "node") != "n2" {
				t.Errorf("row %+v", row.Data)
			}
			if n := len(w.checker.requests()); n != 0 || w.checkout.count() != 0 {
				t.Errorf("%d checks and %d checkouts on a held receipt", n, w.checkout.count())
			}
			if ok, _, _ := w.lane.Ops()[NameCrosscheck].Proven(context.Background(), w.j.Events(), row); ok {
				t.Error("a held crosscheck is proven")
			}
		})
	}
}

// TestCrosscheckEngineIsOfAnotherFamilyThanTheAuthors: round 1 takes the first engine (astra, then grok) whose family is no author's
// family, over the run's role pairs and the milestone's author calls; with no such engine it refuses and runs nothing.
func TestCrosscheckEngineIsOfAnotherFamilyThanTheAuthors(t *testing.T) {
	for _, c := range []struct {
		name    string
		authors map[string]string
		call    string // the model of an executor call row of the milestone, when set
		engine  string // the profile name, "" for a refusal
		family  string
	}{
		{"claude authors", claudeAuthors(), "", "astra", "gpt"},
		{"a gpt executor", map[string]string{"architect": "claude-fake-opus", "executor": "gpt-6.1-sol", "executor-t1": "claude-fake-sonnet"}, "", "grok", "grok"},
		{"a grok architect", map[string]string{"architect": "grok-4.7", "executor": "claude-fake-haiku", "executor-t1": "claude-fake-sonnet"}, "", "astra", "gpt"},
		{"claude and gpt", map[string]string{"architect": "claude-fake-opus", "executor": "claude-fake-haiku", "executor-t1": "openai/gpt-6.1-sol"}, "", "grok", "grok"},
		{"gpt and grok", map[string]string{"architect": "grok-4.7", "executor": "gpt-6.1-sol", "executor-t1": "claude-fake-sonnet"}, "", "", ""},
		{"a gpt author call beside claude pairs", claudeAuthors(), "gpt-fake-sol", "grok", "grok"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newCCWorld(t)
			w.authors = c.authors
			w.build()
			if c.call != "" {
				w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_exec1", Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "n1", "model": c.call}})
			}
			out, row := w.crosscheck(ccHead, nil)
			calls := w.checker.requests()
			if c.engine == "" {
				if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckNoIndependentEngine || len(calls) != 0 || w.checkout.count() != 0 {
					t.Errorf("outcome %+v, %d checks, %d checkouts", out, len(calls), w.checkout.count())
				}
				if dataStr(row, "reason") != ReasonCrosscheckNoIndependentEngine {
					t.Errorf("row %+v", row.Data)
				}
				return
			}
			if out.State != CrosscheckStateVerdict || out.Verdict.EngineFamily != c.family || len(calls) != 2 {
				t.Fatalf("outcome %+v, %d checks", out, len(calls))
			}
			for _, call := range calls {
				if call.Request.Profile.Name != c.engine {
					t.Errorf("pass %s ran on %s, want %s", call.Pass, call.Request.Profile.Name, c.engine)
				}
			}
			if slices.Contains(out.Verdict.AuthorFamilies, out.Verdict.EngineFamily) {
				t.Errorf("the engine %s is an author's family %v", out.Verdict.EngineFamily, out.Verdict.AuthorFamilies)
			}
			if c.call != "" && !slices.Contains(out.Verdict.Authors, CrosscheckAuthor{Source: "call", Name: "c_exec1", Model: c.call, Family: "gpt"}) {
				t.Errorf("the author call is not among the authors: %+v", out.Verdict.Authors)
			}
		})
	}
}

// TestCrosscheckRefusesWhenAnAuthorPairIsUndecided: an author role with no pair, or an author call of the milestone that names no
// model, refuses the crosscheck before any checkout or pass, naming what is undecided; no family is guessed for it.
func TestCrosscheckRefusesWhenAnAuthorPairIsUndecided(t *testing.T) {
	t.Run("a role", func(t *testing.T) {
		w := newCCWorld(t)
		w.authors = map[string]string{"architect": "", "executor": "claude-fake-haiku", "executor-t1": "claude-fake-sonnet"}
		w.build()
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckAuthorUndecided || !strings.Contains(out.Detail, "architect") {
			t.Errorf("outcome %+v", out)
		}
		if len(w.checker.requests()) != 0 || w.checkout.count() != 0 {
			t.Error("a pass ran or a checkout was made")
		}
	})
	t.Run("a call", func(t *testing.T) {
		w := newCCWorld(t)
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_exec_nomodel", Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "n1", "model": ""}})
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckAuthorUndecided || !strings.Contains(out.Detail, "c_exec_nomodel") {
			t.Errorf("outcome %+v", out)
		}
		if len(w.checker.requests()) != 0 || w.checkout.count() != 0 {
			t.Error("a pass ran or a checkout was made")
		}
	})
	t.Run("no author at all", func(t *testing.T) {
		w := newCCWorld(t)
		w.authors = nil
		w.build()
		out, _ := w.crosscheck(ccHead, nil)
		if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckAuthorUndecided {
			t.Errorf("outcome %+v", out)
		}
	})
}

// TestCrosscheckRefusesAPromptOverTheArgvLimit: a pass prompt longer than CrosscheckPromptLimit is not sent (the tools take it as one
// command-line word): the crosscheck fails before any checkout.
func TestCrosscheckRefusesAPromptOverTheArgvLimit(t *testing.T) {
	w := newCCWorld(t)
	big := "pass A: " + strings.Repeat("x", CrosscheckPromptLimit)
	out, _ := w.crosscheck(ccHead, map[string]any{"prompts": map[string]any{"A": big, "B": "pass B: bug hunt\n"}})
	if out.State != CrosscheckStateFailed || out.Reason != ReasonCrosscheckPromptTooLarge || out.Verdict != nil {
		t.Errorf("outcome %+v", out)
	}
	if len(w.checker.requests()) != 0 || w.checkout.count() != 0 {
		t.Error("a pass ran or a checkout was made")
	}
	if CrosscheckPromptLimit != 120<<10 {
		t.Errorf("limit %d", CrosscheckPromptLimit)
	}
}

// TestCrosscheckFailsOnAPassStateItDoesNotKnow (R4-F3): a pass state the op has no rule for fails the crosscheck by the default branch,
// never a verdict and never a rerun. The state is one no adapter registers.
func TestCrosscheckFailsOnAPassStateItDoesNotKnow(t *testing.T) {
	w := newCCWorld(t)
	w.checker.script("A", ccAnswer{State: "future_unknown"})
	out, row := w.crosscheck(ccHead, nil)
	if out.State != CrosscheckStateFailed || out.Reason != "crosscheck_pass_future_unknown" || out.Verdict != nil {
		t.Errorf("outcome %+v", out)
	}
	if dataStr(row, "state") != CrosscheckStateFailed || row.Data["verdict"] != nil {
		t.Errorf("row %+v", row.Data)
	}
	if n := len(w.checker.requests()); n != 2 {
		t.Errorf("%d checks, want 2 (no rerun)", n)
	}
}

// TestCrosscheckServesRoundOneOnly: this op serves round 1; a round other than 1 or any prior is refused before anything runs.
func TestCrosscheckServesRoundOneOnly(t *testing.T) {
	prior := func(ms, verdict string) map[string]any {
		return map[string]any{"guide_rev": CrosscheckGuideRev, "milestone": ms, "plan": "plan-1", "round": 1, "verdict": verdict, "head": ccHead}
	}
	for _, c := range []struct {
		name string
		opts map[string]any
	}{
		{"round 2 with no prior", map[string]any{"round": 2}},
		{"a prior whose verdict is verified", map[string]any{"round": 2, "prior": prior("M1", "verified")}},
		{"a prior of another milestone", map[string]any{"round": 2, "prior": prior("M2", "needs_changes")}},
		{"round 1 with a prior", map[string]any{"round": 1, "prior": prior("M1", "needs_changes")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newCCWorld(t)
			out, _ := w.crosscheck(ccHead, c.opts)
			if out.State != CrosscheckStateRefused || out.Reason != ReasonCrosscheckPriorInvalid || out.Verdict != nil {
				t.Errorf("outcome %+v", out)
			}
			if len(w.checker.requests()) != 0 || w.checkout.count() != 0 {
				t.Error("a pass ran or a checkout was made")
			}
		})
	}
}

// TestMilestoneCloseCarriesTheCrosscheckEvidence: crw.milestoneClose carries the crosscheck of its evidence into its answer as it is,
// and records a close whose evidence has a final review and no crosscheck as before, with a null crosscheck; it refuses nothing.
func TestMilestoneCloseCarriesTheCrosscheckEvidence(t *testing.T) {
	w := newWorld(t)
	tip := w.relay.tip
	v := json.RawMessage(`{"verdict":"verified","guide_rev":"crw-crosscheck/1","milestone":"M1","head":"` + tip + `","findings":[]}`)
	raw := w.must(NameMilestoneClose, "M1", map[string]any{"gate": map[string]any{"head": tip}, "crosscheck": v})
	var out CloseResultOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(t, out.Crosscheck, v) || out.Reflection != "" || string(out.Final) != "null" || out.Moved {
		t.Errorf("close %+v", out)
	}
	final := json.RawMessage(`{"verdict":"verified"}`)
	raw = w.must(NameMilestoneClose, "M1", map[string]any{"gate": map[string]any{"head": tip}, "final": final})
	out = CloseResultOutcome{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if string(out.Crosscheck) != "null" || !jsonEqual(t, out.Final, final) || out.GateHead != tip {
		t.Errorf("close %+v", out)
	}
	// a needs_changes crosscheck is carried too: the refusal is the close's to add (u1473-close)
	nc := json.RawMessage(`{"verdict":"needs_changes"}`)
	if _, err := w.op(NameMilestoneClose, "M1", map[string]any{"gate": map[string]any{"head": tip}, "crosscheck": nc}); err != nil {
		t.Errorf("a close with a needs_changes crosscheck: %v", err)
	}
}
