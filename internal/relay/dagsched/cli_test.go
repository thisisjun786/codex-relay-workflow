package dagsched

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// crw runs the built binary as an operator would, one process per command: crw relay --state <state> <args>.
func crw(t testing.TB, state string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(testsupport.CRW(t), append([]string{"relay", "--state", state}, args...)...)
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		code = exit.ExitCode()
	}
	return out.String(), code
}

func parseOut(t testing.TB, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return m
}

// cliState builds a store under a state directory with the fork/join plan and its first two nodes accepted, then closes it so the binary is the only user.
func cliState(t *testing.T) (state string, f *fixture) {
	t.Helper()
	state = filepath.Join(t.TempDir(), "state")
	f = newFixtureAt(t, filepath.Join(state, "relay.sqlite3"))
	forkJoinPlan(f, "p1")
	f.projectParent()
	f.acceptNode("p1", "research", acceptOpts{})
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	return state, f
}

func catalogOf(t testing.TB, path string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT type || ' ' || name, COALESCE(sql, '') FROM sqlite_master")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

func passCount(t testing.TB, state string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM dag_passes").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCLIReady(t *testing.T) {
	state, _ := cliState(t)
	first, code := crw(t, state, "dag-ready", "--plan", "p1")
	second, _ := crw(t, state, "dag-ready", "--plan", "p1")
	if code != 0 || first != second {
		t.Fatalf("two reads of one store differ (exit %d):\n%s\n%s", code, first, second)
	}
	reading := parseOut(t, first)
	if reading["ok"] != true || reading["schema"] != SchemaReading || reading["plan_id"] != "p1" || reading["plan_revision"] != float64(1) {
		t.Fatalf("envelope:\n%s", first)
	}
	pass := reading["pass"].(map[string]any)
	if pass["free_slots"] != float64(6) || pass["ceiling"] != float64(6) || pass["ceiling_source"] != "standing_cap" || pass["held"] != float64(0) ||
		pass["ready_count"] != float64(1) || pass["deciding_limit"] != "none" {
		t.Fatalf("pass = %v", pass)
	}
	ready := reading["ready"].([]any)
	if len(ready) != 1 || ready[0].(map[string]any)["node_id"] != "design" {
		t.Fatalf("ready = %v, want design (research is accepted)", ready)
	}
	rank := ready[0].(map[string]any)["rank"].(map[string]any)
	if rank["critical_path"] != float64(4) || rank["descendants"] != float64(5) || rank["ready_since"] == nil {
		t.Fatalf("rank = %v", rank)
	}
	var ids []string
	reasons := map[string]any{}
	for _, n := range reading["nodes"].([]any) {
		m := n.(map[string]any)
		ids = append(ids, m["node_id"].(string))
		reasons[m["node_id"].(string)] = m["reason"]
	}
	if !reflect.DeepEqual(ids, []string{"design", "impl-a", "impl-b", "join", "research", "ship", "stack2"}) {
		t.Fatalf("nodes = %v, want every live node sorted by id", ids)
	}
	if reasons["research"] != DoneAccepted || reasons["impl-a"] != WaitEdge("e1") || reasons["ship"] != WaitEdge("e5") && reasons["ship"] != DeferAuthorityPending {
		t.Fatalf("reasons = %v", reasons)
	}

	// reading writes nothing; recording needs an actor and keeps a row each time
	if n := passCount(t, state); n != 0 {
		t.Fatalf("two reads left %d passes", n)
	}
	if out, code := crw(t, state, "dag-ready", "--plan", "p1", "--record"); code != 4 {
		t.Fatalf("--record without --actor: exit %d\n%s", code, out)
	}
	if n := passCount(t, state); n != 0 {
		t.Fatalf("a refused record left %d passes", n)
	}
	for want := 1; want <= 2; want++ {
		out, code := crw(t, state, "dag-ready", "--plan", "p1", "--record", "--actor", "parent")
		m := parseOut(t, out)
		if code != 0 || m["pass_seq"] != float64(want) || m["input_digest"] != reading["input_digest"] {
			t.Fatalf("record %d: exit %d\n%s", want, code, out)
		}
	}
	if out, code := crw(t, state, "dag-ready", "--plan", "nope"); code != 2 || parseOut(t, out)["reason"] != "unregistered_scope" {
		t.Fatalf("unknown plan: exit %d\n%s", code, out)
	}
}

func TestCLIRegionDeclare(t *testing.T) {
	state, _ := cliState(t)
	regions := `[{"repository":"owner/repo","path":"internal/x","kind":"tree"},{"repository":"owner/repo","path":"go.mod","kind":"file"}]`
	out, code := crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", regions)
	m := parseOut(t, out)
	if code != 0 || m["declaration_seq"] != float64(1) || m["replayed"] != false {
		t.Fatalf("declare: exit %d\n%s", code, out)
	}
	got := m["regions"].([]any)
	if len(got) != 2 || got[0].(map[string]any)["path"] != "go.mod" || got[0].(map[string]any)["exclusive"] != true || got[1].(map[string]any)["exclusive"] != false {
		t.Fatalf("regions = %v, want sorted with go.mod exclusive", got)
	}
	file := filepath.Join(t.TempDir(), "regions.json")
	if err := os.WriteFile(file, []byte(regions), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = crw(t, state, "dag-region-declare", "--plan", "p1", "--node", "impl-a", "--actor", "parent", "--regions", "@"+file)
	if m := parseOut(t, out); code != 0 || m["declaration_seq"] != float64(1) || m["replayed"] != true {
		t.Fatalf("replay from a file: exit %d\n%s", code, out)
	}
	for _, c := range []struct {
		name   string
		args   []string
		code   int
		reason string
	}{
		{"an unknown plan", []string{"--plan", "nope", "--node", "impl-a", "--regions", regions}, 2, "unregistered_scope"},
		{"a node that edits nothing", []string{"--plan", "p1", "--node", "design", "--regions", regions}, 2, "disposition_conflict"},
		{"a malformed region", []string{"--plan", "p1", "--node", "impl-a", "--regions", `[{"repository":"owner/repo","path":"/abs","kind":"file"}]`}, 2, "malformed_receipt"},
		{"a document that is not JSON", []string{"--plan", "p1", "--node", "impl-a", "--regions", "{"}, 4, ""},
		{"an unknown field", []string{"--plan", "p1", "--node", "impl-a", "--regions", `[{"repository":"owner/repo","path":"a.go","kind":"file","colour":"red"}]`}, 4, ""},
		{"a missing file", []string{"--plan", "p1", "--node", "impl-a", "--regions", "@/nonexistent/regions.json"}, 4, ""},
	} {
		out, code := crw(t, state, append([]string{"dag-region-declare", "--actor", "parent"}, c.args...)...)
		if code != c.code || (c.reason != "" && parseOut(t, out)["reason"] != c.reason) {
			t.Errorf("%s: exit %d\n%s", c.name, code, out)
		}
	}
	// the declaration is in force: impl-a now holds regions, so an undeclared impl-b waits on it once impl-a is running.
	out, _ = crw(t, state, "dag-ready", "--plan", "p1")
	if parseOut(t, out)["ok"] != true {
		t.Fatalf("a reading after a declaration:\n%s", out)
	}
}

// A read of a store that predates the DAG zone answers unregistered_scope and leaves the schema alone.
func TestReadOnlyReadNeverCreatesTheZone(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(state, "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	before := catalogOf(t, path)
	out, code := crw(t, state, "dag-ready", "--plan", "p1")
	if m := parseOut(t, out); code != 2 || m["reason"] != "unregistered_scope" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !reflect.DeepEqual(catalogOf(t, path), before) {
		t.Fatal("a read created the zone")
	}
	// a recorded pass is a write: it opens for writing, creates the zone and still refuses the missing plan
	out, code = crw(t, state, "dag-ready", "--plan", "p1", "--record", "--actor", "parent")
	if m := parseOut(t, out); code != 2 || m["reason"] != "unregistered_scope" {
		t.Fatalf("record on a missing plan: exit %d\n%s", code, out)
	}
	if _, ok := catalogOf(t, path)["table dag_passes"]; !ok {
		t.Fatal("the write open did not create the zone")
	}
}
