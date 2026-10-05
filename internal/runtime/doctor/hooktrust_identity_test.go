package doctor_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
)

// hookTrustIdentityRecorded is one case of testdata/hooktrust/identity-oracle.json, recorded
// by testdata/hooktrust/record-identity.mjs from CXC v0.2.40's dist (3c1459ac). Handler is the
// raw JSON document text the oracle parsed, so a spelling JSON.stringify cannot carry (the
// timeout 1e400 literal) is preserved rather than re-encoded; the test parses it with
// UseNumber, the same double spellings the oracle's JSON.parse held.
type hookTrustIdentityRecorded struct {
	Name      string  `json:"name"`
	Document  string  `json:"document"`
	Event     string  `json:"event"`
	Matcher   *string `json:"matcher"`
	Handler   string  `json:"handler"`
	Hash      string  `json:"hash"`
	Error     string  `json:"error"`
	Canonical bool    `json:"canonical"`
}

type hookTrustIdentityOracle struct {
	Oracle string                       `json:"oracle"`
	Cases  []hookTrustIdentityRecorded `json:"cases"`
}

func hookTrustIdentityRecordedCases(t *testing.T) []hookTrustIdentityRecorded {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hooktrust", "identity-oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded hookTrustIdentityOracle
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.Oracle, "3c1459ac") || len(recorded.Cases) < 60 {
		t.Fatalf("identity-oracle.json holds %d cases for oracle %q", len(recorded.Cases), recorded.Oracle)
	}
	return recorded.Cases
}

func hookTrustIdentityRecordedCase(t *testing.T, name string) hookTrustIdentityRecorded {
	t.Helper()
	for _, recorded := range hookTrustIdentityRecordedCases(t) {
		if recorded.Name == name {
			return recorded
		}
	}
	t.Fatalf("identity-oracle.json holds no case %q", name)
	return hookTrustIdentityRecorded{}
}

// hookTrustIdentityHandler reads a recorded handler the way the caller that reads a hook
// document does, with UseNumber so that a number spelling is not rounded on the way in.
func hookTrustIdentityHandler(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("recorded handler %s: %v", raw, err)
	}
	handler, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("recorded handler %s is not an object", raw)
	}
	return handler
}

func hookTrustIdentityHash(t *testing.T, event string, matcher *string, handler map[string]any) string {
	t.Helper()
	got, err := doctor.HookTrustIdentityHash(event, matcher, handler)
	if err != nil {
		t.Fatalf("HookTrustIdentityHash(%q) = %q, %v", event, got, err)
	}
	return got
}

func hookTrustIdentityCommand(extra map[string]any) map[string]any {
	handler := map[string]any{"type": "command", "command": "echo ok"}
	for key, value := range extra {
		handler[key] = value
	}
	return handler
}

func hookTrustIdentityPointer(text string) *string { return &text }

// TestHookTrustIdentityHash_recordedCases replays every recorded oracle answer, hashes and
// refusals alike, exactly as the dist produced it.
func TestHookTrustIdentityHash_recordedCases(t *testing.T) {
	for _, recorded := range hookTrustIdentityRecordedCases(t) {
		if recorded.Document != "" {
			continue // the live hook file has its own test
		}
		t.Run(recorded.Name, func(t *testing.T) {
			if recorded.Canonical == (recorded.Name == "timeout_1e400") {
				t.Fatalf("canonical=%v for %s: only the 1e400 literal cannot re-encode", recorded.Canonical, recorded.Name)
			}
			got, err := doctor.HookTrustIdentityHash(recorded.Event, recorded.Matcher, hookTrustIdentityHandler(t, recorded.Handler))
			switch {
			case recorded.Error != "":
				if err == nil || err.Error() != recorded.Error {
					t.Fatalf("got %q, %v; want the oracle refusal %q", got, err, recorded.Error)
				}
			case err != nil:
				t.Fatalf("HookTrustIdentityHash(%q, ..., %s) = %v", recorded.Event, recorded.Handler, err)
			case got != recorded.Hash:
				t.Fatalf("HookTrustIdentityHash(%q, ..., %s) = %s, want %s", recorded.Event, recorded.Handler, got, recorded.Hash)
			}
		})
	}
}

