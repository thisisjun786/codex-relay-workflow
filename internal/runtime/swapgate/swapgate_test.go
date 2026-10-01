package swapgate_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	expected "github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestMain(m *testing.M) {
	golden.Helper()
	testsupport.Main(m)
}

// The inputs the Python generator used, by name: every envelope and presence.
func envelopes(t *testing.T) map[string]record.Object {
	t.Helper()
	out := map[string]record.Object{
		"running":        {{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "running", Value: true}}}, {Key: "command", Value: []any{"relay", "service", "status"}}},
		"stopped":        {{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "running", Value: false}}}, {Key: "command", Value: []any{"relay", "service", "status"}}},
		"failed":         {{Key: "ok", Value: false}, {Key: "unreadable", Value: "no binary"}, {Key: "command", Value: []any{"relay"}}},
		"stderr":         {{Key: "ok", Value: false}, {Key: "stderr", Value: "boom"}, {Key: "command", Value: []any{"relay"}}},
		"bare-failure":   {{Key: "ok", Value: false}},
		"no-payload":     {{Key: "ok", Value: true}, {Key: "payload", Value: nil}, {Key: "command", Value: []any{"relay"}}},
		"list-payload":   {{Key: "ok", Value: true}, {Key: "payload", Value: []any{int64(1)}}, {Key: "command", Value: []any{"relay"}}},
		"string-running": {{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "running", Value: "yes"}}}, {Key: "command", Value: []any{"relay"}}},
	}
	contents := func(fields ...record.Object) record.Object {
		c := record.Object{}
		for _, f := range fields {
			c = append(c, f...)
		}
		return record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "contents", Value: c}}}, {Key: "command", Value: []any{"relay", "doctor"}}}
	}
	out["open-0"] = contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: int64(0)}})
	out["open-2"] = contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: int64(2)}})
	out["open-bool"] = contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: true}})
	out["open-str"] = contents(record.Object{{Key: "available", Value: true}, {Key: "openAttempts", Value: "2"}})
	out["unavailable"] = contents(record.Object{{Key: "available", Value: false}, {Key: "detail", Value: "not readable"}})
	out["no-contents"] = record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{}}, {Key: "command", Value: []any{"relay", "doctor"}}}
	return out
}

var presences = map[string]record.Object{
	"none":       nil,
	"absent":     {{Key: "readable", Value: true}, {Key: "present", Value: false}, {Key: "dbPath", Value: "/s/relay.sqlite3"}, {Key: "command", Value: nil}},
	"present":    {{Key: "readable", Value: true}, {Key: "present", Value: true}, {Key: "dbPath", Value: "/s/relay.sqlite3"}, {Key: "command", Value: nil}},
	"unreadable": {{Key: "readable", Value: false}, {Key: "present", Value: nil}, {Key: "dbPath", Value: "/s/relay.sqlite3"}, {Key: "detail", Value: "PermissionError: denied"}, {Key: "command", Value: nil}},
}

func schemas() map[string]record.Object {
	objects := func(pairs ...string) record.Object {
		o := record.Object{}
		for i := 0; i < len(pairs); i += 2 {
			o = append(o, record.Object{{Key: pairs[i], Value: pairs[i+1]}}...)
		}
		return o
	}
	sameObjects := objects("table a", "CREATE TABLE a (x TEXT)", "table b", "CREATE TABLE b (y TEXT)")
	held := func(o any) record.Object {
		return record.Object{{Key: "readable", Value: true}, {Key: "present", Value: true}, {Key: "objects", Value: o}, {Key: "dbPath", Value: "/d"}}
	}
	return map[string]record.Object{
		"same":       held(sameObjects),
		"narrow":     held(objects("table a", "CREATE TABLE a (x TEXT)")),
		"wide":       held(append(append(record.Object{}, sameObjects...), record.Object{{Key: "index c", Value: "CREATE INDEX c ON a (x)"}}...)),
		"differs":    held(objects("table a", "CREATE TABLE a (x TEXT, y INT)", "table b", "CREATE TABLE b (y TEXT)")),
		"spaced":     held(objects("table a", "CREATE  TABLE   a (x TEXT)", "table b", "CREATE TABLE b\n (y TEXT)")),
		"absent":     {{Key: "readable", Value: true}, {Key: "present", Value: false}, {Key: "objects", Value: nil}, {Key: "dbPath", Value: "/d"}},
		"unreadable": {{Key: "readable", Value: false}, {Key: "detail", Value: "denied"}},
		"names-only": held([]any{"table a"}),
		"no-detail":  {{Key: "readable", Value: false}},
	}
}

