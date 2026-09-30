package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Decision 25 across the runtimes, in process: the Go producer and the Go drain against what the
// retained Python fence answered (one Python process per batch, recorded: see pythonCLI). The
// built-binary candidate drain is internal/relay/service Test31GoCandidateDrainsPythonQueuedEntriesBuiltCLI.

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

// pythonCLI is what the fence's cli.main answered for each argv, in one Python process, byte
// for byte (argv that is not UTF-8 arrives surrogate-escaped, as on a command line); recorded,
// see oracleRun.
func pythonCLI(t *testing.T, argvs ...[]string) []answer {
	t.Helper()
	var words []string
	for _, argv := range argvs {
		words = append(words, argv...)
	}
	label := "cli.main"
	if len(argvs) > 0 {
		label += fmt.Sprintf(" x%d %s", len(argvs), oracleLabel(argvs[0]...))
	}
	var recorded []recordedRun
	askOracle(t, oracleKey(t, label), &recorded, func() (any, error) {
		answers, err := livePythonCLI(argvs...)
		out := make([]recordedRun, len(answers))
		for i, a := range answers {
			out[i] = recordedRun{Code: a.code, Stdout: a.stdout}
		}
		return out, err
	}, placeholders(words...)...)
	if len(recorded) != len(argvs) {
		t.Fatalf("%d answers for %d argvs", len(recorded), len(argvs))
	}
	out := make([]answer, len(recorded))
	for i, r := range recorded {
		out[i] = answer{r.Code, r.Stdout}
	}
	return out
}

// livePythonCLI runs each argv through the fence's cli.main in one live Python process. Only a
// capture closure calls it.
func livePythonCLI(argvs ...[]string) ([]answer, error) {
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
		return nil, err
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
	python := exec.Command(filepath.Join(repositoryRootPath(), ".venv", "bin", "python"), "-c", script)
	python.Stdin = bytes.NewReader(input)
	python.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := python.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%v: %s", err, exit.Stderr)
		}
		return nil, err
	}
	var decoded [][2]any
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded) != len(argvs) {
		return nil, fmt.Errorf("%v %s", err, raw)
	}
	out := make([]answer, len(decoded))
	for i, d := range decoded {
		out[i] = answer{int(d[0].(float64)), d[1].(string)}
	}
	return out, nil
}

// pythonInbox is every takeover-inbox entry the fence left in state, by name (recorded right
// after the answers that queued them).
func pythonInbox(t *testing.T, state string) map[string][]byte {
	t.Helper()
	var entries map[string][]byte
	pyoracle.JSON(t, oracleKey(t, "takeover-inbox"), &entries, func() (any, error) {
		out := map[string][]byte{}
		for _, name := range inboxEntries(t, state) {
			raw, err := os.ReadFile(filepath.Join(state, "takeover-inbox", name))
			if err != nil {
				return nil, err
			}
			out[name] = raw
		}
		return out, nil
	}, placeholders(state)...)
	return entries
}

