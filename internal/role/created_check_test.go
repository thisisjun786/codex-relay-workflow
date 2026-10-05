package role

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type createdCheckFake struct {
	reply json.RawMessage
	err   error
	calls int
}

func (f *createdCheckFake) Call(_ context.Context, method string, args map[string]any) (json.RawMessage, error) {
	f.calls++
	if method != "thread/read" || args["threadId"] != "child-a" || args["includeTurns"] != false {
		return nil, errors.New("unexpected host call")
	}
	return f.reply, f.err
}
func createdCheckReply(parent, source, status string) json.RawMessage {
	return must(json.Marshal(map[string]any{"thread": map[string]any{"id": "child-a", "parentThreadId": parent, "threadSource": source, "status": map[string]any{"type": status}}}))
}
func createdCheckInput(attempt, outcome string) map[string]any {
	return map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": outcome, "agentId": "child-a"}
}

func TestCreatedCheckHostWitnessAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  json.RawMessage
		err    error
		accept bool
	}{
		{"child", createdCheckReply("session-test", "subagent", "idle"), nil, true},
		{"active-child", createdCheckReply("session-test", "subagent", "active"), nil, true},
		{"source-witness", json.RawMessage(`{"thread":{"id":"child-a","parentThreadId":"session-test","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"session-test"}}},"status":{"type":"notLoaded"}}}`), nil, true},
		{"foreign-parent", createdCheckReply("other", "subagent", "idle"), nil, false},
		{"root", createdCheckReply("session-test", "cli", "idle"), nil, false},
		{"missing", json.RawMessage(`{}`), nil, false},
		{"wrong-id", json.RawMessage(`{"thread":{"id":"other","parentThreadId":"session-test","threadSource":"subagent"}}`), nil, false},
		{"corrupt", json.RawMessage(`{`), nil, false},
		{"unavailable", nil, errors.New("host unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			before := must(os.ReadFile(file))
			h := &createdCheckFake{reply: tc.reply, err: tc.err}
			out, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, h)
			if tc.accept {
				check(t, err)
				if out.Action != "wait" || out.Attempts[0].AgentID == nil || *out.Attempts[0].AgentID != "child-a" {
					t.Fatalf("created = %+v", out)
				}
				// Complete remains independent of host availability after created.
				h.err = errors.New("disconnected")
				out, err = CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "complete"), env, h)
				check(t, err)
				if out.Action != "complete" || h.calls != 1 {
					t.Fatal("complete rechecked host")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "copy agentId") {
					t.Fatalf("refusal lacks correction: %v", err)
				}
				if string(before) != string(must(os.ReadFile(file))) {
					t.Fatal("refused report changed ledger")
				}
			}
			if temps := must(filepath.Glob(file + ".*.tmp")); len(temps) != 0 {
				t.Fatal("owned temporary remains")
			}
			if _, err := os.Lstat(file + ".lock"); !os.IsNotExist(err) {
				t.Fatal("owned lock remains")
			}
		})
	}
}

