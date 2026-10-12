package managed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// parentKit is a managed start over a fresh store whose parent declares gpt-5 / medium (the fixture's pair); the host
// answers thread/read of the parent as the test says.
type parentKit struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	host  *managedFake
	start *Start
	raw   []byte
}

// newParentKit builds the kit; declare changes the parent settings the request declares before it is frozen.
func newParentKit(t *testing.T, declare func(map[string]any)) *parentKit {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var req map[string]any
	if err := json.Unmarshal(requestFixture(t), &req); err != nil {
		t.Fatal(err)
	}
	if declare != nil {
		declare(pyjson.Map(pyjson.Map(req["parent"])["settings"]))
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	host := &managedFake{operations: map[string]map[string]any{}, settings: pyjson.Map(pyjson.Map(parsed["child"])["settings"]), ledger: map[string]any{"realPath": filepath.Join(dir, "ledger"), "device": 1, "inode": 2}, standby: "completed"}
	start := &Start{StandbyWait: testStandbyWait, StandbyPoll: testStandbyPoll, Store: s, Adapter: host, Now: func() string { return "2026-10-12T00:00:00.000000+00:00" }, Socket: filepath.Join(dir, "socket"), MarkerRoot: filepath.Join(dir, "markers"), StateSelector: dir,
		Readiness: func(context.Context, map[string]any) (string, error) { return "", nil }}
	return &parentKit{t: t, ctx: ctx, store: s, host: host, start: start, raw: raw}
}

func (k *parentKit) run() map[string]any {
	k.t.Helper()
	out, err := k.start.Run(k.ctx, k.raw)
	if err != nil {
		k.t.Fatalf("Run: %v", err)
	}
	return fieldsOf(out)
}

func (k *parentKit) count(query string, args ...any) int {
	k.t.Helper()
	var n int
	if err := k.store.DB.QueryRowContext(k.ctx, query, args...).Scan(&n); err != nil {
		k.t.Fatal(err)
	}
	return n
}

// nothingRecorded asserts that a refused start left no child and no authorized settings behind.
func (k *parentKit) nothingRecorded() {
	k.t.Helper()
	if k.host.created != 0 || k.host.sent != 0 {
		k.t.Fatalf("a refused start made host effects: created %d, sent %d", k.host.created, k.host.sent)
	}
	if n := k.count("SELECT COUNT(*) FROM authorized_settings"); n != 0 {
		k.t.Fatalf("a refused start recorded %d authorized_settings rows", n)
	}
	if n := k.count("SELECT COUNT(*) FROM relationships"); n != 0 {
		k.t.Fatalf("a refused start registered %d relationships", n)
	}
}

func (k *parentKit) recorded(task string) map[string]any {
	k.t.Helper()
	var text string
	if err := k.store.DB.QueryRowContext(k.ctx, "SELECT settings FROM authorized_settings WHERE task_id=?", task).Scan(&text); err != nil {
		k.t.Fatalf("authorized_settings of %s: %v", task, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		k.t.Fatal(err)
	}
	return out
}

// parentSettings is the observation a refusal carries, as a plain map.
func parentSettings(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	value, ok := answer["parentSettings"].(contract.OrderedObject)
	if !ok {
		t.Fatalf("the refusal carries no parentSettings observation: %v", answer)
	}
	var encoded bytes.Buffer
	if err := contract.Emit(&encoded, value); err != nil {
		t.Fatal(err)
	}
	var plain map[string]any
	if err := json.Unmarshal(encoded.Bytes(), &plain); err != nil {
		t.Fatal(err)
	}
	return plain
}

// r2 S6: the parent runs opus with no effort; the request declares sonnet-class settings. The start is refused with both
// values named, and nothing is recorded.
func TestManagedStartRefusesADeclaredParentPairThatDiffersFromTheLiveThread(t *testing.T) {
	t.Parallel()
	k := newParentKit(t, nil)
	k.host.parentLive = map[string]any{"model": "anthropic/claude-opus-5-5", "reasoningEffort": nil}
	got := k.run()
	if got["state"] != "refused" || got["stage"] != "creation" || got["reason"] != ReasonParentSettingsDiffer {
		t.Fatalf("answer = %v, want refused at creation with %s", got, ReasonParentSettingsDiffer)
	}
	observed := parentSettings(t, got)
	declared, live := pyjson.Map(observed["declared"]), pyjson.Map(observed["live"])
	if declared["model"] != "gpt-5" || declared["reasoningEffort"] != "medium" || live["model"] != "anthropic/claude-opus-5-5" || live["reasoningEffort"] != nil {
		t.Fatalf("observation = %v", observed)
	}
	if fmt.Sprint(observed["differing"]) != "[model reasoningEffort]" || observed["taskId"] != "parent" {
		t.Fatalf("observation = %v", observed)
	}
	k.nothingRecorded()
}

func TestManagedStartRefusesAParentWhoseLiveEffortAloneDiffers(t *testing.T) {
	t.Parallel()
	k := newParentKit(t, nil)
	k.host.parentLive = map[string]any{"model": "gpt-5", "reasoningEffort": "xhigh"}
	got := k.run()
	if got["reason"] != ReasonParentSettingsDiffer || fmt.Sprint(parentSettings(t, got)["differing"]) != "[reasoningEffort]" {
		t.Fatalf("answer = %v", got)
	}
	k.nothingRecorded()
}

// A live value the host does not give is not read as the declared one: the start is refused, with the reason that tells
// it from a difference.
func TestManagedStartRefusesWhenTheLiveParentSettingsAreUnobservable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		set  func(*managedFake)
	}{
		{"the host refuses thread/read of the parent", func(f *managedFake) {
			f.parentReadError = &appserver.RPCError{Method: "thread/read", Code: -32600, Message: "thread not found: parent"}
		}},
		{"the answer names no model", func(f *managedFake) { f.parentThread = map[string]any{"reasoningEffort": "medium"} }},
		{"the model is empty", func(f *managedFake) { f.parentThread = map[string]any{"model": "", "reasoningEffort": "medium"} }},
		{"the answer has no reasoningEffort field", func(f *managedFake) { f.parentThread = map[string]any{"model": "gpt-5"} }},
		{"the answer is no thread object", func(f *managedFake) { f.parentThread = map[string]any{} }},
		{"the effort is not text", func(f *managedFake) { f.parentThread = map[string]any{"model": "gpt-5", "reasoningEffort": 3} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newParentKit(t, nil)
			tc.set(k.host)
			got := k.run()
			if got["state"] != "refused" || got["stage"] != "creation" || got["reason"] != ReasonParentSettingsUnobservable {
				t.Fatalf("answer = %v, want refused at creation with %s", got, ReasonParentSettingsUnobservable)
			}
			observed := parentSettings(t, got)
			if observed["live"] != nil || observed["problem"] == nil || pyjson.Map(observed["declared"])["model"] != "gpt-5" {
				t.Fatalf("observation = %v", observed)
			}
			k.nothingRecorded()
		})
	}
}

