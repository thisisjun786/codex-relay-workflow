package manage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// supervisorTestQueue puts a notice into the supervisor queue of a thread, queued at created.
func supervisorTestQueue(t *testing.T, cfg *Config, thread, id, report string, created time.Time) {
	t.Helper()
	n := supervisorNotice{LogicalID: id, Thread: thread, BindingTask: thread, ReportID: report, Initiative: "I-23", Text: "report " + id + " is ready", CreatedAt: created.UTC().Format(time.RFC3339)}
	if _, err := supervisorQueueWrite(cfg, n); err != nil {
		t.Fatal(err)
	}
}

// supervisorTestCallsTo reads the calls of the fake bridge that name a tool and a thread.
func supervisorTestCallsTo(t *testing.T, log, tool, thread string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, call := range deliverSendCallsOf(t, log) {
		if args, _ := call["args"].(map[string]any); call["tool"] == tool && args["thread_id"] == thread {
			out = append(out, call)
		}
	}
	return out
}

// supervisorTestCheckDelivered checks the flow commands the pump called: each is the command and its options, followed by the relay socket
// the configuration names (the flow command must open the store the pump reads).
func supervisorTestCheckDelivered(t *testing.T, got, want [][]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("the pump called %d flow commands %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if len(got[i]) < len(want[i]) || !reflect.DeepEqual(got[i][:len(want[i])], want[i]) {
			t.Errorf("flow command %d = %q, want %q", i, got[i], want[i])
		}
		if rest := got[i][len(want[i]):]; len(rest) != 0 && (len(rest) != 2 || rest[0] != "--socket") {
			t.Errorf("flow command %d carries %q after its options", i, rest)
		}
	}
}

func supervisorTestFlush(t *testing.T, e *Env, cfg *Config, st *pumpState) {
	t.Helper()
	if err := pumpSupervisorFlush(context.Background(), e, cfg, st, pumpSettingsFrom(cfg), e.Now(), false); err != nil {
		t.Fatal(err)
	}
}

// A report the lead's thread could not take at send time sits in the supervisor queue, and the flush delivers it later with the role
// supervisor and the settings the binding records then: an active thread is steered at once, an idle one is sent the notice once it has
// waited max_queue_seconds. The delivered notice moves to sent/, and `crw flow report-delivered` is the call that tells the flow rows.
func TestQueuedSupervisorReportFlushesWithSupervisorRoleAndSettings(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	binding := supervisorTestSettings(t, coreSettings{Model: "queue-time-model", ReasoningEffort: "low"})

	// idle: nothing before the wait is over, a send with the settings of now after it
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	supervisorTestQueue(t, cfg, "lead-thread", "run-1:M1:report", "rep_1", now)
	binding.settings = coreSettings{Model: "flush-time-model", ReasoningEffort: "xhigh"}
	st := pumpTestReadStatePtr(t, cfg)
	supervisorTestFlush(t, e, cfg, st)
	if sends := supervisorTestCallsTo(t, log, deliverToolSend, "lead-thread"); len(sends) != 0 {
		t.Fatalf("an idle thread was sent the notice at once: %v", sends)
	}
	now = now.Add(time.Hour + time.Second)
	supervisorTestFlush(t, e, cfg, st)
	sends := supervisorTestCallsTo(t, log, deliverToolSend, "lead-thread")
	if len(sends) != 1 {
		t.Fatalf("sends %v", sends)
	}
	args := sends[0]["args"].(map[string]any)
	settings, _ := args["expected_settings"].(map[string]any)
	if args["role"] != "supervisor" || settings["model"] != "flush-time-model" || settings["reasoning_effort"] != "xhigh" || args["request_id"] != "run-1:M1:report" || !strings.Contains(args["message"].(string), "report run-1:M1:report is ready") {
		t.Errorf("the send was %v", args)
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", ""); ok {
		t.Error("the delivered notice is still queued")
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", pumpSentDir); !ok {
		t.Error("the delivered notice is not in sent/")
	}
	supervisorTestCheckDelivered(t, flowTestCalls(t, record), [][]string{{"flow", "report-delivered", "--report", "rep_1", "--result", "accepted", "--request-id", "run-1:M1:report"}})
	if len(st.Supervisor.Delivered) != 0 {
		t.Errorf("the confirmed delivery is still owed: %v", st.Supervisor.Delivered)
	}

	// active: steered at once, with no wait
	active, activeLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-7"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg.Bridge.Binary = active
	supervisorTestQueue(t, cfg, "lead-thread", "run-2:M2:report", "rep_2", now)
	supervisorTestFlush(t, e, cfg, st)
	steers := supervisorTestCallsTo(t, activeLog, deliverToolSteer, "lead-thread")
	if len(steers) != 1 || steers[0]["args"].(map[string]any)["expected_turn_id"] != "turn-7" {
		t.Fatalf("steers %v", steers)
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-2:M2:report", pumpSentDir); !ok {
		t.Error("the steered notice is not in sent/")
	}
	if calls := flowTestCalls(t, record); len(calls) != 2 || calls[1][3] != "rep_2" {
		t.Errorf("report-delivered calls %v", calls)
	}
}

