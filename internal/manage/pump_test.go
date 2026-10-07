package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The pump tests drive rounds through injected sources, an injected clock and the fake bridge, so
// no test reaches a real rollout directory, the relay store, the network or a real state directory.

// pumpTestNow is the fixed clock every pump test runs at unless it steps it.
var pumpTestNow = time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)

// pumpTestEnv is an Env with temporary homes and an injectable clock.
func pumpTestEnv(t *testing.T, now *time.Time) *Env {
	t.Helper()
	coreTempHome(t)
	return &Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Getenv: os.Getenv,
		Now: func() time.Time { return *now }, Executable: os.Args[0]}
}

// pumpTestConfig is a configuration whose state directory is temporary and whose bridge is the fake.
func pumpTestConfig(t *testing.T, bridge string) *Config {
	t.Helper()
	cfg := coreDefaults(&Env{Getenv: os.Getenv})
	cfg.StateDir = t.TempDir()
	cfg.ManagementThread = "mgmt-thread"
	cfg.Repository = "owner/repo"
	cfg.Bridge = coreBridge{Binary: bridge, ExecutionPolicy: "policy.json"}
	return cfg
}

// pumpTestSwapSources replaces the source registry for one test and restores it afterwards.
func pumpTestSwapSources(t *testing.T, sources ...pumpSource) {
	t.Helper()
	saved := pumpSources
	pumpSources = sources
	t.Cleanup(func() { pumpSources = saved })
}

// pumpTestSource is a source a later node registers and the contract tests inject.
type pumpTestSource struct {
	name   string
	status string
	events []pumpEvent
	onRead func(st *pumpState)
}

func (s *pumpTestSource) Name() string { return s.name }
func (s *pumpTestSource) Collect(_ context.Context, _ *Env, _ *Config, st *pumpState, _ pumpSettings) pumpSourceResult {
	if s.onRead != nil {
		s.onRead(st)
	}
	status := s.status
	if status == "" {
		status = pumpSourceOK
	}
	return pumpSourceResult{Status: status, Events: s.events}
}

// pumpTestReadStatePtr loads the state as a pointer, for a call that mutates and saves it.
func pumpTestReadStatePtr(t *testing.T, cfg *Config) *pumpState {
	t.Helper()
	st := pumpTestReadState(t, cfg)
	return &st
}

// pumpTestReadState decodes the state document, failing the test on a read error.
func pumpTestReadState(t *testing.T, cfg *Config) pumpState {
	t.Helper()
	st, err := pumpLoadState(cfg)
	if err != nil {
		t.Fatalf("load the state: %v", err)
	}
	return st
}

// pumpTestRollout writes one rollout file for a parent thread under the temporary CODEX_HOME and
// returns its path.
func pumpTestRollout(t *testing.T, thread string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("CODEX_HOME"), "sessions", "2026", "10", "07")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-07T03-00-00-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pumpTestAppendRollout appends one line to a rollout file.
func pumpTestAppendRollout(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func pumpTestTaskComplete(turnID, message string) string {
	line, _ := json.Marshal(map[string]any{"payload": map[string]any{
		"type": "task_complete", "turn_id": turnID, "last_agent_message": message}})
	return string(line)
}

func pumpTestQuestion(arguments string) string {
	line, _ := json.Marshal(map[string]any{"payload": map[string]any{
		"type": "function_call", "name": "request_user_input", "arguments": arguments}})
	return string(line)
}

// The rollout source turns a task_complete into a report and a request_user_input call into a
// question, and a first-sighted rollout sends no history.
func TestPumpRolloutReportAndQuestionAndNoHistory(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Parents = map[string]string{"parent-1": "Parent One"}
	path := pumpTestRollout(t, "parent-1", pumpTestTaskComplete("turn-a", "first report"))
	st := pumpNewState()
	src := pumpRolloutSource{}

	// First sight: the offset goes to the end and no event is produced.
	res := src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 0 {
		t.Fatalf("a first-sighted rollout produced %d events, want 0", len(res.Events))
	}
	if st.Offsets[path] == 0 {
		t.Fatalf("the offset was not set to the file end: %+v", st.Offsets)
	}
	// A new task_complete and a request_user_input call become a report and a question.
	pumpTestAppendRollout(t, path, pumpTestTaskComplete("turn-b", "second report"))
	pumpTestAppendRollout(t, path, pumpTestQuestion("{\"q\":1}"))
	res = src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(res.Events), res.Events)
	}
	if res.Events[0].Kind != pumpKindReport || !strings.Contains(res.Events[0].Text, "second report") {
		t.Errorf("event 0 = %+v, want a report carrying the message", res.Events[0])
	}
	if res.Events[1].Kind != pumpKindQuestion || !strings.Contains(res.Events[1].Text, "request_user_input") || !strings.Contains(res.Events[1].Text, "{\"q\":1}") {
		t.Errorf("event 1 = %+v, want a question", res.Events[1])
	}
}

