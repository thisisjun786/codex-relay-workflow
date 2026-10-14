package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// nightDecision stores the manager's decision record of a night batch and answers it as given ("" leaves it raised).
func nightDecision(t *testing.T, w *world, id, context_ string, answer string) {
	t.Helper()
	rec := decisions.Record{
		Schema: decisions.Schema, DecisionID: id, Kind: decisions.KindPolicy, Context: context_,
		Options:   []decisions.Option{{ID: NightRun, Label: "run the batch tonight", Effect: "crw flow start night-batch --decision starts the run"}, {ID: NightSkip, Label: "skip tonight", Effect: "nothing is started"}},
		Blocking:  []decisions.Blocking{{Kind: decisions.BlockingProject, Ref: "P"}},
		Origin:    decisions.Origin{Project: "P"},
		Source:    decisions.Source{Kind: "night_batch", Ref: "night-2026-10-12"},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, State: decisions.StateRaised, RaisedAt: "2026-10-12T08:00:00Z", Seen: []decisions.Seen{{At: "2026-10-12T08:00:00Z", Source: "test"}},
	}
	rec.Fingerprint = decisions.Fingerprint(rec.Context, rec.Blocking, rec.Options)
	if _, _, err := w.st.Raise(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if answer == "" {
		return
	}
	if _, err := w.st.Update(context.Background(), id, func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.ValidateAnswer(r, decisions.Answer{Option: answer, By: "manager-task", Via: decisions.ViaManagementMessage, Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}})
	}); err != nil {
		t.Fatal(err)
	}
}