// A host that does not answer at all (the transport failed) is a failed call of the start, like any other host call, and
// records nothing; the same request goes through once the host answers.
func TestManagedStartParentReadTransportFailureIsAHostFailureAndRecordsNothing(t *testing.T) {
	t.Parallel()
	k := newParentKit(t, nil)
	sentinel := errors.New("socket closed")
	k.host.parentReadError = sentinel
	if _, err := k.start.Run(k.ctx, k.raw); !errors.Is(err, sentinel) {
		t.Fatalf("Run error = %v, want the transport error", err)
	}
	k.nothingRecorded()
	k.host.parentReadError = nil
	if got := k.run(); got["state"] != "admitted" {
		t.Fatalf("answer after the host answers again = %v", got)
	}
}

// The parent's settings are recorded from what the host reports: the pair the live thread bears out, read before the record
// is written, and the same value the declaration is for the parent's next start.
func TestManagedStartRecordsTheLiveSettingsNotTheDeclaration(t *testing.T) {
	t.Parallel()
	k := newParentKit(t, func(parent map[string]any) { parent["model"], parent["reasoningEffort"] = "gpt-5.4", "high" })
	k.host.parentLive = map[string]any{"model": "gpt-5.4", "reasoningEffort": "high"}
	got := k.run()
	if got["state"] != "admitted" {
		t.Fatalf("answer = %v", got)
	}
	if k.host.parentReads == 0 {
		t.Fatalf("the parent thread was never read: %v", k.host.hostCalls)
	}
	recorded := k.recorded("parent")
	if recorded["model"] != "gpt-5.4" || recorded["reasoningEffort"] != "high" {
		t.Fatalf("parent authorized_settings = %v, want the live pair", recorded)
	}
	var source string
	if err := k.store.DB.QueryRowContext(k.ctx, "SELECT source FROM authorized_settings WHERE task_id='parent'").Scan(&source); err != nil || source != "managed_start" {
		t.Fatalf("source = %q, %v", source, err)
	}
	var req map[string]any
	if err := json.Unmarshal(k.raw, &req); err != nil {
		t.Fatal(err)
	}
	declared := settingsWithRole(pyjson.Map(pyjson.Map(req["parent"])["settings"]), "parent")
	if err := SettingsConflict(k.ctx, k.store, "parent", declared); err != nil {
		t.Fatalf("the recorded settings conflict with the declaration: %v", err)
	}
}