// A half-written last line waits for the next round: the offset stops before it.
func TestPumpRolloutWaitsForACompleteLine(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Parents = map[string]string{"parent-1": "P"}
	path := pumpTestRollout(t, "parent-1", pumpTestTaskComplete("turn-a", "before"))
	st := pumpNewState()
	src := pumpRolloutSource{}
	src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	// A line with no trailing newline is not read yet.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(pumpTestTaskComplete("turn-b", "half")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	res := src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 0 {
		t.Fatalf("a half-written line produced %d events, want 0", len(res.Events))
	}
	// Completing the line lets the next round read it.
	pumpTestAppendRollout(t, path, "")
	res = src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 1 {
		t.Fatalf("the completed line produced %d events, want 1", len(res.Events))
	}
}

// The batching rule is pinned by the injected clock: urgent immediately, normal after
// batch_seconds, low-only after low_priority_seconds.
func TestPumpBatchingRulesWithInjectedClock(t *testing.T) {
	settings := pumpSettings{BatchSeconds: pumpTestInt(120), LowPrioritySeconds: pumpTestInt(900)}
	first := float64(pumpTestNow.Unix())

	question := []pumpEvent{{ID: "q1", Kind: pumpKindQuestion, Text: "q"}}
	normal := []pumpEvent{{ID: "n1", Kind: pumpKindReport, Text: "r"}}
	lowOnly := []pumpEvent{{ID: "l1", Kind: pumpKindLow, Text: "l"}}

	if due, urgent := pumpDue(question, &first, pumpTestNow, settings); !due || !urgent {
		t.Errorf("urgent: due=%v urgent=%v, want true,true", due, urgent)
	}
	if due, _ := pumpDue(normal, &first, pumpTestNow.Add(119*time.Second), settings); due {
		t.Error("normal at 119s: due=true, want false")
	}
	if due, _ := pumpDue(normal, &first, pumpTestNow.Add(120*time.Second), settings); !due {
		t.Error("normal at 120s: due=false, want true")
	}
	if due, _ := pumpDue(lowOnly, &first, pumpTestNow.Add(899*time.Second), settings); due {
		t.Error("low at 899s: due=true, want false")
	}
	if due, _ := pumpDue(lowOnly, &first, pumpTestNow.Add(900*time.Second), settings); !due {
		t.Error("low at 900s: due=false, want true")
	}
	if due, _ := pumpDue(nil, nil, pumpTestNow, settings); due {
		t.Error("empty pending: due=true, want false")
	}
}

func pumpTestInt(v int) *int { return &v }

// A round delivers a due batch and clears pending only on accepted.
func TestPumpRoundClearsPendingOnlyOnAccepted(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); code != 0 || err != nil {
		t.Fatalf("round: code=%d err=%v", code, err)
	}
	if st := pumpTestReadState(t, cfg); len(st.Pending) != 0 {
		t.Errorf("accepted left pending %+v", st.Pending)
	}
}

