package managed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/projectlock"
)

// stepClock is the engine's clock and the host's: a test moves it past the grace period instead of waiting.
type stepClock struct{ now time.Time }

func (c *stepClock) ISO() string { return c.now.UTC().Format("2006-01-02T15:04:05.000000") + "+00:00" }

type appThread struct {
	id, name, status string
	createdAt        int64
	turns            []string
}

// scriptedApp is the managedFake with an App Server behind it: it keeps the threads a creation left, answers the reads
// the reconciliation makes, and lets a test choose what each CreateThread did and what each standby send answered.
type scriptedApp struct {
	*managedFake
	clock        *stepClock
	cwd          string
	outcomes     []string
	threads      map[string]*appThread
	order        []string
	initialTurns []string
	creates      []string
	sends        []string
	calls        []string
	named        []string
	failures     map[string]error
	noTurn       map[string]string
	sendReceipts []map[string]any
	age          time.Duration // how long ago a creation says it began
	endless      bool          // the loaded listing never ends
}

func (h *scriptedApp) addThread() string { return h.addThreadAs(fmt.Sprintf("t-%d", len(h.order)+1)) }

func (h *scriptedApp) addThreadAs(id string) string {
	h.threads[id] = &appThread{id: id, status: "idle", createdAt: h.clock.now.Unix(), turns: append([]string(nil), h.initialTurns...)}
	h.order = append(h.order, id)
	return id
}

func (h *scriptedApp) creationRecord(id string) map[string]any {
	created := map[string]any{}
	for k, v := range h.settings {
		created[k] = v
	}
	created["thread"] = map[string]any{"id": id, "environments": created["environments"]}
	delete(created, "environments")
	return created
}

func (h *scriptedApp) CreateThread(_ context.Context, in CreateThreadRequest) (map[string]any, error) {
	h.creates = append(h.creates, in.RequestID)
	h.managedFake.created++
	outcome := "accept"
	if len(h.outcomes) > 0 {
		outcome, h.outcomes = h.outcomes[0], h.outcomes[1:]
	}
	started := float64(h.clock.now.Add(-h.age).Unix())
	var receipt map[string]any
	switch outcome {
	case "accept":
		id := h.addThread()
		receipt = map[string]any{"status": "accepted", "threadId": id, "turnId": "standby", "creation": h.creationRecord(id)}
	case "lost": // the answer to thread/start was lost and the host did nothing
		receipt = map[string]any{"status": "outcome_unknown", "attemptedEffects": []any{"thread/start"}}
	case "lost-applied": // the answer was lost and the host created the thread
		h.addThread()
		receipt = map[string]any{"status": "outcome_unknown", "attemptedEffects": []any{"thread/start"}}
	case "name-timeout":
		id := h.addThread()
		receipt = map[string]any{"status": "outcome_unknown", "threadId": id, "creation": h.creationRecord(id), "attemptedEffects": []any{"thread/start", "thread/name/set"}}
	case "turn-timeout":
		id := h.addThread()
		receipt = map[string]any{"status": "outcome_unknown", "threadId": id, "creation": h.creationRecord(id), "attemptedEffects": []any{"thread/start", "thread/name/set", "turn/start"}}
	case "saved-title": // the process died after the title was saved: the bridge keeps no attemptedEffects yet
		id := h.addThread()
		receipt = map[string]any{"status": "in_progress_or_unknown", "threadId": id, "creation": h.creationRecord(id), "title": "Verify"}
	default:
		panic("unknown outcome " + outcome)
	}
	receipt["startedAt"], receipt["updatedAt"] = started, started+20
	h.operations[in.RequestID] = receipt
	return receipt, nil
}

func (h *scriptedApp) SendMessage(ctx context.Context, in SendRequest) (map[string]any, error) {
	h.sends = append(h.sends, in.RequestID)
	h.calls = append(h.calls, "send "+in.RequestID[:16])
	standby := strings.HasPrefix(in.RequestID, "managed-standby-")
	if standby && len(h.sendReceipts) > 0 {
		receipt := h.sendReceipts[0]
		h.sendReceipts = h.sendReceipts[1:]
		h.operations[in.RequestID] = receipt
		h.managedFake.sent++
		return receipt, nil
	}
	receipt, err := h.managedFake.SendMessage(ctx, in)
	if err == nil && receipt["status"] == "accepted" {
		if t := h.threads[in.ThreadID]; t != nil {
			t.turns, t.status = append(t.turns, pyjson.Text(receipt["turnId"])), "idle"
		}
		if standby {
			receipt["resumed"] = h.creationRecord(in.ThreadID)
		}
	}
	return receipt, err
}

