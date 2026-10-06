package manage

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fake bridge is this test binary started again, the way core_testhelp_test.go's fake crw is:
// deliverFakeBridge writes a shell script that points the environment at a scenario file and execs
// this binary, and TestDeliverFakeBridge serves that scenario over stdio. No test reaches a real
// bridge, the App Server or the network.

const (
	deliverFakeScenarioEnv = "CRW_MANAGE_TEST_DELIVER_SCENARIO"
	deliverFakeLogEnv      = "CRW_MANAGE_TEST_DELIVER_LOG"
	deliverFakeRun         = "^TestDeliverFakeBridge$"
)

// TestDeliverFakeBridge is the fake bridge when this binary is re-executed with a scenario.
func TestDeliverFakeBridge(t *testing.T) {
	scenario := os.Getenv(deliverFakeScenarioEnv)
	if scenario == "" {
		t.Skip("not the fake bridge")
	}
	deliverFakeServe(scenario)
}

// deliverFakeServe answers initialize, ignores notifications/initialized, and answers each
// tools/call from the next scenario step. A step may instead say eof or die, which leave without
// answering. The process leaves through os.Exit, so the test framework never writes anything onto
// the protocol stream.
func deliverFakeServe(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		os.Exit(91)
	}
	var steps []map[string]any
	if err := json.Unmarshal(raw, &steps); err != nil {
		os.Exit(92)
	}
	log := os.Getenv(deliverFakeLogEnv)
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for next := 0; ; {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(0)
		}
		id, request := message["id"]
		if method, _ := message["method"].(string); method == "initialize" {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": deliverProtocol, "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "fake-bridge", "version": "1"}}})
			continue
		}
		if !request {
			continue
		}
		params, _ := message["params"].(map[string]any)
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		deliverFakeLog(log, name, args)
		step := map[string]any{}
		if next < len(steps) {
			step = steps[next]
		}
		next++
		if step["eof"] == true || step["die"] == true {
			os.Exit(0)
		}
		text, _ := step["text"].(string)
		if payload, ok := step["payload"]; ok && text == "" {
			encoded, err := json.Marshal(payload)
			if err != nil {
				os.Exit(93)
			}
			text = string(encoded)
		}
		// A step may instead answer with the content of a file, which is how a test observes what
		// was on disk at the moment the call arrived.
		if path, ok := step["read"].(string); ok {
			if raw, err := os.ReadFile(path); err == nil {
				text = string(raw)
			} else {
				text = "read failed: " + err.Error()
			}
		}
		result := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		if step["isError"] == true {
			result["isError"] = true
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

// deliverFakeLog appends one JSON line naming the tool and the arguments it was called with.
func deliverFakeLog(path, tool string, args map[string]any) {
	if path == "" {
		return
	}
	line, err := json.Marshal(map[string]any{"tool": tool, "args": args})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, 0x0a))
	_ = f.Close()
}

// deliverFakeBridge writes the fake bridge for steps and returns its path and the file that
// records one JSON line per call it received.
func deliverFakeBridge(t *testing.T, steps []map[string]any) (bridge, log string) {
	t.Helper()
	dir := t.TempDir()
	scenario := filepath.Join(dir, "scenario.json")
	raw, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scenario, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	log = filepath.Join(dir, "calls.jsonl")
	bridge = filepath.Join(dir, "bridge")
	script := "#!/bin/sh\n" + deliverFakeScenarioEnv + "=" + coreShellQuote(scenario) + " " +
		deliverFakeLogEnv + "=" + coreShellQuote(log) + " exec " + coreShellQuote(os.Args[0]) +
		" -test.run " + coreShellQuote(deliverFakeRun) + "\n"
	if err := os.WriteFile(bridge, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bridge, log
}

// deliverSendTestEnv is the Env a delivery runs with: temporary homes and this binary as the
// running executable, so the bridge fallback never names a real program.
func deliverSendTestEnv(t *testing.T) *Env {
	t.Helper()
	coreTempHome(t)
	return &Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Getenv: os.Getenv, Now: time.Now, Executable: os.Args[0]}
}

// deliverSendConfig is a configuration whose outbox lives in a fresh temporary directory and
// whose bridge is the fake. deliverTestConfig in deliver_test.go builds the bridge-less one.
func deliverSendConfig(t *testing.T, bridge string) *Config {
	t.Helper()
	cfg := coreDefaults(&Env{Getenv: os.Getenv})
	cfg.StateDir = t.TempDir()
	cfg.Bridge = coreBridge{Binary: bridge, ExecutionPolicy: "policy.json"}
	return cfg
}

