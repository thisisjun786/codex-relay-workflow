package faults

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestDRelinkOutstandingWriteSelectionAgainstPython(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-link-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	// Seeded at fixed times: Python is given a copy of this store and what it writes and
	// answers echoes the times it holds.
	_, reply := seedCLI(t, 100000, goDir, "fault-observe", "--observation", observation)
	id := reply["faultId"].(string)
	_, reply = seedCLI(t, 100001, goDir, "fault-target", "--product", "crw", "--project", "CRW", "--team", "team-relay", "--project-ref", "P2")
	if reply["error"] != nil {
		t.Fatal(reply)
	}
	s, e := store.Open(context.Background(), filepath.Join(goDir, "relay.sqlite3"), "")
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	statements := []struct {
		sql  string
		args []any
	}{
		{"UPDATE fault_ledger SET external_ref='ISSUE-1' WHERE fault_id=?", []any{id}},
		{"INSERT INTO fault_links(fault_id,external_ref,project_ref,observed_project_ref,state,revision,updated_at) VALUES(?,'ISSUE-1','P2','P1','unlinked',1,'stamp')", []any{id}},
		{"INSERT INTO fault_publications(publication_id,fault_id,kind,trigger_key,cycle,summary,identity_digest,state,created_at,updated_at) VALUES('other-write',?,'update_record','update:set_project:P1:r1',1,'summary','digest','issued','stamp','stamp')", []any{id}},
		{"INSERT INTO fault_publication_payloads(publication_id,payload,updated_at,target_mode) VALUES('other-write',?, 'stamp','none')", []any{`{"op": "set_project", "value": "P1"}`}},
	}
	for _, statement := range statements {
		if _, e = s.Q(ctx).ExecContext(ctx, statement.sql, statement.args...); e != nil {
			t.Fatal(e)
		}
	}
	s.Close()
	pythonCopy(t, goDir, pyDir)
	py := func() map[string]any {
		t.Helper()
		out := pyAnswer(t, "relay fault-relink --limit 1", nil, pyRunPaths(t, home), func() ([]byte, error) {
			cmd := exec.Command("uv", "run", "--no-sync", "codex-session-relay", "--state", pyDir, "--json", "fault-relink", "--limit", "1")
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
			out, e := cmd.Output()
			if e != nil {
				return nil, fmt.Errorf("Python %v: %s", e, out)
			}
			return out, nil
		})
		var reply map[string]any
		if e := json.Unmarshal(out, &reply); e != nil {
			t.Fatal(e)
		}
		return reply
	}
	gotCode, got := cliCall(t, goDir, "fault-relink", "--limit", "1")
	checkGolden(t, "relay fault-relink --limit 1", nil, runPathsOf(t, home), map[string]any{"code": gotCode, "reply": got})
	want := py()
	if gotCode != 0 || !reflect.DeepEqual(got, want) || got["relinked"] != float64(0) {
		t.Fatalf("outstanding: Go %d %v Python %v", gotCode, got, want)
	}
	settle := "UPDATE fault_publications SET state='confirmed' WHERE publication_id='other-write'"
	s, e = store.Open(ctx, filepath.Join(goDir, "relay.sqlite3"), "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Q(ctx).ExecContext(ctx, settle); e != nil {
		t.Fatal(e)
	}
	s.Close()
	seedPython(t, filepath.Join(pyDir, "relay.sqlite3"), seedSQL(settle)...)
	gotCode, got = cliCall(t, goDir, "fault-relink", "--limit", "1")
	checkGolden(t, "relay fault-relink --limit 1", nil, runPathsOf(t, home), map[string]any{"code": gotCode, "reply": got})
	want = py()
	if gotCode != 0 || !reflect.DeepEqual(got, want) || got["relinked"] != float64(1) {
		t.Fatalf("settled: Go %d %v Python %v", gotCode, got, want)
	}
	// relinked reads the link and the relink write of the store in dir.
	type relinked struct {
		State      string `json:"state"`
		ProjectRef string `json:"projectRef"`
		Revision   int64  `json:"revision"`
		Found      bool   `json:"found"`
		WriteState string `json:"writeState"`
		Summary    string `json:"summary"`
		Payload    string `json:"payload"`
	}
	read := func(dir string) relinked {
		var out relinked
		readStore(t, ctx, filepath.Join(dir, "relay.sqlite3"), func(ctx context.Context, s *store.Store) error {
			r, e := s.One(ctx, "SELECT state,project_ref,revision FROM fault_links WHERE fault_id=?", id)
			if e != nil {
				t.Fatalf("%s link %v %v", dir, r, e)
			}
			out.State, out.ProjectRef, out.Revision = text(r, "state"), text(r, "project_ref"), integer(r, "revision")
			p, e := s.One(ctx, "SELECT p.state,p.summary,pp.payload FROM fault_publications p JOIN fault_publication_payloads pp ON pp.publication_id=p.publication_id WHERE p.fault_id=? AND p.trigger_key='update:set_project:P2:r2'", id)
			if e != nil {
				t.Fatalf("%s write %v %v", dir, p, e)
			}
			out.Found = p != nil
			out.WriteState, out.Summary, out.Payload = text(p, "state"), text(p, "summary"), text(p, "payload")
			return nil
		})
		return out
	}
	goWrite := read(goDir)
	checkGolden(t, "store after relink", nil, runPathsOf(t, home), goWrite)
	// What the store Python wrote holds after its relinks.
	var pyWrite relinked
	pyValue(t, "python store after relink", nil, pyRunPaths(t, home), &pyWrite, func() (any, error) { return read(pyDir), nil })
	for dir, w := range map[string]relinked{goDir: goWrite, pyDir: pyWrite} {
		if w.State != "unlinked" || w.ProjectRef != "P2" || w.Revision != 2 {
			t.Fatalf("%s link %+v", dir, w)
		}
		if !w.Found || w.WriteState != "pending" {
			t.Fatalf("%s write %+v", dir, w)
		}
	}
	if pyWrite.Summary != goWrite.Summary || pyWrite.Payload != goWrite.Payload {
		t.Fatalf("relink write mismatch Go %+v Python %+v", goWrite, pyWrite)
	}
}
