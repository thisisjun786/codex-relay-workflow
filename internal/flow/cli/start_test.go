package cli

import (
	"encoding/json"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStartPreviewThenDetached: `crw flow start` prints the preview (the nodes, the role pairs, the budget and the five facts)
// before it starts the run process, writes the run's spec, starts the process once with the spec and the state directory, and
// answers with the run id. The process it starts is detached: a session of its own, no input, no descriptor of the caller's, and
// when the descriptors cannot be marked the run is not started.
func TestStartPreviewThenDetached(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	code, out, _ := td.do("start", "night-batch", "--thread", "thread-owner", "--state", w.state, "--decision", "ud-none")
	if code == 0 {
		t.Fatalf("a night batch without a readable decision started: %s", out)
	}
	handoff := defaultHandoff(t)
	code, out, errOut := td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--args", `{ "plan": "plan-1", "milestone": "M1" }`, "--token-budget", "5000", "--handoff", handoff)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	pre, post, found := strings.Cut(out, "<<spawned>>")
	if !found {
		t.Fatalf("the process was not started: %s", out)
	}
	for _, want := range []string{"crw flow start project-lane", "role pairs", "budget: 5000 tokens", "preflight:", "assignment_routed", "idle_wake_observed"} {
		if !strings.Contains(pre, want) {
			t.Errorf("the preview printed before the start lacks %q:\n%s", want, pre)
		}
	}
	if len(td.spawned) != 1 {
		t.Fatalf("%d processes started", len(td.spawned))
	}
	req := td.spawned[0]
	if req.Resume || req.State != w.state || !strings.HasPrefix(req.Spec.RunID, "run-") || req.Spec.Thread != "thread-owner" || req.Spec.TokenBudget != 5000 || !strings.HasPrefix(string(req.Spec.Args), `{"plan":"plan-1","milestone":"M1","handoff":`) {
		t.Errorf("the process is started with %+v", req)
	}
	if !strings.Contains(post, "run "+req.Spec.RunID+" started (process 4242)") {
		t.Errorf("the answer after the start: %q", post)
	}
	spec, err := ReadSpec(w.state, req.Spec.RunID)
	if err != nil || spec.Workflow != "project-lane" || spec.Script != "" || spec.Thread != "thread-owner" {
		t.Errorf("the stored spec: %+v %v", spec, err)
	}

	// --json is one document: the preview and the run
	code, out, _ = td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--json", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", handoff)
	var ans StartAnswer
	if code != 0 || json.Unmarshal([]byte(strings.TrimPrefix(out, "<<spawned>>\n")), &ans) != nil || ans.Run == "" || ans.PID != 4242 || len(ans.Plan.Facts) != 5 {
		t.Errorf("start --json: exit %d %s", code, out)
	}

	// the command the process is started with is detached
	log, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := DetachedCommand("/bin/crw", req, log)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid || cmd.Stdin != nil || cmd.Stdout != log || cmd.Stderr != log || cmd.Dir != req.Dir {
		t.Errorf("the run process is not detached: %+v", cmd)
	}
	if got := strings.Join(cmd.Args, " "); got != "/bin/crw flow _run --state "+w.state+" "+req.Spec.RunID {
		t.Errorf("the process command is %q", got)
	}
	resumed := req
	resumed.Resume = true
	if got := strings.Join(DetachedCommand("/bin/crw", resumed, log).Args, " "); !strings.Contains(got, "--resume "+req.Spec.RunID) {
		t.Errorf("the resume command is %q", got)
	}

	// descriptors that cannot be kept out of the process stop the start before anything is started
	old := markInherited
	defer func() { markInherited = old }()
	markInherited = func() error { return syscall.EINVAL }
	if pid, err := SpawnDetached(ctx, req); err == nil || pid != 0 || !strings.Contains(err.Error(), "not started") {
		t.Errorf("SpawnDetached with unmarked descriptors: %d %v", pid, err)
	}
}