func (h *scriptedApp) failure(method, thread string) error {
	if err := h.failures[method+"|"+thread]; err != nil {
		return err
	}
	return h.failures[method+"|"]
}

func (h *scriptedApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	h.calls = append(h.calls, method)
	id := pyjson.Text(params["threadId"])
	if err := h.failure(method, id); err != nil {
		return nil, err
	}
	thread := h.threads[id]
	switch method {
	case "thread/loaded/list":
		if h.endless {
			return map[string]any{"data": []any{}, "nextCursor": "more"}, nil
		}
		data := []any{}
		for _, known := range h.order {
			data = append(data, known)
		}
		return map[string]any{"data": data}, nil
	case "thread/list":
		data := []any{}
		for _, known := range h.order {
			if params["archived"] != true {
				data = append(data, map[string]any{"id": known})
			}
		}
		return map[string]any{"data": data}, nil
	case "thread/read":
		if thread == nil {
			return nil, errors.New("thread/read: thread not found")
		}
		preview := ""
		if len(thread.turns) > 0 {
			preview = "first message"
		}
		var name any
		if thread.name != "" {
			name = thread.name
		}
		return map[string]any{"thread": map[string]any{"id": id, "cwd": h.cwd, "createdAt": thread.createdAt, "name": name, "model": h.settings["model"], "reasoningEffort": h.settings["reasoningEffort"],
			"preview": preview, "ephemeral": false, "parentThreadId": nil, "status": map[string]any{"type": thread.status}, "canAcceptDirectInput": true}}, nil
	case "thread/turns/list":
		if thread == nil {
			return nil, errors.New("thread/turns/list: thread not found")
		}
		if text := h.noTurn[id]; text != "" {
			return nil, errors.New(text)
		}
		if len(thread.turns) == 0 {
			return nil, errors.New("thread/turns/list: not materialized yet; unavailable before first user message")
		}
		rows := []any{}
		for _, turn := range thread.turns {
			rows = append(rows, map[string]any{"id": turn, "status": "completed"})
		}
		return map[string]any{"data": rows}, nil
	case "thread/name/set":
		if thread == nil {
			return nil, errors.New("thread/name/set: thread not found")
		}
		thread.name = pyjson.Text(params["name"])
		h.named = append(h.named, id)
		return map[string]any{}, nil
	}
	return h.managedFake.HostCall(ctx, method, params)
}

type reconcileKit struct {
	t     *testing.T
	start *Start
	host  *scriptedApp
	clock *stepClock
	raw   []byte
}