// Every cell swapgate.py fills, and the verdict it decides, is the golden's (which began as
// Python's answers): the daemon cell from the service reading, the in-flight cell from presence
// then openAttempts, the schema cell over whole CREATE statements, and the verdict over every
// declared cell.
func TestCellsAndVerdicts(t *testing.T) {
	same := func(key string, got any) { t.Helper(); expected.Check(t, key, []byte(golden.Canon(got))) }
	envs := envelopes(t)
	for name, envelope := range envs {
		same("daemon "+name, swapgate.DaemonCell(envelope))
		for presence, value := range presences {
			got := swapgate.InflightCell(envelope, value)
			same("inflight "+name+"/"+presence, got)
			if name == "list-payload" && presence == "absent" {
				// Python raised out of inflight_cell here; Go answers, and never as a count.
				if record.Get(got, "readable") == true && record.Get(got, "answer") != swapgate.NoAttempts {
					t.Errorf("inflight %s/%s: %s", name, presence, golden.Canon(got))
				}
			}
		}
	}
	all := schemas()
	for storeName, store := range all {
		for candidateName, candidate := range all {
			same("schema "+storeName+"|"+candidateName, swapgate.SchemaCell(store, candidate))
		}
	}
	inputs, err := reading.Decode(expected.Fixture(t, "normalised-inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range golden.List(inputs) {
		got := swapgate.Normalised(input)
		var value any
		if got != nil {
			value = *got
		}
		same("normalised "+golden.Canon(input), value)
	}
	for _, daemon := range []string{"running", "stopped", "failed", "missing"} {
		for _, inflight := range []string{"open-0", "open-2", "unavailable", "missing"} {
			for _, schema := range []string{"same|same", "narrow|same", "unreadable|same", "absent|same", "missing"} {
				cells := map[string]record.Object{}
				if daemon != "missing" {
					cells["daemon"] = swapgate.DaemonCell(envs[daemon])
				}
				if inflight != "missing" {
					cells["inFlight"] = swapgate.InflightCell(envs[inflight], nil)
				}
				if schema != "missing" {
					storeName, candidateName, _ := strings.Cut(schema, "|")
					cells["storeSchema"] = swapgate.SchemaCell(all[storeName], all[candidateName])
				}
				same("decide "+golden.Canon([]any{daemon, inflight, schema}), swapgate.Decide(cells))
			}
		}
	}
}

func TestBlockingComesOffTheDeclaredCells(t *testing.T) {
	if got := swapgate.Decide(nil); record.Get(got, "verdict") != swapgate.Unestablished || len(golden.List(record.Get(got, "unreadable"))) != len(swapgate.Cells) {
		t.Fatalf("a gate nobody read: %s", golden.Canon(got))
	}
	if swapgate.Blocking("daemon", swapgate.Cell("RUNNING", false, "", nil, nil)) != nil {
		t.Fatal("an unreadable cell answered")
	}
}

// Store presence and the store's schema are read without creating anything: an absent store
// is established absence (NO_STORE, no attempt open), and a store a Go relay created holds
// exactly the schema this build declares, so the gate agrees, and the read leaves the state
// directory's file set as it found it.
func TestStoreReadingsCreateNothingAndAgreeWithTheDeclaredSchema(t *testing.T) {
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	presence := swapgate.StorePresence(state, "")
	if record.Get(presence, "present") != false || record.Get(presence, "readable") != true {
		t.Fatalf("an absent store: %s", golden.Canon(presence))
	}
	cell := swapgate.InflightCell(record.Object{{Key: "ok", Value: true}, {Key: "payload", Value: record.Object{{Key: "contents", Value: record.Object{{Key: "available", Value: false}}}}}}, presence)
	if record.Get(cell, "answer") != swapgate.NoAttempts || record.Get(cell, "readable") != true {
		t.Fatalf("no store means no attempt open: %s", golden.Canon(cell))
	}
	declared := swapgate.DeclaredSchema(ctx)
	if record.Get(declared, "readable") != true {
		t.Fatalf("the declared schema: %s", golden.Canon(declared))
	}
	if got := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared); record.Get(got, "answer") != swapgate.NoStore {
		t.Fatalf("an absent store's schema: %s", golden.Canon(got))
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Fatalf("reading an absent store created %v", entries)
	}
	s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := listing(t, state)
	held := swapgate.StoreSchema(ctx, state, "")
	if record.Get(held, "readable") != true {
		t.Fatalf("a created store: %s", golden.Canon(held))
	}
	if got := swapgate.SchemaCell(held, declared); record.Get(got, "answer") != swapgate.Agrees {
		t.Fatalf("a store this build created: %s", golden.Canon(got))
	}
	if after := listing(t, state); after != before {
		t.Fatalf("the read changed the state directory:\n before %s\n after  %s", before, after)
	}
	// With a relay holding the store open, both sidecars exist and are read, not re-created.
	live, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.DB.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS only_in_wal (x)"); err != nil {
		t.Fatal(err)
	}
	during := listing(t, state)
	if got := swapgate.SchemaCell(swapgate.StoreSchema(ctx, state, ""), declared); record.Get(got, "answer") != swapgate.Narrows || !strings.Contains(scopeText(got), "table only_in_wal") {
		t.Fatalf("a table committed only to the WAL was not read: %s", golden.Canon(got))
	}
	if after := listing(t, state); strings.Count(after, ",") != strings.Count(during, ",") {
		t.Fatalf("the read changed the state directory's file set:\n before %s\n after  %s", during, after)
	}
	if count := len(golden.Obj(record.Get(declared, "objects"))); count < 100 {
		t.Fatalf("the declared schema holds only %d objects", count)
	}
}

