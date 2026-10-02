package dag

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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

func writeDoc(t testing.TB, dir, name string, d doc) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw(t, d), 0o600); err != nil {
		t.Fatal(err)
	}
	return "@" + path
}

func parseOut(t testing.TB, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return m
}

func ids(list any, key string) []string {
	var out []string
	for _, item := range list.([]any) {
		out = append(out, item.(map[string]any)[key].(string))
	}
	return out
}

// The round trip an operator makes: define a fork/join plan, store it, read it back, revise it, read the log. Every
// command is its own process on the same store, so what is read back is what the store holds, not what the writer built.
func TestRoundTripCLI(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	file := writeDoc(t, dir, "r1.json", forkJoin("plan", "r1"))

	out, code := crw(t, state, "dag-plan-put", "--request", file)
	put := parseOut(t, out)
	if code != 0 || put["ok"] != true || put["replayed"] != false || put["revision_no"] != float64(1) || put["project_key"] != "P-TEST" {
		t.Fatalf("put: exit %d\n%s", code, out)
	}
	if digests := put["node_digests"].(map[string]any); len(digests) != 5 {
		t.Fatalf("put answered %d node digests", len(digests))
	}

	out, code = crw(t, state, "dag-plan-show", "--plan", "plan")
	show := parseOut(t, out)
	if code != 0 || show["schema"] != SchemaSnapshot || show["revision_no"] != float64(1) || show["head_revision_no"] != float64(1) || show["digests_verified"] != true {
		t.Fatalf("show: exit %d\n%s", code, out)
	}
	// the documented place of the plan in the envelope: top-level "nodes" and "edges"
	if got := ids(show["nodes"], "node_id"); !reflect.DeepEqual(got, []string{"design", "impl-a", "impl-b", "join", "ship"}) {
		t.Fatalf("nodes: %v", got)
	}
	if got := ids(show["edges"], "edge_id"); !reflect.DeepEqual(got, []string{"e1", "e2", "e3", "e4", "e5"}) {
		t.Fatalf("edges: %v", got)
	}
	// what a normal fork/join plan means survives the trip: the fork, the join and what each edge needs
	edges := map[string]map[string]any{}
	for _, e := range show["edges"].([]any) {
		m := e.(map[string]any)
		edges[m["edge_id"].(string)] = m
	}
	if edges["e1"]["from_node_id"] != "design" || edges["e1"]["to_node_id"] != "impl-a" || edges["e2"]["to_node_id"] != "impl-b" ||
		edges["e3"]["to_node_id"] != "join" || edges["e4"]["to_node_id"] != "join" || edges["e3"]["target_base_ref"] != "dev" ||
		edges["e5"]["kind"] != "decision" || edges["e5"]["decision_subject"] != "merge holds" {
		t.Fatalf("edges read back: %v", edges)
	}
	nodes := map[string]map[string]any{}
	for _, n := range show["nodes"].([]any) {
		m := n.(map[string]any)
		nodes[m["node_id"].(string)] = m
		if m["slice_digest"] != put["node_digests"].(map[string]any)[m["node_id"].(string)] {
			t.Fatalf("node %v: the digest read is not the digest written", m["node_id"])
		}
	}
	if nodes["impl-a"]["issue_key"] != "CRW-impl-a" || nodes["impl-a"]["kind"] != "implementation" || nodes["join"]["kind"] != "non_pr" {
		t.Fatalf("nodes read back: %v", nodes)
	}

	// a revision, then the earlier revision still reads as it was and the log carries both
	file2 := writeDoc(t, dir, "r2.json", revisions()[1])
	if out, code = crw(t, state, "dag-plan-put", "--request", file2); code != 0 {
		t.Fatalf("put 2: exit %d\n%s", code, out)
	}
	out, _ = crw(t, state, "dag-plan-show", "--plan", "plan", "--revision", "1")
	old := parseOut(t, out)
	if old["revision_no"] != float64(1) || old["head_revision_no"] != float64(2) || old["state_digest"] != show["state_digest"] {
		t.Fatalf("revision 1 after revision 2:\n%s", out)
	}
	out, _ = crw(t, state, "dag-plan-show", "--plan", "plan", "--verify")
	head := parseOut(t, out)
	if head["revision_no"] != float64(2) || head["log_verified"] != true || head["state_digest"] == show["state_digest"] {
		t.Fatalf("head with --verify:\n%s", out)
	}
	if title := nodeByID(head["nodes"])["impl-a"]["title"]; title != "Implementation A" {
		t.Fatalf("the edited node reads back as %v", title)
	}
	out, code = crw(t, state, "dag-plan-log", "--plan", "plan", "--after", "1")
	log := parseOut(t, out)
	events := log["events"].([]any)
	if code != 0 || len(events) != 1 || log["cursor"] != float64(2) || log["head_revision_no"] != float64(2) || events[0].(map[string]any)["revision_no"] != float64(2) {
		t.Fatalf("log after 1: exit %d\n%s", code, out)
	}
	if changes := events[0].(map[string]any)["changes"].([]any); changes[0].(map[string]any)["op"] != OpUpdateNode {
		t.Fatalf("the event does not carry its typed change: %v", changes)
	}

	// the same request again is the stored result
	out, code = crw(t, state, "dag-plan-put", "--request", file)
	if again := parseOut(t, out); code != 0 || again["replayed"] != true || again["revision_no"] != float64(1) || again["state_digest"] != put["state_digest"] {
		t.Fatalf("repeated put: exit %d\n%s", code, out)
	}
}

