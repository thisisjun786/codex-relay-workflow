package dagsched

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-283: the seven commands through the real binary, as the parent runs them: JSON on stdout, exit 0 for an answer and 2 for a refusal with its reason.

// summaryState is a store under a state directory with the fork/join plan, the project's parent, and an execution ledger (a running child, an accepted result, a delivery), closed so
// the binary is the only user.
func summaryState(t *testing.T) string {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	f := newFixtureAt(t, filepath.Join(state, "relay.sqlite3"))
	forkJoinPlan(f, "p1")
	f.projectParent()
	seedExecutionLedger(f)
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	return state
}

func summaryRun(t *testing.T, state string, wantExit int, args ...string) map[string]any {
	t.Helper()
	out, code := crw(t, state, args...)
	if code != wantExit {
		t.Fatalf("%s: exit %d, want %d\n%s", strings.Join(args, " "), code, wantExit, out)
	}
	return parseOut(t, out)
}

func objectOf(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	o, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in %v", key, m)
	}
	return o
}

// Criterion c3, through the binary: from the first open to the last write, every command of the parent's flow, a lost response and its recovery included, changes the summary table
// and nothing else in the store (the journal sequence, every other table and every object stay as they were), and the flow's answers are the ones the skill relies on.
func TestCLISummaryFlowTouchesOnlyTheSummaryTable(t *testing.T) {
	state := summaryState(t)
	db := filepath.Join(state, "relay.sqlite3")
	stage := func(what string, run func()) {
		t.Helper()
		before := dumpStore(t, db)
		run()
		after := dumpStore(t, db)
		for key := range after {
			if key != "rows dag_summary_outbox" && after[key] != before[key] {
				t.Errorf("%s changed %s", what, key)
			}
		}
	}
	// the linear document, kept in a file as the parent keeps its read of it
	page := filepath.Join(t.TempDir(), "doc.txt")
	write := func(text string) {
		if err := os.WriteFile(page, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var id, token, container, empty string
	stage("enqueue", func() {
		out := summaryRun(t, state, 0, "dag-summary-enqueue", "--plan", "p1", "--actor", "parent", "--document", "doc-1")
		entry := objectOf(t, out, "entry")
		id = entry["summary_id"].(string)
		if out["schema"] != "dag-summary-enqueue/1" || out["replayed"] != false || entry["state"] != "pending" || entry["seq"] != float64(1) || entry["document"] != "doc-1" {
			t.Fatalf("%v", out)
		}
		if _, leaked := entry["summary"]; leaked {
			t.Fatal("the entry object carries the whole summary text")
		}
	})
	stage("enqueue again", func() {
		out := summaryRun(t, state, 0, "dag-summary-enqueue", "--plan", "p1", "--actor", "parent", "--document", "doc-1")
		if out["replayed"] != true || objectOf(t, out, "entry")["summary_id"] != id {
			t.Fatalf("%v", out)
		}
	})
	stage("claim", func() {
		out := summaryRun(t, state, 0, "dag-summary-claim", "--summary", id, "--actor", "parent")
		op := objectOf(t, out, "operation")
		token = out["claim_token"].(string)
		container, empty = op["container"].(string), op["empty_container"].(string)
		if token == "" || container == "" || empty == "" || op["document"] != "doc-1" || op["block"] == "" || len(op["protocol"].([]any)) < 4 {
			t.Fatalf("%v", out)
		}
	})
	// the write lands and its response is lost: the parent records a failure
	write("# page\n\n" + container + "\n")
	stage("fail", func() {
		out := summaryRun(t, state, 0, "dag-summary-fail", "--summary", id, "--actor", "parent", "--claim-token", token, "--error", "the response was lost")
		if e := objectOf(t, out, "entry"); e["state"] != "pending" || e["attempts"] != float64(1) || e["last_error"] != "the response was lost" {
			t.Fatalf("%v", out)
		}
	})
	stage("a stale token", func() {
		out := summaryRun(t, state, 2, "dag-summary-complete", "--summary", id, "--actor", "parent", "--claim-token", token, "--document", "doc-1", "--readback", "@"+page)
		if out["reason"] != "sync_not_claimable" {
			t.Fatalf("%v", out)
		}
	})
	stage("claim again", func() {
		out := summaryRun(t, state, 0, "dag-summary-claim", "--summary", id, "--actor", "parent")
		token = out["claim_token"].(string)
	})
	stage("reconcile", func() {
		out := summaryRun(t, state, 0, "dag-summary-reconcile", "--summary", id, "--observed", "@"+page)
		if out["schema"] != "dag-summary-reconcile/1" || out["outcome"] != "already_written" || out["writable"] != true {
			t.Fatalf("%v", out)
		}
	})
	stage("complete", func() {
		out := summaryRun(t, state, 0, "dag-summary-complete", "--summary", id, "--actor", "parent", "--claim-token", token, "--document", "doc-1", "--readback", "@"+page)
		if e := objectOf(t, out, "entry"); e["state"] != "confirmed" || out["replayed"] != false || e["confirmed_at"] == nil {
			t.Fatalf("%v", out)
		}
	})
	stage("status", func() {
		out := summaryRun(t, state, 0, "dag-summary-status", "--plan", "p1", "--history")
		streams := out["documents"].([]any)
		if out["schema"] != "dag-summary-status/1" || len(streams) != 1 {
			t.Fatalf("%v", out)
		}
		s := streams[0].(map[string]any)
		if s["document"] != "doc-1" || s["owed"] != false || s["up_to_date"] != true || len(s["entries"].([]any)) != 1 {
			t.Fatalf("%v", s)
		}
	})
	stage("a retry of a confirmed entry", func() {
		out := summaryRun(t, state, 2, "dag-summary-retry", "--summary", id, "--actor", "parent")
		if out["reason"] != "sync_not_claimable" {
			t.Fatalf("%v", out)
		}
	})
}

// The commands that only read open the store read-only: a store that still owes the repairs a writer's open makes is left exactly as it is, and a state directory with no store is
// the refusal every read-only command gives.
func TestCLISummaryReadOnlyCommandsChangeNothing(t *testing.T) {
	state, db := owingState(t)
	if out := summaryRun(t, state, 0, "dag-summary-status", "--plan", "p1"); len(out["documents"].([]any)) != 0 {
		t.Fatalf("a plan with no summary reads %v", out)
	}
	before := dumpStore(t, db)
	for _, args := range [][]string{
		{"dag-summary-status", "--plan", "p1"},
		{"dag-summary-status", "--plan", "p1", "--document", "doc-1", "--history"},
	} {
		summaryRun(t, state, 0, args...)
	}
	if out := summaryRun(t, state, 2, "dag-summary-reconcile", "--summary", "sum-none", "--observed", "text"); out["reason"] != "unregistered_scope" {
		t.Fatalf("%v", out)
	}
	sameStore(t, "the read-only summary commands", before, dumpStore(t, db))
	empty := filepath.Join(t.TempDir(), "none")
	out, code := crw(t, empty, "dag-summary-status", "--plan", "p1")
	if code != 2 || parseOut(t, out)["reason"] != "store_absent" {
		t.Fatalf("a state directory with no store: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(empty, "relay.sqlite3")); err == nil {
		t.Fatal("a read created a store")
	}
}

// A refusal is exit 2 with an existing reason and writes nothing.
func TestCLISummaryRefusals(t *testing.T) {
	state := summaryState(t)
	db := filepath.Join(state, "relay.sqlite3")
	before := dumpStore(t, db)
	cases := []struct {
		name   string
		reason string
		args   []string
	}{
		{"an unknown plan", "unregistered_scope", []string{"dag-summary-enqueue", "--plan", "nope", "--actor", "parent", "--document", "doc-1"}},
		{"an actor that is not the project's parent", "scope_role_mismatch", []string{"dag-summary-enqueue", "--plan", "p1", "--actor", "intruder", "--document", "doc-1"}},
		{"a padded document", "malformed_receipt", []string{"dag-summary-enqueue", "--plan", "p1", "--actor", "parent", "--document", " doc-1"}},
		{"a claim of an entry that is not there", "unregistered_scope", []string{"dag-summary-claim", "--summary", "sum-none", "--actor", "parent"}},
		{"a failure of an entry that is not there", "unregistered_scope", []string{"dag-summary-fail", "--summary", "sum-none", "--actor", "parent", "--claim-token", "t", "--error", "e"}},
		{"a retry of an entry that is not there", "unregistered_scope", []string{"dag-summary-retry", "--summary", "sum-none", "--actor", "parent"}},
		{"a status of an unknown plan", "unregistered_scope", []string{"dag-summary-status", "--plan", "nope"}},
	}
	for _, c := range cases {
		out, code := crw(t, state, c.args...)
		if m := parseOut(t, out); code != 2 || m["reason"] != c.reason {
			t.Errorf("%s: exit %d\n%s", c.name, code, out)
		}
	}
	sameStore(t, "refused summary commands", before, dumpStore(t, db))
}

// A document is read whole or refused: a copy cut at a limit could hide a second container or an unclosed marker beyond the cut and read as clean. The document here carries the entry's
// block at the start and a second container after 1.5 MiB of text; a reader that stopped at 1 MiB would say already_written. A document over the limit is a usage error (exit 4), and an option that is missing is the
// parser's (exit 2, usage on stderr, nothing on stdout).
func TestCLISummaryDocumentsAreReadWholeOrRefused(t *testing.T) {
	state := summaryState(t)
	enq := summaryRun(t, state, 0, "dag-summary-enqueue", "--plan", "p1", "--actor", "parent", "--document", "doc-1")
	id := objectOf(t, enq, "entry")["summary_id"].(string)
	claim := summaryRun(t, state, 0, "dag-summary-claim", "--summary", id, "--actor", "parent")
	token := claim["claim_token"].(string)
	container := objectOf(t, claim, "operation")["container"].(string)
	dir := t.TempDir()
	file := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return "@" + path
	}
	clean := file("clean.txt", "# page\n\n"+container+"\n")
	if out := summaryRun(t, state, 0, "dag-summary-reconcile", "--summary", id, "--observed", clean); out["outcome"] != "already_written" || out["repair"] != "none" {
		t.Fatalf("%v", out)
	}
	padding := strings.Repeat("a line of text that is not a marker\n", 45000) // 1.6 MiB
	long := file("long.txt", "# page\n\n"+container+"\n\n"+padding+container+"\n")
	if len(padding) < 1<<20+1<<19 {
		t.Fatalf("the padding is only %d bytes", len(padding))
	}
	out := summaryRun(t, state, 0, "dag-summary-reconcile", "--summary", id, "--observed", long)
	if out["outcome"] != "malformed" || out["repair"] != "manual" || out["container"] != "present" {
		t.Fatalf("a second container beyond 1 MiB was not seen: %v", out)
	}
	refused := summaryRun(t, state, 2, "dag-summary-complete", "--summary", id, "--actor", "parent", "--claim-token", token, "--document", "doc-1", "--readback", long)
	if refused["reason"] != "readback_mismatch" {
		t.Fatalf("a readback with a second container was confirmed: %v", refused)
	}
	if out, code := crw(t, state, "dag-summary-reconcile", "--summary", id, "--observed", file("huge.txt", strings.Repeat("x", 8<<20+1))); code != 4 {
		t.Fatalf("a document over 8 MiB: exit %d\n%.200s", code, out)
	}
	if out, code := crw(t, state, "dag-summary-reconcile", "--summary", id, "--observed", "@"+filepath.Join(dir, "absent.txt")); code != 4 {
		t.Fatalf("a document that cannot be read: exit %d\n%.200s", code, out)
	}
	if out, code := crw(t, state, "dag-summary-claim", "--actor", "parent"); code != 2 || out != "" {
		t.Fatalf("a missing option: exit %d, stdout %q", code, out)
	}
}
