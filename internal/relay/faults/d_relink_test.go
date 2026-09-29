package faults

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestDRelinkRepointsBoundedWritesAgainstPython(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-relink-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	goDir := filepath.Join(home, "go")
	pyDir := filepath.Join(home, "python")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"rel-1","turn":"turn-7"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	for _, dir := range []string{goDir, pyDir} {
		for _, args := range [][]string{{"fault-observe", "--observation", observation}, {"fault-target", "--product", "crw", "--project", "CRW", "--team", "team-relay", "--project-ref", "P1"}} {
			code, r := cliCall(t, dir, args...)
			if code != 0 {
				t.Fatalf("seed: %d %v", code, r)
			}
		}
		s, e := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.Q(context.Background()).ExecContext(context.Background(), "UPDATE fault_publications SET tracker_ref='stale'"); e != nil {
			t.Fatal(e)
		}
		if e = s.Close(); e != nil {
			t.Fatal(e)
		}
	}
	// Go seeded Python's store as well; Python runs on it after a takeover.
	testsupport.HandOver(t, filepath.Join(pyDir, "relay.sqlite3"), "python")
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "codex-session-relay", "--state", pyDir, "--json", "fault-relink", "--limit", "1")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
	want, e := cmd.Output()
	if e != nil {
		t.Fatalf("python: %v", e)
	}
	var got, stderr bytes.Buffer
	code, handled := executeAsCLI(context.Background(), []string{"--state", goDir, "--json", "fault-relink", "--limit", "1"}, &got, &stderr)
	if !handled || code != 0 || !bytes.Equal(want, got.Bytes()) {
		t.Fatalf("python %s; go %d %s; stderr %s", want, code, got.String(), stderr.String())
	}
	for _, dir := range []string{goDir, pyDir} {
		readStore(t, context.Background(), filepath.Join(dir, "relay.sqlite3"), func(ctx context.Context, s *store.Store) error {
			r, e := s.One(ctx, "SELECT tracker_ref FROM fault_publications LIMIT 1")
			if e != nil || text(r, "tracker_ref") != "team-relay" {
				t.Fatalf("%s repoint: %v %v", dir, r, e)
			}
			return nil
		})
	}
}