func nodeByID(list any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, n := range list.([]any) {
		m := n.(map[string]any)
		out[m["node_id"].(string)] = m
	}
	return out
}

// The same invalid input is answered the same way, byte for byte, with a reason a program can read and a detail a person can act on.
func TestInvalidPlansAreRefusedTheSameWayEveryTime(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	cycle := writeDoc(t, dir, "cycle.json", revDoc("plan", "c", 0, addNode("a", NodeNonPR), addNode("b", NodeNonPR),
		addEdge("e1", "a", "b", EdgeArtifactVerified), addEdge("e2", "b", "a", EdgeArtifactVerified)))
	inline := writeDoc(t, dir, "inline.json", with(revDoc("plan", "i", 0, addNode("a", NodeNonPR)), "nodes", []any{nodeDoc("b", NodeNonPR)}))
	empty := writeDoc(t, dir, "empty.json", revDoc("plan", "e", 0))
	for name, tc := range map[string]struct{ file, want string }{
		"a cycle":                    {cycle, "[cycle] plan: nodes a -> b -> a form a cycle"},
		"nodes where changes belong": {inline, "[unknown_field] $.nodes: unknown field \"nodes\": a revision carries its nodes and edges as changes"},
		"no change at all":           {empty, "[empty_changes] $.changes: a revision must carry at least one change"},
	} {
		first, code1 := crw(t, state, "dag-plan-put", "--request", tc.file)
		second, code2 := crw(t, state, "dag-plan-put", "--request", tc.file)
		if code1 != 2 || code2 != 2 || first != second {
			t.Fatalf("%s: exits %d %d\n%s\n%s", name, code1, code2, first, second)
		}
		out := parseOut(t, first)
		if out["error"] != "refused" || out["reason"] != "malformed_receipt" || !strings.Contains(out["detail"].(string), tc.want) {
			t.Fatalf("%s:\n%s", name, first)
		}
	}
	// unknown plan and revision: refused, never an empty plan
	good := writeDoc(t, dir, "good.json", forkJoin("plan", "r1"))
	if _, code := crw(t, state, "dag-plan-put", "--request", good); code != 0 {
		t.Fatal("the valid plan was refused")
	}
	for _, args := range [][]string{{"dag-plan-show", "--plan", "ghost"}, {"dag-plan-show", "--plan", "plan", "--revision", "99"}, {"dag-plan-log", "--plan", "ghost"}, {"dag-plan-log", "--plan", "plan", "--after", "9"}} {
		out, code := crw(t, state, args...)
		if m := parseOut(t, out); code != 2 || m["reason"] != "unregistered_scope" {
			t.Errorf("%v: exit %d\n%s", args, code, out)
		}
	}
	// a document that is not JSON is a usage error, the relay's exit 4
	if out, code := crw(t, state, "dag-plan-put", "--request", "not json"); code != 4 || !strings.Contains(out, "not valid JSON") {
		t.Errorf("not JSON: exit %d\n%s", code, out)
	}
	if out, code := crw(t, state, "dag-plan-put", "--request", "@"+filepath.Join(dir, "missing.json")); code != 4 {
		t.Errorf("a missing file: exit %d\n%s", code, out)
	}
}

