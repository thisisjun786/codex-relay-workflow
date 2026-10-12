package manage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The ledger bridge is the fake bridge of the post-evaluation tests of CRW-1272/CRW-1273 that keeps what
// the real bridge keeps: a request-id ledger (internal/bridge/ledger) whose entries carry a
// fingerprint of the arguments. A send under a request id the ledger holds with other arguments is a
// tool error (ErrConflict); the same arguments are answered with the stored receipt; get_operation
// reads an entry by id and answers "Unknown request_id" for an id it never recorded. Its state lives
// in a directory, because every delivery starts the bridge again. The mode file says what the next
// send does.

const (
	pumpLedgerDirEnv = "CRW_MANAGE_TEST_LEDGER_DIR"
	pumpLedgerRun    = "^TestPumpLedgerFakeBridge$"
)

const (
	// pumpLedgerDown: the send fails before any mutation, the bridge answers not_attempted / not_delivered
	// (the App Server cannot be reached) and the ledger keeps that receipt under the request id.
	pumpLedgerDown = "down"
	// pumpLedgerUp: the host takes the message.
	pumpLedgerUp = "up"
	// pumpLedgerError: the send fails in argument validation, a tool error before the ledger records anything.
	pumpLedgerError = "error"
	// pumpLedgerUnknown: the ledger keeps an in-progress receipt and the answer is undetermined.
	pumpLedgerUnknown = "unknown"
	// pumpLedgerLostReply: the host took the message and the ledger holds it accepted, but the reply is a tool error.
	pumpLedgerLostReply = "lostreply"
)

type pumpLedgerEntry struct {
	Fingerprint string         `json:"fingerprint"`
	Receipt     map[string]any `json:"receipt"`
}

// TestPumpLedgerFakeBridge is the ledger bridge when this binary is re-executed with a state directory.
func TestPumpLedgerFakeBridge(t *testing.T) {
	dir := os.Getenv(pumpLedgerDirEnv)
	if dir == "" {
		t.Skip("not the ledger bridge")
	}
	pumpLedgerServe(dir)
}

func pumpLedgerServe(dir string) {
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var message map[string]any
		if err := decoder.Decode(&message); err != nil {
			os.Exit(0)
		}
		id, request := message["id"]
		if method, _ := message["method"].(string); method == "initialize" {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": deliverProtocol, "capabilities": map[string]any{},
				"serverInfo": map[string]any{"name": "ledger-bridge", "version": "1"}}})
			continue
		}
		if !request {
			continue
		}
		params, _ := message["params"].(map[string]any)
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		deliverFakeLog(filepath.Join(dir, "calls.jsonl"), name, args)
		text, isError := pumpLedgerAnswer(dir, name, args)
		result := map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
		if isError {
			result["isError"] = true
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
}

func pumpLedgerLoad(dir string) map[string]pumpLedgerEntry {
	ledger := map[string]pumpLedgerEntry{}
	if raw, err := os.ReadFile(filepath.Join(dir, "ledger.json")); err == nil {
		_ = json.Unmarshal(raw, &ledger)
	}
	return ledger
}

func pumpLedgerStore(dir string, ledger map[string]pumpLedgerEntry) {
	raw, _ := json.Marshal(ledger)
	_ = os.WriteFile(filepath.Join(dir, "ledger.json"), raw, 0o600)
}

func pumpLedgerAnswer(dir, tool string, args map[string]any) (string, bool) {
	payload := func(v map[string]any) string { raw, _ := json.Marshal(v); return string(raw) }
	switch tool {
	case deliverToolActive:
		return payload(map[string]any{"observation": "idle"}), false
	case deliverToolOperation:
		requestID, _ := args["request_id"].(string)
		entry, ok := pumpLedgerLoad(dir)[requestID]
		if !ok {
			return "get_operation: " + deliverUnknownRequestID, true
		}
		return payload(entry.Receipt), false
	case deliverToolSend, deliverToolSteer:
		requestID, _ := args["request_id"].(string)
		rest := map[string]any{}
		for key, value := range args {
			if key != "request_id" {
				rest[key] = value
			}
		}
		raw, _ := json.Marshal(rest)
		sum := sha256.Sum256(raw)
		fingerprint := hex.EncodeToString(sum[:])
		ledger := pumpLedgerLoad(dir)
		if entry, ok := ledger[requestID]; ok {
			if entry.Fingerprint != fingerprint {
				return "Error: request_id " + requestID + " conflicts with an earlier request carrying other arguments", true
			}
			return payload(entry.Receipt), false
		}
		modeRaw, _ := os.ReadFile(filepath.Join(dir, "mode"))
		mode := strings.TrimSpace(string(modeRaw))
		var receipt map[string]any
		switch mode {
		case pumpLedgerDown:
			receipt = map[string]any{"status": "not_attempted", "delivery": "not_delivered"}
		case pumpLedgerUp:
			receipt = map[string]any{"status": "accepted", "delivery": "turn_started"}
		case pumpLedgerUnknown:
			receipt = map[string]any{"status": "in_progress_or_unknown", "retrySafe": false}
		case pumpLedgerLostReply:
			receipt = map[string]any{"status": "accepted", "delivery": "turn_started"}
			ledger[requestID] = pumpLedgerEntry{Fingerprint: fingerprint, Receipt: receipt}
			pumpLedgerStore(dir, ledger)
			return "Error: the receipt could not be saved", true
		default:
			return "Error: expected_settings is required", true
		}
		ledger[requestID] = pumpLedgerEntry{Fingerprint: fingerprint, Receipt: receipt}
		pumpLedgerStore(dir, ledger)
		return payload(receipt), false
	}
	return "Error: unknown tool " + tool, true
}