// deliverSendCallsOf reads the calls the fake bridge saw, oldest first.
func deliverSendCallsOf(t *testing.T, log string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var call map[string]any
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatalf("call record %q: %v", line, err)
		}
		out = append(out, call)
	}
	return out
}

// deliverSendToolsOf is the tool names of the calls the fake bridge saw, in order.
func deliverSendToolsOf(t *testing.T, log string) []string {
	t.Helper()
	names := []string{}
	for _, call := range deliverSendCallsOf(t, log) {
		names = append(names, call["tool"].(string))
	}
	return names
}

// deliverSendRequestIDOf is the request id one call carried.
func deliverSendRequestIDOf(t *testing.T, call map[string]any) string {
	t.Helper()
	args, ok := call["args"].(map[string]any)
	if !ok {
		t.Fatalf("call %v carried no arguments", call)
	}
	id, _ := args["request_id"].(string)
	return id
}

// The contract inputs of the issue body's table classify as the allowlist says, and an unknown
// response neither deletes the record nor confirms that it was sent.
func TestDeliverClassifiesTheContractInputs(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"accepted", map[string]any{"status": "accepted", "delivery": "turn_started"}, deliverClassAccepted},
		{"ledger in progress", map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}, deliverClassUnknown},
		{"outcome unknown", map[string]any{"status": "outcome_unknown", "delivery": "outcome_unknown"}, deliverClassUnknown},
		{"outcome unknown with an error", map[string]any{"status": "outcome_unknown", "error": "boom"}, deliverClassUnknown},
		{"empty response", map[string]any{}, deliverClassUnknown},
		{"rejected", map[string]any{"delivery": "rejected"}, deliverClassRefused},
		{"refused", map[string]any{"status": "refused"}, deliverClassRefused},
		{"not attempted", map[string]any{"status": "not_attempted", "retrySafe": true}, deliverClassRefused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := deliverSendTestEnv(t)
			bridge, _ := deliverFakeBridge(t, []map[string]any{
				{"payload": map[string]any{"observation": "idle"}},
				{"payload": c.payload},
			})
			cfg := deliverSendConfig(t, bridge)
			out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
			if err != nil {
				t.Fatalf("Deliver: %v", err)
			}
			if out.Class != c.want {
				t.Errorf("class = %q, want %q (receipt %v)", out.Class, c.want, out.Receipt)
			}
			if out.RequestID == "" {
				t.Error("the outcome names no request id")
			}
			record := deliverRecordOf(t, cfg, "m1")
			if record.RequestID != out.RequestID || record.Applied {
				t.Errorf("the record after the call: %+v", record)
			}
			switch c.want {
			case deliverClassAccepted:
				if record.State != deliverStateAccepted || !record.Received {
					t.Errorf("an accepted delivery recorded %+v", record)
				}
			case deliverClassUnknown:
				if record.State != deliverStateUnknown || record.Received {
					t.Errorf("an unknown delivery recorded %+v", record)
				}
				if n := len(record.Attempts); n == 0 || record.Attempts[n-1].Class != deliverClassUnknown {
					t.Errorf("the unknown attempt was not preserved: %+v", record.Attempts)
				}
			case deliverClassRefused:
				if record.State != deliverStateRefused || record.Received {
					t.Errorf("a refused delivery recorded %+v", record)
				}
			}
		})
	}
}

// A steer into a running turn is acceptance into that turn, never application.
func TestDeliverSteerIsAcceptedNotApplied(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "active", "activeTurnId": "turn-1"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "accepted_not_applied"}},
	})
	cfg := deliverSendConfig(t, bridge)
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || out.Class != deliverClassAccepted {
		t.Fatalf("Deliver: %q %v", out.Class, err)
	}
	record := deliverRecordOf(t, cfg, "m1")
	if !record.Received || record.Applied || record.Tool != deliverToolSteer {
		t.Errorf("the steer recorded %+v", record)
	}
}

// A response lost after tools/call went out is unknown, never refused.
func TestDeliverLostResponseIsUnknown(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"eof": true},
	})
	cfg := deliverSendConfig(t, bridge)
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || out.Class != deliverClassUnknown {
		t.Fatalf("Deliver: %q %v", out.Class, err)
	}
	if record := deliverRecordOf(t, cfg, "m1"); record.State != deliverStateUnknown || record.Received {
		t.Errorf("the lost response recorded %+v", record)
	}
}