// newReconcileKit builds a managed start over a scripted App Server whose CreateThread answers the given outcomes in turn and then accepts.
func newReconcileKit(t *testing.T, outcomes ...string) *reconcileKit {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw := requestFixture(t)
	req, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	settings := pyjson.Map(pyjson.Map(req["child"])["settings"])
	clock := &stepClock{now: time.Date(2026, 10, 3, 8, 22, 0, 0, time.UTC)}
	fake := &managedFake{operations: map[string]map[string]any{}, settings: settings, ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	host := &scriptedApp{managedFake: fake, clock: clock, cwd: pyjson.Text(settings["cwd"]), outcomes: outcomes, threads: map[string]*appThread{}, failures: map[string]error{}, noTurn: map[string]string{}}
	start := &Start{Store: s, Adapter: host, Now: clock.ISO, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir, Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	return &reconcileKit{t: t, start: start, host: host, clock: clock, raw: raw}
}

func (k *reconcileKit) runErr() (map[string]any, error) {
	result, err := k.start.Run(context.Background(), k.raw)
	if err != nil {
		return nil, err
	}
	got := map[string]any{}
	for _, f := range result {
		got[f.Key] = f.Value
	}
	return got, nil
}

func (k *reconcileKit) run() map[string]any {
	k.t.Helper()
	got, err := k.runErr()
	if err != nil {
		k.t.Fatal(err)
	}
	return got
}

// recon is the answer's creationReconciliation as a map; empty when the answer carries none.
func recon(got map[string]any) map[string]any {
	out := map[string]any{}
	object, _ := got["creationReconciliation"].(contract.OrderedObject)
	for _, f := range object {
		out[f.Key] = f.Value
	}
	return out
}

func (k *reconcileKit) expect(got map[string]any, state, reason, why string) {
	k.t.Helper()
	if got["state"] != state || pyjson.Text(got["reason"]) != reason || pyjson.Text(recon(got)["state"]) != why {
		k.t.Fatalf("want %s/%s/%s, got %v/%v/%v (%v)", state, reason, why, got["state"], got["reason"], recon(got)["state"], recon(got))
	}
}

func (k *reconcileKit) effects(created, sent int) {
	k.t.Helper()
	if k.host.created != created || k.host.sent != sent {
		k.t.Fatalf("host effects created/sent %d/%d, want %d/%d", k.host.created, k.host.sent, created, sent)
	}
}

// A thread/start whose answer was lost and which left no thread is created again, once, after the grace period, under the same request.
func TestReconcileNoThreadWaitsThenCreatesAgain(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "lost", "accept")
	for range 2 {
		got := k.run()
		k.expect(got, "incomplete", "creation_unknown", "pending")
		if recon(got)["repeatAfter"] == nil {
			t.Fatalf("a pending answer says when to repeat: %v", recon(got))
		}
	}
	k.effects(1, 0)
	k.clock.now = k.clock.now.Add(3 * time.Minute)
	got := k.run()
	k.expect(got, "admitted", "", "recreated")
	k.effects(2, 1)
	if len(k.host.creates) != 2 || k.host.creates[0] == k.host.creates[1] || !strings.HasPrefix(k.host.creates[1], "managed-create-") || got["childTaskId"] != "t-1" {
		t.Fatalf("the second creation is a new operation of the same request: %v child %v", k.host.creates, got["childTaskId"])
	}
	// attempt 0's thread shows up after all: it is neither adopted nor sent to, and a repeat creates nothing.
	k.host.addThread()
	again := k.run()
	k.expect(again, "admitted", "", "")
	if again["childTaskId"] != "t-1" {
		t.Fatalf("the registered child changed: %v", again["childTaskId"])
	}
	k.effects(2, 1)
}

// A thread the creation left behind is continued under the same request, never replaced.
func TestReconcileAdoptsAThreadTheCreationLeftBehind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, outcome string
		later         bool
	}{
		{"thread/start applied and its answer lost", "lost-applied", false},
		{"thread appears inside the grace period", "lost", true},
		{"thread/name/set timed out", "name-timeout", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k := newReconcileKit(t, c.outcome)
			if c.later {
				k.expect(k.run(), "incomplete", "creation_unknown", "pending")
				k.host.addThread()
			}
			got := k.run()
			k.expect(got, "admitted", "", "adopted")
			k.effects(1, 2)
			if got["childTaskId"] != "t-1" || len(k.host.creates) != 1 || len(k.host.named) != 1 {
				t.Fatalf("one child, the thread the creation left: %v creates %v named %v", got["childTaskId"], k.host.creates, k.host.named)
			}
			send, name := -1, -1
			for i, call := range k.host.calls {
				if call == "send managed-standby-" && send < 0 {
					send = i
				}
				if call == "thread/name/set" {
					name = i
				}
			}
			if send < 0 || name < send {
				t.Fatalf("the title is set after the standby send is accepted: %v", k.host.calls)
			}
			k.expect(k.run(), "admitted", "", "adopted")
			k.effects(1, 2)
			if len(k.host.named) != 1 {
				t.Fatalf("a replay does not name the thread again: %v", k.host.named)
			}
		})
	}
}

