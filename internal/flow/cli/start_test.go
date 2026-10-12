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
	code, out, errOut := td.do("start", "night-batch", "--thread", "thread-owner", "--state", w.state, "--decision", "ud-none")
	if code == 0 {
		t.Fatalf("a night batch without a readable decision started: %s", out)
	}
	code, out, errOut = td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--args", `{ "plan": "plan-1", "milestone": "M1" }`, "--token-budget", "5000")
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
	if req.Resume || req.State != w.state || !strings.HasPrefix(req.Spec.RunID, "run-") || req.Spec.Thread != "thread-owner" || req.Spec.TokenBudget != 5000 || string(req.Spec.Args) != `{"plan":"plan-1","milestone":"M1"}` {
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
	code, out, _ = td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--json")
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

// TestStartPreviewListsFivePreflightFacts: the preview lists the five facts in their order with pass or fail and the reading each
// was judged on; a run whose fact failed is not started unless --force says so, and --preview starts nothing. The facts of the
// store prober are judged on what the relay records.
func TestStartPreviewListsFivePreflightFacts(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	td.facts[3] = Fact{Name: FactWaits, Pass: false, Reading: "crw.decide waits and nothing delivers"}
	code, out, errOut := td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--preview", "--json")
	if code != 1 || len(td.spawned) != 0 {
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
	if ans.Plan.Facts[3].Pass || !ans.Plan.Facts[0].Pass {
		t.Errorf("pass and fail: %+v", ans.Plan.Facts)
	}
	code, out, errOut = td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state)
	if code != 1 || len(td.spawned) != 0 || !strings.Contains(out, "FAIL  waits_deliverable") || !strings.Contains(errOut, "waits_deliverable failed") {
		t.Fatalf("a failed fact started the run: exit %d, %d processes\n%s%s", code, len(td.spawned), out, errOut)
	}
	code, out, errOut = td.do("start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--force")
	if code != 0 || len(td.spawned) != 1 || strings.Join(td.spawned[0].Spec.Forced, ",") != FactWaits {
		t.Fatalf("--force: exit %d, spawned %+v\n%s%s", code, td.spawned, out, errOut)
	}

	// the prober of the shipped binary reads the relay's store: nothing recorded is five fails with their readings
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
		if f.Name != Facts[i] || f.Pass || f.Reading == "" {
			t.Errorf("fact %d on an empty store: %+v", i, f)
		}
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
	insert(`INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES ('thread-owner','{}','test',?)`, at)
	facts = StoreProber{}.Facts(ctx, PreflightRequest{Reader: r, Thread: "thread-owner", Waits: []string{"crw.decide"}, Now: td.Now()})
	if got := passes(facts); got != "pass,fail,pass,fail,fail" {
		t.Errorf("bound, looked at and reachable, but nothing delivered or woken: %s %+v", got, facts)
	}
	insert(`INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, dispatch_turn_id, created_at, updated_at) VALUES ('e1','r1','report','thread-owner','thread-owner','dispatched','turn-1',?,?)`, at, at)
	insert(`INSERT INTO delivery_wakes (event_id, woken_at, original_deadline, spent_at) VALUES ('e1', 1.0, 2.0, 3.0)`)
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
}

func passes(facts []Fact) string {
	var out []string
	for _, f := range facts {
		out = append(out, map[bool]string{true: "pass", false: "fail"}[f.Pass])
	}
	return strings.Join(out, ",")
}