func batchJSON(t *testing.T, mutate func(*NightBatch)) string {
	t.Helper()
	b := NightBatch{Schema: NightBatchSchema, Label: "night-2026-10-12", Issues: []string{"CRW-11", "CRW-12", "CRW-13"}, Plan: "plan-1", Milestone: "M1", TokenBudget: 750000, Deadline: "2026-10-13T06:00:00Z", Cap: 2,
		Handoff: refOf(t, defaultHandoff(t))}
	if mutate != nil {
		mutate(&b)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestNightBatchStartsFromDecision: `crw flow start night-batch --decision D` starts the run for the issues the manager's decision
// selected, with the decision's token budget and deadline, and shows them in the preview; a decision that is not answered `run`,
// holds no night batch, has no budget or a deadline that is no time, an `--args` beside the decision, and a night batch without a
// decision start nothing.
func TestNightBatchStartsFromDecision(t *testing.T) {
	w := newWorld(t)
	nightDecision(t, w, "ud-night", batchJSON(t, nil), NightRun)
	td := w.deps()
	code, out, errOut := td.do("start", "night-batch", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state)
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	spec := td.spawned[0].Spec
	if spec.Workflow != "night-batch" || spec.Script != "" || spec.TokenBudget != 750000 || spec.Deadline != "2026-10-13T06:00:00Z" || spec.Decision != "ud-night" || spec.Thread != "thread-owner" {
		t.Errorf("the spec: %+v", spec)
	}
	var args struct {
		Decision  string   `json:"decision"`
		Label     string   `json:"label"`
		Issues    []string `json:"issues"`
		Plan      string   `json:"plan"`
		Milestone string   `json:"milestone"`
		Cap       int      `json:"cap"`
	}
	if err := json.Unmarshal(spec.Args, &args); err != nil || strings.Join(args.Issues, ",") != "CRW-11,CRW-12,CRW-13" || args.Decision != "ud-night" || args.Plan != "plan-1" || args.Milestone != "M1" || args.Cap != 2 || args.Label != "night-2026-10-12" {
		t.Errorf("the arguments of the workflow: %s %v", spec.Args, err)
	}
	pre, _, _ := strings.Cut(out, "<<spawned>>")
	for _, want := range []string{"crw flow start night-batch", "nodes: 3, up to 2 at once", "CRW-11", "CRW-13", "budget: 750000 tokens", "deadline: 2026-10-13T06:00:00Z"} {
		if !strings.Contains(pre, want) {
			t.Errorf("the preview lacks %q:\n%s", want, pre)
		}
	}
	// the flags may lower the budget the decision gives; the decision's values are the defaults
	if code, _, _ := td.do("start", "night-batch", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state, "--token-budget", "1000"); code != 0 || td.spawned[1].Spec.TokenBudget != 1000 {
		t.Errorf("--token-budget: %d %+v", code, td.spawned[len(td.spawned)-1].Spec)
	}

	refused := []struct {
		name string
		id   string
		args []string
		want string
	}{
		{"a decision the manager answered skip", "ud-skip", nil, `answered "skip"`},
		{"a decision not answered", "ud-raised", nil, "answers it"},
		{"a decision that holds no night batch", "ud-text", nil, "holds no night batch document"},
		{"a night batch with no budget", "ud-nobudget", nil, "no token budget"},
		{"a night batch with no handoff", "ud-nohandoff", nil, "names no handoff"},
		{"a deadline that is no time", "ud-time", nil, "not an RFC 3339 time"},
		{"a decision that does not exist", "ud-ghost", nil, "ud-ghost"},
		{"arguments beside the decision", "ud-night", []string{"--args", "{}"}, "--args is not given"},
	}
	nightDecision(t, w, "ud-skip", batchJSON(t, nil), NightSkip)
	nightDecision(t, w, "ud-raised", batchJSON(t, func(b *NightBatch) { b.Label = "raised" }), "")
	nightDecision(t, w, "ud-text", "start the batch tonight please", NightRun)
	nightDecision(t, w, "ud-nobudget", batchJSON(t, func(b *NightBatch) { b.Label, b.TokenBudget = "nobudget", 0 }), NightRun)
	nightDecision(t, w, "ud-nohandoff", batchJSON(t, func(b *NightBatch) { b.Label, b.Handoff = "nohandoff", nil }), NightRun)
	nightDecision(t, w, "ud-time", batchJSON(t, func(b *NightBatch) { b.Label, b.Deadline = "time", "tomorrow morning" }), NightRun)
	before := len(td.spawned)
	for _, tc := range refused {
		code, _, errOut := td.do(append([]string{"start", "night-batch", "--decision", tc.id, "--thread", "thread-owner", "--state", w.state}, tc.args...)...)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: exit %d %q", tc.name, code, errOut)
		}
	}
	if code, _, errOut := td.do("start", "night-batch", "--thread", "thread-owner", "--state", w.state); code != 2 || !strings.Contains(errOut, "starts from a decision record") {
		t.Errorf("a night batch with no decision: %d %q", code, errOut)
	}
	if code, _, errOut := td.do("start", "project-lane", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state); code != 2 || !strings.Contains(errOut, "starts a night batch only") {
		t.Errorf("a decision for another workflow: %d %q", code, errOut)
	}
	if len(td.spawned) != before {
		t.Errorf("%d processes started by refused commands", len(td.spawned)-before)
	}
}

// TestNightBatchOverridesOnlyNarrowTheDecision: the flags of `start` may lower the budget and bring the deadline forward, never
// remove or widen what the manager approved: a negative budget (the engine reads it as no limit), a budget above the decision's,
// and a later deadline start nothing (CRW-1205 d4).
func TestNightBatchOverridesOnlyNarrowTheDecision(t *testing.T) {
	w := newWorld(t)
	nightDecision(t, w, "ud-night", batchJSON(t, nil), NightRun)
	td := w.deps()
	start := func(extra ...string) (int, string) {
		code, _, errOut := td.do(append([]string{"start", "night-batch", "--decision", "ud-night", "--thread", "thread-owner", "--state", w.state}, extra...)...)
		return code, errOut
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a negative budget":           {[]string{"--token-budget", "-1"}, "token budget"},
		"a budget above the approved": {[]string{"--token-budget", "1500000"}, "750000"},
		"a later deadline":            {[]string{"--deadline", "2026-10-13T09:00:00Z"}, "2026-10-13T06:00:00Z"},
	} {
		before := len(td.spawned)
		if code, errOut := start(tc.args...); code == 0 || !strings.Contains(errOut, tc.want) || len(td.spawned) != before {
			t.Errorf("%s: exit %d %q, %d processes", name, code, errOut, len(td.spawned)-before)
		}
	}
	if code, errOut := start("--token-budget", "750000", "--deadline", "2026-10-13T05:00:00Z"); code != 0 {
		t.Fatalf("a budget as high as the decision's and an earlier deadline: %d %q", code, errOut)
	}
	if spec := td.spawned[len(td.spawned)-1].Spec; spec.TokenBudget != 750000 || spec.Deadline != "2026-10-13T05:00:00Z" {
		t.Errorf("the spec: %+v", spec)
	}
	// a plain start refuses a negative budget too
	if code, _, errOut := td.do("start", "--preview", "--thread", "thread-owner", "--state", w.state, "--token-budget", "-5", "project-lane"); code == 0 || !strings.Contains(errOut, "token budget") {
		t.Errorf("a negative budget on a plain start: %d %q", code, errOut)
	}
}

// TestNightBatchResumeKeepsTheDecisionBudget: `crw flow resume RUN SCRIPT` of a run whose spec carries a decision and its budget
// goes on under the spec's budget and deadline: a negative --token-budget and one above the spec's are refused before the run is
// taken, a lower one narrows it, no --token-budget takes the spec's (and so does 0, the remainder: TestScriptResumeTokenBudgetZeroIsTheRemainder
// holds it to the spec's budget, never to no ceiling), and the run pauses at the spec's deadline. Without this the engine's command
// ran the run with no ceiling and no deadline.
func TestNightBatchResumeKeepsTheDecisionBudget(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	prev := engine.ResumeEnv
	t.Cleanup(func() { engine.ResumeEnv = prev })
	engine.ResumeEnv = func(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
		w, err := pw.td.World(ctx, WorldRequest{Spec: Spec{RunID: req.RunID}, State: req.State, Resume: true})
		if err != nil {
			return engine.ResumeSetup{}, err
		}
		return engine.ResumeSetup{Journal: w.Journal, Backend: w.Backend, Close: w.Close}, nil
	}
	// the script returns the status and the reason of its second call, which the resume prints in the run's result
	resume := func(run string, extra ...string) (int, string, string) {
		return pw.td.do(append([]string{"resume", run, pw.script, "--state", pw.state}, extra...)...)
	}
	heldBy := func(out, reason string) bool {
		return strings.Contains(out, `"held"`) && strings.Contains(out, `"`+reason+`"`)
	}

	// a run paused at its first call by the operator; the resume sends that call again, and it spends more than the
	// decision's budget, so under the budget the second call is held budget_exhausted, and with no ceiling it is sent
	sigs := make(chan os.Signal, 2)
	pausing := true
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
			if pausing {
				sigs <- syscall.SIGUSR1
				time.Sleep(200 * time.Millisecond)
				return engine.FakeReply{}
			}
			return engine.FakeReply{Tokens: &engine.Tokens{Input: 100, Output: 50, Total: 150}}
		}
		return engine.FakeReply{}
	}
	paused := func(id string, budget int64, deadline string) {
		t.Helper()
		pausing = true
		defer func() { pausing = false }()
		spec := pw.spec(id, func(s *Spec) { s.Decision, s.TokenBudget, s.Deadline = "ud-night", budget, deadline })
		if code, errOut := pw.execute(spec, false, sigs); code != 0 {
			t.Fatalf("the first process of %s: exit %d %s", id, code, errOut)
		}
		if info := pw.info(id); info.State != StateHeld || info.Processes != 1 {
			t.Fatalf("the paused run %s: %+v", id, info)
		}
	}
	paused("run-narrow", 100, "2026-10-13T06:00:00Z")
	for _, budget := range []string{"-1", "101"} {
		if code, _, errOut := resume("run-narrow", "--token-budget", budget); code == 0 || !strings.Contains(errOut, "100") {
			t.Errorf("--token-budget %s: exit %d %q", budget, code, errOut)
		}
	}
	if info := pw.info("run-narrow"); info.Processes != 1 {
		t.Errorf("a refused resume took the run: %+v", info)
	}
	if code, out, errOut := resume("run-narrow", "--token-budget", "50"); code != 0 || !heldBy(out, engine.ReasonBudgetExhausted) {
		t.Errorf("a lower budget: exit %d %s %q", code, out, errOut)
	}
	paused("run-spec", 100, "2026-10-13T06:00:00Z")
	if code, out, errOut := resume("run-spec"); code != 0 || !heldBy(out, engine.ReasonBudgetExhausted) {
		t.Errorf("the run went on past the decision's budget: exit %d %s %q", code, out, errOut)
	}

	// the run pauses at the spec's deadline
	paused("run-late", 750000, "2026-10-12T10:00:00Z")
	now := pw.td.Now
	pw.td.Now = func() time.Time { return time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC) }
	defer func() { pw.td.Now = now }()
	if code, out, errOut := resume("run-late"); code != 0 || !heldBy(out, engine.ReasonEnginePaused) {
		t.Errorf("the run went on past the decision's deadline: exit %d %s %q", code, out, errOut)
	}
	events, _, err := loadRun(pw.world, "run-late")
	if err != nil {
		t.Fatal(err)
	}
	var byDeadline bool
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == engine.EvRunPaused && e.Data["by"] == "deadline" {
			byDeadline = true
		}
	}
	if !byDeadline {
		t.Errorf("no run_paused row of the deadline")
	}
}