// The thread unloads between the probe and the send: the bridge refuses (unverified_pair_for_unloaded_thread). The notice stays queued,
// the refusal is not counted against the refusal limit, one report_undelivered is raised, and the next attempt goes out under <id>-l1 so
// the settled refusal of the first id is not replayed.
func TestSupervisorFlushUnloadRaceRetriesUnderNewID(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	race, raceLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "failed", "rpcError": map[string]any{"code": "unverified_pair_for_unloaded_thread", "message": "Thread is not loaded"}}},
	})
	cfg := pumpTestConfig(t, race)
	supervisorTestQueue(t, cfg, "lead-thread", "run-1:M1:report", "rep_1", now.Add(-2*time.Hour))
	st := pumpTestReadStatePtr(t, cfg)
	supervisorTestFlush(t, e, cfg, st)
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", ""); !ok {
		t.Fatal("the notice left the queue")
	}
	if sends := supervisorTestCallsTo(t, raceLog, deliverToolSend, "lead-thread"); len(sends) != 1 || sends[0]["args"].(map[string]any)["request_id"] != "run-1:M1:report" {
		t.Fatalf("the first attempt: %v", sends)
	}
	if got := st.Supervisor.Attempts["run-1:M1:report"]; got.Loaded != 1 || got.Refused != 0 {
		t.Errorf("the attempt counts are %+v", got)
	}
	if len(st.QueueRefused) != 0 {
		t.Errorf("the unload race was counted as a refusal: %v", st.QueueRefused)
	}
	var undelivered []pumpEvent
	for _, ev := range st.Pending {
		if ev.Kind == pumpKindReportUndelivered {
			undelivered = append(undelivered, ev)
		}
	}
	if len(undelivered) != 1 || undelivered[0].ID != "report_undelivered:rep_1" || !strings.Contains(undelivered[0].Text, "rep_1") || !strings.Contains(undelivered[0].Text, "I-23") {
		t.Fatalf("report_undelivered events: %v", st.Pending)
	}
	// the next round: the thread is loaded now and takes the notice under the new id; the event is not raised again
	loaded, loadedLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg.Bridge.Binary = loaded
	supervisorTestFlush(t, e, cfg, st)
	sends := supervisorTestCallsTo(t, loadedLog, deliverToolSend, "lead-thread")
	if len(sends) != 1 || sends[0]["args"].(map[string]any)["request_id"] != "run-1:M1:report-l1" {
		t.Fatalf("the second attempt: %v", sends)
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", pumpSentDir); !ok {
		t.Error("the notice is not in sent/")
	}
	if n := countKind(st.Pending, pumpKindReportUndelivered); n != 1 {
		t.Errorf("%d report_undelivered events", n)
	}
	supervisorTestCheckDelivered(t, flowTestCalls(t, record), [][]string{{"flow", "report-delivered", "--report", "rep_1", "--result", "accepted", "--request-id", "run-1:M1:report-l1"}})
}