// sameInbox requires the entries Go queued in state to be, name for name and byte for byte, the
// ones the fence queued.
func sameInbox(t *testing.T, state string, python map[string][]byte, count int) {
	t.Helper()
	entries := inboxEntries(t, state)
	var names []string
	for name := range python {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(entries, names) || len(entries) != count {
		t.Fatalf("go queued %v, python %v", entries, names)
	}
	for _, name := range entries {
		raw, _ := os.ReadFile(filepath.Join(state, "takeover-inbox", name))
		if !bytes.Equal(raw, python[name]) {
			t.Fatalf("%s:\n go     %s\n python %s", name, raw, python[name])
		}
	}
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

// Go queues under a Python owner: the Go producer publishes the fence's own bytes and answers
// with the fence's own stdout (checked against the fence queueing the same request under a Go
// owner: acceptance, retry, conflict and both usage rejections). The Python owner's drain of
// those entries (the second half of this test while the fence ran) left with the Python runtime
// (todo 44): the Go drain of queued entries is Test31_receipts_queued_while_draining_apply_once_
// after_activation and Test31_every_writable_command_drains_before_its_handler_reads_its_arguments.
func Test31_go_queues_under_a_python_owner_as_the_fence_does(t *testing.T) {
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
	queued := pythonInbox(t, stateGo)
	for i, request := range requests {
		got := goCLI(t, append([]string{"--state", statePython}, request...)...)
		if got != want[i] {
			t.Fatalf("%q:\n go     %d %s\n python %d %s", request, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
	sameInbox(t, statePython, queued, 3)
}

// Todo 31 QA: 100 receipts submitted while the store drains - through the Go producer, and
// while a transfer barrier holds the write gate exclusively - are applied once when Go owns the
// store in phase active again: 100 domain rows, 0 duplicates. (The first 40 were the Python
// producer's while the fence ran; that producer left with the Python runtime, todo 44, and the
// fence's entry bytes are Test31_go_queues_under_a_python_owner_as_the_fence_does's.) The same
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
	var barrier *os.File
	for turn := 0; turn < 100; turn++ {
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

// The owner drains at daemon start (cmd_daemon; cutover.md Wire format, "Both owners drain"):
// an acknowledgment queued while the store drained is judged by the owner's `daemon` before its
// first tick. (The Python owner's daemon draining Go's queue under it left with the Python
// runtime, todo 44.)
func Test31_the_owners_daemon_drains_at_start(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	event := strings.Repeat("0a", 16)
	for runtime, relay := range map[string]func(argv ...string) run{
		"go": func(argv ...string) run { return binaryRun(t, alias, argv...) },
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
	// The Python-owned store bound to its socket, as the fence's store-challenge --write leaves it.
	testsupport.Create(t, filepath.Join(statePython, "relay.sqlite3"), bound, "python")
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
	queued := pythonInbox(t, stateGo)
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
	sameInbox(t, statePython, queued, len(requests))
}

// cli.py main runs check_start for every command that is neither read-only nor answers without
// the selected store, before the selection refusal, --kind-module and the handler (backlog before
// todo 42, decision 31). On a store the other runtime owns, a write form therefore answers the
// ownership refusal, byte for byte in both runtimes, whether its --kind-module cannot be imported,
// it names no --socket (daemon and managed-start refuse that in their handler), or its --socket
// is not the one the store recorded. Under its own owner each runtime still answers the
// mismatched socket with the selection refusal. The Go side is the built binary, which registers
// every command family (sync-target included).
func Test31_check_start_precedes_the_selection_kind_module_and_handler_refusals(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	bound := filepath.Join(home, "bound.sock")
	other := filepath.Join(home, "other.sock")
	statePython := ownerOnlyState(t, home, "python-owned")
	stateGo := ownerOnlyState(t, home, "go-owned")
	testsupport.Create(t, filepath.Join(statePython, "relay.sqlite3"), bound, "python")
	if got := goCLI(t, "--state", stateGo, "--socket", bound, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	markers := filepath.Join(home, "markers")
	commands := [][]string{
		{"relationship-status", "--relationship", "r-1", "--status", "paused", "--actor", "a"},
		{"fault-target", "--product", "crw", "--team", "team"},
		{"store-challenge", "--write"},
		{"sync-target", "--relationship", "r-1", "--target-ref", "ISSUE-1"},
		{"merge-turn-request", "--repository", "repo", "--base-ref", "main", "--project", "P", "--task", "task", "--host", "host", "--head", "abc"},
		{"slot-reserve", "--kind", "k", "--subject", "s", "--parent-task", "parent", "--project", "P", "--actor", "a"},
		{"region-propose", "--repository", "repo", "--revision", "rev", "--path", "p", "--kind", "file", "--left-project", "L", "--right-project", "R", "--peer-link", "link", "--task", "t", "--constraint", "c"},
		{"daemon", "--max-ticks", "0"},
		{"managed-start", "--request", "{}", "--marker-root", markers},
		{"service", "start"},
		{"service", "stop"},
	}
	scenarios := []struct {
		name    string
		globals []string
	}{
		{"an unimportable --kind-module", []string{"--socket", bound, "--kind-module", "nosuch"}},
		{"no --socket", nil},
		{"a --socket the store did not record", []string{"--socket", other}},
	}
	var fence [][]string
	for _, scenario := range scenarios {
		for _, command := range commands {
			fence = append(fence, append(append([]string{"--state", stateGo}, scenario.globals...), command...))
		}
	}
	own := []string{"--socket", other, "store-challenge", "--write"}
	fence = append(fence, append([]string{"--state", statePython}, own...))
	want := pythonCLI(t, fence...)
	i := 0
	for _, scenario := range scenarios {
		for _, command := range commands {
			ran := binaryRun(t, alias, append(append([]string{"--state", statePython}, scenario.globals...), command...)...)
			got := answer{ran.code, ran.stdout}
			if got != want[i] || got.code != 2 || object(t, got.stdout)["reason"] != "store_owned_by_other" {
				t.Errorf("%s, %q:\n go     %d %s\n python %d %s", scenario.name, command, got.code, got.stdout, want[i].code, want[i].stdout)
			}
			i++
		}
	}
	refused := binaryRun(t, alias, append([]string{"--state", stateGo}, own...)...)
	if python := want[i]; refused.code != 2 || python.code != 2 || object(t, refused.stdout)["reason"] != "state_directory_serves_another_socket" || object(t, python.stdout)["reason"] != "state_directory_serves_another_socket" {
		t.Fatalf("own owner:\n go     %+v\n python %+v", refused, python)
	}
	if _, err := os.Stat(markers); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused managed-start wrote its marker root: %v", err)
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
	phases := []string{"corrupt", "valid", "readback"}
	states := map[string]string{}
	for _, phase := range phases {
		states["go-"+phase] = ownerOnlyState(t, home, "go-"+phase)
		states["python-"+phase] = filepath.Join(home, "python-"+phase)
		if got := goCLI(t, "--state", states["go-"+phase], "store-challenge", "--write"); got.code != 0 {
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
	queue := func(runtime string) error {
		for phase, files := range entries {
			directory := filepath.Join(states[runtime+phase], "takeover-inbox")
			if err := os.MkdirAll(directory, 0700); err != nil {
				return err
			}
			for name, raw := range files {
				if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := queue("go-"); err != nil {
		t.Fatal(err)
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
	// The fence's side, one recorded answer: its own stores created by its writer, the same
	// entries queued in them, each command's answer, and then the markers its drain left and the
	// entries it kept (see oracleRun).
	var python struct {
		Answers []recordedRun             `json:"answers"`
		Markers map[string]map[string]any `json:"markers"`
		Left    map[string][]string       `json:"left"`
	}
	var fence [][]string
	for _, argv := range refusedArguments {
		fence = append(fence, append([]string{"--state", states["python-corrupt"]}, argv...))
	}
	fence = append(fence, []string{"--state", states["python-valid"], "deliver", "--limit", "1"},
		[]string{"--state", states["python-readback"], "--socket", socket, "store-challenge", "--write"})
	var anchors []string
	for _, argv := range fence {
		anchors = append(anchors, argv...)
	}
	pyoracle.JSON(t, "fence", &python, func() (any, error) {
		var creates [][]string
		for _, phase := range phases {
			if err := os.MkdirAll(states["python-"+phase], 0o700); err != nil {
				return nil, err
			}
			creates = append(creates, []string{"--state", states["python-"+phase], "store-challenge", "--write"})
		}
		created, err := livePythonCLI(creates...)
		if err != nil {
			return nil, err
		}
		for _, got := range created {
			if got.code != 0 {
				return nil, fmt.Errorf("the fence's writer: %+v", got)
			}
		}
		if err = queue("python-"); err != nil {
			return nil, err
		}
		answers, err := livePythonCLI(fence...)
		if err != nil {
			return nil, err
		}
		python.Answers, python.Markers, python.Left = nil, map[string]map[string]any{}, map[string][]string{}
		for _, a := range answers {
			python.Answers = append(python.Answers, recordedRun{Code: a.code, Stdout: a.stdout})
		}
		for phase, files := range entries {
			python.Left[phase] = inboxEntries(t, states["python-"+phase])
			for name := range files {
				if marker, ok := meta(t, filepath.Join(states["python-"+phase], "relay.sqlite3"), "inbox:"+name); ok {
					python.Markers[name] = marker
				}
			}
		}
		return python, nil
	}, placeholders(anchors...)...)
	want := make([]answer, len(python.Answers))
	for i, r := range python.Answers {
		want[i] = answer{r.Code, r.Stdout}
	}
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
		left := inboxEntries(t, states["go-"+phase])
		if phase == "corrupt" {
			if !slices.Equal(left, []string{"ack.corrupt"}) || !slices.Equal(python.Left[phase], []string{"ack.corrupt"}) {
				t.Fatalf("%s: go kept %v, python %v", phase, left, python.Left[phase])
			}
			continue
		}
		for name := range files {
			goMarker, goOK := meta(t, filepath.Join(states["go-"+phase], "relay.sqlite3"), "inbox:"+name)
			pythonMarker, pythonOK := python.Markers[name]
			if !goOK || !pythonOK || !reflect.DeepEqual(normalizeJSONNumbers(goMarker), normalizeJSONNumbers(pythonMarker)) || goMarker["exit"] != float64(2) {
				t.Fatalf("%s:\n go     %v\n python %v", name, goMarker, pythonMarker)
			}
		}
		if len(left) != 0 || len(python.Left[phase]) != 0 {
			t.Fatalf("%s: go kept %v, python %v", phase, left, python.Left[phase])
		}
	}
	readback, _ := meta(t, filepath.Join(states["go-readback"], "relay.sqlite3"), "inbox:supervisor-read.message-1")
	if answer, _ := readback["answer"].(map[string]any); answer["reason"] != "not_claimable" || answer["detail"] != "no supervisor message is staged as 'message-1'" {
		t.Fatalf("%v", readback)
	}
}

// normalizeJSONNumbers is v as encoding/json decodes it into any, whichever decoder read it.
func normalizeJSONNumbers(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err = json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}
