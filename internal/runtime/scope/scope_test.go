package scope_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	expected "github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestMain(m *testing.M) {
	golden.Helper()
	testsupport.Main(m)
}

// service_state keeps its four answers apart with the invocation first: a command that did
// not run is never a stopped daemon. Each answer is the golden, which began as Python's.
func TestServiceStateReadings(t *testing.T) {
	for _, name := range []string{"running", "stopped", "failed", "stderr", "bare-failure", "no-payload", "list-payload", "string-running",
		"open-0", "open-2", "open-bool", "open-str", "unavailable", "no-contents"} {
		expected.Check(t, name, []byte(golden.Canon(scope.ServiceState(envelopeOf(t, name)))))
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
	case "no-contents":
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{}}, {Key: "command", Value: []any{"relay", "doctor"}}}
	}
	// The doctor envelopes swapgate's tests read too: a status the service never sends, so the
	// state says it carries no boolean running.
	contents := func(fields record.Object) record.Object {
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "contents", Value: fields}}}, {Key: "command", Value: []any{"relay", "doctor"}}}
	}
	switch name {
	case "open-0":
		return contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: int64(0)}})
	case "open-2":
		return contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: int64(2)}})
	case "open-bool":
		return contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: true}})
	case "open-str":
		return contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: "2"}})
	case "unavailable":
		return contents(record.Object{{Key: "available", Value: false}, {Key: "detail", Value: "not readable"}})
	}
	return nil
}

