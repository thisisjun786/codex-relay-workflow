package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Decision 25 across the runtimes, in process: the Go producer and the Go drain against the
// retained Python fence's (live, one Python process per batch). The built-binary candidate
// drain is internal/relay/service Test31GoCandidateDrainsPythonQueuedEntriesBuiltCLI.

type answer struct {
	code   int
	stdout string
}

// goCLI runs the Go relay CLI in process, as codex-session-relay, without touching ownership.
func goCLI(t *testing.T, argv ...string) answer {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.ExecuteAs(context.Background(), "codex-session-relay", argv, &stdout, &stderr)
	return answer{code, stdout.String()}
}

// pythonCLI runs each argv through the fence's cli.main in one Python process, byte for byte
// (argv that is not UTF-8 arrives surrogate-escaped, as on a command line).
func pythonCLI(t *testing.T, argvs ...[]string) []answer {
	t.Helper()
	var encoded [][]string
	for _, argv := range argvs {
		var items []string
		for _, a := range argv {
			items = append(items, base64.StdEncoding.EncodeToString([]byte(a)))
		}
		encoded = append(encoded, items)
	}
	input, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	script := `import base64, contextlib, io, json, os, sys
from codex_session_relay import cli
out = []
for argv in json.load(sys.stdin):
    printed = io.StringIO()
    with contextlib.redirect_stdout(printed):
        code = cli.main([os.fsdecode(base64.b64decode(a)) for a in argv])
    out.append([code, printed.getvalue()])
json.dump(out, sys.stdout)
`
	python := exec.Command(filepath.Join(repositoryRoot(t), ".venv", "bin", "python"), "-c", script)
	python.Stdin = bytes.NewReader(input)
	python.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := python.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("%v: %s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	var decoded [][2]any
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded) != len(argvs) {
		t.Fatalf("%v %s", err, raw)
	}
	out := make([]answer, len(decoded))
	for i, d := range decoded {
		out[i] = answer{int(d[0].(float64)), d[1].(string)}
	}
	return out
}

func object(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return value
}

// ownerOnlyState is an S the way both runtimes create one (0700).
func ownerOnlyState(t *testing.T, root, name string) string {
	t.Helper()
	state := filepath.Join(root, name)
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	return state
}