// A plan that does not agree with itself is the host's failure (exit 3) on a plain read, with no --verify asked.
func TestATamperedPlanIsTheHostsFailureOnAPlainRead(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if _, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "r1.json", forkJoin("plan", "r1"))); code != 0 {
		t.Fatal("put")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{"DROP TRIGGER dag_plan_revisions_no_update", "UPDATE dag_plan_revisions SET state_digest = '" + strings.Repeat("0", 64) + "'"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out, code := crw(t, state, "dag-plan-show", "--plan", "plan")
	if m := parseOut(t, out); code != 3 || m["error"] != "host" || !strings.Contains(m["detail"].(string), "corrupt") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// listing is a directory's files with their sizes. Unless sidecars is set, SQLite's own -wal and -shm files are
// left out: the relay's selection checks, which run before any handler (dispatch.CheckSelection), read an existing
// WAL-mode store through an ordinary read-only connection, and that connection creates them whatever the command is.
func listing(t testing.TB, dir string, sidecars ...bool) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{"<absent>"}
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if len(sidecars) == 0 && (strings.HasSuffix(e.Name(), "-wal") || strings.HasSuffix(e.Name(), "-shm")) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, fmt.Sprintf("%s %d", e.Name(), info.Size()))
	}
	return names
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

// Nothing a rejected request asks for is left behind: no state directory, no store, no zone, no byte of an existing store.
func TestRefusedRequestsLeaveTheStateAsTheyFoundIt(t *testing.T) {
	cycle := func(request string, parent int) doc {
		return revDoc("plan", request, parent, addNode("a", NodeNonPR), addNode("b", NodeNonPR),
			addEdge("e1", "a", "b", EdgeArtifactVerified), addEdge("e2", "b", "a", EdgeArtifactVerified))
	}
	valid := func(request string, parent int) doc {
		return revDoc("plan", request, parent, addNode("fine", NodeNonPR))
	}
	cases := map[string]struct {
		doc  doc
		code int
		why  string
	}{
		"an invalid first revision":              {cycle("c", 0), 2, "malformed_receipt"},
		"a revision whose parent does not exist": {valid("p", 3), 2, "plan_revision_conflict"},
		"an invalid revision after a parent":     {cycle("c2", 1), 2, "plan_revision_conflict"},
	}
	t.Run("a state directory with no store", func(t *testing.T) {
		for name, tc := range cases {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "d.json", tc.doc))
			if m := parseOut(t, out); code != tc.code || m["reason"] != tc.why {
				t.Fatalf("%s: exit %d\n%s", name, code, out)
			}
			if got := listing(t, state); !reflect.DeepEqual(got, []string{"<absent>"}) {
				t.Fatalf("%s created %v", name, got)
			}
		}
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		if _, code := crw(t, state, "dag-plan-put", "--request", "{"); code != 4 || listing(t, state)[0] != "<absent>" {
			t.Fatalf("a document that is not JSON created %v", listing(t, state))
		}
	})
	t.Run("a store that predates the zone", func(t *testing.T) {
		for name, tc := range cases {
			dir := t.TempDir()
			state := filepath.Join(dir, "state")
			testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
			before, catalog := listing(t, state), catalogOf(t, filepath.Join(state, "relay.sqlite3"))
			out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "d.json", tc.doc))
			if m := parseOut(t, out); code != tc.code || m["reason"] != tc.why {
				t.Fatalf("%s: exit %d\n%s", name, code, out)
			}
			if got := listing(t, state); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s changed the directory:\n %v\n %v", name, before, got)
			}
			if got := catalogOf(t, filepath.Join(state, "relay.sqlite3")); !reflect.DeepEqual(got, catalog) {
				t.Fatalf("%s changed the schema of a store that had no zone", name)
			}
		}
		// the control: a valid first revision is what creates the zone and the plan
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
		if out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "ok.json", valid("v", 0))); code != 0 {
			t.Fatalf("the valid revision: exit %d\n%s", code, out)
		}
		if _, ok := catalogOf(t, filepath.Join(state, "relay.sqlite3"))["table dag_plans"]; !ok {
			t.Fatal("the valid revision did not create the zone")
		}
	})
	t.Run("a read leaves a store without the zone as it is", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
		before := catalogOf(t, filepath.Join(state, "relay.sqlite3"))
		out, code := crw(t, state, "dag-plan-show", "--plan", "plan")
		if m := parseOut(t, out); code != 2 || m["reason"] != "unregistered_scope" {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if !reflect.DeepEqual(catalogOf(t, filepath.Join(state, "relay.sqlite3")), before) {
			t.Fatal("a read created the zone")
		}
	})
	t.Run("an existing store with plans keeps every row", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		if _, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "r1.json", forkJoin("plan", "r1"))); code != 0 {
			t.Fatal("put")
		}
		db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		before := zoneRows(t, db)
		for name, d := range map[string]doc{
			"an invalid revision": revDoc("plan", "bad", 1, addEdge("loop", "ship", "design", EdgeArtifactVerified)),
			"a stale parent":      revDoc("plan", "stale", 0, addNode("late", NodeNonPR)),
			"a reused request id": revDoc("plan", "r1", 0, addNode("other", NodeNonPR)),
		} {
			if out, code := crw(t, state, "dag-plan-put", "--request", writeDoc(t, dir, "x.json", d)); code != 2 {
				t.Fatalf("%s: exit %d\n%s", name, code, out)
			}
			if got := zoneRows(t, db); !reflect.DeepEqual(got, before) {
				t.Fatalf("%s changed a row", name)
			}
		}
	})
}