// summarise answers from the selected store when one is selected, never borrowing the
// discovered store's fields, and says which reading answered. The readings are a fixture; each
// summary is the golden, which began as Python's without its assignmentFind.
func TestSummariseIsPythons(t *testing.T) {
	inputs, err := reading.Decode(expected.Fixture(t, "summarise-inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	env := scope.Env{}
	for _, f := range golden.Obj(record.Get(golden.Obj(inputs), "env")) {
		env = env.With(f.Key, f.Value.(string))
	}
	for _, f := range golden.Obj(record.Get(golden.Obj(inputs), "readings")) {
		got := scope.Summarise(golden.Obj(f.Value), env, record.Object{{Key: "state", Value: "x"}})
		expected.Check(t, "summary "+f.Key, []byte(golden.Canon(got)))
	}
	for i, payload := range []record.Object{{{Key: "stateDirectory", Value: "/s"}}, {{Key: "siblingStores", Value: nil}},
		{{Key: "siblingStores", Value: record.Object{{Key: "checked", Value: false}, {Key: "reason", Value: "chosen"}}}},
		{{Key: "siblingStores", Value: record.Object{{Key: "checked", Value: true}}}}} {
		expected.Check(t, fmt.Sprintf("siblings %d", i), []byte(scope.SiblingReading(payload)))
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
	if record.Get(missing, "ok") != false || !strings.HasSuffix(record.Text(missing, "unreadable"), "missing: no such file or directory") {
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

// A relay the deadline ended has no exit status, and nothing the process printed before it hung
// is kept as its answer, so the service state names the timeout. A relay a signal ended exits -N.
func TestARelayTheDeadlineEndedIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay")
	if err := os.WriteFile(relay, []byte("#!/bin/sh\nprintf '%s\\n' '{\"running\": false}'\nexec sleep 20\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	env := scope.Env(os.Environ())
	got := scope.Relay(ctx, []string{"service", "status"}, relay, "", "", env, false, time.Second)
	said := `the relay command ["` + relay + `", "service", "status"] did not finish within 1s`
	want := record.Object{{Key: "ok", Value: false}, {Key: "command", Value: []any{relay, "service", "status"}}, {Key: "unreadable", Value: said}}
	if golden.Canon(got) != golden.Canon(want) {
		t.Fatalf("a relay the deadline ended\n go: %s\n py: %s", golden.Canon(got), golden.Canon(want))
	}
	if state := scope.ServiceState(got); record.Get(state, "state") != "ACCESS_ERROR" || record.Get(state, "detail") != "the service could not be asked: "+said {
		t.Fatalf("the service state of a relay that timed out: %s", golden.Canon(state))
	}
	if err := os.WriteFile(relay, []byte("#!/bin/sh\nprintf '%s\\n' '{\"running\": false}'\nkill -TERM $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	signalled := scope.Relay(ctx, []string{"service", "status"}, relay, "", "", env, false, 0)
	if record.Get(signalled, "ok") != false || record.Get(signalled, "exitCode") != int64(-15) {
		t.Fatalf("a relay SIGTERM ended: %s", golden.Canon(signalled))
	}
}

// A process the relay leaves behind holding its stdout does not hold the reading: at the
// deadline the relay is killed and its output waited for WaitDelay at most, and a relay that
// exits while such a process keeps its output open is not read, since what it printed may not
// be all of it. Both answers come back within their bounds rather than when that process exits.
func TestARelayThatLeavesItsOutputOpenIsBounded(t *testing.T) {
	defer func(previous time.Duration) { scope.WaitDelay = previous }(scope.WaitDelay)
	scope.WaitDelay = time.Second
	dir := t.TempDir()
	relay := filepath.Join(dir, "relay")
	ctx := context.Background()
	env := scope.Env(os.Environ())
	for _, c := range []struct {
		name, tail string
		timeout    time.Duration
		said       string
	}{
		{"hangs", "exec sleep 20\n", time.Second, "the relay command "},
		{"exits", "printf '%s\\n' '{\"running\": false}'\nexit 0\n", 20 * time.Second, "the relay exited, but a process it left behind kept its output open 1s past that"},
	} {
		pidFile := filepath.Join(dir, c.name+".pid")
		script := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\n" + c.tail
		if err := os.WriteFile(relay, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		got := scope.Relay(ctx, []string{"service", "status"}, relay, "", "", env, false, c.timeout)
		elapsed := time.Since(started)
		killLeftBehind(t, pidFile)
		if bound := 10 * time.Second; elapsed > bound {
			t.Errorf("%s: the reading took %s, past its %s bound, waiting for a process the relay left behind", c.name, elapsed, bound)
		}
		if record.Get(got, "ok") != false || record.Get(got, "payload") != nil || !strings.HasPrefix(record.Text(got, "unreadable"), c.said) {
			t.Errorf("%s: %s", c.name, golden.Canon(got))
		}
	}
}

func killLeftBehind(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// A relay doctor payload may carry any JSON value where a path belongs. The stores it names
// are told apart as Python's == tells them apart, by value (1, 1.0 and true alike), and never
// by a comparison that panics on a list or an object.
func TestStoresSeenComparesPathsByValue(t *testing.T) {
	env := scope.Env{"XDG_STATE_HOME=/nonexistent-crw-scope-test", "HOME=/nonexistent-crw-scope-test"}
	decode := func(text string) any {
		value, err := reading.Decode([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, c := range []struct{ payload, python string }{
		{`{"siblingStores": {"checked": true, "withoutProvenance": [["a"], ["b"]]}}`,
			`[{"path": ["a"], "foundBy": "discovery: records no socket"}, {"path": ["b"], "foundBy": "discovery: records no socket"}]`},
		{`{"stateDirectory": {"a": 1}, "siblingStores": {"withoutProvenance": [{"a": 1}]}}`,
			`[{"path": {"a": 1}, "foundBy": "discovery; discovery: records no socket", "database": null, "kind": null}]`},
		{`{"stateDirectory": [1], "siblingStores": {"withoutProvenance": [[true], [1.0], [2]], "claimingThisSocket": [{"k": [1]}, {"k": [1.0]}]}}`,
			`[{"path": [1], "foundBy": "discovery; discovery: records no socket", "database": null, "kind": null}, {"path": [2], "foundBy": "discovery: records no socket"}, {"path": {"k": [1]}, "foundBy": "discovery: claims this socket", "database": null, "kind": null}]`},
	} {
		readings := record.Object{
			{Key: "discovery", Value: record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: decode(c.payload)}}},
			{Key: "selected", Value: record.Object{{Key: "ok", Value: false}, {Key: "skipped", Value: "none"}}},
			{Key: "rootCandidate", Value: record.Object{{Key: "skipped", Value: "none"}}},
		}
		if got := scope.StoresSeen(readings, env); golden.Canon(got) != golden.Canon(decode(c.python)) {
			t.Errorf("%s\n go: %s\n py: %s", c.payload, golden.Canon(got), golden.Canon(decode(c.python)))
		}
		if got := record.Get(scope.Summarise(readings, env, nil), "storesSeen"); golden.Canon(got) != golden.Canon(decode(c.python)) {
			t.Errorf("summarised %s\n go: %s\n py: %s", c.payload, golden.Canon(got), golden.Canon(decode(c.python)))
		}
	}
}

// A siblingStores that is not an object carries no conflict inventory: it could not be read,
// never that the inventory was checked.
func TestSiblingStoresThatIsNotAnObjectIsNotChecked(t *testing.T) {
	for _, c := range []struct {
		value any
		kind  string
	}{{[]any{"x"}, "an array"}, {"unavailable", "a string"}, {false, "a boolean"}, {int64(0), "a number"}, {[]any{}, "an array"}, {1.5, "a number"}} {
		payload := record.Object{{Key: "siblingStores", Value: c.value}}
		want := "not readable: siblingStores is " + c.kind + ", not an object, so the conflict inventory could not be read from it"
		if got := scope.SiblingReading(payload); got != want {
			t.Errorf("%s: %q", golden.Canon(c.value), got)
		}
		readings := record.Object{{Key: "discovery", Value: record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: payload}}}}
		if got := record.Get(scope.Summarise(readings, scope.Env{"XDG_STATE_HOME=/nonexistent-crw-scope-test"}, nil), "siblingDiscovery"); got != want {
			t.Errorf("summarised %s: %v", golden.Canon(c.value), got)
		}
	}
	if got := scope.SiblingReading(record.Object{{Key: "siblingStores", Value: record.Object{{Key: "checked", Value: "no"}}}}); got != "checked" {
		t.Errorf("an object without checked false, as Python reads it: %q", got)
	}
}
