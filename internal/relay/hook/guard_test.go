package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// guardCases are every case guard_compare.py lays out, in its order.
var guardCases = []string{"unmanaged", "unclaimed", "uncorrelated", "unbound", "other", "unregistered",
	"undeclared", "ready_missing", "ready_receipted", "ready_staged", "artifacts_changed",
	"frozen", "generation", "dispatch", "generation_absent", "inactive", "foreign_turn",
	"malformed_disposition", "unreadable_disposition", "malformed_marker", "bad_identity",
	"active", "hold_spent", "generation_spent", "window_spent", "observe",
	"in_progress", "blocked_needs_input", "interrupted", "failed", "no_record",
	"frozen_at_depth", "frozen_past_depth"}

// compareGuard replays real marker, store and receipt fixtures through the built Go CLI and
// compares each full byte envelope, observation record and store with Python's guard.evaluate
// (guard_compare.py). Python lays each case out and answers it (guard_compare.py prepare); both
// are recorded (pythonFixture) under a fixed path, since a receipt's revision and event ids
// digest the artifact paths.
func compareGuard(t *testing.T, cases string) {
	t.Helper()
	names := strings.Split(cases, ",")
	if cases == "all" {
		names = guardCases
	}
	root := canonicalRoot(t)
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		return key == "HOME" || key == "CODEX_HOME" || key == "XDG_STATE_HOME"
	})
	env = append(env, "HOME="+root, "CODEX_HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "xdg"))
	for _, name := range names {
		base := filepath.Join(root, name)
		if err := os.Mkdir(base, 0o700); err != nil {
			t.Fatal(err)
		}
		out, _ := pythonFixture(t, name, base, func() ([]byte, error) {
			return pythonScript(t, nil, nil, "testdata/guard_compare.py", "-", root, "prepare", name)
		}, nil)
		var python struct {
			Stdout, Mode       string
			NoRecord           bool
			Record, RecordText *string
		}
		if err := json.Unmarshal(out, &python); err != nil {
			t.Fatalf("%s: %v %s", name, err, out)
		}
		db := filepath.Join(base, "state", "relay.sqlite3")
		before := storeRows(t, db)
		args := []string{"relay", "--state", filepath.Join(base, "state"), "guard-evaluate", "--marker-root", filepath.Join(base, "markers"),
			"--stop-input", filepath.Join(base, "stop.json"), "--mode", python.Mode, "--now", "2026-01-01T00:06:00+00:00"}
		if python.NoRecord {
			args = append(args, "--no-record")
		}
		command := exec.Command(binary(t), args...)
		command.Env = env
		got := runOutcome(t, command)
		if got.Code != 0 || got.Stderr != "" {
			t.Fatalf("%s: %+v", name, got)
		}
		// The FULL byte envelope, key order included, as cached adapters consume it.
		if got.Stdout != python.Stdout {
			t.Fatalf("%s:\n go     %s\n python %s", name, got.Stdout, python.Stdout)
		}
		if python.Record != nil {
			raw, err := os.ReadFile(filepath.Join(base, *python.Record))
			if err != nil || string(raw) != *python.RecordText {
				t.Fatalf("%s: record %v\n go     %s\n python %s", name, err, raw, *python.RecordText)
			}
		}
		if !slices.Equal(storeRows(t, db), before) {
			t.Fatalf("%s: the guard changed relay evidence", name)
		}
		if _, err := os.Stat(filepath.Join(root, "crw-completion-hook", "stop-events")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: the legacy command re-claimed the event: %v", name, err)
		}
	}
	t.Logf("%d full byte envelopes and marker/DB effects equal: %s", len(names), strings.Join(names, ","))
}
func Test33GuardBinaryPython(t *testing.T) {
	compareGuard(t, "unmanaged,unregistered,ready_missing,ready_receipted,ready_staged,artifacts_changed,frozen,generation,dispatch,generation_absent,hold_spent,malformed_disposition,in_progress,no_record")
}

// A frozen copy nested deeper than json.loads can descend leaves guard.deliverable_state as the
// RecursionError its except clauses do not name, so the guard faults and releases, recorded. One
// level less is read, and the receipt stands at the head through its frozen copy.
func Test33GuardFrozenCopyAtTheDecoderDepth(t *testing.T) {
	compareGuard(t, "frozen_at_depth,frozen_past_depth")
}

