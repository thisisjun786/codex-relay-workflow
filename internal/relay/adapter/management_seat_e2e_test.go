package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1333 (design CRW-1314 §1-2), end to end over the scripted App Server: management seats per
// thread, the lead edges their registrations write, the lead's messages staged on the supervisor
// channel, and the daemon's own tick opening a turn in the idle management thread they are for. The
// seats and edges are written by the relay's own command lines; the transport is the real adapter
// over the fake host's socket.

const (
	mgmtA, mgmtB, mgmtNew, storeSeat = "01mgmtA", "01mgmtB", "01mgmtNew", "01storeseat"
	leadA, leadB, leadC              = "01leadA", "01leadB", "01leadC"
)

// mgmtWorld is one temporary store, the scripted host and a daemon over both.
type mgmtWorld struct {
	t     *testing.T
	ctx   context.Context
	state string
	s     *store.Store
	host  *fakehost.Server
	d     *daemon.Daemon
	c     *supervisor.Channel
	a     *Adapter
	clock *delivery.FakeClock
	mu    sync.Mutex
	turns map[string][]string
}

func newMgmtWorld(t *testing.T) *mgmtWorld {
	t.Helper()
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"supervisor":{"expectation":"record"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
	t.Setenv("CODEX_THREAD_ID", "")
	// the role policy is read once per process: a test that ran before with another one leaves its snapshot
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	w := &mgmtWorld{t: t, ctx: context.Background(), state: filepath.Join(t.TempDir(), "state"), turns: map[string][]string{}}
	w.host = fakehost.Start(t)
	w.scriptHost()
	return w
}

// relay runs one relay command line against the world's store and decodes its answer.
func (w *mgmtWorld) relay(argv ...string) map[string]any {
	w.t.Helper()
	var stdout, stderr bytes.Buffer
	code := dispatch.Execute(w.ctx, "codex-session-relay", append([]string{"--state", w.state}, argv...), &stdout, &stderr)
	if code != 0 {
		w.t.Fatalf("%v: exit %d stdout %q stderr %q", argv, code, stdout.String(), stderr.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil {
		w.t.Fatalf("%v answered %q: %v", argv, stdout.String(), err)
	}
	return answer
}

// open opens the store the commands wrote, records every recipient's authorized settings, and
// starts the adapter and the daemon over it.
func (w *mgmtWorld) open(recipients ...string) {
	w.t.Helper()
	s, err := openStore(w.ctx, filepath.Join(w.state, "relay.sqlite3"), w.host.SocketPath)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = s.Close() })
	w.s = s
	settings, err := json.Marshal(authorized())
	if err != nil {
		w.t.Fatal(err)
	}
	for _, task := range recipients {
		w.settings(task, string(settings))
	}
	client := appserver.New(w.host.SocketPath, appserver.DefaultBounds)
	l, err := ledger.OpenWithOptions(filepath.Join(w.t.TempDir(), "operations.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		w.t.Fatal(err)
	}
	a := New(Options{RPC: client, Ledger: l, Policy: execution.Policy{}})
	w.t.Cleanup(func() { _ = a.Close() })
	w.a = a
	w.clock = &delivery.FakeClock{T: 1700000000}
	w.c = &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: "codex-session-relay", Socket: w.host.SocketPath}
	w.d = daemon.New(s, a, w.clock, w.c)
}

func (w *mgmtWorld) settings(task, settings string) {
	w.t.Helper()
	if _, err := w.s.DB.ExecContext(w.ctx, "INSERT OR REPLACE INTO authorized_settings(task_id,settings,source,recorded_at) VALUES (?,?,'creation_result','2023-11-14T22:13:20.000000+00:00')", task, settings); err != nil {
		w.t.Fatal(err)
	}
}

// scriptHost answers every thread as idle and loaded, and records the text of each turn it starts,
// by thread.
func (w *mgmtWorld) scriptHost() {
	w.host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}, "canAcceptDirectInput": true}}}
	})
	w.host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"status": nil}}})
	// Every thread of the world is loaded and not archived.
	w.host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		data := []any{}
		if params["archived"] == false {
			for _, id := range []string{mgmtA, mgmtB, mgmtNew, storeSeat, leadA, leadB, leadC, ownerA, ownerB, ownerC} {
				data = append(data, map[string]any{"id": id})
			}
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	w.host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	w.host.Handle("turn/start", func(raw json.RawMessage) fakehost.Reply {
		var params struct {
			ThreadID string           `json:"threadId"`
			Input    []map[string]any `json:"input"`
		}
		_ = json.Unmarshal(raw, &params)
		w.mu.Lock()
		defer w.mu.Unlock()
		text := ""
		for _, item := range params.Input {
			if s, ok := item["text"].(string); ok {
				text += s
			}
		}
		w.turns[params.ThreadID] = append(w.turns[params.ThreadID], text)
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-" + params.ThreadID + "-" + strconv.Itoa(len(w.turns[params.ThreadID]))}}}
	})
	// The turns and items a thread holds are the turns this host started in it, so a send that
	// reads back what it started finds it.
	w.host.Handle("thread/turns/list", func(raw json.RawMessage) fakehost.Reply {
		thread := threadOf(raw)
		w.mu.Lock()
		defer w.mu.Unlock()
		data := []any{}
		for i := range w.turns[thread] {
			data = append(data, map[string]any{"id": "turn-" + thread + "-" + strconv.Itoa(i+1), "status": "completed", "startedAt": 1700000000})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	w.host.Handle("thread/items/list", func(raw json.RawMessage) fakehost.Reply {
		thread := threadOf(raw)
		w.mu.Lock()
		defer w.mu.Unlock()
		data := []any{}
		for i, text := range w.turns[thread] {
			data = append(data, map[string]any{"turnId": "turn-" + thread + "-" + strconv.Itoa(i+1), "item": map[string]any{"id": "item-" + strconv.Itoa(i+1), "type": "userMessage", "text": text}})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
}

// threadOf is the threadId a request names.
func threadOf(raw json.RawMessage) string {
	var params struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(raw, &params)
	return params.ThreadID
}

// stage stages one lead message through the channel's direct staging.
func (w *mgmtWorld) stage(kind, initiative, subject, summary, text string) map[string]any {
	w.t.Helper()
	answer, err := w.c.StageDirect(w.ctx, supervisor.DirectMessage{Kind: kind, Relationship: "initiative:" + initiative, Subject: subject, Summary: summary, Text: text}, nil)
	if err != nil {
		w.t.Fatalf("staging %s for %s: %v", kind, initiative, err)
	}
	return answer
}

// tick runs daemon ticks, five seconds apart, until nothing is left to send or n ticks ran.
func (w *mgmtWorld) tick(n int) {
	w.t.Helper()
	for range n {
		report, err := w.d.Tick(w.ctx)
		if err != nil {
			w.t.Fatal(err)
		}
		if len(report.Notes) > 0 {
			w.t.Logf("tick at %.0f: %q", w.clock.T, report.Notes)
		}
		w.clock.T += 5
	}
}

// dump is what the store says about the messages, for a failure.
func (w *mgmtWorld) dump() string {
	w.t.Helper()
	rows, err := w.s.All(w.ctx, "SELECT kind, subject, detail FROM journal WHERE kind LIKE 'supervisor%' ORDER BY seq")
	if err != nil {
		return err.Error()
	}
	messages, _ := w.s.All(w.ctx, "SELECT message_id, state, hold_reason, next_eligible_at, attempt_count, recipient_task_id FROM supervisor_messages")
	attempts, _ := w.s.All(w.ctx, "SELECT * FROM supervisor_attempts")
	methods := []string{}
	for _, r := range w.host.Requests() {
		methods = append(methods, r.Method)
	}
	return fmt.Sprintf("journal %v\nmessages %v\nattempts %v\nhost %v", rows, messages, attempts, methods)
}

// turnsOf is the texts of the turns the host started in thread.
func (w *mgmtWorld) turnsOf(thread string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.turns[thread]...)
}

// messageState is the state of one supervisor message.
func (w *mgmtWorld) messageState(id string) string {
	w.t.Helper()
	row, err := w.s.SupervisorMessage(w.ctx, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return row.State
}

// TestManagementSeatE2ETwoSessionsEachHearOnlyTheirOwnLead: two management sessions register their
// own seats and their own leads; each lead's message opens a turn in its own management thread and
// in no other, and the transport's record says dispatched.
func TestManagementSeatE2ETwoSessionsEachHearOnlyTheirOwnLead(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtB, "--task", mgmtB, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope", "INI-A", "--task", leadA, "--host", "host", "--creator", mgmtA)
	w.relay("linkage-bind", "--role", "supervisor", "--scope", "INI-B", "--task", leadB, "--host", "host", "--creator", mgmtB)
	w.open(mgmtA, mgmtB)
	decision := w.stage(supervisor.KindLeadDecisionRequest, "INI-A", "lrp_a1", "approve the release-gate exception for P-CRW-140", "lead A asks for the gate exception")
	report := w.stage(supervisor.KindLeadReport, "INI-B", "lrp_b1", "delivery scope complete", "lead B's scope is complete")
	if decision["recipient"] != mgmtA || report["recipient"] != mgmtB {
		t.Fatalf("staged to %v and %v, want %s and %s", decision["recipient"], report["recipient"], mgmtA, mgmtB)
	}
	w.tick(4)
	a, b := w.turnsOf(mgmtA), w.turnsOf(mgmtB)
	if len(a) != 1 || !strings.Contains(a[0], "lead A asks for the gate exception") || strings.Contains(a[0], "lead B") ||
		!strings.HasPrefix(a[0], "[codex-session-relay] management message from lead "+leadA) {
		t.Fatalf("management A's turns %q\n%s", a, w.dump())
	}
	if len(b) != 1 || !strings.Contains(b[0], "lead B's scope is complete") || strings.Contains(b[0], "lead A") {
		t.Fatalf("management B's turns %q", b)
	}
	if len(w.turnsOf(leadA))+len(w.turnsOf(leadB)) != 0 {
		t.Fatal("a lead was sent its own message")
	}
	for _, id := range []string{decision["messageId"].(string), report["messageId"].(string)} {
		if state := w.messageState(id); state != "dispatched" {
			t.Fatalf("message %s is %s after the ticks, want it sent", id, state)
		}
	}
}

// TestManagementSeatE2EHandoverMovesTheSeat: a new management session takes the old seat by
// handover; the record is the archived binding and the scope_handover row, and the lead's next
// message opens a turn in the new owner's thread.
func TestManagementSeatE2EHandoverMovesTheSeat(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope", "INI-A", "--task", leadA, "--host", "host", "--creator", mgmtA)
	moved := w.relay("linkage-handover", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--expect-task", mgmtA,
		"--task", mgmtNew, "--host", "host", "--evidence", "the management session was replaced", "--actor", "test")
	if moved["taskId"] != mgmtNew || moved["handoverNote"] != "the management session was replaced" {
		t.Fatalf("handover answered %v", moved)
	}
	w.open(mgmtA, mgmtNew)
	var handovers int
	if err := w.s.DB.QueryRowContext(w.ctx, "SELECT COUNT(*) FROM journal WHERE kind = 'scope_handover'").Scan(&handovers); err != nil || handovers != 1 {
		t.Fatalf("%d scope_handover rows (%v)", handovers, err)
	}
	staged := w.stage(supervisor.KindLeadBlocked, "INI-A", "lrp_a2", "blocked on the review gate", "lead A is blocked")
	if staged["recipient"] != mgmtNew {
		t.Fatalf("staged to %v, want the new owner %s", staged["recipient"], mgmtNew)
	}
	w.tick(3)
	if got := w.turnsOf(mgmtNew); len(got) != 1 || !strings.Contains(got[0], "lead A is blocked") {
		t.Fatalf("the new owner's turns %q\n%s", got, w.dump())
	}
	if got := w.turnsOf(mgmtA); len(got) != 0 {
		t.Fatalf("the old owner was sent %q", got)
	}
}

// TestManagementSeatE2EStoreSeatFallback: an initiative with no management edge is heard by the
// single store seat.
func TestManagementSeatE2EStoreSeatFallback(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "store", "--scope", "store", "--task", storeSeat, "--host", "host")
	w.relay("linkage-bind", "--role", "supervisor", "--scope", "INI-C", "--task", leadC, "--host", "host")
	w.open(storeSeat)
	staged := w.stage(supervisor.KindLeadReport, "INI-C", "lrp_c1", "scope complete", "lead C reports")
	if staged["recipient"] != storeSeat || staged["recipientScopeKind"] != "store" {
		t.Fatalf("staged %v, want the store seat", staged)
	}
	w.tick(3)
	if got := w.turnsOf(storeSeat); len(got) != 1 || !strings.Contains(got[0], "lead C reports") {
		t.Fatalf("the store seat's turns %q\n%s", got, w.dump())
	}
}