// pumpLedgerBridge writes the ledger bridge and returns its path and its state directory.
func pumpLedgerBridge(t *testing.T) (bridge, dir string) {
	t.Helper()
	dir = t.TempDir()
	bridge = filepath.Join(dir, "bridge")
	script := "#!/bin/sh\n" + pumpLedgerDirEnv + "=" + coreShellQuote(dir) + " exec " + coreShellQuote(os.Args[0]) +
		" -test.run " + coreShellQuote(pumpLedgerRun) + "\n"
	syscall.ForkLock.RLock()
	err := os.WriteFile(bridge, []byte(script), 0o700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return bridge, dir
}

func pumpLedgerSetMode(t *testing.T, dir, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
}

// pumpLedgerSends lists the sends the bridge saw: request id and thread, oldest first.
func pumpLedgerSends(t *testing.T, dir string) (ids, threads []string) {
	t.Helper()
	for _, call := range deliverSendCallsOf(t, filepath.Join(dir, "calls.jsonl")) {
		if call["tool"] != deliverToolSend && call["tool"] != deliverToolSteer {
			continue
		}
		ids = append(ids, deliverSendRequestIDOf(t, call))
		args, _ := call["args"].(map[string]any)
		thread, _ := args["thread_id"].(string)
		threads = append(threads, thread)
	}
	return ids, threads
}

// pumpLedgerHarness is one test's pump: a source whose events a test extends, the clock the rounds
// advance and the ledger bridge.
type pumpLedgerHarness struct {
	t      *testing.T
	e      *Env
	cfg    *Config
	now    *time.Time
	source *pumpTestSource
	dir    string
}

func newPumpLedgerHarness(t *testing.T) *pumpLedgerHarness {
	t.Helper()
	now := pumpTestNow
	h := &pumpLedgerHarness{t: t, now: &now}
	h.e = pumpTestEnv(t, h.now)
	h.cfg = pumpTestConfig(t, "")
	h.cfg.Settings.Management = coreSettings{Model: "m1", ReasoningEffort: "low"}
	h.source = &pumpTestSource{name: "fake", events: []pumpEvent{{ID: "e1", Kind: pumpKindQuestion, Text: "first"}}}
	pumpTestSwapSources(t, h.source)
	h.cfg.Bridge.Binary, h.dir = pumpLedgerBridge(t)
	return h
}

func (h *pumpLedgerHarness) round() pumpState {
	h.t.Helper()
	*h.now = h.now.Add(61 * time.Second)
	pumpRefusedRound(h.t, h.e, h.cfg)
	return pumpTestReadState(h.t, h.cfg)
}

func (h *pumpLedgerHarness) addEvent(id string) {
	h.source.events = append(h.source.events, pumpEvent{ID: id, Kind: pumpKindQuestion, Text: "event " + id})
}

// CRW-1273 (post-evaluation 2, d1): the ordinal of a refused management batch is a string Deliver's own
// retry ids can never produce. After an App Server outage Deliver has spent B, B-r1 and B-r2, each
// answered not_attempted / not_delivered and kept by the ledger; the next batch goes out under an id
// of its own and is delivered, whether the same events are pending (the ordinal) or a new event
// changed the batch.
func TestPumpManagementRefusalOrdinalNeverCollidesWithDeliverRetries(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  bool
	}{{"same events", false}, {"a new event arrives", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPumpLedgerHarness(t)
			pumpLedgerSetMode(t, h.dir, pumpLedgerDown)
			st := h.round()
			base := pumpBatchID(st.Pending)
			ids, _ := pumpLedgerSends(t, h.dir)
			if strings.Join(ids, " ") != base+" "+base+"-r1 "+base+"-r2" || st.Attempt != nil || st.MgmtRefused.Count != 1 {
				t.Fatalf("the outage round sent %v, state attempt %+v refused %+v, want B, B-r1, B-r2 and the refusal counted", ids, st.Attempt, st.MgmtRefused)
			}
			pumpLedgerSetMode(t, h.dir, pumpLedgerUp)
			for i := 0; i < 4; i++ {
				if tc.add && i == 1 {
					h.addEvent("e2")
				}
				st = h.round()
			}
			if !st.Sent["e1"] || (tc.add && !st.Sent["e2"]) || len(st.Pending) != 0 || st.Attempt != nil {
				ids, _ = pumpLedgerSends(t, h.dir)
				t.Fatalf("after the outage ended: sent %v pending %d attempt %+v (send ids %v), want every event delivered", st.Sent, len(st.Pending), st.Attempt, ids)
			}
			ids, _ = pumpLedgerSends(t, h.dir)
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] {
					t.Errorf("request id %s was sent more than once: %v", id, ids)
				}
				seen[id] = true
				if len(id) > 128 {
					t.Errorf("request id %q is longer than 128 characters", id)
				}
			}
			if !tc.add && !strings.HasSuffix(ids[len(ids)-1], "-m1") {
				t.Errorf("the batch after the refusal went out under %s, want the -m1 ordinal", ids[len(ids)-1])
			}
		})
	}
}