// An unsettled record is reconciled with get_operation before anything else, whatever state it
// was left in, and that reconciliation never mints a new request id. The first case is the process
// that died after the ledger write and before the call; the second is the one whose response was
// lost; the third is a receipt the bridge already holds.
func TestDeliverReconcilesAnUnsettledRecordFirst(t *testing.T) {
	cases := []struct {
		name   string
		state  string
		answer map[string]any
		class  string
	}{
		{"died before the call", deliverStatePending, map[string]any{"isError": true, "text": "Error executing tool get_operation: Unknown request_id"}, deliverClassUnknown},
		{"response lost after the call", deliverStateUnknown, map[string]any{"payload": map[string]any{"status": "outcome_unknown"}}, deliverClassUnknown},
		{"already accepted on the bridge", deliverStateUnknown, map[string]any{"payload": map[string]any{"status": "accepted"}}, deliverClassAccepted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := deliverSendTestEnv(t)
			bridge, log := deliverFakeBridge(t, []map[string]any{c.answer})
			cfg := deliverSendConfig(t, bridge)
			if err := deliverSave(cfg, deliverRecord{LogicalID: "m1", RequestID: "m1", Tool: deliverToolSend, TargetThread: "thread-1", State: c.state}); err != nil {
				t.Fatal(err)
			}
			out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
			if err != nil || out.Class != c.class || out.RequestID != "m1" {
				t.Fatalf("Deliver: %q %q %v, want %q under m1", out.Class, out.RequestID, err, c.class)
			}
			if tools := deliverSendToolsOf(t, log); len(tools) != 1 || tools[0] != deliverToolOperation {
				t.Fatalf("the fake bridge saw %v, want only get_operation", tools)
			}
			if id := deliverSendRequestIDOf(t, deliverSendCallsOf(t, log)[0]); id != "m1" {
				t.Errorf("get_operation asked for %q, want m1", id)
			}
			if record := deliverRecordOf(t, cfg, "m1"); record.RequestID != "m1" || record.State != c.class {
				t.Errorf("the record after reconciliation: %+v", record)
			}
		})
	}
}

// A not_attempted reconciliation resends under the same request id: nothing left the process, so
// the same id makes the attempt rather than replaying an answer.
func TestDeliverResendsNotAttemptedUnderTheSameRequestID(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"status": "not_attempted", "retrySafe": true}},
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := deliverSendConfig(t, bridge)
	if err := deliverSave(cfg, deliverRecord{LogicalID: "m1", RequestID: "m1", Tool: deliverToolSend, TargetThread: "thread-1", State: deliverStateUnknown}); err != nil {
		t.Fatal(err)
	}
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || out.Class != deliverClassAccepted || out.RequestID != "m1" {
		t.Fatalf("Deliver: %q %q %v, want accepted under m1", out.Class, out.RequestID, err)
	}
	for _, call := range deliverSendCallsOf(t, log) {
		if id := deliverSendRequestIDOf(t, call); id != "" && id != "m1" {
			t.Errorf("the bridge saw request id %q, want m1", id)
		}
	}
}

// A replay of an accepted logical message stays one accepted attempt and sends nothing again.
func TestDeliverDuplicateReplayStaysOneAcceptedAttempt(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := deliverSendConfig(t, bridge)
	message := Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"}
	first, err := Deliver(context.Background(), e, cfg, message)
	if err != nil || first.Class != deliverClassAccepted {
		t.Fatalf("the first delivery: %q %v", first.Class, err)
	}
	second, err := Deliver(context.Background(), e, cfg, message)
	if err != nil || second.Class != deliverClassAccepted || second.RequestID != first.RequestID {
		t.Fatalf("the replay: %q %q %v", second.Class, second.RequestID, err)
	}
	if tools := deliverSendToolsOf(t, log); len(tools) != 2 {
		t.Errorf("the fake bridge saw %v, want one get_active_turn and one send", tools)
	}
	accepted := 0
	for _, attempt := range deliverRecordOf(t, cfg, "m1").Attempts {
		if attempt.Class == deliverClassAccepted {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("the record carries %d accepted attempts", accepted)
	}
}

// Only not_delivered retries, under a new request id, at most twice.
func TestDeliverNotDeliveredRetriesAtMostTwice(t *testing.T) {
	e := deliverSendTestEnv(t)
	round := []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "failed", "delivery": "not_delivered"}},
	}
	steps := append(append(append([]map[string]any{}, round...), round...), round...)
	bridge, log := deliverFakeBridge(t, steps)
	cfg := deliverSendConfig(t, bridge)
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || out.Class != deliverClassRefused {
		t.Fatalf("Deliver: %q %v", out.Class, err)
	}
	var ids []string
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSend {
			ids = append(ids, deliverSendRequestIDOf(t, call))
		}
	}
	want := []string{"m1", "m1-r1", "m1-r2"}
	if len(ids) != len(want) {
		t.Fatalf("the retries were %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("retry %d used %q, want %q", i, ids[i], want[i])
		}
	}
}