// TestHookTrustIdentityHash_liveStopHookGolden ports the live Stop golden
// (hook-trust.test.ts:53-63): the shipped CRW Stop hook is unchanged since the oracle
// recorded it and hashes to the recorded value. Editing the hook forces a re-recording.
func TestHookTrustIdentityHash_liveStopHookGolden(t *testing.T) {
	recorded := hookTrustIdentityRecordedCase(t, "crw_stop_live")
	path := filepath.Join(golden.Root(), filepath.FromSlash(recorded.Document))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	group := hookTrustIdentityGroup(t, document, recorded.Event)
	if matcher := group["matcher"]; matcher != nil {
		t.Fatalf("%s declares matcher %v; re-record the case", recorded.Document, matcher)
	}
	handler := hookTrustIdentityOnlyHandler(t, group)
	if !reflect.DeepEqual(handler, hookTrustIdentityHandler(t, recorded.Handler)) {
		t.Fatalf("%s changed since the oracle recorded %s; re-record the case", recorded.Document, recorded.Name)
	}
	if got := hookTrustIdentityHash(t, recorded.Event, nil, handler); got != recorded.Hash {
		t.Fatalf("the live Stop hook hashes %s, the oracle recorded %s", got, recorded.Hash)
	}
}

func hookTrustIdentityGroup(t *testing.T, document map[string]any, event string) map[string]any {
	t.Helper()
	hooks, ok := document["hooks"].(map[string]any)
	if !ok {
		t.Fatal("the document holds no hooks object")
	}
	groups, ok := hooks[event].([]any)
	if !ok || len(groups) == 0 {
		t.Fatalf("the document holds no %s group", event)
	}
	group, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatalf("the first %s group is not an object", event)
	}
	return group
}

func hookTrustIdentityOnlyHandler(t *testing.T, group map[string]any) map[string]any {
	t.Helper()
	handlers, ok := group["hooks"].([]any)
	if !ok || len(handlers) != 1 {
		t.Fatalf("the group declares %v handlers", group["hooks"])
	}
	handler, ok := handlers[0].(map[string]any)
	if !ok {
		t.Fatal("the handler is not an object")
	}
	return handler
}

// TestHookTrustIdentityHash_matcherByEvent ports hook-trust.test.ts:106-110: the matcher is
// part of the identity for every event except UserPromptSubmit and Stop, an empty matcher
// included.
func TestHookTrustIdentityHash_matcherByEvent(t *testing.T) {
	handler := hookTrustIdentityCommand(nil)
	for _, event := range []string{"Stop", "UserPromptSubmit"} {
		without := hookTrustIdentityHash(t, event, nil, handler)
		if with := hookTrustIdentityHash(t, event, hookTrustIdentityPointer("^ignored$"), handler); with != without {
			t.Fatalf("%s: a matcher changed the identity, but the oracle drops it there", event)
		}
	}
	without := hookTrustIdentityHash(t, "PreToolUse", nil, handler)
	for _, matcher := range []string{"^kept$", ""} {
		if with := hookTrustIdentityHash(t, "PreToolUse", hookTrustIdentityPointer(matcher), handler); with == without {
			t.Fatalf("PreToolUse: matcher %q did not enter the identity", matcher)
		}
	}
}

