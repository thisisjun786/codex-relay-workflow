package cli_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// goStore is a Go-owned store, closed, as the writer form that creates it leaves it: its
// database, ownership mirror and write gate.
func goStore(t *testing.T, alias, state string) {
	t.Helper()
	if got := binaryRun(t, alias, "--state", state, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("create the store: %+v", got)
	}
}

// doctorWatched runs the built binary's doctor on state with the state directory watched, and
// returns the report with every name the run created, removed or moved and whether it opened the
// write gate. A report that asked for a proof it could not give (--expect-nonce alone leaves the
// store unproven) exits 2 with the whole diagnosis, as it always has.
func doctorWatched(t *testing.T, alias, state string, argv ...string) (report map[string]any, changed []string, gateOpened bool) {
	t.Helper()
	watch := testsupport.WatchDir(t, state)
	got := binaryRun(t, alias, append([]string{"--state", state, "doctor"}, argv...)...)
	events := watch.Drain()
	if got.code != 0 && !(got.code == 2 && slices.Contains(argv, "--expect-nonce")) {
		t.Fatalf("doctor %v: %+v", argv, got)
	}
	for _, op := range []string{"create", "delete", "move"} {
		changed = append(changed, testsupport.Names(events, op)...)
	}
	return decode(t, got.stdout), changed, slices.Contains(testsupport.Names(events, "open"), "write-gate.lock")
}

func writeProbeOf(t *testing.T, report map[string]any) [3]any {
	t.Helper()
	probe := obj(report["writeProbe"])
	if len(probe) != 3 {
		t.Fatalf("writeProbe %v", report["writeProbe"])
	}
	return [3]any{probe["requested"], probe["ran"], probe["judgedBy"]}
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	listed, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range listed {
		names = append(names, entry.Name())
	}
	return names
}

var fencedEntries = []string{"relay.sqlite3", "takeover.json", "write-gate.lock"}

// The default doctor reads: it creates, removes and moves nothing in the state directory, never
// opens write-gate.lock, and says its writability readings are judged. They are still true of a
// store this process may write, in every place the report carries them.
func TestDoctor_by_default_creates_nothing_and_takes_no_write_gate(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	state := filepath.Join(home, "state")
	goStore(t, alias, state)

	report, changed, gate := doctorWatched(t, alias, state)

	if len(changed) != 0 || gate {
		t.Errorf("the default doctor changed %v or opened the write gate (%v)", changed, gate)
	}
	if got := writeProbeOf(t, report); got != [3]any{false, false, "permission"} {
		t.Errorf("writeProbe %v", got)
	}
	access := obj(report["access"])
	if access["dbWritable"] != true || access["directoryWritable"] != true || access["detail"] != nil {
		t.Errorf("access %v", access)
	}
	if obj(report["actorReachability"])["stateDirectoryWritable"] != true {
		t.Errorf("actorReachability %v", report["actorReachability"])
	}
	if observed := obj(obj(report["accessReceipt"])["observedAccess"]); observed["write"] != true || observed["directoryWritable"] != true {
		t.Errorf("observedAccess %v", observed)
	}
	if got := entries(t, state); !slices.Equal(got, fencedEntries) {
		t.Errorf("the default doctor left %v", got)
	}
}

// The write probe is what asks for the temporary file and the write gate, and says it measured.
func TestDoctor_probe_write_measures_by_writing(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	state := filepath.Join(home, "state")
	goStore(t, alias, state)
	watch := testsupport.WatchDir(t, state)

	got := binaryRun(t, alias, "--state", state, "doctor", "--probe-write")
	events := watch.Drain()

	if got.code != 0 {
		t.Fatalf("doctor --probe-write: %+v", got)
	}
	report := decode(t, got.stdout)
	if probe := writeProbeOf(t, report); probe != [3]any{true, true, "measured"} {
		t.Errorf("writeProbe %v", probe)
	}
	if access := obj(report["access"]); access["dbWritable"] != true || access["directoryWritable"] != true || access["detail"] != nil {
		t.Errorf("access %v", access)
	}
	named := func(op string) bool {
		return slices.ContainsFunc(testsupport.Names(events, op), func(name string) bool { return strings.HasPrefix(name, ".probe-") })
	}
	if !named("create") || !named("delete") || !slices.Contains(testsupport.Names(events, "open"), "write-gate.lock") {
		t.Errorf("the write probe left no trace of its file or its gate (events %v)", events)
	}
	if left := entries(t, state); !slices.Equal(left, fencedEntries) {
		t.Errorf("the write probe left %v", left)
	}
}