// CRW-1273 (post-evaluation 2, d2): management_thread changes while an attempt is frozen with an undetermined
// outcome. The thread of the frozen attempt's outbox record is no longer the configured one, so the
// delivery core refuses it locally in every round. The pump reconciles that request id with
// get_operation (the bridge's lookup does not depend on the thread), settles the attempt from the
// answer and goes on: events collected meanwhile reach the new thread.
func TestPumpFrozenAttemptSurvivesAManagementThreadChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		// mode is what the first send does; a lost reply leaves the frozen attempt accepted in the ledger.
		mode     string
		add      bool
		wantSent []string // events sent to the old thread by the first attempt, as the ledger holds them
	}{
		{"the first send never reached the ledger, a new event arrives", pumpLedgerError, true, nil},
		{"the first send never reached the ledger, no new event", pumpLedgerError, false, nil},
		{"the ledger holds the first send accepted", pumpLedgerLostReply, true, []string{"e1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newPumpLedgerHarness(t)
			pumpLedgerSetMode(t, h.dir, tc.mode)
			st := h.round()
			if st.Attempt == nil {
				t.Fatal("the undetermined send did not freeze the attempt")
			}
			h.cfg.ManagementThread = "mgmt-thread-2"
			pumpLedgerSetMode(t, h.dir, pumpLedgerUp)
			for i := 0; i < 5; i++ {
				if tc.add && i == 1 {
					h.addEvent("e2")
				}
				st = h.round()
			}
			if !st.Sent["e1"] || (tc.add && !st.Sent["e2"]) || len(st.Pending) != 0 || st.Attempt != nil {
				t.Fatalf("after the thread changed: sent %v pending %d attempt %+v, want every event delivered", st.Sent, len(st.Pending), st.Attempt)
			}
			_, threads := pumpLedgerSends(t, h.dir)
			for i, thread := range threads {
				if i == 0 {
					continue
				}
				if thread != "mgmt-thread-2" {
					t.Errorf("send %d went to %s, want the configured mgmt-thread-2 (threads %v)", i, thread, threads)
				}
			}
			if tc.wantSent == nil && len(threads) < 2 {
				t.Errorf("the events never reached the new thread: threads %v", threads)
			}
			if tc.wantSent != nil && !tc.add && len(threads) != 1 {
				t.Errorf("an accepted frozen attempt was sent again: threads %v", threads)
			}
		})
	}
}

// CRW-1273 (post-evaluation 2, d2): when get_operation cannot place the frozen attempt either (the ledger keeps an
// in-progress receipt), nothing is sent and the attempt stays frozen: a message that may be on the
// host is never sent a second time.
func TestPumpFrozenAttemptStaysWhenTheLedgerCannotPlaceItAfterAThreadChange(t *testing.T) {
	h := newPumpLedgerHarness(t)
	pumpLedgerSetMode(t, h.dir, pumpLedgerUnknown)
	st := h.round()
	if st.Attempt == nil {
		t.Fatal("the undetermined send did not freeze the attempt")
	}
	frozen := *st.Attempt
	h.cfg.ManagementThread = "mgmt-thread-2"
	pumpLedgerSetMode(t, h.dir, pumpLedgerUp)
	h.addEvent("e2")
	before := len(mustSends(t, h.dir))
	for i := 0; i < 3; i++ {
		st = h.round()
	}
	if st.Attempt == nil || st.Attempt.LogicalID != frozen.LogicalID || st.Sent["e1"] || st.Sent["e2"] {
		t.Fatalf("attempt %+v sent %v, want the attempt %s kept and nothing sent", st.Attempt, st.Sent, frozen.LogicalID)
	}
	if after := len(mustSends(t, h.dir)); after != before {
		t.Fatalf("%d sends after the thread change, want none", after-before)
	}
}

func mustSends(t *testing.T, dir string) []string {
	t.Helper()
	ids, _ := pumpLedgerSends(t, dir)
	return ids
}