const threeCalls = `export const meta = { name: 'three', phases: ['one'] }
phase('one')
const a = await agent.call('first', { agentType: 'executor', node: 'n1', label: 'first' })
const b = await agent.call('second', { agentType: 'executor', node: 'n1', label: 'second' })
const c = await agent.call('third', { agentType: 'executor', node: 'n1', label: 'third' })
return [a.status, b.status, c.status, c.reason]
`

// scriptResumeEnv installs the engine's resume environment over the world of a process test: `crw flow resume RUN SCRIPT` opens the
// same store and fake host as the run process.
func scriptResumeEnv(t *testing.T, pw *processWorld) {
	t.Helper()
	prev := engine.ResumeEnv
	t.Cleanup(func() { engine.ResumeEnv = prev })
	engine.ResumeEnv = func(ctx context.Context, req engine.ResumeEnvRequest) (engine.ResumeSetup, error) {
		w, err := pw.td.World(ctx, WorldRequest{Spec: Spec{RunID: req.RunID}, State: req.State, Resume: true})
		if err != nil {
			return engine.ResumeSetup{}, err
		}
		return engine.ResumeSetup{Journal: w.Journal, Backend: w.Backend, Close: w.Close}, nil
	}
}

// TestNightBatchResumeDoesNotRefillTheBudget (CRW-1278): the token budget of a night batch is the run's, not the process's. A resume,
// by the spec path and by an explicit SCRIPT alike, allows only the spec's budget minus the tokens the run's journal shows it spent
// (each call's terminal rows counted once, rows of older versions read as before); a resume whose remainder is 0 or less does not
// start the engine, says budget_exhausted and that more budget needs a new decision, and does not take the run.
func TestNightBatchResumeDoesNotRefillTheBudget(t *testing.T) {
	for _, path := range []string{"spec", "script"} {
		t.Run(path, func(t *testing.T) {
			pw := newProcessWorld(t, threeCalls)
			scriptResumeEnv(t, pw)
			run := "run-refill-" + path
			resume := func(extra ...string) (int, string, string) {
				args := []string{"resume", run}
				if path == "script" {
					args = append(args, pw.script)
				}
				return pw.td.do(append(append(args, "--state", pw.state), extra...)...)
			}
			// the first process: the first call spends 70 of the decision's 100 tokens, and the operator pauses the run in the second
			sigs := make(chan os.Signal, 2)
			first := true
			pw.handle = func(r engine.FakeRequest) engine.FakeReply {
				switch {
				case r.Kind != "turn":
				case strings.Contains(r.Prompt, "first"):
					return engine.FakeReply{Tokens: &engine.Tokens{Input: 60, Output: 10, Total: 70}}
				case strings.Contains(r.Prompt, "second") && first:
					sigs <- syscall.SIGUSR1
					time.Sleep(200 * time.Millisecond)
					return engine.FakeReply{Tokens: &engine.Tokens{}}
				case strings.Contains(r.Prompt, "second"):
					return engine.FakeReply{Tokens: &engine.Tokens{Input: 30, Output: 10, Total: 40}}
				}
				return engine.FakeReply{Tokens: &engine.Tokens{Input: 5, Output: 5, Total: 10}}
			}
			spec := pw.spec(run, func(s *Spec) { s.Script = pw.script; s.Decision, s.TokenBudget = "ud-night", 100 })
			if code, errOut := pw.execute(spec, false, sigs); code != 0 {
				t.Fatalf("the first process: exit %d %s", code, errOut)
			}
			first = false
			if info := pw.info(run); info.State != StateHeld || info.Tokens != 70 || info.Processes != 1 {
				t.Fatalf("the paused run: %+v", info)
			}
			// the resume goes on with the second call, which brings the run to 110 of its 100 tokens: the third call is held
			// budget_exhausted (a budget refilled for the process would have let it run)
			code, out, errOut := resume()
			if code != 0 {
				t.Fatalf("the resume: exit %d %s %s", code, out, errOut)
			}
			info := pw.info(run)
			if info.Processes != 2 || info.Tokens != 110 || !contains(info.HeldReasons, engine.ReasonBudgetExhausted) {
				t.Errorf("the resumed run went on past its budget: %+v", info)
			}
			events, _, err := loadRun(pw.world, run)
			if err != nil {
				t.Fatal(err)
			}
			third := ""
			for _, e := range events {
				if e.Kind == engine.KindCall && e.Label == "third" && (e.Event == engine.EvHeld || e.Event == engine.EvCompleted) {
					third = e.Event + ":" + str(e.Data, "reason")
				}
				if e.Kind == engine.KindFlow && e.Event == "budget_warning" && num(e.Data, "spent") < 80 {
					t.Errorf("a budget warning below 80 of 100 tokens: %v", e.Data)
				}
			}
			if third != engine.EvHeld+":"+engine.ReasonBudgetExhausted {
				t.Errorf("the third call ended %q", third)
			}
			// nothing is left: the next resume does not start, names budget_exhausted and the decision, and leaves the run as it was
			code, out, errOut = resume()
			if code == 0 || !strings.Contains(errOut, engine.ReasonBudgetExhausted) || !strings.Contains(errOut, "110 of the 100 tokens") || !strings.Contains(errOut, "new decision") {
				t.Errorf("a resume with no budget left: exit %d %s %q", code, out, errOut)
			}
			if info := pw.info(run); info.Processes != 2 {
				t.Errorf("a resume with no budget left took the run: %+v", info)
			}
			// a --token-budget may still only narrow the spec's: above it is refused as before
			if path == "script" {
				if code, _, errOut := resume("--token-budget", "101"); code == 0 || !strings.Contains(errOut, "100") {
					t.Errorf("--token-budget 101: exit %d %q", code, errOut)
				}
			}
		})
	}
}