// An unknown outcome keeps pending and the next round reconciles under the same logical id.
func TestPumpUnknownKeepsPendingAndReconcilesSameID(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}},
		// The next round reconciles the same request id through get_operation.
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpTestSwapSources(t, &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "hello"}}})
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadState(t, cfg)
	if len(st.Pending) != 1 {
		t.Fatalf("unknown cleared pending: %+v", st.Pending)
	}
	firstID := pumpBatchID(st.Pending)
	// The second round reconciles under the same logical id.
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, log)
	if len(calls) == 0 || calls[len(calls)-1]["tool"] != deliverToolOperation {
		t.Fatalf("the second round did not reconcile with get_operation: %+v", deliverSendToolsOf(t, log))
	}
	if got := deliverSendRequestIDOf(t, calls[len(calls)-1]); got != firstID {
		t.Errorf("the reconcile used request id %q, want the same logical id %q", got, firstID)
	}
}

// A second pump is refused by the lock.
func TestPumpSecondInstanceIsLocked(t *testing.T) {
	pumpTestEnv(t, &pumpTestNow)
	cfg := pumpTestConfig(t, "")
	release, err := pumpLock(cfg)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer release()
	if _, err := pumpLock(cfg); err == nil {
		t.Fatal("a second lock succeeded, want a refusal")
	} else if !strings.Contains(err.Error(), "pump_locked") {
		t.Fatalf("the refusal = %v, want pump_locked", err)
	}
}

// After a restart the event after the offset is sent once, with no duplicate.
func TestPumpRestartSendsNextEventOnce(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	cfg.Parents = map[string]string{"parent-1": "P"}
	path := pumpTestRollout(t, "parent-1", pumpTestTaskComplete("turn-a", "before"))
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	pumpTestAppendRollout(t, path, pumpTestTaskComplete("turn-b", "after"))
	now = now.Add(2 * time.Minute)
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	sends := 0
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("the bridge saw %d sends, want 1", sends)
	}
}

// rollout_reports=false opens no rollout file and produces no report or question event.
func TestPumpRolloutReportsFalseSkipsRollout(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Parents = map[string]string{"parent-1": "P"}
	path := pumpTestRollout(t, "parent-1", pumpTestTaskComplete("turn-a", "before"))
	st := pumpNewState()
	off := false
	settings := pumpSettingsFrom(cfg)
	settings.RolloutReports = &off
	src := pumpRolloutSource{}
	res := src.Collect(context.Background(), e, cfg, &st, settings)
	if len(res.Events) != 0 {
		t.Errorf("rollout_reports=false produced %d events, want 0", len(res.Events))
	}
	if _, ok := st.Offsets[path]; ok {
		t.Errorf("rollout_reports=false opened the rollout file (offset %+v)", st.Offsets)
	}
	// The default (true) reads it.
	res = src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 0 {
		t.Errorf("the default first sight produced %d events, want 0", len(res.Events))
	}
	if _, ok := st.Offsets[path]; !ok {
		t.Errorf("the default did not open the rollout file")
	}
}

// A signal source still emits while rollout_reports is false.
func TestPumpSignalSourceEmitsWithRolloutReportsFalse(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Parents = map[string]string{"parent-1": "P"}
	pumpTestRollout(t, "parent-1", pumpTestTaskComplete("turn-a", "before"))
	off := false
	settings := pumpSettingsFrom(cfg)
	settings.RolloutReports = &off
	src := &pumpTestSource{name: "signal", events: []pumpEvent{{ID: "s1", Kind: pumpKindLow, Text: "signal"}}}
	pumpTestSwapSources(t, src, pumpRolloutSource{})
	st := pumpNewState()
	var events []pumpEvent
	for _, source := range pumpSources {
		res := source.Collect(context.Background(), e, cfg, &st, settings)
		events = append(events, res.Events...)
	}
	if len(events) != 1 || events[0].ID != "s1" {
		t.Fatalf("events = %+v, want the signal event only", events)
	}
}