func scopeText(cell record.Object) string { return golden.Canon(record.Get(cell, "detail")) }

func listing(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		info, _ := e.Info()
		names = append(names, fmt.Sprintf("%s:%d", e.Name(), info.Size()))
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

// The candidate's schema is whatever the candidate binary prints for `crw doctor
// declared-schema --json`; a candidate that cannot be run or does not answer JSON is an
// unreadable cell, never an agreeing one.
func TestCandidateSchemaIsAskedOfTheCandidate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	answering := filepath.Join(dir, "crw")
	script := "#!/bin/sh\n[ \"$1 $2 $3\" = \"doctor declared-schema --json\" ] || exit 9\nprintf '%s\\n' '{\"readable\": true, \"objects\": {\"table a\": \"CREATE TABLE a (x)\"}, \"schemaVersion\": \"1\", \"detail\": null}'\n"
	if err := os.WriteFile(answering, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got := swapgate.CandidateSchema(ctx, answering)
	if record.Get(got, "readable") != true || golden.Canon(record.Get(got, "command")) != golden.Canon([]any{answering, "doctor", "declared-schema", "--json"}) {
		t.Fatalf("a candidate that answers: %s", golden.Canon(got))
	}
	silent := filepath.Join(dir, "silent")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\necho oops >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{silent, filepath.Join(dir, "missing")} {
		got := swapgate.CandidateSchema(ctx, candidate)
		cell := swapgate.SchemaCell(swapgate.StoreSchema(ctx, filepath.Join(dir, "state"), ""), got)
		if record.Get(got, "readable") != false || record.Get(cell, "readable") != false || record.Get(cell, "answer") != reading.Unreadable {
			t.Fatalf("%s: %s / %s", candidate, golden.Canon(got), golden.Canon(cell))
		}
	}
}

// A candidate that failed is not asked again for what it printed on the way out: a nonzero
// exit or a signal after a well-formed answer leaves the schema cell unread, so a gate whose
// other cells are clear stays UNESTABLISHED rather than ALLOWED. What it printed is kept as a
// diagnostic. An answer of readable false at exit 0 is the candidate's own answer and is kept.
func TestAFailedCandidateIsUnreadableWhateverItPrinted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	answer := `{"readable": true, "objects": {}, "schemaVersion": "1", "detail": null}`
	clear := map[string]swapgate.Object{
		"daemon":   swapgate.Cell("STOPPED", true, "not running", nil, false),
		"inFlight": swapgate.Cell(swapgate.NoAttempts, true, "no attempt is open", nil, swapgate.NoAttempts),
	}
	// No store exists at this selection, so a readable candidate answer would settle the cell
	// as NO_STORE and the gate as ALLOWED.
	storeAnswer := swapgate.StoreSchema(ctx, filepath.Join(dir, "state"), "")
	for _, failure := range []struct{ name, tail, ended string }{
		{"exits-1", "echo broken >&2\nexit 1\n", "exit status 1"},
		{"killed", "kill -KILL $$\n", "signal: killed"},
	} {
		candidate := filepath.Join(dir, failure.name)
		script := "#!/bin/sh\nprintf '%s\\n' '" + answer + "'\n" + failure.tail
		if err := os.WriteFile(candidate, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		got := swapgate.CandidateSchema(ctx, candidate)
		detail, _ := record.Get(got, "detail").(string)
		if record.Get(got, "readable") != false || record.Get(got, "objects") != nil || !strings.Contains(detail, failure.ended) || !strings.Contains(detail, "stdout: "+answer) {
			t.Errorf("%s: a failed candidate's JSON was read: %s", failure.name, golden.Canon(got))
		}
		cells := map[string]swapgate.Object{"storeSchema": swapgate.SchemaCell(storeAnswer, got)}
		for name, cell := range clear {
			cells[name] = cell
		}
		if verdict := record.Get(swapgate.Decide(cells), "verdict"); verdict != swapgate.Unestablished {
			t.Errorf("%s: a failed candidate made the gate %v", failure.name, verdict)
		}
	}
	refusing := filepath.Join(dir, "refusing")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"readable\": false, \"objects\": null, \"schemaVersion\": \"1\", \"detail\": \"no DDL\"}'\n"
	if err := os.WriteFile(refusing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := swapgate.CandidateSchema(ctx, refusing); record.Get(got, "readable") != false || record.Get(got, "detail") != "no DDL" {
		t.Fatalf("a candidate's own readable-false answer at exit 0 was not kept: %s", golden.Canon(got))
	}
}

// A store whose WAL holds frames with no shared-memory index beside it (an unclean shutdown)
// has commits an immutable read of the main file would miss, here a table the candidate does
// not declare: its schema is unreadable, never AGREES, and the read creates nothing. An empty
// WAL beside no index holds no commit, so the main file is read and agrees. A relay.sqlite3 that
// is a symbolic link is read as SQLite reads it, with the sidecars beside the file it names: a
// table committed only to the live WAL there makes the gate NARROWS, and a crashed WAL there is
// unreadable, never an AGREES taken from the main file alone.
func TestAStoreWhoseWALHasNoIndexIsUnreadable(t *testing.T) {
	ctx := context.Background()
	live := filepath.Join(t.TempDir(), "live")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(ctx, filepath.Join(live, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	checkpointed, err := os.ReadFile(filepath.Join(live, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	s, err = store.Open(ctx, filepath.Join(live, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS only_in_wal (x)"); err != nil {
		t.Fatal(err)
	}
	wal, err := os.ReadFile(filepath.Join(live, "relay.sqlite3-wal"))
	if err != nil || len(wal) <= 32 {
		t.Fatalf("the live WAL holds no frame: %d %v", len(wal), err)
	}
	crashed := filepath.Join(t.TempDir(), "crashed")
	if err := os.MkdirAll(crashed, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{"relay.sqlite3": checkpointed, "relay.sqlite3-wal": wal} {
		if err := os.WriteFile(filepath.Join(crashed, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	declared := swapgate.DeclaredSchema(ctx)
	linkTo := func(target string) string {
		state := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(target, "relay.sqlite3"), filepath.Join(state, "relay.sqlite3")); err != nil {
			t.Fatal(err)
		}
		return state
	}
	linkedLive, linkedCrashed := linkTo(live), linkTo(crashed)
	liveBefore, crashedBefore := listing(t, live), listing(t, crashed)
	if got := swapgate.SchemaCell(swapgate.StoreSchema(ctx, linkedLive, ""), declared); record.Get(got, "answer") != swapgate.Narrows || !strings.Contains(scopeText(got), "table only_in_wal") {
		t.Errorf("a link to a live store was read without the WAL beside the file it names: %s", golden.Canon(got))
	}
	held := swapgate.StoreSchema(ctx, linkedCrashed, "")
	if cell := swapgate.SchemaCell(held, declared); record.Get(held, "readable") != false || !strings.Contains(scopeText(held), "shared-memory index") || record.Get(cell, "answer") != reading.Unreadable {
		t.Errorf("a link to a WAL with frames and no index was read as the store's schema: %s / %s", golden.Canon(held), golden.Canon(cell))
	}
	for _, state := range []string{linkedLive, linkedCrashed} {
		if entries, _ := os.ReadDir(state); len(entries) != 1 {
			t.Errorf("the read created a sidecar beside the link: %v", entries)
		}
	}
	if after := listing(t, crashed); after != crashedBefore {
		t.Errorf("the read through a link changed the crashed directory:\n before %s\n after  %s", crashedBefore, after)
	}
	if after := listing(t, live); strings.Count(after, ",") != strings.Count(liveBefore, ",") {
		t.Errorf("the read through a link changed the live directory's file set:\n before %s\n after  %s", liveBefore, after)
	}
	before := listing(t, crashed)
	held = swapgate.StoreSchema(ctx, crashed, "")
	cell := swapgate.SchemaCell(held, declared)
	if record.Get(held, "readable") != false || !strings.Contains(scopeText(held), "shared-memory index") || record.Get(cell, "answer") != reading.Unreadable {
		t.Errorf("a WAL with frames and no index was read as the store's schema: %s / %s", golden.Canon(held), golden.Canon(cell))
	}
	if after := listing(t, crashed); after != before {
		t.Errorf("the read changed the state directory:\n before %s\n after  %s", before, after)
	}
	if err := os.WriteFile(filepath.Join(crashed, "relay.sqlite3-wal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := swapgate.SchemaCell(swapgate.StoreSchema(ctx, crashed, ""), declared); record.Get(got, "answer") != swapgate.Agrees {
		t.Errorf("an empty WAL beside no index: %s", golden.Canon(got))
	}
}

// A candidate that leaves a process behind holding its output does not hold the gate: at the
// deadline the candidate is killed and its output waited for scope.WaitDelay at most, and a
// candidate that exits while such a process keeps its output open is not read, so the schema
// cell is unreadable within its bound rather than when that process exits.
func TestACandidateThatLeavesItsOutputOpenIsBounded(t *testing.T) {
	defer func(previous time.Duration) { scope.WaitDelay = previous }(scope.WaitDelay)
	scope.WaitDelay = time.Second
	dir := t.TempDir()
	answer := `{"readable": true, "objects": {}, "schemaVersion": "1", "detail": null}`
	for _, c := range []struct {
		name, tail string
		deadline   time.Duration
		said       string
	}{
		{"hangs", "exec sleep 20\n", time.Second, "signal: killed"},
		{"exits", "printf '%s\\n' '" + answer + "'\nexit 0\n", 20 * time.Second, "kept its output open 1s past that"},
	} {
		candidate := filepath.Join(dir, c.name)
		pidFile := candidate + ".pid"
		script := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\n" + c.tail
		if err := os.WriteFile(candidate, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), c.deadline)
		started := time.Now()
		got := swapgate.CandidateSchema(ctx, candidate)
		elapsed := time.Since(started)
		cancel()
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		if bound := 10 * time.Second; elapsed > bound {
			t.Errorf("%s: the candidate's reading took %s, past its %s bound, waiting for a process it left behind", c.name, elapsed, bound)
		}
		detail, _ := record.Get(got, "detail").(string)
		if record.Get(got, "readable") != false || record.Get(got, "objects") != nil || !strings.Contains(detail, c.said) {
			t.Errorf("%s: %s", c.name, golden.Canon(got))
		}
	}
}