// The direct send-parent --role supervisor path met the unloaded-thread refusal and queued the notice with the next ordinal: the flush that
// meets it raises the same report_undelivered once, even when the thread takes the notice then, and delivers it under the -l1 id.
func TestSupervisorFlushRaisesUndeliveredForADirectPathUnloadRefusal(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	loaded, loadedLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, loaded)
	n := supervisorNotice{LogicalID: "run-1:M1:report", Thread: "lead-thread", BindingTask: "lead-thread", ReportID: "rep_1", Initiative: "I-23", Text: "report is ready", CreatedAt: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)}
	if _, err := supervisorQueueWrite(cfg, withLoadRetry(n, 1)); err != nil {
		t.Fatal(err)
	}
	st := pumpTestReadStatePtr(t, cfg)
	supervisorTestFlush(t, e, cfg, st)
	if k := countKind(st.Pending, pumpKindReportUndelivered); k != 1 {
		t.Errorf("%d report_undelivered events, want 1", k)
	}
	if sends := supervisorTestCallsTo(t, loadedLog, deliverToolSend, "lead-thread"); len(sends) != 1 || sends[0]["args"].(map[string]any)["request_id"] != "run-1:M1:report-l1" {
		t.Fatalf("the delivery: %v", sends)
	}
	supervisorTestCheckDelivered(t, flowTestCalls(t, record), [][]string{{"flow", "report-delivered", "--report", "rep_1", "--result", "accepted", "--request-id", "run-1:M1:report-l1"}})
	// and a notice that was never refused raises none
	other := pumpTestConfig(t, loaded)
	supervisorTestQueue(t, other, "lead-thread", "run-2:M2:report", "rep_2", now.Add(-2*time.Hour))
	st2 := pumpTestReadStatePtr(t, other)
	supervisorTestFlush(t, e, other, st2)
	if k := countKind(st2.Pending, pumpKindReportUndelivered); k != 0 {
		t.Errorf("%d report_undelivered events for a notice that was never refused", k)
	}
}