// TestStartPreviewListsFivePreflightFacts: the preview lists the five facts in their order with their level (pass, warn or fail)
// and the reading each was judged on. A fact never stops the start (CRW-1318): a run whose fact failed is started without --force,
// the fact is printed and recorded in the run's spec, --force is accepted and changes nothing, and --preview starts nothing. The
// facts of the store prober are judged on what the relay records.
func TestStartPreviewListsFivePreflightFacts(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	td.facts[3] = Fact{Name: FactWaits, Pass: false, Reading: "crw.decide waits and nothing delivers"}
	lane := []string{"--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t)}
	code, out, errOut := td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--preview", "--json"}, lane...)...)
	if code != 0 || len(td.spawned) != 0 {
		t.Fatalf("a preview with a failed fact: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	var ans StartAnswer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var names []string
	for _, f := range ans.Plan.Facts {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "assignment_routed,delivery_ticking,thread_reachable,waits_deliverable,idle_wake_observed" {
		t.Errorf("the facts are %s", got)
	}
	if ans.Plan.Facts[3].Pass || ans.Plan.Facts[3].Level != LevelFail || !ans.Plan.Facts[0].Pass || ans.Plan.Facts[0].Level != LevelPass {
		t.Errorf("pass and fail: %+v", ans.Plan.Facts)
	}
	code, out, errOut = td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state}, lane...)...)
	if code != 0 || len(td.spawned) != 1 || !strings.Contains(out, "FAIL  waits_deliverable") || !strings.Contains(errOut, "waits_deliverable") {
		t.Fatalf("a failed fact did not start the run with the fact printed: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	if got := td.spawned[0].Spec.Preflight; len(got) != 1 || got[0].Name != FactWaits || got[0].Level != LevelFail || got[0].Reading != "crw.decide waits and nothing delivers" {
		t.Errorf("the spec records the facts that did not pass: %+v", got)
	}
	if spec, err := ReadSpec(w.state, td.spawned[0].Spec.RunID); err != nil || len(spec.Preflight) != 1 || spec.Preflight[0].Name != FactWaits {
		t.Errorf("the stored spec: %+v %v", spec.Preflight, err)
	}
	code, out, errOut = td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--force"}, lane...)...)
	if code != 0 || len(td.spawned) != 2 || len(td.spawned[1].Spec.Preflight) != 1 || td.spawned[1].Spec.Preflight[0].Name != FactWaits {
		t.Fatalf("--force is accepted and changes nothing: exit %d, spawned %+v\n%s%s", code, td.spawned, out, errOut)
	}

	// the prober of the shipped binary reads the relay's store: on an empty store the facts that have no record yet are warnings
	// with the reason, and the facts the records contradict fail
	r, err := OpenReader(ctx, w.state, "")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	facts := StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if len(facts) != 5 {
		t.Fatalf("%d facts", len(facts))
	}
	for i, f := range facts {
		if f.Name != Facts[i] || f.Pass || f.Reading == "" || f.Reason == "" {
			t.Errorf("fact %d on an empty store: %+v", i, f)
		}
	}
	if got := levels(facts); got != "fail,fail,warn,warn,warn" {
		t.Errorf("an empty store: %s %+v", got, facts)
	}
	// a thread the relay routes to, looks at, reaches and has woken passes all five; a fact the records do not show fails alone
	at := td.Now().Format("2006-01-02T15:04:05.000000000Z07:00")
	insert := func(q string, args ...any) {
		t.Helper()
		if _, err := w.st.DB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	insert(`INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b1','parent','project','P','thread-owner','h','active',1,?,?)`, at, at)
	insert(`INSERT INTO recipient_lifecycle (task_id, runtime_status, archived, goal_status, can_accept_input, deliverable, observed_at) VALUES ('thread-owner','idle',0,'active',1,'yes',?)`, at)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if got := levels(facts); got != "pass,fail,warn,warn,warn" {
		t.Errorf("bound and looked at, no authorized settings yet, nothing delivered or woken: %s %+v", got, facts)
	}
	insert(`INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES ('thread-owner','{}','test',?)`, at)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if got := levels(facts); got != "pass,fail,pass,warn,warn" {
		t.Errorf("bound, looked at and reachable, but nothing delivered or woken: %s %+v", got, facts)
	}
	insert(`INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, dispatch_turn_id, created_at, updated_at) VALUES ('e1','r1','report','thread-owner','thread-owner','dispatched','turn-1',?,?)`, at, at)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if got := levels(facts); got != "pass,pass,pass,pass,warn" {
		t.Errorf("delivered, no wake in the store (an answered wake is deleted): %s %+v", got, facts)
	}
	// a wake the relay holds now (woken, not yet answered) is an observed idle wake
	insert(`INSERT INTO delivery_wakes (event_id, woken_at, original_deadline, spent_at) VALUES ('e1', 1.0, 2.0, NULL)`)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if got := passes(facts); got != "pass,pass,pass,pass,pass" {
		t.Errorf("a thread the relay routes to, looks at, reaches and has woken: %s %+v", got, facts)
	}
	// a lifecycle reading older than a quarter of an hour is no tick
	stale := td.Now().Add(-time.Hour).Format("2006-01-02T15:04:05.000000000Z07:00")
	insert(`UPDATE recipient_lifecycle SET observed_at = ? WHERE task_id = 'thread-owner'`, stale)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Now: td.Now()})
	if got := passes(facts); got != "pass,fail,pass,pass,pass" {
		t.Errorf("a stale lifecycle reading, and a workflow that never waits: %s %+v", got, facts)
	}
	// an archived thread is a fail, not a warning: the record says it cannot be reached
	insert(`UPDATE recipient_lifecycle SET archived = 1, observed_at = ? WHERE task_id = 'thread-owner'`, at)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if got := levels(facts); got != "pass,pass,fail,fail,pass" {
		t.Errorf("an archived thread: %s %+v", got, facts)
	}
}

// TestStartFirstRunOfANewThreadStartsWithWarnings (CRW-1318): the first run of a new orchestrator thread in an empty store starts
// without --force; the shipped prober judges the facts that have no record yet as warnings with their reason, and the output and
// the run's spec carry them.
func TestStartFirstRunOfANewThreadStartsWithWarnings(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	td.Probe = nil
	code, out, errOut := td.do("start", "project-lane", "--thread", "thread-new", "--state", w.state, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t))
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("the first run of a new thread: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	for _, want := range []string{"warn  thread_reachable", "warn  waits_deliverable", "warn  idle_wake_observed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the preview lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "thread_reachable") || !strings.Contains(errOut, "recorded") {
		t.Errorf("the start does not say what it recorded: %s", errOut)
	}
	spec, err := ReadSpec(w.state, td.spawned[0].Spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Fact{}
	for _, f := range spec.Preflight {
		got[f.Name] = f
	}
	for _, name := range []string{FactReachable, FactWaits, FactIdleWake} {
		if f := got[name]; f.Level != LevelWarn || f.Reason == "" {
			t.Errorf("the spec records %s as %+v", name, f)
		}
	}
}

// TestStartWarnsWhenThePlanKeyIsNotTheThreadsBinding (CRW-1317): before the run process (and so before any role call) starts, the
// plan's project key is compared with the scope keys of the owner thread's live parent bindings. A key the thread holds no binding
// of is a warning that names both keys, printed and recorded in the run's spec, and the run starts; a key the thread is bound to
// prints nothing.
func TestStartWarnsWhenThePlanKeyIsNotTheThreadsBinding(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	at := td.Now().Format("2006-01-02T15:04:05.000000000Z07:00")
	for _, q := range []string{
		`INSERT INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES ('plan-1','P-CRW-140','thread-owner','` + at + `')`,
		`INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b1','parent','project','3b46bc6e-uuid','thread-owner','h','active',1,'` + at + `','` + at + `')`,
	} {
		if _, err := w.st.DB.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	lane := []string{"--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t)}
	code, out, errOut := td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state}, lane...)...)
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("a plan under another key: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	pre, _, _ := strings.Cut(out, "<<spawned>>")
	if !strings.Contains(pre, "warn  "+FactProjectKey) || !strings.Contains(pre, "P-CRW-140") || !strings.Contains(pre, "3b46bc6e-uuid") {
		t.Errorf("the warning before the run process starts names both keys:\n%s", pre)
	}
	if !strings.Contains(errOut, FactProjectKey+" warn") {
		t.Errorf("stderr: %s", errOut)
	}
	var recorded *Fact
	for _, f := range td.spawned[0].Spec.Preflight {
		if f.Name == FactProjectKey {
			recorded = &f
		}
	}
	if recorded == nil || recorded.Level != LevelWarn || !strings.Contains(recorded.Reading, "P-CRW-140") || !strings.Contains(recorded.Reading, "3b46bc6e-uuid") || recorded.Reason == "" {
		t.Errorf("the spec records the key warning: %+v", td.spawned[0].Spec.Preflight)
	}

	// the thread bound to the plan's key: nothing is printed of the comparison
	if _, err := w.st.DB.ExecContext(ctx, `UPDATE scope_bindings SET scope_key = 'P-CRW-140' WHERE binding_id = 'b1'`); err != nil {
		t.Fatal(err)
	}
	code, out, errOut = td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state}, lane...)...)
	if code != 0 || len(td.spawned) != 2 || strings.Contains(out, FactProjectKey) || strings.Contains(errOut, FactProjectKey) || len(td.spawned[1].Spec.Preflight) != 0 {
		t.Errorf("a plan under the thread's key: exit %d\n%s%s %+v", code, out, errOut, td.spawned[1].Spec.Preflight)
	}
}

func factNames(facts []Fact) string {
	var out []string
	for _, f := range facts {
		out = append(out, f.Name)
	}
	return strings.Join(out, ",")
}

func levels(facts []Fact) string {
	var out []string
	for _, f := range facts {
		out = append(out, f.Level)
	}
	return strings.Join(out, ",")
}

func passes(facts []Fact) string {
	var out []string
	for _, f := range facts {
		out = append(out, map[bool]string{true: "pass", false: "fail"}[f.Pass])
	}
	return strings.Join(out, ",")
}