// A store whose committed state cannot be read without writing it (WAL frames, no index) is the host's failure, found before
// the writing open, and nothing is created or recovered on the way.
func TestPreflightDoesNotWriteWhenTheStoreCannotBeReadInPlace(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src", "relay.sqlite3")
	testsupport.Create(t, src, "", "go")
	db, err := sql.Open("sqlite", "file:"+src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA wal_autocheckpoint=0", "INSERT INTO execution_limits VALUES ('lim-1','project','PRJ-A','runs','runs',6,1,'task-a','policy',1,'2026-10-01T00:00:00Z','2026-10-01T00:00:00Z')"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "relay.sqlite3")
	for _, suffix := range []string{"", "-wal"} {
		data, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst+suffix, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := listing(t, filepath.Dir(dst), true)
	err = Preflight(t.Context(), dst, decode(t, revDoc("plan", "r", 1, addNode("a", NodeNonPR))))
	if !errors.Is(err, store.ErrWALWithoutIndex) {
		t.Fatalf("the preflight of a store with a WAL and no index: %v", err)
	}
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		t.Fatalf("an unreadable store is not a refusal of the request: %v", err)
	}
	if got := listing(t, filepath.Dir(dst), true); !reflect.DeepEqual(got, before) {
		t.Fatalf("the preflight changed the directory:\n %v\n %v", before, got)
	}
}

// Writers in separate processes that expect one parent: exactly one of them wins and the others are told
// plan_revision_conflict; the log has one revision and one set of rows.
func TestRaceBetweenProcesses(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	const writers = 5
	var wg sync.WaitGroup
	outs, codes := make([]string, writers), make([]int, writers)
	files := make([]string, writers)
	for i := range files {
		files[i] = writeDoc(t, dir, fmt.Sprintf("w%d.json", i), forkJoin("plan", fmt.Sprintf("writer-%d", i)))
	}
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			outs[i], codes[i] = crw(t, state, "dag-plan-put", "--request", files[i])
		}(i)
	}
	close(start)
	wg.Wait()
	wins, conflicts := 0, 0
	for i := range outs {
		switch {
		case codes[i] == 0:
			wins++
		case codes[i] == 2 && parseOut(t, outs[i])["reason"] == "plan_revision_conflict":
			conflicts++
		default:
			t.Fatalf("writer %d: exit %d\n%s", i, codes[i], outs[i])
		}
	}
	if wins != 1 || conflicts != writers-1 {
		t.Fatalf("%d winners and %d conflicts", wins, conflicts)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for query, want := range map[string]int{"SELECT count(*) FROM dag_plan_revisions": 1, "SELECT count(*) FROM dag_plans": 1, "SELECT count(*) FROM dag_nodes": 5, "SELECT count(*) FROM dag_edges": 5} {
		var n int
		if err := db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatalf("%s = %d (%v), want %d", query, n, err, want)
		}
	}
}
