package dagsched

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-1274: the parent settings a release declares are checked against the live parent thread (thread/read) before the child
// is created. The tests run the real managed engine over the real bridge adapter and the fake App Server (realKit).

// r2 S6: the parent thread runs opus with no effort while the release declares the policy's pair. The release is refused with
// the declared and the live values, nothing is created or recorded, and it goes through once the declaration is true.
// sequential: t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR") is process-wide.
func TestDagReleaseRefusesADeclaredParentPairThatDiffersFromTheLiveThread(t *testing.T) {
	k := newRealKit(t)
	releasePlan(k.fixture, "rp")
	k.mu.Lock()
	k.parentThread = map[string]any{"status": map[string]any{"type": "idle"}, "model": "anthropic/claude-opus-5-5", "reasoningEffort": nil}
	k.mu.Unlock()
	for i := range 2 {
		res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Bound || res.State != "refused" || res.Stage != "creation" || res.Reason != "parent_settings_differ_from_thread" {
			t.Fatalf("call %d = state %s stage %s reason %s bound %v", i, res.State, res.Stage, res.Reason, res.Bound)
		}
		observed := parentSettingsOf(t, res)
		declared, live := observed["declared"].(map[string]any), observed["live"].(map[string]any)
		if declared["model"] != "gpt-5.4" || declared["reasoningEffort"] != "medium" || live["model"] != "anthropic/claude-opus-5-5" || live["reasoningEffort"] != nil {
			t.Fatalf("call %d observation = %v", i, observed)
		}
	}
	if k.host.Count("thread/start") != 0 || k.host.Count("turn/start") != 0 {
		t.Fatalf("a refused release reached the host: %d thread/start, %d turn/start", k.host.Count("thread/start"), k.host.Count("turn/start"))
	}
	if n := k.count("SELECT COUNT(*) FROM authorized_settings"); n != 0 {
		t.Fatalf("a refused release recorded %d authorized_settings rows", n)
	}
	if rows := countRows(k.fixture); rows.executions != 0 || rows.releases != 1 || rows.slots != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	// The parent thread now runs what the policy declares: the same release, replayed from its intent, creates the child.
	k.mu.Lock()
	k.parentThread = nil
	k.mu.Unlock()
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || !res.Bound || res.ChildTaskID != "real-child" {
		t.Fatalf("release after the thread bears the declaration out = %v %+v", err, res)
	}
}

// sequential: t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR") is process-wide.
func TestDagReleaseRefusesWhenTheLiveParentSettingsAreUnobservable(t *testing.T) {
	for name, thread := range map[string]map[string]any{
		"no model":           {"status": map[string]any{"type": "idle"}, "reasoningEffort": "medium"},
		"no reasoningEffort": {"status": map[string]any{"type": "idle"}, "model": "gpt-5.4"},
	} {
		t.Run(name, func(t *testing.T) {
			k := newRealKit(t)
			releasePlan(k.fixture, "rp")
			k.mu.Lock()
			k.parentThread = thread
			k.mu.Unlock()
			res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
			if err != nil {
				t.Fatal(err)
			}
			if res.Bound || res.State != "refused" || res.Stage != "creation" || res.Reason != "parent_settings_unobservable" {
				t.Fatalf("release = state %s stage %s reason %s bound %v", res.State, res.Stage, res.Reason, res.Bound)
			}
			if observed := parentSettingsOf(t, res); observed["live"] != nil || observed["problem"] == nil {
				t.Fatalf("observation = %v", observed)
			}
			if k.host.Count("thread/start") != 0 || k.count("SELECT COUNT(*) FROM authorized_settings") != 0 {
				t.Fatalf("a refused release created or recorded something: %d thread/start", k.host.Count("thread/start"))
			}
		})
	}
}

// The release records what the host reported for the parent, under the source managed_start.
// sequential: t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR") is process-wide.
func TestDagReleaseRecordsTheLiveParentSettings(t *testing.T) {
	k := newRealKit(t)
	releasePlan(k.fixture, "rp")
	res, err := k.sched.Release(context.Background(), "rp", "A", "parent", k.request())
	if err != nil || !res.Bound {
		t.Fatalf("release = %v %+v", err, res)
	}
	var text, source string
	if err := k.s.DB.QueryRow("SELECT settings, source FROM authorized_settings WHERE task_id = 'parent'").Scan(&text, &source); err != nil {
		t.Fatal(err)
	}
	var recorded map[string]any
	if err := json.Unmarshal([]byte(text), &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded["model"] != "gpt-5.4" || recorded["reasoningEffort"] != "medium" || source != "managed_start" {
		t.Fatalf("parent authorized_settings = %v from %s", recorded, source)
	}
}

// parentSettingsOf is the observation a refused release carries in its managed answer, as a plain map.
func parentSettingsOf(t *testing.T, res ReleaseResult) map[string]any {
	t.Helper()
	value, ok := res.Managed.Get("parentSettings").(contract.OrderedObject)
	if !ok {
		t.Fatalf("the managed answer carries no parentSettings observation: %v", res.Managed)
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
