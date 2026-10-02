package dagsched

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// crwRun runs the built binary as an operator would, with a chosen environment, and returns both streams.
func crwRun(t testing.TB, state string, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(testsupport.CRW(t), append([]string{"relay", "--state", state}, args...)...)
	if env != nil {
		cmd.Env = env
	}
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), errs.String(), code
}

// envWithPath is the test process's environment with PATH replaced.
func envWithPath(path string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") {
			out = append(out, kv)
		}
	}
	return append(out, "PATH="+path)
}

// dumpStore is everything a reader could tell apart about a store: the text of every object and a hash of every row of every table.
func dumpStore(t testing.TB, path string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT type, name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	var tables []string
	for rows.Next() {
		var typ, name, ddl string
		if err := rows.Scan(&typ, &name, &ddl); err != nil {
			t.Fatal(err)
		}
		out[typ+" "+name] = ddl
		if typ == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		r, err := db.Query("SELECT * FROM \"" + table + "\"")
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		h := sha256.New()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%v\n", vals)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		out["rows "+table] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

func journalSeq(t testing.TB, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var seq int64
	if err := db.QueryRow("SELECT COALESCE(MAX(seq), 0) FROM journal").Scan(&seq); err != nil {
		t.Fatal(err)
	}
	return seq
}

func sameStore(t testing.TB, what string, before, after map[string]string) {
	t.Helper()
	for key, value := range before {
		if after[key] != value {
			t.Errorf("%s changed %s", what, key)
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			t.Errorf("%s added %s", what, key)
		}
	}
}

// Criterion c2: the query reads the store and changes nothing in it. The command runs with PATH emptied (no git, no gh to start) and the journal sequence, every object and every row are the same
// afterwards.
func TestCLIProgressReadsOnlyTheStore(t *testing.T) {
	state, _ := cliState(t)
	db := filepath.Join(state, "relay.sqlite3")
	// a journal that is not empty, so a query that wrote a row would move a sequence that has somewhere to move from
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := raw.Exec("INSERT INTO journal (at, kind, subject, detail) VALUES ('2026-10-02T00:00:00+00:00', 'fixture', 'p1', '{}')"); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	before, seq := dumpStore(t, db), journalSeq(t, db)
	if seq < 3 {
		t.Fatalf("the journal sequence is %d: the fixture did not write its rows", seq)
	}
	out, errs, code := crwRun(t, state, envWithPath(t.TempDir()), "dag-progress", "--plan", "p1")
	if code != 0 {
		t.Fatalf("dag-progress exit %d\n%s\n%s", code, out, errs)
	}
	doc := parseOut(t, out)
	if doc["ok"] != true || doc["schema"] != SchemaProgress || doc["plan_id"] != "p1" || doc["plan_revision"] != float64(1) {
		t.Fatalf("answer = %v", doc)
	}
	denominator, _ := doc["denominator"].(map[string]any)
	if denominator["nodes"] != float64(7) || denominator["changed"] != true {
		t.Errorf("denominator = %v", denominator)
	}
	stages, _ := doc["stages"].([]any)
	got := map[string]float64{}
	for _, item := range stages {
		s, _ := item.(map[string]any)
		got[s["stage"].(string)] = s["nodes"].(float64)
	}
	if got[StageAccepted] != 1 || got[StageReady] != 1 || got[StageWaitingPredecessor] != 4 || got[StageWaitingDecision] != 1 {
		t.Errorf("stages = %v, want research accepted, design ready, four waiting on a predecessor and ship on a decision", got)
	}
	again, _, _ := crwRun(t, state, nil, "dag-progress", "--plan", "p1")
	if again != out {
		t.Error("two runs against one store print different documents")
	}
	sameStore(t, "dag-progress", before, dumpStore(t, db))
	if journalSeq(t, db) != seq {
		t.Errorf("the journal sequence moved from %d to %d", seq, journalSeq(t, db))
	}
}

// owingState is a store that still owes the repairs a writer's open makes: a settlement backfilled from an observation, and an index that was dropped. Each call builds its own, so the control and
// the command under test never share a path (the ownership record names the path a store was created at).
func owingState(t *testing.T) (state, db string) {
	t.Helper()
	state, _ = cliState(t)
	db = filepath.Join(state, "relay.sqlite3")
	raw, err := sql.Open("sqlite", "file:"+db)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"DELETE FROM assignment_settlements",
		"INSERT INTO observations (thread_id, turn_id, terminal_status, relationship_id, classification, event_id, observed_at) VALUES ('child-research', 't1', 'completed', 'rel-p1-research', 'x', NULL, '2026-10-02T00:00:00+00:00')",
		"DROP INDEX IF EXISTS journal_kind",
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return state, db
}

// Criterion c2, the opener: the command opens the store strictly read-only. A store that still owes the repairs a writer's open makes stays exactly as it is; the control shows that the writer's opener,
// which dag-ready uses, does repair such a store, so the check can tell the two openers apart.
func TestCLIProgressOpensTheStoreReadOnly(t *testing.T) {
	controlState, controlDB := owingState(t)
	owed := dumpStore(t, controlDB)
	if _, errs, code := crwRun(t, controlState, nil, "dag-ready", "--plan", "p1"); code != 0 {
		t.Fatalf("control dag-ready exit %d: %s", code, errs)
	}
	repaired := dumpStore(t, controlDB)
	if _, ok := repaired["index journal_kind"]; !ok || repaired["rows assignment_settlements"] == owed["rows assignment_settlements"] {
		t.Fatal("the control did not repair the store: the writer's opener is not what dag-ready uses here, and this test would not tell the openers apart")
	}
	state, db := owingState(t)
	before := dumpStore(t, db)
	out, errs, code := crwRun(t, state, nil, "dag-progress", "--plan", "p1")
	if code != 0 {
		t.Fatalf("dag-progress exit %d\n%s\n%s", code, out, errs)
	}
	after := dumpStore(t, db)
	sameStore(t, "dag-progress", before, after)
	if _, ok := after["index journal_kind"]; ok {
		t.Error("dag-progress recreated a dropped index")
	}
}

// The refusals are the relay's: a missing option is the parser's (exit 2, usage on stderr), an unknown plan is unregistered_scope, and a state directory with no store is the reason every read-only
// relay command gives, which dag-progress must give as dag-ready does.
func TestCLIProgressRefusals(t *testing.T) {
	state, _ := cliState(t)
	db := filepath.Join(state, "relay.sqlite3")
	before := dumpStore(t, db)
	if _, errs, code := crwRun(t, state, nil, "dag-progress"); code != 2 || !strings.Contains(errs, "--plan") {
		t.Errorf("no --plan: exit %d, stderr %q", code, errs)
	}
	out, _, code := crwRun(t, state, nil, "dag-progress", "--plan", "nope")
	if code != 2 || parseOut(t, out)["reason"] != "unregistered_scope" {
		t.Errorf("unknown plan: exit %d %s", code, out)
	}
	sameStore(t, "a refused dag-progress", before, dumpStore(t, db))

	absent := filepath.Join(t.TempDir(), "nowhere")
	readyOut, _, readyCode := crwRun(t, absent, nil, "dag-ready", "--plan", "p1")
	progressOut, _, progressCode := crwRun(t, absent, nil, "dag-progress", "--plan", "p1")
	if progressCode != readyCode || parseOut(t, progressOut)["reason"] != parseOut(t, readyOut)["reason"] || parseOut(t, progressOut)["reason"] != "store_absent" {
		t.Errorf("no store: dag-ready exit %d %s, dag-progress exit %d %s", readyCode, readyOut, progressCode, progressOut)
	}
	if _, err := os.Stat(absent); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dag-progress created %s", absent)
	}

	// a partial store (a write gate with no database) is refused as dag-ready refuses it, not reported as absent
	partial := filepath.Join(t.TempDir(), "partial")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "write-gate.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	readyOut, _, readyCode = crwRun(t, partial, nil, "dag-ready", "--plan", "p1")
	progressOut, _, progressCode = crwRun(t, partial, nil, "dag-progress", "--plan", "p1")
	if progressCode != readyCode || progressOut != readyOut || parseOut(t, progressOut)["reason"] == "store_absent" {
		t.Errorf("a partial store: dag-ready exit %d %s, dag-progress exit %d %s", readyCode, readyOut, progressCode, progressOut)
	}
	if _, err := os.Stat(filepath.Join(partial, "relay.sqlite3")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dag-progress created a database in the partial store directory")
	}
}

// Criterion c2 and c3 end to end: with every artifact file deleted the printed document is the same bytes, where dag-ready, which reads the files, now blocks a node.
func TestCLIProgressDoesNotReadArtifacts(t *testing.T) {
	state, _ := cliState(t)
	var artifacts []string
	db := filepath.Join(state, "relay.sqlite3")
	raw, err := sql.Open("sqlite", "file:"+db+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := raw.Query("SELECT receipt FROM events")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var receipt string
		if err := rows.Scan(&receipt); err != nil {
			t.Fatal(err)
		}
		artifacts = append(artifacts, receiptEntriesOf(receipt)...)
	}
	_ = rows.Close()
	_ = raw.Close()
	if len(artifacts) == 0 {
		t.Fatal("the fixture has no artifact to delete")
	}
	with, _, code := crwRun(t, state, nil, "dag-progress", "--plan", "p1")
	if code != 0 {
		t.Fatal(with)
	}
	readyWith, _, _ := crwRun(t, state, nil, "dag-ready", "--plan", "p1")
	for _, file := range artifacts {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	without, _, code := crwRun(t, state, nil, "dag-progress", "--plan", "p1")
	if code != 0 || without != with {
		t.Errorf("deleting the artifacts changed the progress document (exit %d)", code)
	}
	readyWithout, _, _ := crwRun(t, state, nil, "dag-ready", "--plan", "p1")
	if readyWithout == readyWith {
		t.Error("the control failed: dag-ready, which reads the artifact files, did not notice them gone")
	}
}

// receiptEntriesOf lists the artifact paths a stored receipt declares.
func receiptEntriesOf(receipt string) []string {
	entries, ok := receiptEntries(receipt)
	if !ok {
		return nil
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}
