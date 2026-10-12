package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var ctx = context.Background()

const (
	eventID  = "0123456789abcdef0123456789abcdef"
	childRun = "run-child"
	digestA  = "digest-of-criteria-a"
)

// fx is one application: a temporary store holding the child's run, a temporary git repository holding its commits, a clock the test moves
// and a relay that records what it was asked.
type fx struct {
	t    *testing.T
	st   *store.Store
	j    *journal.Journal
	repo string
	base string
	now  time.Time
	seq  int
	deps Deps
	ev   EventInfo

	sleeps    int
	onSleep   func()
	receives  []ReceiveInput
	files     []string
	corrected []CorrectionInput
	refuse    error
}

func git(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFx(t *testing.T) *fx {
	t.Helper()
	f := &fx{t: t, now: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	f.repo = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(f.repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, f.repo, "init", "-q", "-b", "main")
	f.commit("base.txt", "base")
	f.base = git(t, f.repo, "rev-parse", "HEAD")
	f.j, err = journal.Open(ctx, st, childRun, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "child-task", EpochNonce: "n"}, journal.Options{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.j.Append(engine.Event{RunID: childRun, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": IssueLane, "script_digest": "d", "args_digest": "a"}})
	f.ev = EventInfo{ID: eventID, Relationship: "rel-1", ChildTask: "child-task", ParentTask: "orch-task", CriteriaDigest: digestA, Plan: "plan-1", Node: "A"}
	f.deps = Deps{
		Store:  st,
		Events: eventsFunc(func() (EventInfo, error) { return f.ev, nil }),
		Commits: func(ctx context.Context, repo, base, head string) ([]string, error) {
			return Git(ctx, repo, base, head)
		},
		Receive: func(_ context.Context, in ReceiveInput) (json.RawMessage, error) {
			f.receives = append(f.receives, in)
			raw, _ := os.ReadFile(in.VerdictFile)
			f.files = append(f.files, string(raw))
			if f.refuse != nil {
				return nil, f.refuse
			}
			return json.RawMessage(`{"claim":{"proceed":true},"ack":{"state":"verified"},"verdict":{"verdict":"recorded"}}`), nil
		},
		Correct: func(_ context.Context, in CorrectionInput) (string, error) {
			f.corrected = append(f.corrected, in)
			return "crw-correction manifest m1 path /tmp/m1 sha abc (dispatch request dr-1)", nil
		},
		Repo: f.repo,
		Now:  func() time.Time { return f.now },
		Sleep: func(_ context.Context, d time.Duration) error {
			f.sleeps++
			f.now = f.now.Add(d)
			if f.onSleep != nil {
				f.onSleep()
			}
			return nil
		},
	}
	return f
}

type eventsFunc func() (EventInfo, error)

func (e eventsFunc) Event(context.Context, string) (EventInfo, error) { return e() }

// commit makes a commit in the repository and answers its id.
func (f *fx) commit(file, text string) string {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.repo, file), []byte(text), 0o644); err != nil {
		f.t.Fatal(err)
	}
	git(f.t, f.repo, "add", "-A")
	git(f.t, f.repo, "commit", "-q", "-m", text)
	return git(f.t, f.repo, "rev-parse", "HEAD")
}

func (f *fx) row(call, op, event string, data map[string]any) {
	f.t.Helper()
	f.seq++
	f.now = f.now.Add(time.Second)
	f.j.Append(engine.Event{RunID: childRun, Kind: engine.KindCall, CallID: call, Path: "/", Position: f.seq, Op: op, Event: event, Data: data})
	if err := f.j.Flush(); err != nil {
		f.t.Fatal(err)
	}
}

// agentCall journals a completed agent call of a role on a configured model.
func (f *fx) agentCall(call, role, family, model, effort string) {
	f.row(call, "agent", engine.EvPrepared, map[string]any{"role": role, "family": family, "node": "A", "model": model, "effort": effort,
		"allocation": map[string]any{"start_commit": f.base, "kind": "fresh", "path": "/w"}})
	f.row(call, "agent", engine.EvThreadStarted, map[string]any{"thread_id": "t-" + call, "model": model, "effort": effort})
	f.row(call, "agent", engine.EvCompleted, map[string]any{"thread_id": "t-" + call, "result": "{}"})
}

// executor journals the executor call that made commit.
func (f *fx) executor(call, commit, model string) {
	f.agentCall(call, "executor", "executor", model, "xhigh")
	f.row("commit-"+call, "crw.commit", engine.EvCompleted, map[string]any{"node": "A", "commit": commit, "parent": f.base, "call": call, "result": "{}"})
}

// review journals the reviewer call and the review row it left, on head.
func (f *fx) review(call, head, model, verdict, criteria string) {
	f.agentCall(call, "reviewer", "reviewer", model, "high")
	doc := map[string]any{"verdict": verdict,
		"criteria": []map[string]any{{"id": "c1", "met": verdict == "verified", "evidence": "read the head"}, {"id": "c2", "met": true, "evidence": "tests"}},
		"defects":  []map[string]any{}, "summary": "s"}
	if verdict == "needs_changes" {
		doc["defects"] = []map[string]any{{"ref": "d1", "kind": "correctness", "severity": "P1", "file": "a.go", "line": 7, "finding": "the loop never ends"}}
	}
	raw, _ := json.Marshal(map[string]any{"state": "verdict", "verdict": doc, "head": head, "criteria_digest": criteria})
	f.row("review-"+call, "crw.review", engine.EvCompleted, map[string]any{"node": "A", "head": head, "criteria_digest": criteria, "state": "verdict", "verdict": verdict, "call": call, "reviewer_model": model, "result": string(raw)})
}

func (f *fx) apply(req Request) Result {
	f.t.Helper()
	if req.Event == "" {
		req.Event = eventID
	}
	if req.Turn == "" {
		req.Turn = "turn-orch-1"
	}
	res, err := Apply(ctx, f.deps, req)
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return res
}

func (f *fx) flowRows(kinds ...string) []report.Row {
	f.t.Helper()
	rows, err := report.ReadRows(ctx, f.st.DB, childRun, kinds...)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// goodRun journals one executor commit and a verified review of it, and answers the head.
func (f *fx) goodRun() string {
	head := f.commit("a.txt", "work of the executor")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	f.review("rv-1", head, "gpt-fake-sol", "verified", digestA)
	return head
}

// The wait is a read of the child's journal: the verdict that appears while apply waits is the one it applies, claim, acknowledgement and
// verdict go from the orchestrator's own turn, and the relay is asked once.
func TestApplyWaitsForJournalVerdict(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work of the executor")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	f.onSleep = func() {
		if f.sleeps == 3 {
			f.review("rv-1", head, "gpt-fake-sol", "verified", digestA)
		}
	}
	res := f.apply(Request{Wait: 10 * time.Minute})
	if res.State != StateApplied || res.Verdict != "verified" || res.Run != childRun {
		t.Fatalf("result %+v", res)
	}
	if f.sleeps != 3 {
		t.Errorf("slept %d times, want 3", f.sleeps)
	}
	if len(f.receives) != 1 || f.receives[0].Event != eventID || f.receives[0].Turn != "turn-orch-1" {
		t.Fatalf("receives %+v", f.receives)
	}
	var file map[string]any
	if err := json.Unmarshal([]byte(f.files[0]), &file); err != nil {
		t.Fatal(err)
	}
	if file["verdict"] != "verified" || file["expect-criteria-digest"] != digestA || file["verified-head"] != head {
		t.Errorf("the verdict file is %v", file)
	}
	if len(res.Attributions) != 1 || res.Attributions[0].Commit != head || res.Attributions[0].Call != "ex-1" {
		t.Errorf("attributions %+v", res.Attributions)
	}
	if rows := f.flowRows(KindApplied); len(rows) != 1 || rows[0].Str("event") != eventID || rows[0].Str("turn") != "turn-orch-1" {
		t.Errorf("applied rows %+v", rows)
	}
	// a second application replays: the relay replays its steps, the record stands once
	if again := f.apply(Request{}); again.State != StateApplied || len(f.flowRows(KindApplied)) != 1 {
		t.Errorf("the replay: %+v", again)
	}
}

// The criteria the reviewer saw must be the ones the relationship is judged under; a digest that differs is refused and nothing is
// claimed, acknowledged or ruled.
func TestApplyCriteriaDigestMismatchRefused(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	f.review("rv-1", head, "gpt-fake-sol", "verified", "digest-the-reviewer-saw")
	res := f.apply(Request{})
	if res.State != StateRefused || res.Reason != ReasonCriteriaDigestMismatch {
		t.Fatalf("result %+v", res)
	}
	if len(f.receives) != 0 {
		t.Errorf("the relay was asked: %+v", f.receives)
	}
	// an override never lets a criteria digest through
	if res := f.apply(Request{Override: "I looked"}); res.State != StateRefused || res.Reason != ReasonCriteriaDigestMismatch {
		t.Errorf("override: %+v", res)
	}
}

// A needs_changes verdict carries the dag-correct --prepare instruction line as the restoration block of one finding, and the defects of
// the reviewer with it.
func TestApplyNeedsChangesCarriesCorrectionManifest(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	f.review("rv-1", head, "gpt-fake-sol", "needs_changes", digestA)
	res := f.apply(Request{ManifestRequest: "/tmp/manifest-request.json"})
	if res.State != StateApplied || res.Verdict != "needs_changes" || !strings.Contains(res.Correction, "manifest m1") {
		t.Fatalf("result %+v", res)
	}
	if len(f.corrected) != 1 || f.corrected[0] != (CorrectionInput{Plan: "plan-1", Node: "A", Actor: "orch-task", ManifestRequest: "/tmp/manifest-request.json"}) {
		t.Fatalf("correction asked %+v", f.corrected)
	}
	var file struct {
		Verdict     string `json:"verdict"`
		Restoration string `json:"restoration"`
		Criteria    []struct {
			ID, Verdict, Note string
			Restoration       bool
		} `json:"criteria"`
	}
	if err := json.Unmarshal([]byte(f.files[0]), &file); err != nil {
		t.Fatal(err)
	}
	if file.Verdict != "needs_changes" || file.Restoration != "c1" || len(file.Criteria) != 2 {
		t.Fatalf("file %+v", file)
	}
	carrier := file.Criteria[0]
	if carrier.ID != "c1" || !carrier.Restoration || carrier.Verdict != "needs_changes" || !strings.Contains(carrier.Note, "crw-correction manifest m1 path /tmp/m1 sha abc") || !strings.Contains(carrier.Note, "the loop never ends") {
		t.Errorf("the carrier is %+v", carrier)
	}
	if file.Criteria[1].Restoration {
		t.Errorf("a second carrier: %+v", file.Criteria[1])
	}
	// without a manifest request the line cannot be had: refused, and nothing is ruled
	g := newFx(t)
	h := g.commit("a.txt", "work")
	g.ev.Head = h
	g.executor("ex-1", h, "claude-fake-haiku")
	g.review("rv-1", h, "gpt-fake-sol", "needs_changes", digestA)
	if res := g.apply(Request{}); res.State != StateRefused || res.Reason != ReasonCorrectionUnavailable || len(g.receives) != 0 {
		t.Errorf("without a manifest request: %+v", res)
	}
}

// A wait that ends before the verdict writes nothing and answers that the receipt is applied in a later turn.
func TestApplyTimeoutWritesNothing(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	// a review of another head is no verdict for this event
	f.review("rv-0", f.base, "gpt-fake-sol", "verified", digestA)
	before := len(f.flowRows())
	res := f.apply(Request{Wait: 90 * time.Second})
	if res.State != StateNextTurn || res.Reason != "" || !strings.Contains(res.Detail, "later turn") {
		t.Fatalf("result %+v", res)
	}
	if f.sleeps == 0 {
		t.Error("apply did not wait")
	}
	if len(f.receives) != 0 || len(f.flowRows()) != before || len(f.flowRows(KindApplied, KindRefused, KindOverride)) != 0 {
		t.Errorf("the timeout wrote: receives %v, rows %d -> %d", f.receives, before, len(f.flowRows()))
	}
	if f.now.Sub(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)) < 90*time.Second {
		t.Errorf("the wait ended after %v", f.now)
	}
	// wait 0 reads once
	if res := f.apply(Request{}); res.State != StateNextTurn {
		t.Errorf("wait 0: %+v", res)
	}
}

// A reviewer of the family of an author is refused; --override with a reason lets it go on and records the reason, in the run and in the
// verdict.
func TestApplyOverrideRecorded(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	f.review("rv-1", head, "claude-fake-opus", "verified", digestA)
	res := f.apply(Request{})
	if res.State != StateRefused || res.Reason != ReasonIndependenceRefused || len(f.receives) != 0 {
		t.Fatalf("without an override: %+v", res)
	}
	res = f.apply(Request{Override: "no GPT reviewer is available tonight"})
	if res.State != StateApplied || res.Override != "no GPT reviewer is available tonight" || len(f.receives) != 1 {
		t.Fatalf("with an override: %+v", res)
	}
	rows := f.flowRows(KindOverride)
	if len(rows) != 1 || rows[0].Str("reason") != "no GPT reviewer is available tonight" || rows[0].Str("check") != ReasonIndependenceRefused || rows[0].Str("event") != eventID {
		t.Fatalf("override rows %+v", rows)
	}
	if !strings.Contains(f.files[0], "override: no GPT reviewer is available tonight") {
		t.Errorf("the verdict does not carry the reason: %s", f.files[0])
	}
}

// A commit of base..head that no executor call of the run made - the child thread's own - is refused as author_unattributed and escalated.
func TestOversizedChildDirectCommitRefused(t *testing.T) {
	f := newFx(t)
	made := f.commit("a.txt", "made by the executor")
	f.executor("ex-1", made, "claude-fake-haiku")
	direct := f.commit("b.txt", "made by the child thread itself")
	f.ev.Head = direct
	f.review("rv-1", direct, "gpt-fake-sol", "verified", digestA)
	res := f.apply(Request{})
	if res.State != StateRefused || res.Reason != ReasonAuthorUnattributed || !res.Escalated || len(res.Unattributed) != 1 || res.Unattributed[0] != direct {
		t.Fatalf("result %+v", res)
	}
	if len(f.receives) != 0 {
		t.Errorf("the relay was asked: %+v", f.receives)
	}
	esc := f.flowRows(engine.EvEscalation)
	if len(esc) != 1 || esc[0].Str("reason") != ReasonAuthorUnattributed || esc[0].Str("class") != "integrity" {
		t.Fatalf("escalation rows %+v", esc)
	}
	if rows := f.flowRows(KindRefused); len(rows) != 1 {
		t.Errorf("refused rows %+v", rows)
	}
	// asking again is the same refusal and the same one row
	f.apply(Request{})
	if len(f.flowRows(engine.EvEscalation)) != 1 || len(f.flowRows(KindRefused)) != 1 {
		t.Errorf("a second refusal wrote again")
	}
	// the override does not reach an unattributed commit
	if res := f.apply(Request{Override: "trust me"}); res.Reason != ReasonAuthorUnattributed {
		t.Errorf("override: %+v", res)
	}
}

// A child on a SOL pair coordinates and does not author: the run's executors are Claude, the reviewer is GPT sol, and the coordinator's own
// pair is not an author. An executor on the reviewer's family, the direct implementation the design ends, is not independent.
func TestSolCoordinatorClaudeExecutorsAccepted(t *testing.T) {
	f := newFx(t)
	f.j.Append(engine.Event{RunID: childRun, Kind: engine.KindFlow, Event: "coordinator", Data: map[string]any{"model": "gpt-fake-sol", "effort": "high"}})
	h1 := f.commit("a.txt", "first")
	f.executor("ex-1", h1, "claude-fake-haiku")
	h2 := f.commit("b.txt", "second")
	f.executor("ex-2", h2, "claude-fake-sonnet")
	f.ev.Head = h2
	f.review("rv-1", h2, "gpt-fake-sol", "verified", digestA)
	res := f.apply(Request{})
	if res.State != StateApplied || len(res.Attributions) != 2 || res.Attributions[0].Call != "ex-1" || res.Attributions[1].Model != "claude-fake-sonnet" {
		t.Fatalf("result %+v", res)
	}
	if res.ReviewerModel != "gpt-fake-sol" {
		t.Errorf("reviewer %q", res.ReviewerModel)
	}

	g := newFx(t)
	head := g.commit("a.txt", "work")
	g.ev.Head = head
	g.executor("ex-1", head, "gpt-fake-sol")
	g.review("rv-1", head, "gpt-fake-sol", "verified", digestA)
	if res := g.apply(Request{}); res.State != StateRefused || res.Reason != ReasonIndependenceRefused {
		t.Errorf("an executor on the reviewer's family: %+v", res)
	}
}

func TestApplyRefusesBadRequestsAndUnknownEvents(t *testing.T) {
	f := newFx(t)
	if _, err := Apply(ctx, f.deps, Request{Event: "nope", Turn: "t"}); err == nil {
		t.Error("a bad event id was accepted")
	}
	if _, err := Apply(ctx, f.deps, Request{Event: eventID}); err == nil {
		t.Error("a missing turn was accepted")
	}
	f.deps.Events = eventsFunc(func() (EventInfo, error) { return EventInfo{}, ErrUnknownEvent })
	if res := f.apply(Request{}); res.State != StateRefused || res.Reason != ReasonUnknownEvent {
		t.Errorf("unknown event: %+v", res)
	}
	f.deps.Events = eventsFunc(func() (EventInfo, error) { return f.ev, nil })
	f.ev.Head = f.base
	if res := f.apply(Request{Run: "run-missing"}); res.State != StateRefused || res.Reason != ReasonRunNotFound {
		t.Errorf("unknown run: %+v", res)
	}
	// the relay's refusal of a step is the application's refusal, recorded once
	g := newFx(t)
	g.goodRun()
	g.refuse = fmt.Errorf("not_acknowledged: the acknowledgement is unverified")
	if res := g.apply(Request{}); res.State != StateRefused || res.Reason != ReasonReceiveRefused || !strings.Contains(res.Detail, "not_acknowledged") {
		t.Errorf("relay refusal: %+v", res)
	}
}

// The summary of the plan is brought up to date every time apply runs, whatever it came to: the hook is called with the event and the
// child's run after the relay's steps, its failure is told in the result without undoing the verdict, and a wait that ended first or a
// refusal reconciles too (a run that died after its last batch is recovered by the next apply). An event the relay does not hold calls
// nothing.
func TestApplyBringsTheSummaryUpToDateEveryTimeItRuns(t *testing.T) {
	f := newFx(t)
	f.goodRun()
	var calls []string
	f.deps.Reconcile = func(_ context.Context, ev EventInfo, run string) error {
		calls = append(calls, ev.ID+"@"+run)
		return nil
	}
	if res := f.apply(Request{}); res.State != StateApplied || res.SummaryError != "" {
		t.Fatalf("result %+v", res)
	}
	if len(calls) != 1 || calls[0] != eventID+"@"+childRun || len(f.receives) != 1 {
		t.Fatalf("calls %v receives %d", calls, len(f.receives))
	}
	f.deps.Reconcile = func(context.Context, EventInfo, string) error { return fmt.Errorf("the outbox is busy") }
	if res := f.apply(Request{}); res.State != StateApplied || res.SummaryError != "the outbox is busy" {
		t.Fatalf("result %+v", res)
	}

	// a refusal reconciles
	g := newFx(t)
	h := g.commit("a.txt", "work")
	g.ev.Head = h
	g.executor("ex-1", h, "claude-fake-haiku")
	g.review("rv-1", h, "gpt-fake-sol", "verified", "other-digest")
	var gcalls []string
	g.deps.Reconcile = func(_ context.Context, ev EventInfo, run string) error {
		gcalls = append(gcalls, ev.ID+"@"+run)
		return nil
	}
	if res := g.apply(Request{}); res.State != StateRefused || len(gcalls) != 1 || gcalls[0] != eventID+"@"+childRun {
		t.Errorf("a refusal: %+v calls %v", res, gcalls)
	}

	// a wait that ended before the verdict reconciles, and a failure of the hook is told without changing the state
	n := newFx(t)
	head := n.commit("a.txt", "work")
	n.ev.Head = head
	n.executor("ex-1", head, "claude-fake-haiku")
	var ncalls []string
	n.deps.Reconcile = func(_ context.Context, ev EventInfo, run string) error {
		ncalls = append(ncalls, ev.ID+"@"+run)
		return fmt.Errorf("the outbox is busy")
	}
	if res := n.apply(Request{Wait: 10 * time.Second}); res.State != StateNextTurn || res.SummaryError != "the outbox is busy" || len(ncalls) != 1 || ncalls[0] != eventID+"@"+childRun {
		t.Errorf("a wait that expired: %+v calls %v", res, ncalls)
	}

	// an event the relay does not hold is no relationship to reconcile
	u := newFx(t)
	ucalled := false
	u.deps.Reconcile = func(context.Context, EventInfo, string) error { ucalled = true; return nil }
	u.deps.Events = eventsFunc(func() (EventInfo, error) { return EventInfo{}, ErrUnknownEvent })
	if res := u.apply(Request{}); res.Reason != ReasonUnknownEvent || ucalled {
		t.Errorf("unknown event: %+v called %v", res, ucalled)
	}
}

// An explicit --run is still bound to the event: the run must be an issue-lane run of the child task the event came from. The journal of
// another child that holds the same head, criteria digest and an independent verdict does not certify this child's receipt.
// (post-evaluation d2)
func TestApplyExplicitRunMustBelongToTheEventsChild(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work of the executor")
	f.ev.Head = head
	// another child's finished run with the same head, the same criteria digest, an executor and an independent verdict
	const other = "run-other"
	oj, err := journal.Open(ctx, f.st, other, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "other-task", EpochNonce: "n"}, journal.Options{Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	pos := 0
	row := func(call, op, event string, data map[string]any) {
		pos++
		oj.Append(engine.Event{RunID: other, Kind: engine.KindCall, CallID: call, Path: "/", Position: pos, Op: op, Event: event, Data: data})
	}
	oj.Append(engine.Event{RunID: other, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": IssueLane, "script_digest": "d", "args_digest": "a"}})
	agent := func(call, role, model string) {
		row(call, "agent", engine.EvPrepared, map[string]any{"role": role, "family": role, "node": "A", "model": model, "effort": "xhigh", "allocation": map[string]any{"start_commit": f.base, "kind": "fresh", "path": "/w"}})
		row(call, "agent", engine.EvThreadStarted, map[string]any{"thread_id": "t-" + call, "model": model, "effort": "xhigh"})
		row(call, "agent", engine.EvCompleted, map[string]any{"thread_id": "t-" + call, "result": "{}"})
	}
	agent("ex-o", "executor", "claude-fake-haiku")
	row("commit-ex-o", "crw.commit", engine.EvCompleted, map[string]any{"node": "A", "commit": head, "parent": f.base, "call": "ex-o", "result": "{}"})
	agent("rv-o", "reviewer", "gpt-fake-sol")
	doc, _ := json.Marshal(map[string]any{"state": "verdict", "head": head, "criteria_digest": digestA, "verdict": map[string]any{"verdict": "verified",
		"criteria": []map[string]any{{"id": "c1", "met": true, "evidence": "e"}}, "defects": []map[string]any{}, "summary": "s"}})
	row("review-rv-o", "crw.review", engine.EvCompleted, map[string]any{"node": "A", "head": head, "criteria_digest": digestA, "state": "verdict", "verdict": "verified", "call": "rv-o", "reviewer_model": "gpt-fake-sol", "result": string(doc)})
	if err := oj.Flush(); err != nil {
		t.Fatal(err)
	}

	res := f.apply(Request{Run: other})
	if res.State != StateRefused || res.Reason != ReasonRunMismatch {
		t.Fatalf("another child's run certified the receipt: %+v", res)
	}
	if len(f.receives) != 0 || len(f.flowRows(KindApplied)) != 0 {
		t.Errorf("the relay was asked (%d) or an applied row was written", len(f.receives))
	}
	// the override reaches the independence check only
	if res := f.apply(Request{Run: other, Override: "I looked"}); res.Reason != ReasonRunMismatch {
		t.Errorf("override: %+v", res)
	}
	// a run of another workflow under the child's own task is no issue-lane run either
	f.ev.ChildTask = "other-task"
	if res := f.apply(Request{Run: childRun}); res.Reason != ReasonRunMismatch {
		t.Errorf("a run of another actor: %+v", res)
	}
	// the child's own run, named explicitly, applies as before
	f.ev.ChildTask = "child-task"
	f.executor("ex-1", head, "claude-fake-haiku")
	f.review("rv-1", head, "gpt-fake-sol", "verified", digestA)
	if res := f.apply(Request{Run: childRun}); res.State != StateApplied {
		t.Errorf("the child's own run: %+v", res)
	}
}

func (f *fx) runRow(event string, data map[string]any) {
	f.t.Helper()
	f.j.Append(engine.Event{RunID: childRun, Kind: engine.KindRun, Event: event, Data: data})
	if err := f.j.Flush(); err != nil {
		f.t.Fatal(err)
	}
}

// A child that spent its rounds without a reviewer verdict for the head (verification_failed: the CI never passed) returns from its run
// with no verdict row, and none will come. Apply answers that end at once - refused, escalated, nothing claimed - instead of waiting for a
// row the finished run never writes; a run that is held, or resumed after its finish, still has its verdict to come.
// (post-evaluation d7)
func TestApplyRunEndedWithoutVerdictIsRefusedAndEscalated(t *testing.T) {
	f := newFx(t)
	head := f.commit("a.txt", "work of the executor")
	f.ev.Head = head
	f.executor("ex-1", head, "claude-fake-haiku")
	f.runRow("finished", map[string]any{"status": "completed"})

	res := f.apply(Request{Wait: 10 * time.Minute})
	if res.State != StateRefused || res.Reason != ReasonRunEndedUnverified || !res.Escalated || f.sleeps != 0 {
		t.Fatalf("result %+v (slept %d)", res, f.sleeps)
	}
	if len(f.receives) != 0 {
		t.Errorf("the relay was asked: %+v", f.receives)
	}
	if esc := f.flowRows(engine.EvEscalation); len(esc) != 1 || esc[0].Str("reason") != ReasonRunEndedUnverified {
		t.Fatalf("escalation rows %+v", esc)
	}
	if again := f.apply(Request{}); again.Reason != ReasonRunEndedUnverified || len(f.flowRows(engine.EvEscalation)) != 1 || len(f.flowRows(KindRefused)) != 1 {
		t.Errorf("the second answer: %+v rows %d/%d", again, len(f.flowRows(engine.EvEscalation)), len(f.flowRows(KindRefused)))
	}

	// the run goes on after a resume: its verdict is still to come
	g := newFx(t)
	h := g.commit("a.txt", "work")
	g.ev.Head = h
	g.executor("ex-1", h, "claude-fake-haiku")
	g.runRow("finished", map[string]any{"status": "completed"})
	g.runRow("started", map[string]any{"workflow": IssueLane})
	if res := g.apply(Request{}); res.State != StateNextTurn {
		t.Errorf("a resumed run: %+v", res)
	}
	// a head the finished run did not make is not its to judge: a later run of the child may still rule on it
	n := newFx(t)
	made := n.commit("a.txt", "work")
	n.executor("ex-1", made, "claude-fake-haiku")
	n.ev.Head = n.commit("b.txt", "a commit on top that the run did not make")
	n.runRow("finished", map[string]any{"status": "completed"})
	if res := n.apply(Request{}); res.State != StateNextTurn {
		t.Errorf("a head the run did not make: %+v", res)
	}
	// a run held for the host ended its process too and waits for `crw flow resume`
	k := newFx(t)
	kh := k.commit("a.txt", "work")
	k.ev.Head = kh
	k.executor("ex-1", kh, "claude-fake-haiku")
	k.row("hold-1", "crw.hold", engine.EvCompleted, map[string]any{"node": "A", "reason": "verify_unavailable"})
	k.runRow("finished", map[string]any{"status": "completed"})
	if res := k.apply(Request{}); res.State != StateNextTurn {
		t.Errorf("a held run: %+v", res)
	}
	// a verdict the finished run wrote still applies
	m := newFx(t)
	mh := m.commit("a.txt", "work")
	m.ev.Head = mh
	m.executor("ex-1", mh, "claude-fake-haiku")
	m.review("rv-1", mh, "gpt-fake-sol", "verified", digestA)
	m.runRow("finished", map[string]any{"status": "completed"})
	if res := m.apply(Request{}); res.State != StateApplied {
		t.Errorf("a finished run with its verdict: %+v", res)
	}
}
