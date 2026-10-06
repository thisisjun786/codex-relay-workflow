package manage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The signal layer is tested through injected seams only: a fake relay executable, replaced gh and
// status-page seams, and a temporary relay store for the receipt wait.

func capacityTestStore(t *testing.T, path string) {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE deliveries (event_id TEXT PRIMARY KEY, recipient_task_id TEXT NOT NULL, created_at TEXT NOT NULL)",
		"CREATE TABLE acks (event_id TEXT PRIMARY KEY, ack_at TEXT NOT NULL)",
		"CREATE TABLE events (event_id TEXT PRIMARY KEY, outcome TEXT NOT NULL)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

func capacityTestRelay(t *testing.T, f *capacityFixture, reading map[string]any) {
	t.Helper()
	answer, err := json.Marshal(reading)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(f.env.Executable), "answer.json")
	if err := os.WriteFile(file, answer, 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" + "case \"$*\" in\n" +
		"  *dag-ready*) cat " + coreShellQuote(file) + " ;;\n" +
		"  *) echo 'relay: refused' >&2; exit 2 ;;\n" + "esac\n"
	if err := os.WriteFile(f.env.Executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// capacityTestReading is a relay reading: waiting are ready nodes, extraWaiting are nodes
// deferred for want of capacity, hostMemory the bound's state (empty prints none).
func capacityTestReading(waiting, extraWaiting []string, held, ceiling int, hostMemory string) map[string]any {
	nodes := func(keys []string, reason any, disposition string) []any {
		out := make([]any, len(keys))
		for i, key := range keys {
			out[i] = map[string]any{"node_id": "n" + key, "issue_key": key, "reason": reason, "disposition": disposition}
		}
		return out
	}
	pass := map[string]any{"free_slots": ceiling - held, "ceiling": ceiling, "held": held}
	if hostMemory != "" {
		pass["host_memory"] = map[string]any{"state": hostMemory}
	}
	return map[string]any{"ok": true, "schema": "dag-ready/1", "pass": pass,
		"ready": nodes(waiting, nil, "ready"), "nodes": nodes(extraWaiting, "defer:no_capacity", "defer")}
}

func capacityTestReceipt(t *testing.T, f *capacityFixture, event, parent string, waitMinutes float64, interrupted bool) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.relayDir, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created := capacityTestNow.Add(-time.Duration(waitMinutes+1) * time.Minute)
	outcome := "ready_for_review"
	if interrupted {
		outcome = "interrupted"
	}
	exec := func(query string, args ...any) {
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO deliveries (event_id, recipient_task_id, created_at) VALUES (?,?,?)", event, parent, created.Format(time.RFC3339))
	exec("INSERT INTO acks (event_id, ack_at) VALUES (?,?)", event, created.Add(time.Duration(waitMinutes*float64(time.Minute))).Format(time.RFC3339))
	exec("INSERT INTO events (event_id, outcome) VALUES (?,?)", event, outcome)
}

// capacityTestSeams replaces the gh and status-page seams for one test: merges is the lane
// count, a nil status makes gh fail and a non-nil statusErr makes the status read fail.
func capacityTestSeams(t *testing.T, merges int, status []byte, statusErr error) {
	t.Helper()
	exec, get := capacityExec, capacityHTTPGet
	t.Cleanup(func() { capacityExec, capacityHTTPGet = exec, get })
	capacityExec = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "gh" {
			return nil, errors.New("unexpected command " + name)
		}
		if status == nil && statusErr == nil {
			return nil, errors.New("gh is not available")
		}
		rows := make([]any, merges)
		for i := range rows {
			rows[i] = map[string]any{"number": i + 1}
		}
		return json.Marshal(rows)
	}
	capacityHTTPGet = func(_ context.Context, url string) ([]byte, error) {
		if statusErr != nil {
			return nil, statusErr
		}
		return status, nil
	}
}

func capacityTestUsageLog(t *testing.T, f *capacityFixture, status int, model string, at time.Time) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "usage.jsonl")
	data, err := json.Marshal(map[string]any{"timestamp": at.UnixMilli(),
		"attempts": []any{map[string]any{"status": status, "model": model}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	capacityTestSection(t, f, "usage_log", log)
	capacityTestSection(t, f, "child_models", []string{"deepseek-v4.1-flash"})
}

// C3: a failed status read is unknown and an unreadable usage log is unmeasured; neither
// suppresses. An empty window is measured 0, and another model's 429 changes nothing.
func TestCapacityUnreadableSignalsDoNotSuppress(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	capacityTestSeams(t, 0, nil, errors.New("no route to the status page"))
	report := capacityTestRun(t, f, false)
	if report.Actions.State != capacityUnknown || report.Actions.Incident != nil {
		t.Fatalf("actions = %+v, want unknown with no incident", report.Actions)
	}
	if report.Child429.State != capacityUnmeasured || report.Child429.Count != nil {
		t.Fatalf("child_429 = %+v, want unmeasured with no count", report.Child429)
	}
	if report.Plans[0].Verdict != capacityExpand {
		t.Fatalf("an unreadable signal suppressed: %+v", report.Plans[0])
	}

	log := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(log, []byte("{\"timestamp\":1,\"attempts\":[]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	capacityTestSection(t, f, "usage_log", log)
	capacityTestSection(t, f, "child_models", []string{"deepseek-v4.1-flash"})
	report = capacityTestRun(t, f, false)
	if report.Child429.State != capacityMeasured || report.Child429.Count == nil || *report.Child429.Count != 0 {
		t.Fatalf("child_429 = %+v, want measured 0", report.Child429)
	}
	if report.Plans[0].Verdict != capacityExpand {
		t.Fatalf("a measured zero suppressed: %+v", report.Plans[0])
	}

	capacityTestUsageLog(t, f, 429, "some-other-model", capacityTestNow)
	capacityTestReceipt(t, f, "e2", "parent-1", 90, true)
	report = capacityTestRun(t, f, false)
	if report.Child429.Count == nil || *report.Child429.Count != 0 {
		t.Fatalf("child_429 = %+v, want 0", report.Child429)
	}
	if report.Plans[0].ReceiptWait.Count != 1 || report.Plans[0].Verdict != capacityExpand {
		t.Fatalf("an interrupted delivery counted: %+v", report.Plans[0])
	}
}