// Every way a creation stays unknown ends in an answer that says why, with nothing created and nothing sent.
func TestReconcileStopsAndSaysWhy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		outcomes []string
		setup    func(*reconcileKit)
		why      string
		detail   string
	}{
		{"the loaded listing fails", []string{"lost"}, func(k *reconcileKit) { k.host.failures["thread/loaded/list|"] = errors.New("host unavailable") }, "unobservable", "host unavailable"},
		{"the creation time is not in the receipt", nil, func(k *reconcileKit) {
			create, _ := OperationIDs("managed-1")
			k.host.operations[create] = map[string]any{"status": "outcome_unknown"}
		}, "unobservable", "time"},
		{"the loaded listing never ends", []string{"lost"}, func(k *reconcileKit) { k.host.endless = true }, "unobservable", "to its end"},
		{"two threads fit", []string{"lost"}, func(k *reconcileKit) { k.host.addThread(); k.host.addThread() }, "ambiguous", "2"},
		{"the thread has a turn", []string{"name-timeout"}, func(k *reconcileKit) { k.host.initialTurns = []string{"x"} }, "thread_has_turn", "t-1"},
		{"the standby turn/start was attempted", []string{"turn-timeout"}, nil, "standby_turn_unknown", "turn/start"},
		{"the process died after the title was saved", []string{"saved-title"}, nil, "standby_turn_unknown", "title"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			k := newReconcileKit(t, c.outcomes...)
			if c.setup != nil {
				c.setup(k)
			}
			for range 2 {
				got := k.run()
				k.expect(got, "incomplete", "creation_unknown", c.why)
				if !strings.Contains(pyjson.Text(recon(got)["detail"]), c.detail) {
					t.Fatalf("detail %q lacks %q", recon(got)["detail"], c.detail)
				}
			}
			if k.host.sent != 0 || k.host.created > 1 || len(k.host.named) != 0 {
				t.Fatalf("a stopped start effects: created %d sent %d named %v", k.host.created, k.host.sent, k.host.named)
			}
		})
	}
}

func TestReconcileStopsAfterThreeCreations(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "lost", "lost", "lost")
	k.expect(k.run(), "incomplete", "creation_unknown", "pending")
	for range 2 {
		k.clock.now = k.clock.now.Add(3 * time.Minute)
		k.expect(k.run(), "incomplete", "creation_unknown", "recreated")
	}
	k.clock.now = k.clock.now.Add(3 * time.Minute)
	k.expect(k.run(), "incomplete", "creation_unknown", "attempts_exhausted")
	k.effects(3, 0)
}

// A standby send whose outcome is unknown is not sent again (I-473 for a send).
func TestReconcileUnknownStandbySendIsNotRepeated(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "name-timeout")
	k.host.sendReceipts = []map[string]any{{"status": "outcome_unknown", "threadId": "t-1"}}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	if len(k.host.sends) != 1 {
		t.Fatalf("an unknown send was repeated: sends %v", k.host.sends)
	}
}

// A standby send the guard refused (not_attempted) is sent again under the same id once the guard lets it through.
func TestReconcileNotAttemptedStandbySendIsRetried(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "name-timeout")
	k.host.sendReceipts = []map[string]any{{"status": "not_attempted", "retrySafe": true, "threadId": "t-1", "attemptedEffects": []any{}}}
	k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
	k.expect(k.run(), "admitted", "", "adopted")
	if len(k.host.sends) != 3 || k.host.sends[0] != k.host.sends[1] || len(k.host.creates) != 1 {
		t.Fatalf("the refused send is repeated under its own id: %v", k.host.sends)
	}
}

// A name that cannot be set after the standby is accepted is the caller's error; the repeat names the thread without sending again.
func TestReconcileNameFailureIsRetriedWithoutResending(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "name-timeout")
	k.host.failures["thread/name/set|t-1"] = errors.New("thread/name/set: timeout")
	if _, err := k.runErr(); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("the name failure is returned: %v", err)
	}
	delete(k.host.failures, "thread/name/set|t-1")
	got := k.run()
	k.expect(got, "admitted", "", "adopted")
	standby := 0
	for _, id := range k.host.sends {
		if strings.HasPrefix(id, "managed-standby-") {
			standby++
		}
	}
	if standby != 1 || len(k.host.named) != 1 {
		t.Fatalf("one standby send and one name: sends %v named %v", k.host.sends, k.host.named)
	}
}

