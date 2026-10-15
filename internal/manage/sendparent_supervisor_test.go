package manage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// supervisorTestSettings replaces the read of the binding's authorization with a fake that answers the settings the test holds now and
// records the tasks it was asked about.
type supervisorTestBinding struct {
	settings coreSettings
	err      error
	asked    []string
}

func supervisorTestSettings(t *testing.T, settings coreSettings) *supervisorTestBinding {
	t.Helper()
	b := &supervisorTestBinding{settings: settings}
	saved := supervisorSettingsRead
	supervisorSettingsRead = func(_ context.Context, _ *Env, _ *Config, task string) (coreSettings, error) {
		b.asked = append(b.asked, task)
		return b.settings, b.err
	}
	t.Cleanup(func() { supervisorSettingsRead = saved })
	return b
}

// supervisorTestNotice reads the notice file the supervisor queue holds for a logical id, "" for sub meaning still queued.
func supervisorTestNotice(t *testing.T, cfg *Config, thread, id, sub string) (supervisorNotice, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, supervisorQueueDir, thread, sub, id+".json"))
	if err != nil {
		if os.IsNotExist(err) {
			return supervisorNotice{}, false
		}
		t.Fatal(err)
	}
	var n supervisorNotice
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	return n, true
}

func supervisorTestSend(t *testing.T, extra ...string) (int, map[string]any) {
	t.Helper()
	args := append([]string{"--role", "supervisor", "--thread", "lead-thread", "--message-file", sendParentMessageFile(t, "the milestone closed"), "--logical-id", "run-1:M1:report"}, extra...)
	code, stdout := sendParentRunCommand(t, args...)
	return code, sendParentReply(t, stdout)
}

func supervisorTestTools(t *testing.T, log string) []string {
	t.Helper()
	return deliverSendToolsOf(t, log)
}

// --role supervisor sends with the role supervisor and the settings the supervision binding of the thread records - not the parent
// settings of the configuration - after the active-turn probe found the thread idle.
func TestSendParentRoleSupervisorUsesBindingSettings(t *testing.T) {
	coreTempHome(t)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := deliverSendConfig(t, bridge)
	cfg.Settings.Parent = coreSettings{Model: "parent-model", ReasoningEffort: "low"}
	sendParentWithConfig(t, cfg)
	binding := supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	code, reply := supervisorTestSend(t, "--report", "rep_1", "--initiative", "I-23")
	if code != sendParentExitAccepted || reply["class"] != deliverClassAccepted || reply["role"] != "supervisor" || reply["logicalId"] != "run-1:M1:report" || reply["requestId"] == "" {
		t.Fatalf("exit %d reply %v", code, reply)
	}
	if !reflect.DeepEqual(binding.asked, []string{"lead-thread"}) {
		t.Errorf("the binding was asked about %v", binding.asked)
	}
	var sends int
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] != deliverToolSend {
			continue
		}
		sends++
		args := call["args"].(map[string]any)
		settings, _ := args["expected_settings"].(map[string]any)
		if args["role"] != "supervisor" || settings["model"] != "gpt-supervisor" || settings["reasoning_effort"] != "xhigh" || args["thread_id"] != "lead-thread" {
			t.Errorf("the send was %v", args)
		}
	}
	if sends != 1 {
		t.Errorf("sent %d times: %v", sends, supervisorTestTools(t, log))
	}
	if _, queued := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", ""); queued {
		t.Error("a delivered notice was queued")
	}
}