// A start whose creation was armed by an earlier call is not judged again at the preflight, but nothing is recorded
// from a declaration the host does not bear out: the parent is read when its settings are about to be recorded.
func TestManagedStartArmedRequestReadsTheParentBeforeRecordingItsSettings(t *testing.T) {
	t.Parallel()
	k := newParentKit(t, nil)
	// The first call arms the creation and loses its answer: nothing is registered or recorded.
	k.host.creationStatus = "outcome_unknown"
	if got := k.run(); got["reason"] != "creation_unknown" || k.host.created != 1 {
		t.Fatalf("first call = %v, created %d", got, k.host.created)
	}
	// The host then shows the creation it made: the receipt the request's creation id retains.
	k.host.creationStatus = ""
	create, _ := OperationIDs("managed-1")
	if _, err := k.host.CreateThread(k.ctx, CreateThreadRequest{RequestID: create}); err != nil {
		t.Fatal(err)
	}
	k.host.created = 1
	k.host.parentLive = map[string]any{"model": "anthropic/claude-opus-5-5", "reasoningEffort": nil}
	got := k.run()
	if got["state"] != "refused" || got["stage"] != "settings" || got["reason"] != ReasonParentSettingsDiffer {
		t.Fatalf("second call = %v, want refused at settings with %s", got, ReasonParentSettingsDiffer)
	}
	if n := k.count("SELECT COUNT(*) FROM authorized_settings"); n != 0 {
		t.Fatalf("the refusal recorded %d settings rows", n)
	}
	if k.host.sent != 0 {
		t.Fatalf("the business turn was sent despite the refusal")
	}
	// Once the thread bears the declaration out, the same request goes through.
	k.host.parentLive = nil
	if got := k.run(); got["state"] != "admitted" || k.host.created != 1 {
		t.Fatalf("third call = %v, created %d", got, k.host.created)
	}
}

// A start already admitted is a replay, answered from what was recorded: a parent whose settings changed since is not
// asked again.
func TestManagedStartAdmittedReplayIsNotRefusedForALaterParentChange(t *testing.T) {
	t.Parallel()
	k := newParentKit(t, nil)
	if got := k.run(); got["state"] != "admitted" {
		t.Fatalf("first call = %v", got)
	}
	k.host.parentLive = map[string]any{"model": "anthropic/claude-opus-5-5", "reasoningEffort": nil}
	if got := k.run(); got["state"] != "admitted" {
		t.Fatalf("replay = %v", got)
	}
	if k.host.created != 1 || k.host.sent != 1 {
		t.Fatalf("the replay made host effects: %d/%d", k.host.created, k.host.sent)
	}
}