// A rejected delivery is never resent.
func TestDeliverRejectedIsNotResent(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "failed", "delivery": "rejected"}},
	})
	cfg := deliverSendConfig(t, bridge)
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || out.Class != deliverClassRefused {
		t.Fatalf("Deliver: %q %v", out.Class, err)
	}
	sends := 0
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSend {
			sends++
		}
	}
	if sends != 1 {
		t.Errorf("a rejected delivery was sent %d times", sends)
	}
}

// An unconfigured execution policy refuses before any bridge process starts and before any file
// is written.
func TestDeliverRefusesWithoutAnExecutionPolicy(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	cfg.Bridge.ExecutionPolicy = ""
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), deliverPolicyUnconfigured) || out.Class != deliverClassRefused {
		t.Fatalf("Deliver: %q %v, want the unconfigured-policy refusal", out.Class, err)
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("the bridge was started anyway: %v", calls)
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("a refused policy wrote %v", entries)
	}
}

// The record is on disk before the bridge is even started, so a crash between the two leaves it.
func TestDeliverWritesTheRecordBeforeTheBridgeStarts(t *testing.T) {
	e := deliverSendTestEnv(t)
	cfg := deliverSendConfig(t, filepath.Join(t.TempDir(), "not-a-bridge"))
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err == nil || out.Class != deliverClassRefused {
		t.Fatalf("Deliver: %q %v, want the dial refusal", out.Class, err)
	}
	record := deliverRecordOf(t, cfg, "m1")
	if record.LogicalID != "m1" || record.RequestID != out.RequestID || record.MessageSHA256 == "" || record.CreatedAt == "" {
		t.Errorf("the record written before the call: %+v", record)
	}
}

// A logical id that is not one path element is refused before any file is written, so a
// caller-supplied value cannot steer the outbox outside the state directory.
func TestDeliverSendRefusesAnUnsafeLogicalID(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	for _, id := range []string{"..", ".", "a/b", "a" + string(rune(0x5c)) + "b", "../escape"} {
		if _, err := Deliver(context.Background(), e, cfg, Message{LogicalID: id, Thread: "thread-1", Text: "hello"}); err == nil {
			t.Errorf("the logical id %q was accepted", id)
		}
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("the bridge was started for an unsafe logical id: %v", calls)
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("an unsafe logical id wrote %v", entries)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.StateDir), "escape.json")); !os.IsNotExist(err) {
		t.Errorf("a record escaped the state directory: %v", err)
	}
}

// The record a delivery reads back and rewrites keeps every field it carried: the identity, the
// digest, the target and the attempts. A reader that normalized, capped or dropped one of them
// would lose it here.
func TestDeliverPreservesEveryFieldOfTheRecordItRewrites(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, _ := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "accepted", "delivery": "turn_started"}},
	})
	cfg := deliverSendConfig(t, bridge)
	seed := deliverRecord{
		LogicalID: "m1", RequestID: "m1", TargetThread: "thread-1",
		MessageSHA256: "seed-digest", CreatedAt: "2026-10-06T00:00:00Z", State: deliverStatePending,
		Attempts: []deliverAttempt{{At: "2026-10-06T00:00:00Z", Class: deliverClassUnknown, ReceiptExcerpt: "seed"}},
	}
	if err := deliverSave(cfg, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	record := deliverRecordOf(t, cfg, "m1")
	if record.LogicalID != seed.LogicalID || record.TargetThread != seed.TargetThread ||
		record.MessageSHA256 != seed.MessageSHA256 || record.CreatedAt != seed.CreatedAt {
		t.Errorf("the rewrite dropped a field: %+v", record)
	}
	if len(record.Attempts) != 2 || record.Attempts[0] != seed.Attempts[0] {
		t.Errorf("the rewrite dropped or reordered the attempts: %+v", record.Attempts)
	}
}