// TestDetachedResumeRefusesAnExhaustedBudget (CRW-1278): a run whose spec's budget has nothing left is refused before a run process
// is started for it: `crw flow resume RUN --detach` (and an answer's resume, which starts the process the same way) reports
// budget_exhausted and the decision and starts no process, instead of a process that would take the run lock and end at once.
func TestDetachedResumeRefusesAnExhaustedBudget(t *testing.T) {
	pw := newProcessWorld(t, threeCalls)
	run := "run-detach-exhausted"
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 50, Output: 10, Total: 60}}
	}
	spec := pw.spec(run, func(s *Spec) { s.Decision, s.TokenBudget = "ud-night", 50 })
	if code, errOut := pw.execute(spec, false, make(chan os.Signal, 1)); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	if info := pw.info(run); info.Tokens != 60 || !contains(info.HeldReasons, engine.ReasonBudgetExhausted) {
		t.Fatalf("the exhausted run: %+v", info)
	}
	before := len(pw.td.spawned)
	code, out, errOut := pw.td.do("resume", run, "--detach", "--state", pw.state)
	if code == 0 || !strings.Contains(errOut, engine.ReasonBudgetExhausted) || !strings.Contains(errOut, "60 of the 50 tokens") || !strings.Contains(errOut, "ud-night") || strings.Contains(out, "resumed") {
		t.Errorf("a detached resume with no budget left: exit %d %q %q", code, out, errOut)
	}
	if n := len(pw.td.spawned) - before; n != 0 {
		t.Errorf("a detached resume with no budget left started %d processes", n)
	}
	// a run with budget left is still started (the spec of a new decision)
	spec.TokenBudget = 1000
	if err := os.Remove(specPath(pw.state, run)); err != nil {
		t.Fatal(err)
	}
	if err := WriteSpec(pw.state, spec); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := pw.td.do("resume", run, "--detach", "--state", pw.state); code != 0 || len(pw.td.spawned)-before != 1 {
		t.Errorf("a detached resume with budget left: exit %d %q %q, %d processes", code, out, errOut, len(pw.td.spawned)-before)
	}
}