func TestCreatedCheckValidationPrecedesHost(t *testing.T) {
	ws, env, start, _ := dispatchTestFixture(t)
	h := &createdCheckFake{err: errors.New("unavailable")}
	input := createdCheckInput(start.AttemptID, "created")
	_, err := CheckedDispatch(context.Background(), ws, input, env, h)
	if err == nil || !strings.Contains(err.Error(), "claim") || h.calls != 0 {
		t.Fatalf("precedence: %v %d", err, h.calls)
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	input["attemptId"] = "stale"
	_, err = CheckedDispatch(context.Background(), ws, input, env, h)
	if err == nil || !strings.Contains(err.Error(), "stale") || h.calls != 0 {
		t.Fatal("stale reached host")
	}
	input["attemptId"] = start.AttemptID
	input["agentId"] = "../bad"
	_, err = CheckedDispatch(context.Background(), ws, input, env, h)
	if err == nil || !strings.Contains(err.Error(), "invalid agentId") || h.calls != 0 {
		t.Fatal("invalid identity reached host")
	}
}

// Independently recorded oracle answers: invented created -> wait; correcting
// that ID -> agentId changed; stopped report -> invalid report outcome.
func TestCreatedCheckChangedOracleAnswersAndHonestClose(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	oracle := dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "PLACEHOLDER"})
	if oracle.Action != "wait" {
		t.Fatal("parity oracle fixture")
	}
	correction := createdCheckInput(start.AttemptID, "created")
	if _, err := RunDispatch(ws, correction, env); err == nil || err.Error() != "agentId changed" {
		t.Fatal("oracle correction fixture")
	}
	stop := createdCheckInput(start.AttemptID, "stopped")
	stop["agentId"] = "PLACEHOLDER"
	stop["executionState"] = "stopped"
	stop["reconciliation"] = "actual child stopped; partial work inspected"
	if _, err := RunDispatch(ws, stop, env); err == nil || err.Error() != "invalid report outcome" {
		t.Fatal("oracle close fixture")
	}
	for _, field := range []string{"reconciliation", "executionState", "agentId"} {
		bad := make(map[string]any)
		for k, v := range stop {
			bad[k] = v
		}
		delete(bad, field)
		if _, err := CheckedDispatch(context.Background(), ws, bad, env, &createdCheckFake{err: errors.New("missing")}); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	out, err := CheckedDispatch(context.Background(), ws, stop, env, &createdCheckFake{err: errors.New("unknown invented thread")})
	check(t, err)
	if out.Action != "stop" || len(out.Attempts) != 1 || *out.Attempts[0].AgentID != "PLACEHOLDER" || out.Attempts[0].Reconciliation == nil || out.Attempts[0].Code != nil {
		t.Fatalf("closure = %+v", out)
	}
	stored := must(dispatchRead(file, "session-test", "task-test"))
	if stored.Status != "stopped" || stored.Attempts[0].Status != "failed" {
		t.Fatal("closure not durable")
	}
	for _, action := range []string{"status", "claim"} {
		out := dispatchTestCall(t, ws, env, map[string]any{"action": action, "attemptId": start.AttemptID})
		if out.Action != "stop" {
			t.Fatalf("terminal %s = %+v", action, out)
		}
	}
}

func TestCreatedCheckCloseRefusesActiveAndKeepsLock(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"})
	input := createdCheckInput(start.AttemptID, "stopped")
	input["executionState"] = "stopped"
	input["reconciliation"] = "inspected child"
	before := must(os.ReadFile(file))
	h := &createdCheckFake{reply: createdCheckReply("session-test", "subagent", "active")}
	if _, err := CheckedDispatch(context.Background(), ws, input, env, h); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatal("active child closure accepted")
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("active refusal wrote state")
	}
	check(t, os.Mkdir(file+".lock", 0700))
	if _, err := CheckedDispatch(context.Background(), ws, input, env, h); err == nil {
		t.Fatal("held lock closure accepted")
	}
}

// Synthetic host-owned markers use the schema and source shape observed on a
// real native child. Every DB is created in a temporary CODEX_HOME.
func createdCheckSeed(t *testing.T, native, id, parent string) {
	t.Helper()
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	_, err := db.Exec("CREATE TABLE IF NOT EXISTS threads (id TEXT PRIMARY KEY, source TEXT, archived INTEGER)")
	check(t, err)
	source := string(must(json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}})))
	_, err = db.Exec("INSERT OR REPLACE INTO threads VALUES (?,?,0)", id, source)
	check(t, err)
}
func TestCreatedCheckDefaultHostSpawnMarker(t *testing.T) {
	for _, tc := range []struct {
		name, parent string
		accept       bool
	}{{"child", "session-test", true}, {"foreign", "other", false}} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, start, _ := dispatchTestFixture(t)
			native := t.TempDir()
			createdCheckSeed(t, native, "child-a", tc.parent)
			old := env
			env = func(k string) (string, bool) {
				if k == "CODEX_HOME" {
					return native, true
				}
				return old(k)
			}
			before := must(os.ReadFile(filepath.Join(native, "state_5.sqlite")))
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			out, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
			if tc.accept {
				check(t, err)
				if out.Action != "wait" {
					t.Fatal("host marker not accepted")
				}
			} else if err == nil {
				t.Fatal("foreign marker accepted")
			}
			if string(before) != string(must(os.ReadFile(filepath.Join(native, "state_5.sqlite")))) {
				t.Fatal("host DB changed")
			}
		})
	}
}