// The default role is what it was: the same command line, the same answer (three members), the parent role and the parent settings, one
// probe, and no read of any binding.
func TestSendParentDefaultRoleUnchanged(t *testing.T) {
	for _, extra := range [][]string{nil, {"--role", "parent"}} {
		coreTempHome(t)
		bridge, log := deliverFakeBridge(t, []map[string]any{
			{"payload": map[string]any{"observation": "idle"}},
			{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
		})
		cfg := deliverSendConfig(t, bridge)
		cfg.Settings.Parent = coreSettings{Model: "parent-model", ReasoningEffort: "low"}
		sendParentWithConfig(t, cfg)
		binding := supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
		code, stdout := sendParentRunCommand(t, append([]string{"--thread", "thread-1", "--message-file", sendParentMessageFile(t, "a notice"), "--logical-id", "m1"}, extra...)...)
		if code != sendParentExitAccepted {
			t.Fatalf("exit %d: %s", code, stdout)
		}
		reply := sendParentReply(t, stdout)
		keys := make([]string, 0, len(reply))
		for k := range reply {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, []string{"class", "logicalId", "requestId"}) || reply["class"] != deliverClassAccepted {
			t.Errorf("the answer is %v", reply)
		}
		if got := supervisorTestTools(t, log); !reflect.DeepEqual(got, []string{deliverToolActive, deliverToolSend}) {
			t.Errorf("the bridge saw %v", got)
		}
		for _, call := range deliverSendCallsOf(t, log) {
			if call["tool"] == deliverToolSend {
				args := call["args"].(map[string]any)
				settings, _ := args["expected_settings"].(map[string]any)
				if args["role"] != "parent" || settings["model"] != "parent-model" {
					t.Errorf("the send was %v", args)
				}
			}
		}
		if len(binding.asked) != 0 {
			t.Errorf("a binding was read for the parent role: %v", binding.asked)
		}
		if _, err := os.Stat(filepath.Join(cfg.StateDir, supervisorQueueDir)); err == nil {
			t.Error("the supervisor queue exists after a parent send")
		}
	}
	// a role the command does not know is a usage error, and the options of the supervisor belong to that role
	for _, args := range [][]string{{"--role", "child"}, {"--report", "r1"}, {"--role", "parent", "--initiative", "I-1"}} {
		var out, errOut strings.Builder
		code := Run(context.Background(), append([]string{"send-parent", "--thread", "t", "--message-file", "m.txt"}, args...), strings.NewReader(""), &out, &errOut)
		if code != usageExit {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}

// A thread that is busy keeps the notice in the supervisor queue, with the role and the place its settings come from; nothing is sent.
func TestSendParentSupervisorBusyQueuesTheNotice(t *testing.T) {
	coreTempHome(t)
	bridge, log := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-9"}}})
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	code, reply := supervisorTestSend(t, "--report", "rep_1", "--initiative", "I-23")
	if code != sendParentExitAccepted || reply["class"] != sendParentClassQueued || reply["observation"] != "active" || reply["requestId"] != "" {
		t.Fatalf("exit %d reply %v", code, reply)
	}
	if got := supervisorTestTools(t, log); !reflect.DeepEqual(got, []string{deliverToolActive}) {
		t.Errorf("the bridge saw %v: a busy thread is not steered from the direct path", got)
	}
	n, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", "")
	if !ok || n.Role != "supervisor" || n.BindingTask != "lead-thread" || n.ReportID != "rep_1" || n.Initiative != "I-23" || n.Text != "the milestone closed" || n.Schema != supervisorNoticeSchema {
		t.Errorf("the notice is %+v", n)
	}
	// asking again finds the notice and writes nothing; other text under the same id is refused
	code, again := supervisorTestSend(t)
	if code != sendParentExitAccepted || again["queue"] != "pending" {
		t.Errorf("the second ask: exit %d %v", code, again)
	}
	other := sendParentMessageFile(t, "another text")
	if code, _ := sendParentRunCommand(t, "--role", "supervisor", "--thread", "lead-thread", "--message-file", other, "--logical-id", "run-1:M1:report"); code != sendParentExitRefused {
		t.Errorf("other text under the same id: exit %d", code)
	}
}

// The thread is not loaded: the delivery core would answer unknown without sending, so the command does not call it. No send or steer
// reaches the bridge, no outbox record is written (the retry budget is untouched), and the answer says queued with the observation.
func TestSendParentSupervisorNotLoadedQueuesWithoutDeliver(t *testing.T) {
	coreTempHome(t)
	bridge, log := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"observation": "notLoaded"}}})
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	code, reply := supervisorTestSend(t)
	if code != sendParentExitAccepted || reply["class"] != sendParentClassQueued || reply["observation"] != "notLoaded" {
		t.Fatalf("exit %d reply %v", code, reply)
	}
	if got := supervisorTestTools(t, log); !reflect.DeepEqual(got, []string{deliverToolActive}) {
		t.Errorf("the bridge saw %v", got)
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("an outbox record was written: %v", entries)
	}
	if _, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", ""); !ok {
		t.Error("the notice is not in the supervisor queue")
	}
}

// A thread that unloads between the probe and the send is refused by the bridge (unverified_pair_for_unloaded_thread): the answer says
// refused with that reason, and the notice is in the supervisor queue with the ordinal of its next attempt.
func TestSendParentSupervisorUnloadRaceQueuesTheNotice(t *testing.T) {
	coreTempHome(t)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "failed", "rpcError": map[string]any{"code": "unverified_pair_for_unloaded_thread", "message": "Thread is not loaded"}}},
	})
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	supervisorTestSettings(t, coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"})
	code, reply := supervisorTestSend(t)
	if code != sendParentExitRefused || reply["class"] != deliverClassRefused || reply["reason"] != "unverified_pair_for_unloaded_thread" || reply["queue"] != "queued" {
		t.Fatalf("exit %d reply %v", code, reply)
	}
	n, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", "")
	if !ok || n.LoadRetry != 1 {
		t.Errorf("the notice is %+v (%v)", n, ok)
	}
}

