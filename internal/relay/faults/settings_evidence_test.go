package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A real withheld delivery and its settled settings refusal: compare the complete
// derived observation, including its recovery, with the golden, which began as the Python
// faultsweep source's.
func Test22_SettingsHoldWholeObservation(t *testing.T) {
	goldenParent(t)
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprint(held), func(t *testing.T) { testSettingsHoldWholePythonObservation(t, held) })
	}
}
func testSettingsHoldWholePythonObservation(t *testing.T, held bool) {
	home := t.TempDir()
	seed := func(dir string) *store.Store {
		s, e := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		state, hold, attempts := "withheld_pre_send", any("attempt_cap"), 3
		if !held {
			state, hold, attempts = "queued", nil, 1
		}
		_, e = s.Q(context.Background()).ExecContext(context.Background(), `INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,hold_reason,attempt_count,created_at,updated_at) VALUES('evt1','rel','completion','parent','thread',?,?,?,'now','now')`, state, hold, attempts)
		if e != nil {
			t.Fatal(e)
		}
		for _, statement := range []string{
			`INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('req1','evt1',1,'completion','settled','settings_rejected','now')`,
			`INSERT INTO journal(at,kind,subject,detail) VALUES('now','delivery_attempted','evt1','{"requestId":"req1","settingsRefusal":{"reason":"settings_not_preserved","field":"runtimeWorkspaceRoots"}}')`,
		} {
			if _, e = s.Q(context.Background()).ExecContext(context.Background(), statement); e != nil {
				t.Fatal(e)
			}
		}
		return s
	}
	gs := seed(filepath.Join(home, "go"))
	defer gs.Close()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	sw := &Sweeper{Store: gs, HostRecordPath: testHostRecordPath(), MaxAttempts: 3, Current: func(context.Context, string) (bool, error) { return true, nil }, Installation: Installation{Package: "codex-session-relay", Version: "test", Location: "test"}, Program: func() []string { return []string{"codex-session-relay"} }}
	var got page
	var e error
	if held {
		got, e = sw.deliveryFaults(context.Background(), "crw", nil)
	} else {
		got, e = sw.retryFaults(context.Background(), "crw", nil)
	}
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(got.observations)
	if e != nil {
		t.Fatal(e)
	}
	var observed []any
	if e = json.Unmarshal(raw, &observed); e != nil {
		t.Fatal(e)
	}
	// Each observation's fields as the source's page names them; a recovery command is compared
	// after the program and its --state directory, which are this run's.
	fields := [][2]string{{"Product", "product"}, {"FaultClass", "faultClass"}, {"Signature", "signature"}, {"OccurrenceKey", "occurrenceKey"}, {"Scope", "scope"}, {"Detail", "detail"}, {"Evidence", "evidence"}, {"Cleared", "cleared"}}
	normalized := []any{}
	for _, raw := range observed {
		g := raw.(map[string]any)
		for _, item := range g["Evidence"].([]any) {
			if evidence := item.(map[string]any); evidence["kind"] == "recovery" {
				recovery := evidence["observed"].(map[string]any)
				recovery["command"] = strings.TrimPrefix(recovery["command"].(string), sw.Program()[0]+" --state "+filepath.Join(home, "go"))
			}
		}
		entry := map[string]any{}
		for _, pair := range fields {
			entry[pair[1]] = g[pair[0]]
		}
		normalized = append(normalized, entry)
	}
	checkGolden(t, "observations", nil, runPathsOf(t, home), normalized)
}