func hostRefusal(text, status string, extra map[string]any) map[string]any {
	receipt := map[string]any{"status": "failed", "threadId": "t-1", "error": text, "rpcError": map[string]any{"code": -32600, "message": text}}
	if status != "" {
		receipt["statusBeforeResume"] = status
	}
	for k, v := range extra {
		receipt[k] = v
	}
	return receipt
}

// A thread the receipt names that cannot be read or resumed stops the start with its reason: nothing is created and nothing more is sent, however many times it is repeated.
func TestReconcileStopsWhenANamedThreadCannotBeReadOrResumed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ method, text string }{
		{"thread/read", "thread/read: thread not found"},
		{"thread/read", "thread/read: no rollout found for thread id t-1"},
		{"thread/turns/list", "thread/turns/list: missing source rollout"},
	} {
		t.Run(c.text, func(t *testing.T) {
			t.Parallel()
			k := newReconcileKit(t, "name-timeout", "accept")
			k.host.failures[c.method+"|t-1"] = errors.New(c.text)
			for range 2 {
				got := k.run()
				k.expect(got, "incomplete", "creation_unknown", "unobservable")
				if !strings.Contains(pyjson.Text(recon(got)["detail"]), c.text) {
					t.Fatalf("detail %q lacks %q", recon(got)["detail"], c.text)
				}
			}
			k.effects(1, 0)
		})
	}
	for _, refusal := range []map[string]any{
		hostRefusal("thread/resume: no rollout found for thread id t-1", "notLoaded", nil),
		hostRefusal("thread/read: thread not found", "", nil),
	} {
		t.Run(pyjson.Text(refusal["error"]), func(t *testing.T) {
			t.Parallel()
			k := newReconcileKit(t, "name-timeout", "accept")
			k.host.sendReceipts = []map[string]any{refusal}
			for range 2 {
				k.expect(k.run(), "incomplete", "creation_unknown", "adopted")
			}
			k.effects(1, 1)
		})
	}
}

// inProject makes the request a project's and binds its parent, as linkage-bind --role parent does.
func (k *reconcileKit) inProject() {
	k.t.Helper()
	var request map[string]any
	if err := json.Unmarshal(k.raw, &request); err != nil {
		k.t.Fatal(err)
	}
	request["projectKey"] = scopeProject
	raw, err := json.Marshal(request)
	if err != nil {
		k.t.Fatal(err)
	}
	k.raw = raw
	reg := &registry.Registry{Store: k.start.Store, Now: k.clock.ISO}
	if _, err := reg.BindScopeAs(context.Background(), "parent", scopeProject, registry.Endpoint{TaskID: "parent", HostID: "host"}, "active"); err != nil {
		k.t.Fatal(err)
	}
}

// reconcileCreation lets go of the project lock createChild took before it asks create for the next attempt: after the Run no start holds it.
func TestReconcileLetsGoOfTheProjectLockBeforeCreatingAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newReconcileKit(t, "lost", "accept")
	k.host.age = time.Hour // the answer was lost an hour ago: the grace period is over at once
	k.inProject()
	k.expect(k.run(), "admitted", "", "recreated")
	k.effects(2, 1)
	within, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	exclusive, err := projectlock.Exclusive(within, k.start.Store.Path, scopeProject)
	if err != nil {
		t.Fatalf("a lock is still held after the Run: %v", err)
	}
	_ = exclusive()
}

// A thread the creation left is not sent to once the project's parent binding has moved: the start is refused before the standby goes out.
func TestReconcileRefusesToContinueAfterTheProjectBindingMoved(t *testing.T) {
	t.Parallel()
	k := newReconcileKit(t, "name-timeout")
	k.inProject()
	k.host.failures["thread/read|t-1"] = errors.New("thread/read: host busy") // the first Run cannot observe the thread
	k.expect(k.run(), "incomplete", "creation_unknown", "unobservable")
	delete(k.host.failures, "thread/read|t-1")
	if _, err := k.start.Store.DB.ExecContext(context.Background(), "DELETE FROM scope_bindings WHERE scope_kind = 'project' AND scope_key = ?", scopeProject); err != nil {
		t.Fatal(err)
	}
	_, err := k.runErr()
	reasonIs(t, err, "unregistered_scope")
	k.effects(1, 0)
}