// Without authorized settings nothing is sent: the notice waits in the queue and the answer names the reason.
func TestSendParentSupervisorWithoutSettingsSendsNothing(t *testing.T) {
	coreTempHome(t)
	bridge, log := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	sendParentWithConfig(t, cfg)
	binding := supervisorTestSettings(t, coreSettings{})
	binding.err = errors.New("no authorized settings are recorded")
	code, reply := supervisorTestSend(t)
	if code != sendParentExitAccepted || reply["class"] != sendParentClassQueued || reply["reason"] != "settings_unavailable" {
		t.Fatalf("exit %d reply %v", code, reply)
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("the bridge was called: %v", calls)
	}
}

// The settings are read from the relay: the authorization recorded for the task of the live supervisor binding, model and effort.
func TestSupervisorSettingsAreReadFromTheBinding(t *testing.T) {
	state, st := flowTestStore(t)
	ctx := context.Background()
	if _, err := supervisorSettingsAt(ctx, state, "lead-thread"); err == nil || !strings.Contains(err.Error(), "no live 부모 binding") {
		t.Fatalf("a task with no binding: %v", err)
	}
	if err := storeseed.InsertScopeBinding(ctx, st, store.ScopeBindingsRow{BindingID: "b1", Role: "supervisor", ScopeKind: "initiative", ScopeKey: "I-23", TaskID: "lead-thread", HostID: "h", Status: "active", Revision: 1,
		CreatedAt: "2026-10-12T00:00:00Z", UpdatedAt: "2026-10-12T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisorSettingsAt(ctx, state, "lead-thread"); err == nil || !strings.Contains(err.Error(), "no authorized settings") {
		t.Fatalf("a binding with no authorization: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)`,
		"lead-thread", `{"model":"gpt-sol","reasoningEffort":"xhigh","cwd":"/w"}`, "test", "2026-10-12T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	got, err := supervisorSettingsAt(ctx, state, "lead-thread")
	if err != nil || got != (coreSettings{Model: "gpt-sol", ReasoningEffort: "xhigh"}) {
		t.Fatalf("settings %+v err %v", got, err)
	}
}

// The relay a report was written in is not always the one the manage configuration names: `crw flow report-up --state S --socket P` and a
// resumed run work on the store they were given. `send-parent --role supervisor --state S --socket P` reads the binding's authorization
// there, and the notice it queues remembers the pair so the pump reads the same settings and tells the same store's flow rows.
// (post-evaluation d6)
func TestSendParentSupervisorUsesTheSelectedRelay(t *testing.T) {
	coreTempHome(t)
	bridge, _ := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"observation": "notLoaded"}}})
	cfg := deliverSendConfig(t, bridge)
	cfg.Relay = coreRelay{State: "/relay/default", Socket: "/sock/default"}
	sendParentWithConfig(t, cfg)
	var seen []coreRelay
	saved := supervisorSettingsRead
	supervisorSettingsRead = func(_ context.Context, _ *Env, c *Config, _ string) (coreSettings, error) {
		seen = append(seen, c.Relay)
		return coreSettings{Model: "gpt-supervisor", ReasoningEffort: "xhigh"}, nil
	}
	t.Cleanup(func() { supervisorSettingsRead = saved })

	code, reply := supervisorTestSend(t, "--report", "rep_1", "--state", "/relay/selected", "--socket", "/sock/selected")
	if code != sendParentExitAccepted || reply["class"] != sendParentClassQueued {
		t.Fatalf("exit %d reply %v", code, reply)
	}
	if len(seen) != 1 || seen[0].State != "/relay/selected" || seen[0].Socket != "/sock/selected" {
		t.Fatalf("the settings were read in %+v, want the selected relay", seen)
	}
	n, ok := supervisorTestNotice(t, cfg, "lead-thread", "run-1:M1:report", "")
	if !ok || n.RelayState != "/relay/selected" || n.RelaySocket != "/sock/selected" {
		t.Fatalf("the queued notice is %+v", n)
	}
	if cfg.Relay.State != "/relay/default" {
		t.Errorf("the shared configuration was changed: %+v", cfg.Relay)
	}
	// without the options the configuration's relay is read, as before
	seen = nil
	if code, _ := supervisorTestSend(t, "--report", "rep_2", "--logical-id", "run-2:M2:report"); code != sendParentExitAccepted && code != sendParentExitRefused {
		t.Fatalf("exit %d", code)
	}
	if len(seen) != 1 || seen[0].State != "/relay/default" {
		t.Errorf("the default read in %+v", seen)
	}
	// the parent role takes no relay options
	if code, _ := sendParentRunCommand(t, "--thread", "t", "--message-file", sendParentMessageFile(t, "x"), "--state", "/relay/selected"); code != usageExit {
		t.Errorf("the parent role accepted --state (exit %d)", code)
	}
}
