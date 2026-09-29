package scope_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) {
	golden.Helper()
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// service_state keeps its four answers apart with the invocation first: a command that did
// not run is never a stopped daemon.
func TestServiceStateIsPythons(t *testing.T) {
	gate := golden.Obj(golden.Section(t, "swapGate"))
	for _, f := range golden.Obj(record.Get(gate, "serviceState")) {
		envelope := envelopeOf(t, f.Key)
		if envelope == nil {
			continue
		}
		if got := scope.ServiceState(envelope); golden.Canon(got) != golden.Canon(f.Value) {
			t.Errorf("%s\n go: %s\n py: %s", f.Key, golden.Canon(got), golden.Canon(f.Value))
		}
	}
}

func envelopeOf(t *testing.T, name string) record.Object {
	t.Helper()
	command := []any{"relay", "service", "status"}
	switch name {
	case "running", "stopped":
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "running", Value: name == "running"}}}, {Key: "command", Value: command}}
	case "failed":
		return record.Object{{Key: "ok", Value: false}, {Key: "unreadable", Value: "no binary"}, {Key: "command", Value: []any{"relay"}}}
	case "stderr":
		return record.Object{{Key: "ok", Value: false}, {Key: "stderr", Value: "boom"}, {Key: "command", Value: []any{"relay"}}}
	case "bare-failure":
		return record.Object{{Key: "ok", Value: false}}
	case "no-payload":
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: nil}}
	case "list-payload":
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: []any{int64(1)}}}
	case "string-running":
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "running", Value: "yes"}}}}
	}
	return nil
}

// summarise answers from the selected store when one is selected, never borrowing the
// discovered store's fields, and says which reading answered.
func TestSummariseIsPythons(t *testing.T) {
	section := golden.Obj(golden.Section(t, "scope"))
	env := scope.Env{}
	for _, f := range golden.Obj(record.Get(section, "env")) {
		env = env.With(f.Key, f.Value.(string))
	}
	readings := golden.Obj(record.Get(section, "readings"))
	for _, f := range golden.Obj(record.Get(section, "summaries")) {
		got := scope.Summarise(golden.Obj(record.Get(readings, f.Key)), env, record.Object{{Key: "state", Value: "x"}})
		want := record.Delete(append(record.Object{}, golden.Obj(f.Value)...), "assignmentFind")
		if golden.Canon(got) != golden.Canon(want) {
			t.Errorf("%s\n go: %s\n py: %s", f.Key, golden.Canon(got), golden.Canon(want))
		}
	}
	siblings := golden.List(record.Get(section, "siblings"))
	for i, payload := range []record.Object{{{Key: "stateDirectory", Value: "/s"}}, {{Key: "siblingStores", Value: nil}},
		{{Key: "siblingStores", Value: record.Object{{Key: "checked", Value: false}, {Key: "reason", Value: "chosen"}}}},
		{{Key: "siblingStores", Value: record.Object{{Key: "checked", Value: true}}}}} {
		if got := scope.SiblingReading(payload); got != siblings[i] {
			t.Errorf("siblings %d: %q vs %q", i, got, siblings[i])
		}
	}
}

// Every relay reading is a subprocess of the selected executable: its JSON parsed, its
// invocation recorded, a failure to run it an unreadable envelope; discovery removes the
// state override and a selection sets both selectors.
func TestRelayRunsTheSelectedExecutable(t *testing.T) {
	dir := t.TempDir()
	relay := filepath.Join(dir, "codex-session-relay")
	script := "#!/bin/sh\nprintf '{\"argv\": \"%s\", \"state\": \"%s\"}\\n' \"$*\" \"$CODEX_SESSION_RELAY_STATE\"\n[ \"$1\" = fail ] && { echo broken >&2; exit 4; }\nexit 0\n"
	if err := os.WriteFile(relay, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := scope.Env(os.Environ()).With(scope.StateEnv, "/inherited")
	discovery := scope.Relay(ctx, []string{"doctor"}, relay, "/sock", "", env, true, 0)
	payload := record.Get(discovery, "payload").(record.Object)
	if record.Get(discovery, "ok") != true || record.Get(payload, "argv") != "--socket /sock doctor" || record.Get(payload, "state") != "" {
		t.Fatalf("discovery: %s", golden.Canon(discovery))
	}
	selected := scope.Relay(ctx, []string{"doctor"}, relay, "", "/chosen", env, false, 0)
	payload = record.Get(selected, "payload").(record.Object)
	if record.Get(payload, "argv") != "--state /chosen doctor" || record.Get(payload, "state") != "/chosen" {
		t.Fatalf("selection: %s", golden.Canon(selected))
	}
	failed := scope.Relay(ctx, []string{"fail"}, relay, "", "", env, false, 0)
	if record.Get(failed, "ok") != false || record.Get(failed, "exitCode") != int64(4) || record.Get(failed, "stderr") != "broken" {
		t.Fatalf("a failing command: %s", golden.Canon(failed))
	}
	missing := scope.Relay(ctx, []string{"doctor"}, filepath.Join(dir, "missing"), "", "", env, false, 0)
	if record.Get(missing, "ok") != false || !strings.HasPrefix(record.Text(missing, "unreadable"), "FileNotFoundError: [Errno 2] No such file or directory") {
		t.Fatalf("a missing executable: %s", golden.Canon(missing))
	}
	if err := os.WriteFile(relay, []byte("#!/bin/sh\necho not json\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := scope.Relay(ctx, []string{"doctor"}, relay, "", "", env, false, 0); record.Get(got, "payload") != nil || record.Get(got, "unreadable") != "the relay did not return JSON" {
		t.Fatalf("a non-JSON answer: %s", golden.Canon(got))
	}
}

// Every database on disk is listed and none is interpreted, so a relay too old to report
// siblings cannot hide one.
func TestFilesystemCandidatesListEveryStoreFile(t *testing.T) {
	root := t.TempDir()
	relayRoot := filepath.Join(root, "codex-session-relay")
	for _, name := range []string{"relay.sqlite3", "default/relay.sqlite3", "scope/operations-scope.sqlite3", "scope/other.txt"} {
		path := filepath.Join(relayRoot, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := scope.Env{"XDG_STATE_HOME=" + root}
	var databases []string
	for _, entry := range scope.FilesystemCandidates(env) {
		databases = append(databases, record.Text(entry, "database"))
	}
	want := []string{filepath.Join(relayRoot, "relay.sqlite3"), filepath.Join(relayRoot, "default", "relay.sqlite3"), filepath.Join(relayRoot, "scope", "operations-scope.sqlite3")}
	if strings.Join(databases, "|") != strings.Join(want, "|") {
		t.Fatalf("listed %v", databases)
	}
}