// The legacy guard CLI's usage envelopes through the built multicall binary, byte for byte the
// Python CLI's (guard_usage.py), whose answers are recorded (pyoracle).
func Test33GuardUsagePython(t *testing.T) {
	home := t.TempDir()
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		key, _, _ := strings.Cut(kv, "=")
		return key == "HOME" || key == "CODEX_HOME" || key == "XDG_STATE_HOME"
	})
	env = append(env, "HOME="+home, "CODEX_HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "xdg"))
	writeTest(t, filepath.Join(home, "bad-utf8"), []byte{0xff})
	writeTest(t, filepath.Join(home, "object"), []byte("{}"))
	writeTest(t, filepath.Join(home, "list"), []byte("[]"))
	writeTest(t, filepath.Join(home, "bad-json"), []byte("{"))
	for i, c := range []struct {
		args    []string
		payload string
	}{
		{[]string{"--stop-input", filepath.Join(home, "absent")}, ""},
		{[]string{"--stop-input", filepath.Join(home, "bad-utf8")}, ""},
		{[]string{"--stop-input", filepath.Join(home, "list")}, ""},
		{[]string{"--stop-input", filepath.Join(home, "bad-json")}, ""},
		{[]string{"--mode", "hold", "--no-record", "--stop-input", filepath.Join(home, "object")}, ""},
		{nil, "not json"},
		{nil, "[]"},
	} {
		run := func(argv ...string) outcomeBytes {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Env = env
			cmd.Stdin = strings.NewReader(c.payload)
			return runOutcome(t, cmd)
		}
		got := run(append([]string{binary(t), "relay", "guard-evaluate"}, c.args...)...)
		want := pythonOutcome(t, strconv.Itoa(i), func() outcomeBytes {
			return run(append([]string{python(t), "-m", "codex_session_relay.cli", "guard-evaluate"}, c.args...)...)
		}, pyoracle.Substitute(home, "<HOME>"))
		if got != want {
			t.Fatalf("%q: Go %+v\nPython %+v", c.args, got, want)
		}
	}
}

func Test33HoldReservation(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "a")
	o := GuardOptions{Root: root, Now: "2026-01-01T00:00:00Z", Mode: Hold}
	ctx := context.Background()
	ok, err := reserveHold(ctx, directory, "s", "t", o)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	ok, err = reserveHold(ctx, directory, "s", "t", o)
	if err != nil || ok {
		t.Fatalf("duplicate %v %v", ok, err)
	}
	counts, bad, unreadable := HoldCounters(ctx, directory, "s", "t", o.Now, root)
	if bad != "" || unreadable != "" || get(counts, "holdsThisTurn") != int64(1) || get(counts, "holdsThisGeneration") != int64(1) || get(counts, "holdsThisSessionWindow") != int64(1) {
		t.Fatal(counts, bad, unreadable)
	}
}
func Test33DecisionBoundsAndIdentity(t *testing.T) {
	assignment := delivery.AssignmentID("dispatch")
	marker := Object{{Key: "intent", Value: Object{{Key: "dispatchRequestIdHash", Value: assignment}}}, {Key: "bound", Value: Object{{Key: "sessionId", Value: "s"}}}, {Key: "relationship", Value: Object{{Key: "relationshipId", Value: "r"}}}, {Key: "claims", Value: []any{Object{{Key: "sessionId", Value: "s"}, {Key: "dispatchRequestId", Value: "dispatch"}, {Key: "factId", Value: "claims/s/claim.json"}}}}}
	stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}}
	o := Observation{Stop: stop, Marker: marker, Assignment: assignment, Now: "2026-01-01T00:00:00Z"}
	for _, c := range []struct {
		mode            string
		counts          Object
		state, decision string
	}{{Hold, Object{}, "undeclared_turn_end", "block"}, {Observe, Object{}, "undeclared_turn_end", "release"}, {Hold, Object{{Key: "holdsThisTurn", Value: int64(1)}}, "hold_in_flight", "release"}, {Hold, Object{{Key: "holdsThisGeneration", Value: int64(2)}, {Key: "holdsThisTurn", Value: int64(1)}}, "unresolved_handoff", "release"}, {Hold, Object{{Key: "holdsThisSessionWindow", Value: int64(3)}}, "unresolved_handoff", "release"}, {Hold, Object{{Key: "holdsThisTurn", Value: nil}}, "marker_malformed", "release"}} {
		v := Decide(o, c.counts, c.mode)
		if get(v, "state") != c.state || get(v, "decision") != c.decision {
			t.Fatal(v)
		}
	}
	o.Disposition = Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "other"}, {Key: "outcome", Value: "failed"}}
	if ClassifyDeclaration(o) != "undeclared_turn_end" {
		t.Fatal("foreign turn released")
	}
	o.Disposition = Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "t"}, {Key: "outcome", Value: "failed"}}
	if !strings.HasPrefix(ClassifyDeclaration(o), "declared_") {
		t.Fatal("own failure did not release")
	}
	o.Receipt = Object{{Key: "sessionId", Value: "s"}, {Key: "turnId", Value: "t"}, {Key: "relationshipId", Value: "foreign"}, {Key: "atCurrentHead", Value: true}}
	if ReceiptMatches(o.Receipt, stop, marker) {
		t.Fatal("foreign assignment receipt matched")
	}
}