// A source whose read fails is unmeasured and emits no event, and the other sources still deliver.
func TestPumpUnmeasuredSourceKeepsOthersDelivering(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	pumpTestSwapSources(t,
		&pumpTestSource{name: "relay", status: pumpSourceUnmeasured},
		&pumpTestSource{name: "signal", events: []pumpEvent{{ID: "s1", Kind: pumpKindQuestion, Text: "signal"}}},
	)
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadState(t, cfg)
	if st.Sources["relay"] != pumpSourceUnmeasured {
		t.Errorf("sources[relay] = %q, want unmeasured", st.Sources["relay"])
	}
	if len(deliverSendToolsOf(t, log)) == 0 {
		t.Fatal("the unmeasured source blocked the signal source's delivery")
	}
}

// A relay id is sent once across a restart, and a gap in the cursor does not stop the read.
func TestPumpRelayIDOnceAcrossRestartAndGap(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	// The source reads relay ids 5 and 9 (a gap) and stores the cursor.
	src := &pumpTestSource{name: "relay", events: []pumpEvent{{ID: "relay-5", Kind: pumpKindQuestion, Text: "a"}},
		onRead: func(st *pumpState) { st.Cursors["relay"] = "9" }}
	pumpTestSwapSources(t, src)
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadState(t, cfg)
	if st.Cursors["relay"] != "9" {
		t.Fatalf("cursor = %q, want 9 (the gap is skipped, not assumed contiguous)", st.Cursors["relay"])
	}
	// A restart re-reads the same id; it must not be sent again.
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	sends := 0
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend {
			sends++
		}
	}
	if sends != 1 {
		t.Fatalf("the relay id was sent %d times, want 1", sends)
	}
}

// A rollout report carries the parent thread id and the turn id in its event id and its body head.
func TestPumpReportCarriesThreadAndTurn(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Parents = map[string]string{"parent-9": "P9"}
	path := pumpTestRollout(t, "parent-9", pumpTestTaskComplete("turn-z", "body"))
	st := pumpNewState()
	src := pumpRolloutSource{}
	src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	pumpTestAppendRollout(t, path, pumpTestTaskComplete("turn-z2", "body2"))
	res := src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(res.Events))
	}
	ev := res.Events[0]
	if !strings.Contains(ev.ID, "parent-9") || !strings.Contains(ev.ID, "turn-z2") {
		t.Errorf("event id %q does not carry the thread and turn", ev.ID)
	}
	if !strings.Contains(ev.Text, "parent-9") || !strings.Contains(ev.Text, "turn-z2") {
		t.Errorf("event text %q does not carry the thread and turn", ev.Text)
	}
}

// The body's first line is the issue's shape, with the urgent marker.
func TestPumpBodyShape(t *testing.T) {
	events := []pumpEvent{{ID: "a", Kind: pumpKindQuestion, Text: "one"}, {ID: "b", Kind: pumpKindReport, Text: "two"}}
	body := pumpBody(events, pumpTestNow, true, "footer text")
	first := strings.SplitN(body, "\n", 2)[0]
	if first != "[management events 03:00] 2 events (urgent)" {
		t.Errorf("first line = %q", first)
	}
	if !strings.Contains(body, "one") || !strings.Contains(body, "two") || !strings.Contains(body, "footer text") {
		t.Errorf("body = %q", body)
	}
	if !strings.HasPrefix(pumpBody(events, pumpTestNow, false, ""), "[management events 03:00] 2 events\n") {
		t.Errorf("non-urgent first line = %q", pumpBody(events, pumpTestNow, false, ""))
	}
}

// The pump's record vocabulary is accepted/received/applied, never read or acknowledged.
func TestPumpRecordsUseAcceptedVocabulary(t *testing.T) {
	cfg := pumpTestConfig(t, "")
	pumpLog(cfg, "deliver abc events=2 class=accepted received=true applied=false")
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpLogFile))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, bad := range []string{"acknowledged", "acked", "acknowledge"} {
		if strings.Contains(text, bad) {
			t.Errorf("the log carries %q: %q", bad, text)
		}
	}
}