// TestRunBudgetNarrowsTheRemainder (CRW-1278): a resume's ceiling counts what the run spent; a --token-budget narrows the remainder and
// never widens it, the spec's budget stays the upper bound, and nothing left refuses the resume.
func TestRunBudgetNarrowsTheRemainder(t *testing.T) {
	spec := Spec{RunID: "run-b", TokenBudget: 100, Decision: "ud-night"}
	for _, c := range []struct {
		spent, asked, want int64
	}{{0, 100, 100}, {70, 100, 100}, {70, 0, 100}, {70, 20, 90}, {70, 50, 100}, {99, 100, 100}} {
		if got, err := runBudget(spec, c.spent, c.asked); err != nil || got != c.want {
			t.Errorf("spent %d asked %d: %d %v, want %d", c.spent, c.asked, got, err, c.want)
		}
	}
	for _, spent := range []int64{100, 110} {
		if _, err := runBudget(spec, spent, 100); err == nil || !strings.Contains(err.Error(), engine.ReasonBudgetExhausted) || !strings.Contains(err.Error(), "ud-night") {
			t.Errorf("spent %d: %v", spent, err)
		}
	}
	// a spec without a budget sets none; a --token-budget of a resume is that resume's own
	if got, err := runBudget(Spec{RunID: "run-n"}, 70, 0); err != nil || got != 0 {
		t.Errorf("no budget: %d %v", got, err)
	}
	if got, err := runBudget(Spec{RunID: "run-n"}, 70, 30); err != nil || got != 100 {
		t.Errorf("no spec budget, 30 asked: %d %v", got, err)
	}
}

