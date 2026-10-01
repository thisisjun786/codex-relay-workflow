package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// checkpoint is one call of an assignment scenario (testdata/fixtures/assignment_checkpoints.json,
// what the retired gen_assignment.py recorded from the Python test scenarios): the whole store as
// SQL at that moment, the clock, the call and the work root the scenario ran in.
type checkpoint struct {
	Op   string            `json:"op"`
	Args map[string]string `json:"args"`
	SQL  string            `json:"sql"`
	Now  float64           `json:"now"`
	ISO  string            `json:"iso"`
	Root string            `json:"root"`
}

func assignmentScenario(t *testing.T, name string) []checkpoint {
	t.Helper()
	var all map[string][]checkpoint
	if err := json.Unmarshal(golden.Fixture(t, "assignment_checkpoints.json"), &all); err != nil {
		t.Fatal(err)
	}
	points, ok := all[name]
	if !ok {
		t.Fatalf("no assignment scenario %q", name)
	}
	return points
}

// loadCheckpoint writes a checkpoint's whole store at path: the frozen Python-produced empty store
// (contract/fixtures/sqlite-ddl) holding every row the checkpoint dumped, schema_meta included. It
// is a store written by hand, so it is then fenced for Go exactly as Go's absent-store initializer
// stamps a store (testsupport.Fence) before Go opens it.
func loadCheckpoint(t *testing.T, path, dump string) {
	t.Helper()
	fixture, err := testsupport.FrozenStore()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := ownership.OpenExisting(ctx(), path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"DELETE FROM schema_meta", dump, "PRAGMA wal_checkpoint(TRUNCATE)"} {
		if _, err = db.ExecContext(ctx(), statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	testsupport.Fence(t, path, "go")
}

// replay loads a checkpoint's rows into a Go-owned store and makes the same call. It returns
// Go's answer and the Go store's directory.
func replay(t *testing.T, point checkpoint) (map[string]any, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	loadCheckpoint(t, filepath.Join(dir, "relay.sqlite3"), point.SQL)
	s, err := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &Registry{Store: s, Now: func() string { return point.ISO }, Policy: ResolveRolePolicy(map[string]string{})}
	view := &AssignmentView{R: r, Clock: func() float64 { return point.Now }, Policy: DefaultRetryPolicy,
		Program: func() []string { return []string{"codex-session-relay"} }}
	var answer any
	switch point.Op {
	case "state":
		answer, err = view.State(ctx(), point.Args["relationship"])
	case "for_issue":
		answer, err = view.ForIssue(ctx(), point.Args["issue"])
	case "mark":
		answer, err = view.Mark(ctx(), point.Args["relationship"], "merged", point.Args["evidence"], point.Args["actor"], point.Args["expected_event"])
	case "register":
		in := Registration{
			Parent:            Endpoint{parent, host, ns("/parent"), nullString()},
			Child:             Endpoint{point.Args["child"], host, ns(point.Root), nullString()},
			IssueKey:          point.Args["issue"],
			ArtifactRoots:     []string{point.Root},
			AllowedRecipients: []string{parent},
			DispatchRequestID: point.Args["dispatch"],
			DispatchTurnID:    ns(point.Args["turn"]),
			Supersedes:        point.Args["supersedes"],
		}
		var x Relationship
		x, err = r.Register(ctx(), in)
		if err == nil {
			answer = x.ContractRecord()
		}
	default:
		t.Fatalf("unknown op %q", point.Op)
	}
	return outcome(t, answer, err), dir
}

// withoutStoreIdentity is answer with the relay store's physical identity (device, inode), which
// every run's store has anew, written as placeholders after asserting it is present.
func withoutStoreIdentity(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	relay := obj(obj(answer["ok"])["relay"])
	if relay == nil {
		return answer
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	identity := obj(obj(obj(out["ok"])["relay"])["store"])
	if identity["inode"] == nil || identity["device"] == nil {
		t.Fatalf("store identity missing: %v", identity)
	}
	identity["inode"], identity["device"] = "<INODE>", "<DEVICE>"
	return out
}

// samePoint compares one checkpoint's answer whole with the golden (the Go store's directory
// written <STATE>, its identity masked) and returns Go's answer.
func samePoint(t *testing.T, name string, index int) map[string]any {
	t.Helper()
	got, dir := replay(t, assignmentScenario(t, name)[index])
	key := fmt.Sprintf("%s[%d]", name, index)
	golden.CheckJSON(t, key, withoutStoreIdentity(t, got), golden.Substitute(dir, "<STATE>"))
	return got
}

// sameScenario compares every checkpoint of a scenario and returns Go's answers.
func sameScenario(t *testing.T, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i := range assignmentScenario(t, name) {
		out = append(out, samePoint(t, name, i))
	}
	return out
}

func okOf(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	ok, present := answer["ok"].(map[string]any)
	if !present {
		t.Fatalf("expected an answer, got %v", answer)
	}
	return ok
}

func refusedOf(answer map[string]any) string {
	r, _ := answer["refused"].(map[string]any)
	s, _ := r["reason"].(string)
	return s
}