func TestCreatedCheckNativeDatabaseRefusalsAndOrdering(t *testing.T) {
	for _, kind := range []string{"numeric-order", "missing", "symlink-highest", "corrupt-highest", "malformed-marker", "root", "other-subagent", "archived"} {
		t.Run(kind, func(t *testing.T) {
			ws, env, start, _ := dispatchTestFixture(t)
			native := t.TempDir()
			createdCheckSeed(t, native, "child-a", "session-test")
			file := filepath.Join(native, "state_5.sqlite")
			switch kind {
			case "numeric-order":
				check(t, os.WriteFile(filepath.Join(native, "state_9.sqlite"), []byte("corrupt older database"), 0600))
				check(t, os.Rename(file, filepath.Join(native, "state_10.sqlite")))
			case "missing":
				check(t, os.Rename(file, filepath.Join(native, "disconnected.sqlite")))
			case "symlink-highest":
				check(t, os.Symlink(file, filepath.Join(native, "state_10.sqlite")))
			case "corrupt-highest":
				check(t, os.WriteFile(filepath.Join(native, "state_10.sqlite"), []byte("corrupt highest database"), 0600))
			default:
				db := must(sql.Open("sqlite", file))
				var source string
				switch kind {
				case "malformed-marker":
					source = "{"
				case "root":
					source = `"cli"`
				case "other-subagent":
					source = `{"subagent":"review"}`
				case "archived":
					_, err := db.Exec("UPDATE threads SET archived=1")
					check(t, err)
				}
				if source != "" {
					_, err := db.Exec("UPDATE threads SET source=?", source)
					check(t, err)
				}
				check(t, db.Close())
			}
			old := env
			env = func(k string) (string, bool) {
				if k == "CODEX_SQLITE_HOME" {
					return native, true
				}
				return old(k)
			}
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			_, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
			// An archived child of this session is a real child: the host archives it when it finishes.
			if kind == "numeric-order" || kind == "archived" {
				check(t, err)
			} else if err == nil {
				t.Fatal("invalid host witness accepted or older DB used")
			}
		})
	}
}

func TestCreatedCheckNativeWALAndEncodedPath(t *testing.T) {
	ws, env, start, _ := dispatchTestFixture(t)
	native := filepath.Join(t.TempDir(), "host ?#%")
	check(t, os.Mkdir(native, 0700))
	createdCheckSeed(t, native, "old", "other")
	file := filepath.Join(native, "state_5.sqlite")
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: file}).String()))
	defer db.Close()
	_, err := db.Exec("PRAGMA journal_mode=WAL")
	check(t, err)
	source := `{"subagent":{"thread_spawn":{"parent_thread_id":"session-test","depth":1}}}`
	_, err = db.Exec("INSERT INTO threads VALUES (?,?,0)", "child-a", source)
	check(t, err)
	// Keep the writer open: the new child exists in WAL, not a closed DB checkpoint.
	before := must(os.ReadFile(file))
	wal := must(os.ReadFile(file + "-wal"))
	old := env
	env = func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return old(k)
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	out, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
	check(t, err)
	if out.Action != "wait" || string(before) != string(must(os.ReadFile(file))) || string(wal) != string(must(os.ReadFile(file+"-wal"))) {
		t.Fatal("WAL witness missing or host records changed")
	}
}