// pausedRun is a run of threeCalls whose first process spent first tokens on its first call and was paused (SIGUSR1) in its second;
// a later process spends 40 on the second call and 10 on the third.
func pausedRun(t *testing.T, pw *processWorld, run string, first int64, mutate func(*Spec)) Spec {
	t.Helper()
	sigs := make(chan os.Signal, 2)
	var paused atomic.Bool
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		switch {
		case r.Kind != "turn":
		case strings.Contains(r.Prompt, "first"):
			return engine.FakeReply{Tokens: &engine.Tokens{Input: first - 10, Output: 10, Total: first}}
		case strings.Contains(r.Prompt, "second") && paused.CompareAndSwap(false, true):
			sigs <- syscall.SIGUSR1
			time.Sleep(200 * time.Millisecond)
			return engine.FakeReply{Tokens: &engine.Tokens{}}
		case strings.Contains(r.Prompt, "second"):
			return engine.FakeReply{Tokens: &engine.Tokens{Input: 30, Output: 10, Total: 40}}
		}
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 5, Output: 5, Total: 10}}
	}
	spec := pw.spec(run, func(s *Spec) {
		s.Script = pw.script
		if mutate != nil {
			mutate(s)
		}
	})
	if code, errOut := pw.execute(spec, false, sigs); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	if info := pw.info(run); info.State != StateHeld || info.Tokens != first || info.Processes != 1 {
		t.Fatalf("the paused run: %+v", info)
	}
	return spec
}

// budgetWarnings are the budget_warning rows of a run.
func budgetWarnings(t *testing.T, pw *processWorld, run string) []engine.Event {
	t.Helper()
	events, _, err := loadRun(pw.world, run)
	if err != nil {
		t.Fatal(err)
	}
	var out []engine.Event
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == "budget_warning" {
			out = append(out, e)
		}
	}
	return out
}