func countKind(events []pumpEvent, kind string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// A report-up whose initiative thread is not loaded: `send-parent --role supervisor` sends nothing and queues, the pump rounds that
// observe notLoaded again send nothing and tell the management thread once, and when the thread is loaded the same flush delivers with
// the role supervisor and the binding's settings and has the delivery row written.
func TestReportUpNotLoadedQueuesThenDeliversAfterLoad(t *testing.T) {
	// send-parent stamps the notice with the real clock, so the pump's clock starts from it
	now := time.Now()
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	pumpTestSwapSources(t)
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	notLoaded, notLoadedLog := deliverFakeBridge(t, []map[string]any{
		{"when_thread": "lead-thread", "payload": map[string]any{"observation": "notLoaded"}},
		{"when_thread": "mgmt-thread", "payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, notLoaded)
	sendParentWithConfig(t, cfg)

	// report-up: the delivery is tried through send-parent --role supervisor
	code, reply := supervisorTestSend(t, "--report", "rep_1", "--initiative", "I-23")
	if code != sendParentExitAccepted || reply["class"] != sendParentClassQueued || reply["observation"] != "notLoaded" {
		t.Fatalf("send-parent: exit %d %v", code, reply)
	}
	if calls := append(supervisorTestCallsTo(t, notLoadedLog, deliverToolSend, "lead-thread"), supervisorTestCallsTo(t, notLoadedLog, deliverToolSteer, "lead-thread")...); len(calls) != 0 {
		t.Fatalf("a send reached the unloaded thread: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "outbox")); err == nil {
		t.Error("an outbox record was written")
	}

	// rounds 1 and 2: the thread is still not loaded
	for round := 1; round <= 2; round++ {
		now = now.Add(2 * time.Hour)
		if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil || code != 0 {
			t.Fatalf("round %d: %d %v", round, code, err)
		}
		if calls := append(supervisorTestCallsTo(t, notLoadedLog, deliverToolSend, "lead-thread"), supervisorTestCallsTo(t, notLoadedLog, deliverToolSteer, "lead-thread")...); len(calls) != 0 {
			t.Fatalf("round %d sent to the unloaded thread: %v", round, calls)
		}
		toManager := supervisorTestCallsTo(t, notLoadedLog, deliverToolSend, "mgmt-thread")
		if len(toManager) != 1 {
			t.Fatalf("after round %d the management thread was sent %d messages: %v", round, len(toManager), toManager)
		}
		text := toManager[0]["args"].(map[string]any)["message"].(string)
		for _, want := range []string{"report undelivered", "report=rep_1", "initiative=I-23", "thread=lead-thread", "observation=notLoaded", "waiting="} {
			if !strings.Contains(text, want) {
				t.Errorf("the event lacks %q: %s", want, text)
			}
		}
	}
	if calls := flowTestCalls(t, record); len(calls) != 0 {
		t.Fatalf("report-delivered was called before the delivery: %v", calls)
	}

	// the thread is loaded: the same flush delivers
	loaded, loadedLog := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg.Bridge.Binary = loaded
	now = now.Add(2 * time.Hour)
	if code, err := pumpRound(context.Background(), e, cfg, pumpSettingsFrom(cfg), false); err != nil || code != 0 {
		t.Fatalf("round 3: %d %v", code, err)
	}
	sends := supervisorTestCallsTo(t, loadedLog, deliverToolSend, "lead-thread")
	if len(sends) != 1 {
		t.Fatalf("sends %v", sends)
	}
	args := sends[0]["args"].(map[string]any)
	settings, _ := args["expected_settings"].(map[string]any)
	if args["role"] != "supervisor" || settings["model"] != "gpt-supervisor" || args["request_id"] != "run-1:M1:report" {
		t.Errorf("the send was %v", args)
	}
	supervisorTestCheckDelivered(t, flowTestCalls(t, record), [][]string{{"flow", "report-delivered", "--report", "rep_1", "--result", "accepted", "--request-id", "run-1:M1:report"}})
}

// Without settings the flush sends nothing and keeps the notice; a refusal that is not the unload race counts against the limit and
// moves the notice to refused/ after pumpQueueRefusalLimit refusals, and the flow rows are told it was refused.
func TestSupervisorFlushKeepsAndRefusesNotices(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	binding := supervisorTestSettings(t, coreSettings{})
	binding.err = os.ErrNotExist
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "t"}},
		{"payload": map[string]any{"status": "refused"}},
	})
	cfg := pumpTestConfig(t, bridge)
	supervisorTestQueue(t, cfg, "lead-thread", "run-1:M1:report", "rep_1", now.Add(-2*time.Hour))
	st := pumpTestReadStatePtr(t, cfg)
	supervisorTestFlush(t, e, cfg, st)
	if sends := supervisorTestCallsTo(t, log, deliverToolSteer, "lead-thread"); len(sends) != 0 {
		t.Fatalf("a notice was sent without settings: %v", sends)
	}
	binding.err = nil
	binding.settings = coreSettings{Model: "m", ReasoningEffort: "high"}
	for i := 0; i < pumpQueueRefusalLimit; i++ {
		supervisorTestFlush(t, e, cfg, st)
	}
	var ids []string
	for _, call := range supervisorTestCallsTo(t, log, deliverToolSteer, "lead-thread") {
		ids = append(ids, call["args"].(map[string]any)["request_id"].(string))
	}
	if !reflect.DeepEqual(ids, []string{"run-1:M1:report", "run-1:M1:report-r1", "run-1:M1:report-r2"}) {
		t.Errorf("the attempts went out under %v", ids)
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", supervisorRefusedDir); !ok {
		t.Error("the refused notice is not in refused/")
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", ""); ok {
		t.Error("the refused notice is still queued")
	}
	calls := flowTestCalls(t, record)
	if len(calls) != 1 || calls[0][1] != "report-delivered" || calls[0][5] != "refused" || calls[0][7] != "run-1:M1:report-r2" {
		t.Errorf("report-delivered calls %v", calls)
	}
}

// The parent queue is what it was: a supervisor notice for the same thread name is not part of its batch, the batch id and body are the
// ones the queue always made, and no flow command is called.
func TestPumpQueueParentNoticesUnchanged(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := pumpTestConfig(t, bridge)
	cfg.Settings.Parent = coreSettings{Model: "parent-model", ReasoningEffort: "low"}
	pumpQueueTestNotice(t, cfg, "lead-thread", "aaaaaaaaaaaaaaaa.txt", "first")
	pumpQueueTestNotice(t, cfg, "lead-thread", "bbbbbbbbbbbbbbbb.txt", "second")
	supervisorTestQueue(t, cfg, "lead-thread", "run-1:M1:report", "rep_1", now)
	old := now.Add(-2 * time.Hour)
	for _, name := range []string{"aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"} {
		if err := os.Chtimes(filepath.Join(cfg.StateDir, pumpQueueDir, "lead-thread", name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := pumpQueueFlush(context.Background(), e, cfg, pumpTestReadStatePtr(t, cfg), pumpSettingsFrom(cfg), false); err != nil {
		t.Fatal(err)
	}
	body := pumpReview776QueueBody([]string{"first", "second"})
	sends := supervisorTestCallsTo(t, log, deliverToolSend, "lead-thread")
	if len(sends) != 1 {
		t.Fatalf("sends %v", sends)
	}
	args := sends[0]["args"].(map[string]any)
	settings, _ := args["expected_settings"].(map[string]any)
	wantID := pumpQueueBatchID("lead-thread", []string{"aaaaaaaaaaaaaaaa.txt", "bbbbbbbbbbbbbbbb.txt"}, body)
	if args["request_id"] != wantID || args["message"] != body || args["role"] != "parent" || settings["model"] != "parent-model" {
		t.Errorf("the batch was %v, want id %s", args, wantID)
	}
	if names := pumpQueueTestNames(t, cfg, "lead-thread"); len(names) != 0 {
		t.Errorf("queued after the flush: %v", names)
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", ""); !ok {
		t.Error("the supervisor notice was touched by the parent queue")
	}
	if calls := flowTestCalls(t, record); len(calls) != 0 {
		t.Errorf("a flow command was called: %v", calls)
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, summarySettledDir)); err == nil {
		t.Error("a notice that is no summary left a settlement")
	}
}

// The bridge refused a send as unverified_pair_for_unloaded_thread, but the response was lost (or the pump died before it kept the retry
// ordinal). The next flush reaches the delivery core with an unsettled record: the core reconciles it with the bridge's own receipt and
// answers unknown. That receipt is the proof that nothing was delivered, so the supervisor path reads the refusal from it, advances the
// ordinal and the following attempt goes out under <id>-l1 instead of reconciling the same id for ever. (post-evaluation d4)
func TestSupervisorFlushRecoversAnUnloadRefusalWhoseOrdinalWasLost(t *testing.T) {
	now := pumpTestNow
	e := pumpTestEnv(t, &now)
	exe, record := flowTestCRW(t)
	e.Executable = exe
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	failed := map[string]any{"status": "failed", "rpcError": map[string]any{"code": "unverified_pair_for_unloaded_thread", "message": "Thread is not loaded"}}
	race, _ := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"observation": "idle"}}, {"payload": failed}})
	cfg := pumpTestConfig(t, race)
	supervisorTestQueue(t, cfg, "lead-thread", "run-1:M1:report", "rep_1", now.Add(-2*time.Hour))
	st := pumpTestReadStatePtr(t, cfg)
	supervisorTestFlush(t, e, cfg, st)
	if got := st.Supervisor.Attempts["run-1:M1:report"]; got.Loaded != 1 {
		t.Fatalf("the first refusal was not counted: %+v", got)
	}
	// the process died before the ordinal was kept: the next pump has no memory of the attempt
	st.Supervisor.Attempts = map[string]pumpSupervisorAttempt{}
	// every dial starts a new bridge process: the probe (about the thread) is answered idle, the delivery core's get_operation (about a
	// request id) reads the receipt of the first attempt
	reconcile, reconcileLog := deliverFakeBridge(t, []map[string]any{{"when_thread": "lead-thread", "payload": map[string]any{"observation": "idle"}}, {"payload": failed}})
	cfg.Bridge.Binary = reconcile
	supervisorTestFlush(t, e, cfg, st)
	if sends := supervisorTestCallsTo(t, reconcileLog, deliverToolSend, "lead-thread"); len(sends) != 0 {
		t.Fatalf("the reconciliation sent: %v", sends)
	}
	if got := st.Supervisor.Attempts["run-1:M1:report"]; got.Loaded != 1 || got.Refused != 0 {
		t.Fatalf("the refusal in the bridge's receipt did not advance the ordinal: %+v", got)
	}
	// the thread is loaded: the notice goes out under the new id
	loaded, loadedLog := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"observation": "idle"}}, {"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}}})
	cfg.Bridge.Binary = loaded
	supervisorTestFlush(t, e, cfg, st)
	sends := supervisorTestCallsTo(t, loadedLog, deliverToolSend, "lead-thread")
	if len(sends) != 1 || sends[0]["args"].(map[string]any)["request_id"] != "run-1:M1:report-l1" {
		t.Fatalf("the attempt after the recovery: %v", sends)
	}
	supervisorTestCheckDelivered(t, flowTestCalls(t, record), [][]string{{"flow", "report-delivered", "--report", "rep_1", "--result", "accepted", "--request-id", "run-1:M1:report-l1"}})
}