// The ledger names the retry's request id before the retry goes out, so a process that dies during
// the retry reconciles the id it actually attempted instead of resending under the id the bridge
// already answered not_delivered for. The fake bridge reads the ledger at the moment of the retry,
// so the order is observed rather than assumed.
func TestDeliverRetriesPersistTheNewRequestIDBeforeTheRetry(t *testing.T) {
	e := deliverSendTestEnv(t)
	cfg := deliverSendConfig(t, "unused")
	bridge, log := deliverFakeBridge(t, []map[string]any{
		{"payload": map[string]any{"observation": "idle"}},
		{"payload": map[string]any{"status": "failed", "delivery": "not_delivered"}},
		{"payload": map[string]any{"observation": "idle"}},
		{"read": deliverOutboxPath(cfg, "m1")},
	})
	cfg.Bridge.Binary = bridge
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || out.Class != deliverClassUnknown || out.RequestID != "m1-r1" {
		t.Fatalf("Deliver: %q %q %v, want unknown under m1-r1", out.Class, out.RequestID, err)
	}
	if id, _ := out.Receipt["request_id"].(string); id != "m1-r1" {
		t.Errorf("the ledger named %q when the retry went out, want m1-r1", id)
	}
	if record := deliverRecordOf(t, cfg, "m1"); record.RequestID != "m1-r1" {
		t.Errorf("the ledger names %q after the retry, want m1-r1", record.RequestID)
	}
	var ids []string
	for _, call := range deliverSendCallsOf(t, log) {
		if call["tool"] == deliverToolSend {
			ids = append(ids, deliverSendRequestIDOf(t, call))
		}
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m1-r1" {
		t.Fatalf("the sends were %v, want m1 then m1-r1", ids)
	}
	// A restart reconciles the id the retry actually used, and sends nothing new.
	restarted, restartLog := deliverFakeBridge(t, []map[string]any{{"payload": map[string]any{"status": "accepted"}}})
	cfg.Bridge.Binary = restarted
	again, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"})
	if err != nil || again.Class != deliverClassAccepted {
		t.Fatalf("the restart: %q %v", again.Class, err)
	}
	if tools := deliverSendToolsOf(t, restartLog); len(tools) != 1 || tools[0] != deliverToolOperation {
		t.Fatalf("the restart saw %v, want only get_operation", tools)
	}
	if id := deliverSendRequestIDOf(t, deliverSendCallsOf(t, restartLog)[0]); id != "m1-r1" {
		t.Errorf("the restart reconciled %q, want m1-r1", id)
	}
}

// A message that cannot be transmitted faithfully is refused rather than mangled: JSON carries
// text as UTF-8, so invalid bytes would reach the bridge as replacement characters and the
// recipient would read a different message than the caller wrote.
func TestDeliverRefusesTextThatIsNotUTF8(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	out, err := Deliver(context.Background(), e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello \xff\xfe"})
	if err == nil || out.Class != "" {
		t.Fatalf("Deliver: %q %v, want a refusal", out.Class, err)
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("the bridge was started for an untransmittable message: %v", calls)
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("an untransmittable message wrote %v", entries)
	}
}

// A context that is already done starts nothing: no record and no bridge process.
func TestDeliverRefusesADoneContext(t *testing.T) {
	e := deliverSendTestEnv(t)
	bridge, log := deliverFakeBridge(t, nil)
	cfg := deliverSendConfig(t, bridge)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Deliver(ctx, e, cfg, Message{LogicalID: "m1", Thread: "thread-1", Text: "hello"}); err == nil {
		t.Error("a cancelled context was accepted")
	}
	if entries, err := os.ReadDir(filepath.Join(cfg.StateDir, "outbox")); err == nil && len(entries) != 0 {
		t.Errorf("a cancelled delivery wrote %v", entries)
	}
	if calls := deliverSendCallsOf(t, log); len(calls) != 0 {
		t.Errorf("a cancelled delivery started the bridge: %v", calls)
	}
}

// A ledger write that fails after the delivery settled is reported rather than swallowed: the
// class still describes what happened to the message, and the caller learns the evidence on disk
// is stale.
func TestDeliverReportsAFailedLedgerWrite(t *testing.T) {
	cfg := deliverSendConfig(t, "unused")
	if err := os.Symlink(t.TempDir(), filepath.Join(cfg.StateDir, "outbox")); err != nil {
		t.Skipf("this host cannot make a symlink: %v", err)
	}
	out, err := deliverSettled(cfg, deliverRecord{LogicalID: "m1", RequestID: "m1", State: deliverStateAccepted}, Outcome{Class: deliverClassAccepted, RequestID: "m1"})
	if err == nil {
		t.Fatal("a failed ledger write was swallowed")
	}
	if out.Class != deliverClassAccepted || out.RequestID != "m1" {
		t.Errorf("the outcome lost its meaning: %+v", out)
	}
}