// setPhase republishes the mirror of a stopped store in phase draining (a transition in
// progress, as `takeover begin` publishes it) or back in phase active.
func setPhase(t *testing.T, dbPath, phase string) {
	t.Helper()
	r, err := ownership.ReadRecord(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	r.Phase, r.Transition = phase, nil
	if phase == "draining" {
		r.Transition = &ownership.Transition{ID: "t31-drain", From: r.Owner, To: map[string]string{"go": "python", "python": "go"}[r.Owner], TargetEpoch: r.Epoch + 1}
	}
	if err = ownership.Publish(dbPath, r, nil); err != nil {
		t.Fatal(err)
	}
}

func inboxEntries(t *testing.T, state string) []string {
	t.Helper()
	all, err := os.ReadDir(filepath.Join(state, "takeover-inbox"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range all {
		if !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// snapshotQuery reads a stopped or live store through a disposable copy, never beside it.
func snapshotQuery(t *testing.T, dbPath, query string, args ...any) []map[string]any {
	t.Helper()
	copyPath, cleanup, err := ownership.CopySnapshot(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	db, err := ownership.OpenExisting(t.Context(), copyPath, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{}
		for i, column := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			row[column] = values[i]
		}
		out = append(out, row)
	}
	return out
}

func meta(t *testing.T, dbPath, key string) (map[string]any, bool) {
	t.Helper()
	rows := snapshotQuery(t, dbPath, "SELECT value FROM schema_meta WHERE key=?", key)
	if len(rows) == 0 {
		return nil, false
	}
	return object(t, rows[0]["value"].(string)), true
}

// registerRelationship records one relationship through the given runtime's CLI and returns its ID.
func registerRelationship(t *testing.T, state, roots string, python bool) string {
	t.Helper()
	argv := []string{"--state", state, "register", "--parent-task", "parent", "--parent-host", "host", "--child-task", "child-1", "--child-host", "host",
		"--issue", "TEST-31", "--artifact-root", roots, "--allowed-recipient", "parent", "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", "turn-0"}
	var got answer
	if python {
		got = pythonCLI(t, argv)[0]
	} else {
		got = goCLI(t, argv...)
	}
	id, _ := object(t, got.stdout)["relationshipId"].(string)
	if got.code != 0 || id == "" {
		t.Fatalf("register: %+v", got)
	}
	return id
}

// fixedClock makes every delivery timestamp of the in-process Go CLI one instant, so a
// replayed receipt and its direct twin compare whole.
func fixedClock(t *testing.T) {
	t.Helper()
	previous := delivery.CommandClock
	delivery.CommandClock = &delivery.FakeClock{T: 1_800_000_000}
	t.Cleanup(func() { delivery.CommandClock = previous })
}

// twin copies a stopped store into its own directory, as a copy made before the requests.
func twin(t *testing.T, dbPath, state string) string {
	t.Helper()
	db, err := ownership.OpenExisting(t.Context(), dbPath, "rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(state, "relay.sqlite3")
	if err = os.WriteFile(copyPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Rehome(t, copyPath)
	return copyPath
}

// Python queues -> Go drains: receipts and acknowledgments the fence queued under a Go owner
// are applied by the Go owner's drain before its next writable command, and what they leave -
// every events and deliveries row, and each marker's answer - is exactly what the same
// commands applied directly by Go leave on a copy of the store made before them.
func Test31_python_queued_requests_apply_through_the_go_drain_as_direct_commands(t *testing.T) {
	home := pythonHome(t)
	roots := ownerOnlyState(t, home, "artifacts")
	if err := os.WriteFile(filepath.Join(roots, "report.txt"), []byte("the deliverable\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stateA := ownerOnlyState(t, home, "a")
	relationship := registerRelationship(t, stateA, roots, false)
	fixedClock(t)
	stateB := ownerOnlyState(t, home, "b")
	dbB := twin(t, filepath.Join(stateA, "relay.sqlite3"), stateB)
	requests := [][]string{
		{"emit", "--relationship", relationship, "--generation", "1", "--outcome", "failed", "--turn-status", "failed", "--turn-thread", "child-1", "--turn-id", "turn-1", "--continues-anchor", "turn-0"},
		{"emit", "--relationship", relationship, "--generation", "1", "--outcome", "interrupted", "--turn-status", "interrupted", "--turn-thread", "child-1", "--turn-id", "turn-2", "--continues-anchor", "turn-0", "--no-enqueue", "--attempt", "2"},
		{"emit", "--relationship", relationship, "--generation", "1", "--outcome", "ready_for_review", "--turn-status", "completed", "--turn-thread", "child-1", "--turn-id", "turn-3", "--continues-anchor", "turn-0", "--continuation-reason", "reviewable", "--artifact", filepath.Join(roots, "report.txt")},
		{"emit", "--relationship", relationship, "--generation", "1", "--outcome", "failed", "--turn-thread", "child-1", "--turn-id", "turn-4"},
		{"emit", "--relationship", "no-such-relationship", "--generation", "1", "--outcome", "failed", "--turn-thread", "child-1", "--turn-id", "turn-5"},
		{"ack", "--event", strings.Repeat("ab", 16), "--ack-turn", "parent-turn", "--ack-proof", "proof-1"},
		{"fault-notification-ack", "--notification", "notice-1", "--token", "token-1", "--ref", "receipt-1"},
	}
	var queued [][]string
	for _, request := range requests {
		queued = append(queued, append([]string{"--state", stateA}, request...))
	}
	for i, got := range pythonCLI(t, queued...) {
		if got.code != 0 || object(t, got.stdout)["status"] != "durably_queued" {
			t.Fatalf("request %d under the Go owner was not queued: %+v", i, got)
		}
	}
	if n := len(inboxEntries(t, stateA)); n != len(requests) {
		t.Fatalf("%d entries", n)
	}
	// The Go owner's next writable command drains first.
	if got := goCLI(t, "--state", stateA, "store-challenge", "--write", "--actor", "t31"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	if left := inboxEntries(t, stateA); len(left) != 0 {
		t.Fatalf("entries left after the drain: %v", left)
	}
	dbA := filepath.Join(stateA, "relay.sqlite3")
	for _, request := range requests {
		direct := goCLI(t, append([]string{"--state", stateB}, request...)...)
		markers := snapshotQuery(t, dbA, "SELECT value FROM schema_meta WHERE key LIKE ?", "inbox:"+request[0]+".%")
		var marker map[string]any
		for _, row := range markers {
			candidate := object(t, row["value"].(string))
			if reflect.DeepEqual(candidate["answer"], object(t, direct.stdout)) {
				marker = candidate
			}
		}
		if marker == nil || marker["exit"] != float64(direct.code) {
			t.Fatalf("%v: no marker holds the direct answer %d %s (markers %v)", request, direct.code, direct.stdout, markers)
		}
	}
	// The drain applies in operation-ID order, the direct run in request order: same rows.
	for _, table := range []string{"events", "deliveries", "delivery_supersession"} {
		a := snapshotQuery(t, dbA, "SELECT * FROM "+table+" ORDER BY event_id")
		b := snapshotQuery(t, dbB, "SELECT * FROM "+table+" ORDER BY event_id")
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s differs:\n drained %v\n  direct %v", table, a, b)
		}
	}
	if events := snapshotQuery(t, dbA, "SELECT event_id FROM events"); len(events) != 3 {
		t.Fatalf("%d events: %v", len(events), snapshotQuery(t, dbA, "SELECT key, value FROM schema_meta WHERE key LIKE 'inbox:%'"))
	}
}

// Go queues -> Python drains: under a Python owner the Go producer publishes the fence's own
// bytes and answers with the fence's own stdout (checked against the fence queueing the same
// request under a Go owner: acceptance, retry, conflict and both usage rejections), and the
// Python owner's next writable command applies them.
func Test31_go_queues_under_a_python_owner_as_the_fence_does_and_python_applies_it(t *testing.T) {
	home := pythonHome(t)
	roots := ownerOnlyState(t, home, "artifacts")
	statePython := ownerOnlyState(t, home, "python-owned")
	dbPython := filepath.Join(statePython, "relay.sqlite3")
	testsupport.Create(t, dbPython, "", "python")
	relationship := registerRelationship(t, statePython, roots, true)
	stateGo := ownerOnlyState(t, home, "go-owned")
	if got := goCLI(t, "--state", stateGo, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	requests := [][]string{
		{"emit", "--relationship", relationship, "--generation", "1", "--outcome", "failed", "--turn-status", "failed", "--turn-thread", "child-1", "--turn-id", "turn-0", "--continuation-reason", "b\u2028é\"\\"},
		{"emit", "--relationship", relationship, "--generation", "1", "--outcome", "failed", "--turn-status", "failed", "--turn-thread", "child-1", "--turn-id", "turn-0", "--continuation-reason", "b\u2028é\"\\"},
		{"ack", "--event", strings.Repeat("cd", 16), "--ack-turn", "parent-turn", "--ack-proof", "proof-1"},
		{"ack", "--event", strings.Repeat("cd", 16), "--ack-turn", "parent-turn", "--ack-proof", "proof-1", "--reject", "stale_generation"},
		{"fault-notification-ack", "--notification", "notice/雪", "--token", "token-1", "--ref", "receipt-1"},
		{"ack", "--event", strings.Repeat("x", 197), "--ack-turn", "t", "--ack-proof", "p"},
		{"ack", "--event", "event-\xff\xfe", "--ack-turn", "t", "--ack-proof", "p"},
	}
	var fence [][]string
	for _, request := range requests {
		fence = append(fence, append([]string{"--state", stateGo}, request...))
	}
	want := pythonCLI(t, fence...)
	for i, request := range requests {
		got := goCLI(t, append([]string{"--state", statePython}, request...)...)
		if got != want[i] {
			t.Fatalf("%q:\n go     %d %s\n python %d %s", request, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
	entries := inboxEntries(t, statePython)
	if !slices.Equal(entries, inboxEntries(t, stateGo)) || len(entries) != 3 {
		t.Fatalf("%v %v", entries, inboxEntries(t, stateGo))
	}
	for _, name := range entries {
		a, _ := os.ReadFile(filepath.Join(statePython, "takeover-inbox", name))
		b, _ := os.ReadFile(filepath.Join(stateGo, "takeover-inbox", name))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s:\n go     %s\n python %s", name, a, b)
		}
	}
	if got := pythonCLI(t, []string{"--state", statePython, "store-challenge", "--write", "--actor", "t31"})[0]; got.code != 0 {
		t.Fatalf("%+v", got)
	}
	if left := inboxEntries(t, statePython); len(left) != 0 {
		t.Fatalf("the Python owner left %v", left)
	}
	events := snapshotQuery(t, dbPython, "SELECT turn_id, outcome FROM events")
	if len(events) != 1 || events[0]["turn_id"] != "turn-0" || events[0]["outcome"] != "failed" {
		t.Fatalf("%v", events)
	}
	for _, name := range entries {
		marker, ok := meta(t, dbPython, "inbox:"+name)
		exit := map[bool]float64{true: 0, false: 2}[strings.HasPrefix(name, "emit.")]
		if !ok || marker["exit"] != exit {
			t.Fatalf("%s: %v", name, marker)
		}
	}
}

// Todo 31 QA: 100 receipts submitted while the store drains - through the Go and the Python
// producer, and while a transfer barrier holds the write gate exclusively - are applied once
// when Go owns the store in phase active again: 100 domain rows, 0 duplicates. The same
// operation ID with different bytes is refused at submit while the entry exists, and a retired
// ID reused with different bytes is recorded as a conflict and never applied.
func Test31_receipts_queued_while_draining_apply_once_after_activation(t *testing.T) {
	home := pythonHome(t)
	roots := ownerOnlyState(t, home, "artifacts")
	state := ownerOnlyState(t, home, "state")
	dbPath := filepath.Join(state, "relay.sqlite3")
	relationship := registerRelationship(t, state, roots, false)
	setPhase(t, dbPath, "draining")
	emit := func(turn int) []string {
		return []string{"--state", state, "emit", "--relationship", relationship, "--generation", "1", "--outcome", "failed", "--turn-status", "failed", "--turn-thread", "child-1", "--turn-id", "turn-" + string(rune('A'+turn/26)) + string(rune('a'+turn%26)), "--continues-anchor", "turn-0"}
	}
	var python [][]string
	for turn := range 40 {
		python = append(python, emit(turn))
	}
	for _, got := range pythonCLI(t, python...) {
		if got.code != 0 || object(t, got.stdout)["status"] != "durably_queued" {
			t.Fatalf("python while draining: %+v", got)
		}
	}
	var barrier *os.File
	for turn := 40; turn < 100; turn++ {
		if turn == 70 {
			// Step 3's transfer barrier: write-gate EX held; the Go writer is refused at once
			// as the fence's lock-free preflight refuses a draining store, so it queues.
			var err error
			if barrier, err = ownership.Lock(filepath.Join(state, "write-gate.lock"), true, false); err != nil {
				t.Fatal(err)
			}
		}
		if got := goCLI(t, emit(turn)...); got.code != 0 || object(t, got.stdout)["status"] != "durably_queued" {
			t.Fatalf("go while draining (turn %d): %+v", turn, got)
		}
	}
	// Other writers still refuse, in the fence's words, and queue nothing.
	refused := goCLI(t, "--state", state, "claim", "--event", strings.Repeat("0", 32))
	if refused.code != 2 || object(t, refused.stdout)["detail"] != "the relay store is draining" {
		t.Fatalf("%+v", refused)
	}
	event := strings.Repeat("ef", 16)
	ack := []string{"--state", state, "ack", "--event", event, "--ack-turn", "parent-turn", "--ack-proof", "proof-1"}
	if got := goCLI(t, ack...); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	rejected := append(append([]string{}, ack...), "--reject", "stale_generation")
	if got := goCLI(t, rejected...); got.code != 2 || object(t, got.stdout)["reason"] != "inbox_conflict" {
		t.Fatalf("same ID, different bytes: %+v", got)
	}
	if n := len(inboxEntries(t, state)); n != 101 {
		t.Fatalf("%d entries", n)
	}
	count := func() (events, succeeded int) {
		t.Helper()
		rows := snapshotQuery(t, dbPath, "SELECT count(*) AS n, count(DISTINCT event_id) AS d FROM events WHERE relationship_id=?", relationship)
		if rows[0]["n"] != rows[0]["d"] {
			t.Fatalf("duplicate events: %v", rows)
		}
		markers := snapshotQuery(t, dbPath, "SELECT count(*) AS n FROM schema_meta WHERE key LIKE 'inbox:emit.%' AND json_extract(value, '$.exit') = 0")
		return int(rows[0]["n"].(int64)), int(markers[0]["n"].(int64))
	}
	if events, _ := count(); events != 0 {
		t.Fatalf("applied while draining: %d", events)
	}
	if err := barrier.Close(); err != nil {
		t.Fatal(err)
	}
	setPhase(t, dbPath, "active")
	if got := goCLI(t, "--state", state, "store-challenge", "--write", "--actor", "t31"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	if events, succeeded := count(); events != 100 || succeeded != 100 {
		t.Fatalf("events=%d markers=%d", events, succeeded)
	}
	retired, ok := meta(t, dbPath, "inbox:ack."+event)
	if !ok || retired["exit"] != float64(2) || len(inboxEntries(t, state)) != 0 {
		t.Fatalf("%v %v", retired, inboxEntries(t, state))
	}
	if got := goCLI(t, "--state", state, "store-challenge", "--write", "--actor", "t31"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	if events, succeeded := count(); events != 100 || succeeded != 100 {
		t.Fatalf("a second drain duplicated: events=%d markers=%d", events, succeeded)
	}
	// The retired ID reused with different bytes in a later window: accepted, then a conflict.
	setPhase(t, dbPath, "draining")
	reused := goCLI(t, rejected...)
	if reused.code != 0 || object(t, reused.stdout)["status"] != "durably_queued" {
		t.Fatalf("%+v", reused)
	}
	setPhase(t, dbPath, "active")
	if got := goCLI(t, "--state", state, "store-challenge", "--write", "--actor", "t31"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	digest := object(t, reused.stdout)["payloadDigest"].(string)
	conflict, ok := meta(t, dbPath, "inbox-conflict:ack."+event+":"+digest[7:23])
	if !ok || conflict["exit"] != float64(2) || conflict["payloadDigest"] != digest {
		t.Fatalf("%v", conflict)
	}
	if now, _ := meta(t, dbPath, "inbox:ack."+event); !reflect.DeepEqual(now, retired) {
		t.Fatalf("the reused ID was applied: %v", now)
	}
	if left := inboxEntries(t, state); len(left) != 0 {
		t.Fatalf("%v", left)
	}
}

// Both owners drain at daemon start (cmd_daemon; cutover.md Wire format, "Both owners drain"):
// an acknowledgment queued while the other runtime owned the store (Go's queue under a Python
// owner) or while it drained (Go's own) is judged by the owner's `daemon` before its first tick.
func Test31_the_owners_daemon_drains_at_start(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	event := strings.Repeat("0a", 16)
	for runtime, relay := range map[string]func(argv ...string) run{
		"go":     func(argv ...string) run { return binaryRun(t, alias, argv...) },
		"python": func(argv ...string) run { return fence(t, argv...) },
	} {
		state := ownerOnlyState(t, home, runtime)
		dbPath := filepath.Join(state, "relay.sqlite3")
		if runtime == "python" {
			testsupport.Create(t, dbPath, "", "python")
		} else {
			if created := goCLI(t, "--state", state, "store-challenge", "--write"); created.code != 0 {
				t.Fatalf("%+v", created)
			}
			setPhase(t, dbPath, "draining")
		}
		queued := goCLI(t, "--state", state, "ack", "--event", event, "--ack-turn", "parent-turn", "--ack-proof", "proof-1")
		if queued.code != 0 || object(t, queued.stdout)["status"] != "durably_queued" {
			t.Fatalf("%s: %+v", runtime, queued)
		}
		if runtime == "go" {
			setPhase(t, dbPath, "active")
		}
		socket := filepath.Join(home, runtime+".sock")
		if done := relay("--state", state, "--socket", socket, "daemon", "--max-ticks", "0", "--allow-isolated-scope"); done.code != 0 {
			t.Fatalf("%s: daemon: %+v", runtime, done)
		}
		if left := inboxEntries(t, state); len(left) != 0 {
			t.Fatalf("%s: the daemon started over %v", runtime, left)
		}
		if marker, ok := meta(t, dbPath, "inbox:ack."+event); !ok || marker["exit"] != float64(2) {
			t.Fatalf("%s: %v", runtime, marker)
		}
	}
}

// cli.main runs its lock-free check_start before the selection refusal and before
// --kind-module, so under another owner a receipt or acknowledgment is queued whatever --socket
// or --kind-module says: a --socket that disagrees with the store's recorded socket and a
// --kind-module that cannot be imported queue the same bytes, with the same answer, in both
// runtimes. Under its own owner each runtime still refuses the mismatched socket first and
// queues nothing.
func Test31_a_queueable_refusal_queues_before_the_selection_and_kind_module_refusals(t *testing.T) {
	home := pythonHome(t)
	bound := filepath.Join(home, "bound.sock")
	other := filepath.Join(home, "other.sock")
	statePython := ownerOnlyState(t, home, "python-owned")
	stateGo := ownerOnlyState(t, home, "go-owned")
	if got := pythonCLI(t, []string{"--state", statePython, "--socket", bound, "store-challenge", "--write"})[0]; got.code != 0 {
		t.Fatalf("%+v", got)
	}
	if got := goCLI(t, "--state", stateGo, "--socket", bound, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	requests := [][]string{
		{"--socket", other, "emit", "--relationship", "r-1", "--generation", "1", "--outcome", "failed", "--turn-thread", "child-1", "--turn-id", "turn-1"},
		{"--socket", other, "ack", "--event", strings.Repeat("ab", 16), "--ack-turn", "parent-turn", "--ack-proof", "proof-1"},
		{"--socket", other, "fault-notification-ack", "--notification", "notice-1", "--token", "token-1", "--ref", "receipt-1"},
		{"--kind-module", "bogus.mod", "emit", "--relationship", "r-2", "--generation", "1", "--outcome", "failed", "--turn-thread", "child-1", "--turn-id", "turn-2"},
		{"--kind-module", "bogus.mod", "ack", "--event", strings.Repeat("cd", 16), "--ack-turn", "parent-turn", "--ack-proof", "proof-1"},
		{"--kind-module", "bogus.mod", "fault-notification-ack", "--notification", "notice-2", "--token", "token-1", "--ref", "receipt-1"},
	}
	var fence [][]string
	for _, request := range requests {
		fence = append(fence, append([]string{"--state", stateGo}, request...))
	}
	// Under their own owners: the selection refusal, nothing queued.
	own := []string{"--socket", other, "ack", "--event", strings.Repeat("ef", 16), "--ack-turn", "parent-turn", "--ack-proof", "proof-1"}
	fence = append(fence, append([]string{"--state", statePython}, own...))
	want := pythonCLI(t, fence...)
	for i, request := range requests {
		got := goCLI(t, append([]string{"--state", statePython}, request...)...)
		if got != want[i] || got.code != 0 || object(t, got.stdout)["status"] != "durably_queued" {
			t.Fatalf("%q:\n go     %d %s\n python %d %s", request, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
	refused := goCLI(t, append([]string{"--state", stateGo}, own...)...)
	if python := want[len(requests)]; refused.code != 2 || python.code != 2 || object(t, refused.stdout)["reason"] != "state_directory_serves_another_socket" || object(t, python.stdout)["reason"] != "state_directory_serves_another_socket" {
		t.Fatalf("own owner:\n go     %+v\n python %+v", refused, python)
	}
	entries := inboxEntries(t, statePython)
	if !slices.Equal(entries, inboxEntries(t, stateGo)) || len(entries) != len(requests) {
		t.Fatalf("%v %v", entries, inboxEntries(t, stateGo))
	}
	for _, name := range entries {
		a, _ := os.ReadFile(filepath.Join(statePython, "takeover-inbox", name))
		b, _ := os.ReadFile(filepath.Join(stateGo, "takeover-inbox", name))
		if !bytes.Equal(a, b) {
			t.Fatalf("%s:\n go     %s\n python %s", name, a, b)
		}
	}
}

// Every writable command drains before its handler reads its own arguments (cli.main:
// _ownership_preflight opens the store, inbox.replay, then the handler), including the ones
// whose handler refuses its arguments before it would reach the store: a corrupt entry fails
// each of them closed as it fails the fence, and valid entries are applied (and retired) by a
// command that then refuses its own arguments, with the markers the fence writes - an empty
// append list included, which the fence's typed check accepts. A legacy supervisor-read entry
// drained by an owner that has a socket is judged by the readback as the fence judges it.
func Test31_every_writable_command_drains_before_its_handler_reads_its_arguments(t *testing.T) {
	home := pythonHome(t)
	socket := filepath.Join(home, "drainer.sock")
	states := map[string]string{}
	var creates [][]string
	for _, phase := range []string{"corrupt", "valid", "readback"} {
		states["go-"+phase] = ownerOnlyState(t, home, "go-"+phase)
		states["python-"+phase] = ownerOnlyState(t, home, "python-"+phase)
		creates = append(creates, []string{"--state", states["python-"+phase], "store-challenge", "--write"})
		if got := goCLI(t, "--state", states["go-"+phase], "store-challenge", "--write"); got.code != 0 {
			t.Fatalf("%+v", got)
		}
	}
	for _, got := range pythonCLI(t, creates...) {
		if got.code != 0 {
			t.Fatalf("%+v", got)
		}
	}
	goldenEntry := func(command string) []byte {
		raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "contract", "golden", "takeover-inbox", command+".json"))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	const emptyList = "emit.edb680737833a16cc18b813934d667cf"
	entries := map[string]map[string][]byte{
		"corrupt":  {"ack.corrupt": []byte("{")},
		"valid":    {"ack.event-1": goldenEntry("ack"), emptyList: []byte(`{"arguments":{"artifact":[],"generation":1,"outcome":"failed","relationship":"relationship-1","turn_id":"turn-1","turn_thread":"child-1"},"command":"emit","inboxVersion":1,"operationId":"emit.edb680737833a16cc18b813934d667cf","payloadDigest":"sha256:edb680737833a16cc18b813934d667cff1b1d2d0ad5336e2dc847e03f55b024b"}`)},
		"readback": {"supervisor-read.message-1": goldenEntry("supervisor-read")},
	}
	for phase, files := range entries {
		for _, runtime := range []string{"go-", "python-"} {
			directory := filepath.Join(states[runtime+phase], "takeover-inbox")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			for name, raw := range files {
				if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	refusedArguments := [][]string{
		{"deliver", "--limit", "1"},
		{"reconcile", "--request-id", "request-1"},
		{"recover"},
		{"verify-acks"},
		{"supervisor-stage"},
		{"supervisor-send", "--message", "message-1"},
		{"supervisor-read", "--message", "message-1", "--turn", "t", "--proof", "p", "--as", "a"},
		{"store-challenge"},
	}
	var fence [][]string
	for _, argv := range refusedArguments {
		fence = append(fence, append([]string{"--state", states["python-corrupt"]}, argv...))
	}
	fence = append(fence, []string{"--state", states["python-valid"], "deliver", "--limit", "1"},
		[]string{"--state", states["python-readback"], "--socket", socket, "store-challenge", "--write"})
	want := pythonCLI(t, fence...)
	for i, argv := range refusedArguments {
		got := goCLI(t, append([]string{"--state", states["go-corrupt"]}, argv...)...)
		if got != want[i] || got.code != 3 {
			t.Fatalf("%q over a corrupt entry:\n go     %d %s\n python %d %s", argv, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
	valid := goCLI(t, "--state", states["go-valid"], "deliver", "--limit", "1")
	if python := want[len(refusedArguments)]; valid != python || valid.code != 4 {
		t.Fatalf("deliver:\n go     %+v\n python %+v", valid, python)
	}
	if got := goCLI(t, "--state", states["go-readback"], "--socket", socket, "store-challenge", "--write"); got.code != 0 || want[len(fence)-1].code != 0 {
		t.Fatalf("%+v %+v", got, want[len(fence)-1])
	}
	for phase, files := range entries {
		if phase == "corrupt" {
			for _, runtime := range []string{"go-", "python-"} {
				if left := inboxEntries(t, states[runtime+phase]); !slices.Equal(left, []string{"ack.corrupt"}) {
					t.Fatalf("%s%s: %v", runtime, phase, left)
				}
			}
			continue
		}
		for name := range files {
			goMarker, goOK := meta(t, filepath.Join(states["go-"+phase], "relay.sqlite3"), "inbox:"+name)
			pythonMarker, pythonOK := meta(t, filepath.Join(states["python-"+phase], "relay.sqlite3"), "inbox:"+name)
			if !goOK || !pythonOK || !reflect.DeepEqual(goMarker, pythonMarker) || goMarker["exit"] != float64(2) {
				t.Fatalf("%s:\n go     %v\n python %v", name, goMarker, pythonMarker)
			}
		}
		for _, runtime := range []string{"go-", "python-"} {
			if left := inboxEntries(t, states[runtime+phase]); len(left) != 0 {
				t.Fatalf("%s%s kept %v", runtime, phase, left)
			}
		}
	}
	readback, _ := meta(t, filepath.Join(states["go-readback"], "relay.sqlite3"), "inbox:supervisor-read.message-1")
	if answer, _ := readback["answer"].(map[string]any); answer["reason"] != "not_claimable" || answer["detail"] != "no supervisor message is staged as 'message-1'" {
		t.Fatalf("%v", readback)
	}
}
