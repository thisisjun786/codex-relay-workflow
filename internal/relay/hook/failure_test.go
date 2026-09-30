package hook

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func managedFixture(t *testing.T) (string, string, Object) {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "work")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	assignment := delivery.AssignmentID("dispatch")
	directory, err := delivery.AssignmentDir(root, workspace, assignment)
	if err != nil {
		t.Fatal(err)
	}
	facts := map[string]Object{"intent.json": {{Key: "dispatchRequestIdHash", Value: assignment}}, "bound.json": {{Key: "sessionId", Value: "s"}}, "relationship.json": {{Key: "relationshipId", Value: "r"}}, "claims/s/claim.json": {{Key: "sessionId", Value: "s"}, {Key: "dispatchRequestId", Value: "dispatch"}}}
	names := []string{}
	for name := range facts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		writeTest(t, filepath.Join(directory, name), []byte(pyjson.Dumps(facts[name], pyjson.Options{})))
	}
	return root, directory, Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "cwd", Value: workspace}}
}
func Test33CancelledEvaluationCannotSpendHold(t *testing.T) {
	root, directory, stop := managedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := Evaluate(ctx, stop, GuardOptions{Root: root, Mode: Hold})
	if err != context.Canceled && get(v, "decision") != "release" {
		t.Fatal(v, err)
	}
	if _, err = os.Stat(filepath.Join(directory, "hook/s/t/hold.json")); !os.IsNotExist(err) {
		t.Fatalf("cancelled evaluation spent hold: %v", err)
	}
}
func Test33UnmanagedDoesNotCreateDatabase(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "missing/relay.sqlite3")
	v, err := Evaluate(context.Background(), Object{}, GuardOptions{Root: root, DBPath: db, Mode: Hold})
	if err != nil || get(v, "state") != "unmanaged" {
		t.Fatal(v, err)
	}
	if _, err = os.Stat(db); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
func Test33ReadOnlyCannotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	ctx := context.Background()
	s, err := fixtureStore(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(ctx, path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err = ro.ExecContext(ctx, "INSERT INTO schema_meta(key,value) VALUES('forbidden','write')"); err == nil {
		t.Fatal("read-only connection admitted a write")
	}
}
func Test33HoldCountersCorruptionAndWindow(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	options := GuardOptions{Root: root, Now: "2026-01-01T00:00:00Z", Mode: Hold}
	for _, turn := range []string{"one", "two"} {
		if ok, err := reserveHold(context.Background(), directory, "s", turn, options); err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	foreign := filepath.Join(root, "b/hook/foreign/x/hold.json")
	writeTest(t, foreign, []byte("{"))
	counts, bad, unreadable := HoldCounters(context.Background(), directory, "s", "three", options.Now, root)
	if bad != "" || unreadable != "" || get(counts, "holdsThisSessionWindow") != int64(2) {
		t.Fatal(counts, bad, unreadable)
	}
	writeTest(t, filepath.Join(root, "c/hook/s/y/hold.json"), []byte(`{"at":null}`))
	counts, bad, unreadable = HoldCounters(context.Background(), directory, "s", "three", options.Now, root)
	if bad != "" || unreadable != "" || get(counts, "holdsThisSessionWindow") != int64(3) {
		t.Fatal(counts, bad, unreadable)
	}
	counts, bad, unreadable = HoldCounters(context.Background(), directory, "s", "three", "2026-01-01T02:00:00Z", root)
	if bad != "" || unreadable != "" || get(counts, "holdsThisSessionWindow") != int64(1) {
		t.Fatal(counts, bad, unreadable)
	}
	writeTest(t, filepath.Join(directory, "hook/s/one/hold.json"), []byte("{"))
	_, bad, _ = HoldCounters(context.Background(), directory, "s", "three", options.Now, root)
	if bad != "hook/s" {
		t.Fatal(bad)
	}
}
func Test33MarkerFIFODeadline(t *testing.T) {
	root, directory, stop := managedFixture(t)
	if err := os.Remove(filepath.Join(directory, "bound.json")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(directory, "bound.json"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	v, err := Evaluate(ctx, stop, GuardOptions{Root: root, Mode: Hold})
	if err != nil || get(v, "state") != "state_unreadable" || get(v, "decision") != "release" {
		t.Fatal(v, err)
	}
}