// TestScriptResumeWithoutASpecCountsTheRunsSpending (CRW-1278, Argon F3 on 2a082733): `crw flow resume RUN SCRIPT` of a run that has
// no spec (no `crw flow start` made it, or its spec is gone) counts what the run's journal shows it spent, as the resume of a run with
// a spec does: the result's tokensSpent is the run's figure, and a --token-budget is this resume's own, on top of what the run spent
// (the ceiling is the spent 70 and the 30 asked).
func TestScriptResumeWithoutASpecCountsTheRunsSpending(t *testing.T) {
	for _, c := range []struct {
		name  string
		extra []string
		spent int64
		total int
	}{
		{name: "no budget", spent: 120},
		{name: "token budget 30", extra: []string{"--token-budget", "30"}, spent: 110, total: 100},
	} {
		t.Run(c.name, func(t *testing.T) {
			pw := newProcessWorld(t, threeCalls)
			scriptResumeEnv(t, pw)
			run := "run-nospec"
			pausedRun(t, pw, run, 70, nil)
			if err := os.Remove(specPath(pw.state, run)); err != nil {
				t.Fatal(err)
			}
			code, out, errOut := pw.td.do(append([]string{"resume", run, pw.script, "--state", pw.state}, c.extra...)...)
			if code != 0 {
				t.Fatalf("the resume: exit %d %s %s", code, out, errOut)
			}
			var res struct {
				Spent int64 `json:"tokensSpent"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if res.Spent != c.spent {
				t.Errorf("the resumed run's tokensSpent %d, want the run's %d (the first process's 70 included)", res.Spent, c.spent)
			}
			if info := pw.info(run); info.Tokens != c.spent || info.Processes != 2 {
				t.Errorf("the resumed run: %+v", info)
			}
			warnings := budgetWarnings(t, pw, run)
			switch {
			case c.total == 0 && len(warnings) != 0:
				t.Errorf("a run without a ceiling warned: %+v", warnings)
			case c.total > 0 && (len(warnings) != 1 || num(warnings[0].Data, "total") != c.total || num(warnings[0].Data, "spent") != int(c.spent)):
				t.Errorf("the budget warnings %+v, want one of %d spent of %d", warnings, c.spent, c.total)
			}
		})
	}
}

// TestResumedRunWarnsOfItsBudgetOnce (CRW-1278, Argon F4 on 2a082733): a run warns of its budget once. The first process spends 85 of the
// spec's 100 tokens (past 80%) and writes the budget_warning row; the resume, which starts at 85, writes no second one when its first
// call spends again, and is held budget_exhausted at the budget as before.
func TestResumedRunWarnsOfItsBudgetOnce(t *testing.T) {
	pw := newProcessWorld(t, threeCalls)
	run := "run-warn-once"
	pausedRun(t, pw, run, 85, func(s *Spec) { s.Decision, s.TokenBudget = "ud-night", 100 })
	if w := budgetWarnings(t, pw, run); len(w) != 1 || num(w[0].Data, "spent") != 85 {
		t.Fatalf("the first process's budget warnings %+v", w)
	}
	if code, out, errOut := pw.td.do("resume", run, "--state", pw.state); code != 0 {
		t.Fatalf("the resume: exit %d %s %s", code, out, errOut)
	}
	if info := pw.info(run); info.Processes != 2 || info.Tokens != 125 || !contains(info.HeldReasons, engine.ReasonBudgetExhausted) {
		t.Errorf("the resumed run: %+v", info)
	}
	if w := budgetWarnings(t, pw, run); len(w) != 1 {
		t.Errorf("the run warned of its budget %d times: %+v", len(w), w)
	}
}

// TestScriptResumeTokenBudgetZeroIsTheRemainder (CRW-1278, Argon F6 on 2a082733): an explicit --token-budget 0 of `crw flow resume RUN
// SCRIPT` is what its help says, the remainder of the spec's budget (as no --token-budget is), never no ceiling: the resume goes on
// and is held budget_exhausted at the spec's 100 tokens. A --token-budget below 0 is refused and does not take the run.
func TestScriptResumeTokenBudgetZeroIsTheRemainder(t *testing.T) {
	pw := newProcessWorld(t, threeCalls)
	scriptResumeEnv(t, pw)
	run := "run-zero"
	pausedRun(t, pw, run, 70, func(s *Spec) { s.Decision, s.TokenBudget = "ud-night", 100 })
	if code, _, errOut := pw.td.do("resume", run, pw.script, "--state", pw.state, "--token-budget", "-1"); code == 0 || !strings.Contains(errOut, "100") {
		t.Errorf("--token-budget -1: exit %d %q", code, errOut)
	}
	if info := pw.info(run); info.Processes != 1 {
		t.Fatalf("a refused resume took the run: %+v", info)
	}
	code, out, errOut := pw.td.do("resume", run, pw.script, "--state", pw.state, "--token-budget", "0")
	if code != 0 {
		t.Fatalf("--token-budget 0: exit %d %s %q", code, out, errOut)
	}
	if info := pw.info(run); info.Processes != 2 || info.Tokens != 110 || !contains(info.HeldReasons, engine.ReasonBudgetExhausted) {
		t.Errorf("the resume under --token-budget 0 did not go on under the remainder: %+v", info)
	}
}