// TestHookTrustIdentityHash_timeoutDefaultAndClamp ports hook-trust.test.ts:112-115: an
// absent timeout is 600, and a timeout below one clamps to one.
func TestHookTrustIdentityHash_timeoutDefaultAndClamp(t *testing.T) {
	base := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(nil))
	if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"timeout": float64(600)})); got != base {
		t.Fatal("an absent timeout is not 600")
	}
	if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"timeout": nil})); got != base {
		t.Fatal("a null timeout is not an absent one")
	}
	clamped := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"timeout": float64(1)}))
	for _, timeout := range []float64{0, 0.5, -3} {
		if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"timeout": timeout})); got != clamped {
			t.Fatalf("timeout %v did not clamp to 1", timeout)
		}
	}
	if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"timeout": int64(600)})); got != base {
		t.Fatal("an int64 600 is not the same double as 600")
	}
	if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"timeout": json.Number("600")})); got != base {
		t.Fatal("a json.Number 600 is not the same double as 600")
	}
}

// TestHookTrustIdentityHash_statusMessageOnlyWhenPresent ports hook-trust.test.ts:117-126: the
// status message enters the identity only when it is not null.
func TestHookTrustIdentityHash_statusMessageOnlyWhenPresent(t *testing.T) {
	base := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(nil))
	if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"statusMessage": nil})); got != base {
		t.Fatal("a null statusMessage entered the identity")
	}
	for _, message := range []string{"Checking", ""} {
		if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"statusMessage": message})); got == base {
			t.Fatalf("statusMessage %q did not enter the identity", message)
		}
	}
}

// TestHookTrustIdentityHash_asyncDefaultAndOverride asserts the async normalization the
// recorded cases cannot separate on their own: absent, null and false are one identity, true
// is another, and anything else refuses.
func TestHookTrustIdentityHash_asyncDefaultAndOverride(t *testing.T) {
	base := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(nil))
	for _, value := range []any{nil, false} {
		if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"async": value})); got != base {
			t.Fatalf("async %v did not normalize to false", value)
		}
	}
	if got := hookTrustIdentityHash(t, "Stop", nil, hookTrustIdentityCommand(map[string]any{"async": true})); got == base {
		t.Fatal("async true did not enter the identity")
	}
	if _, err := doctor.HookTrustIdentityHash("Stop", nil, hookTrustIdentityCommand(map[string]any{"async": 1})); err == nil || err.Error() != "hook async must be a boolean" {
		t.Fatalf("async 1 = %v, want the oracle refusal", err)
	}
}

// TestHookTrustIdentityHash_goSideBranches covers what a recorded case cannot carry: the
// values JSON has no spelling for (NaN, the infinities, negative zero) and the refusal order.
// The oracle dist answered every one of these as the scratch probe of 2026-10-05 recorded.
func TestHookTrustIdentityHash_goSideBranches(t *testing.T) {
	refusals := []struct {
		name    string
		event   string
		handler map[string]any
		want    string
	}{
		{"type NaN", "Stop", map[string]any{"type": math.NaN(), "command": "echo ok"}, "unsupported hook handler type: NaN"},
		{"type +Inf", "Stop", map[string]any{"type": math.Inf(1), "command": "echo ok"}, "unsupported hook handler type: Infinity"},
		{"type -Inf", "Stop", map[string]any{"type": math.Inf(-1), "command": "echo ok"}, "unsupported hook handler type: -Infinity"},
		{"type -0", "Stop", map[string]any{"type": math.Copysign(0, -1), "command": "echo ok"}, "unsupported hook handler type: 0"},
		{"timeout +Inf", "Stop", map[string]any{"type": "command", "command": "echo ok", "timeout": math.Inf(1)}, "hook timeout must be a finite number"},
		{"timeout NaN", "Stop", map[string]any{"type": "command", "command": "echo ok", "timeout": math.NaN()}, "hook timeout must be a finite number"},
		{"timeout beyond the double range", "Stop", map[string]any{"type": "command", "command": "echo ok", "timeout": json.Number("1e400")}, "hook timeout must be a finite number"},
		{"the event refuses before the type", "then", map[string]any{"type": 5, "command": "echo ok"}, "unsupported hook event: then"},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			got, err := doctor.HookTrustIdentityHash(refusal.event, nil, refusal.handler)
			if err == nil || err.Error() != refusal.want {
				t.Fatalf("got %q, %v; want %q", got, err, refusal.want)
			}
		})
	}
}