// The state document preserves an unknown top-level key a later node wrote.
func TestPumpStatePreservesUnknownKeys(t *testing.T) {
	cfg := pumpTestConfig(t, "")
	raw := []byte("{\"offsets\":{},\"pending\":[],\"first_at\":null,\"prs\":{},\"future_key\":{\"x\":1}}\n")
	if err := os.WriteFile(filepath.Join(cfg.StateDir, pumpStateFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadState(t, cfg)
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(cfg.StateDir, pumpStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "future_key") {
		t.Errorf("the unknown key was dropped: %s", out)
	}
}

// The command refuses a second instance with the issue's JSON and exit 1.
func TestPumpRunRefusesLockedWithTheJSON(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := coreDefaults(e)
	cfg.StateDir = t.TempDir()
	release, err := pumpLock(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var out, errOut strings.Builder
	e.Stdout, e.Stderr = &out, &errOut
	// pumpRunRoundLocked is the lock gate the command runs before a round.
	code := pumpLockRefusedExit(e, cfg)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "pump_locked") {
		t.Errorf("stdout = %q, want the pump_locked JSON", out.String())
	}
}

// A newly collected event does not change a frozen attempt's logical id: the next round
// reconciles the same request id instead of resending a batch that may already be accepted.
func TestPumpNewEventDoesNotBypassReconciliation(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	src := &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "A", Kind: pumpKindQuestion, Text: "A"}}}
	pumpTestSwapSources(t, src)
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	first := pumpTestReadState(t, cfg)
	if first.Attempt == nil {
		t.Fatal("the unknown attempt was not frozen")
	}
	firstID := first.Attempt.LogicalID
	// A new event arrives while the attempt is unsettled.
	src.events = []pumpEvent{{ID: "B", Kind: pumpKindQuestion, Text: "B"}}
	if _, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	calls := deliverSendCallsOf(t, log)
	if calls[len(calls)-1]["tool"] != deliverToolOperation {
		t.Fatalf("the second round did not reconcile: %v", deliverSendToolsOf(t, log))
	}
	if got := deliverSendRequestIDOf(t, calls[len(calls)-1]); got != firstID {
		t.Errorf("the reconcile used request id %q, want the frozen %q", got, firstID)
	}
}

// An accepted delivery whose move did not finish is completed on the next round, not resent.
func TestPumpQueueAcceptedMembershipSurvivesAPartialMove(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := pumpTestConfig(t, bridge)
	path := pumpQueueTestNotice(t, cfg, "parent-1", "aaaaaaaaaaaaaaaa.txt", "one")
	// The membership is recorded as if the move had failed after acceptance.
	st := pumpTestReadState(t, cfg)
	st.QueueAccepted["parent-1"] = []string{"aaaaaaaaaaaaaaaa.txt"}
	if err := st.pumpSave(cfg); err != nil {
		t.Fatal(err)
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	for _, tool := range deliverSendToolsOf(t, log) {
		if tool == deliverToolSend || tool == deliverToolSteer {
			t.Fatalf("the completion sent the accepted notice again: %v", deliverSendToolsOf(t, log))
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, pumpQueueDir, "parent-1", pumpSentDir, "aaaaaaaaaaaaaaaa.txt")); err != nil {
		t.Errorf("the accepted notice was not moved: %v", err)
	}
	_ = path
}

// An unusable issue_pattern is unmeasured, not a panic.
func TestPumpPRSourceBadPatternIsUnmeasured(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	bad := "[unclosed"
	settings := pumpSettingsFrom(cfg)
	settings.IssuePattern = bad
	st := pumpTestReadState(t, cfg)
	res := pumpPRSource{}.Collect(context.Background(), e, cfg, &st, settings)
	if res.Status != pumpSourceUnmeasured {
		t.Fatalf("status = %q, want unmeasured", res.Status)
	}
}

// A PR that returns to an earlier state produces a new event id, so a reopening after a closure
// is announced rather than dropped as a repeat.
func TestPumpPRSourceRepeatStateIsNew(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Repository = "owner/repo"
	saved := pumpExec
	t.Cleanup(func() { pumpExec = saved })

	state := func(state string) []byte {
		out, _ := json.Marshal([]map[string]any{{"number": 12, "state": state, "title": "CRW-1: x"}})
		return out
	}
	st := pumpTestReadState(t, cfg)
	src := pumpPRSource{}

	// First run: OPEN is stored, no event.
	pumpExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return state("OPEN"), nil }
	src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	// CLOSED is a change.
	pumpExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return state("CLOSED"), nil }
	res := src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 1 {
		t.Fatalf("CLOSED produced %d events, want 1", len(res.Events))
	}
	closedID := res.Events[0].ID
	// OPEN again is a new transition, not the first OPEN's id.
	pumpExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return state("OPEN"), nil }
	res = src.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 1 {
		t.Fatalf("the reopening produced %d events, want 1", len(res.Events))
	}
	if res.Events[0].ID == closedID {
		t.Errorf("the reopening reused the CLOSED event id %q", closedID)
	}
}

