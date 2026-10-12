package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
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
	b := NightBatch{Schema: NightBatchSchema, Label: "night-2026-10-12", Issues: []string{"CRW-11", "CRW-12", "CRW-13"}, Plan: "plan-1", Milestone: "M1", TokenBudget: 750000, Deadline: "2026-10-13T06:00:00Z", Cap: 2}
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
		{"a deadline that is no time", "ud-time", nil, "not an RFC 3339 time"},
		{"a decision that does not exist", "ud-ghost", nil, "ud-ghost"},
		{"arguments beside the decision", "ud-night", []string{"--args", "{}"}, "--args is not given"},
	}
	nightDecision(t, w, "ud-skip", batchJSON(t, nil), NightSkip)
	nightDecision(t, w, "ud-raised", batchJSON(t, func(b *NightBatch) { b.Label = "raised" }), "")
	nightDecision(t, w, "ud-text", "start the batch tonight please", NightRun)
	nightDecision(t, w, "ud-nobudget", batchJSON(t, func(b *NightBatch) { b.Label, b.TokenBudget = "nobudget", 0 }), NightRun)
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
// goes on under the spec's budget and deadline: a --token-budget of 0 (no ceiling), a negative one and one above the spec's are
// refused before the run is taken, a lower one narrows it, no --token-budget takes the spec's, and the run pauses at the spec's
// deadline. Without this the engine's command ran the run with no ceiling and no deadline.
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
	for _, budget := range []string{"0", "-1", "101"} {
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