// A requested write probe that has nothing to try says so: it ran nothing and its readings are the
// refusal or the absence, never a measurement.
func TestDoctor_probe_write_that_cannot_run_says_it_did_not(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)

	absent := filepath.Join(home, "absent")
	got := binaryRun(t, alias, "--state", absent, "doctor", "--probe-write")
	if got.code != 0 {
		t.Fatalf("doctor --probe-write on an absent directory: %+v", got)
	}
	if probe := writeProbeOf(t, decode(t, got.stdout)); probe != [3]any{true, false, "permission"} {
		t.Errorf("an absent directory: writeProbe %v", probe)
	}
	if _, err := os.Lstat(absent); !os.IsNotExist(err) {
		t.Errorf("the write probe created what it was asked about: %v", err)
	}

	foreign := filepath.Join(home, "python-owned")
	pythonCreates(t, foreign)
	report, changed, gate := doctorWatched(t, alias, foreign, "--probe-write")
	if probe := writeProbeOf(t, report); probe != [3]any{true, false, "permission"} {
		t.Errorf("a foreign store: writeProbe %v", probe)
	}
	if access := obj(report["access"]); access["dbWritable"] != false || len(changed) != 0 || gate {
		t.Errorf("a foreign store: access %v, changed %v, gate %v", access, changed, gate)
	}
}

// What a live writer has committed only to its write-ahead log is what the sidecar-free reads of
// doctor --expect-nonce must still find, beside the live writer and after an unclean shutdown; and
// doctor --issue reads the clean store's rows. None of them creates a name but the index an unclean
// shutdown makes the plain read build.
func TestDoctor_reads_without_sidecars_and_still_reads_the_log(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	live := filepath.Join(home, "live")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	writer, err := store.Open(t.Context(), filepath.Join(live, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err = writer.DB.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	challenge, err := writer.WriteChallengeFor(t.Context(), "wal-only")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(live, "relay.sqlite3-wal")); err != nil || info.Size() <= 32 {
		t.Fatalf("the live log holds no frame: %v %v", info, err)
	}
	crashed := filepath.Join(home, "crashed")
	if err := os.MkdirAll(crashed, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"relay.sqlite3", "relay.sqlite3-wal", "takeover.json", "write-gate.lock"} {
		raw, err := os.ReadFile(filepath.Join(live, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(crashed, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name, state string
		mayCreate   []string
	}{{"beside a live writer", live, nil}, {"after an unclean shutdown", crashed, []string{"relay.sqlite3-shm"}}} {
		report, changed, gate := doctorWatched(t, alias, c.state, "--expect-nonce", challenge.Nonce)
		if nonce := obj(report["nonce"]); nonce["found"] != true || nonce["readable"] != true {
			t.Errorf("%s: a nonce committed only to the log was not found: %v", c.name, report["nonce"])
		}
		for _, name := range changed {
			if !slices.Contains(c.mayCreate, name) {
				t.Errorf("%s: the doctor created, removed or moved %q", c.name, name)
			}
		}
		if probe := writeProbeOf(t, report); gate || probe != [3]any{false, false, "permission"} {
			t.Errorf("%s: gate opened %v, writeProbe %v", c.name, gate, probe)
		}
	}

	clean := filepath.Join(home, "clean")
	register(t, home, clean)
	report, changed, gate := doctorWatched(t, alias, clean, "--issue", issueKey)
	if issue := obj(report["issue"]); issue["holds"] != true || issue["storeAgreement"] != "same" {
		t.Errorf("the clean store's issue rows were not read: %v", report["issue"])
	}
	if len(changed) != 0 || gate {
		t.Errorf("the doctor changed %v or opened the write gate (%v) reading a clean store", changed, gate)
	}
}