// The rollout limits count characters, not bytes, and the marker stays inside the limit.
func TestPumpTruncateCountsCharacters(t *testing.T) {
	// A 6000-character Korean report is 6000 runes but 18000 bytes; it must not be cut early.
	text := strings.Repeat("가", 6000)
	if got := pumpTruncate(text, 6000); got != text {
		t.Errorf("a 6000-character report was changed (len %d)", len([]rune(got)))
	}
	long := strings.Repeat("가", 7000)
	cut := pumpTruncate(long, 6000)
	if n := utf8.RuneCountInString(cut); n > 6000 {
		t.Errorf("the truncation returned %d characters, want at most 6000", n)
	}
	if !strings.HasSuffix(cut, pumpRolloutTruncated) {
		t.Errorf("the cut was not marked: %q", cut[len(cut)-20:])
	}
}

// A burst that would exceed the delivery limit is split into a bounded, stable prefix.
func TestPumpBatchPrefixBoundsTheBody(t *testing.T) {
	events := make([]pumpEvent, 0, 40)
	big := strings.Repeat("x", 5900)
	for i := 0; i < 40; i++ {
		events = append(events, pumpEvent{ID: fmt.Sprintf("e%d", i), Kind: pumpKindLow, Text: big})
	}
	batch := pumpBatchPrefix(events, pumpTestNow, false, "")
	if len(batch) == 0 || len(batch) >= len(events) {
		t.Fatalf("the prefix took %d of %d events", len(batch), len(events))
	}
	if body := pumpBody(batch, pumpTestNow, false, ""); len(body) > pumpBatchLimit {
		t.Errorf("the body is %d bytes, over the %d limit", len(body), pumpBatchLimit)
	}
	// The prefix is stable for the same pending set.
	again := pumpBatchPrefix(events, pumpTestNow, false, "")
	if pumpBatchID(batch) != pumpBatchID(again) {
		t.Error("the prefix is not stable across calls")
	}
}

// A legacy state that carries the prs baseline but not prs_seen still reports the change.
func TestPumpPRSourceLegacyBaselineReportsChange(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	cfg := pumpTestConfig(t, "")
	cfg.Repository = "owner/repo"
	saved := pumpExec
	t.Cleanup(func() { pumpExec = saved })
	st := pumpTestReadState(t, cfg)
	// The legacy document: a baseline, no prs_seen flag.
	st.PRs = map[string]string{"12": "CRW-1 #12 OPEN"}
	st.PRsSeen = false
	out, _ := json.Marshal([]map[string]any{{"number": 12, "state": "MERGED", "title": "CRW-1: x"}})
	pumpExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) { return out, nil }
	res := pumpPRSource{}.Collect(context.Background(), e, cfg, &st, pumpSettingsFrom(cfg))
	if len(res.Events) != 1 {
		t.Fatalf("the legacy baseline reported %d events, want 1", len(res.Events))
	}
}
