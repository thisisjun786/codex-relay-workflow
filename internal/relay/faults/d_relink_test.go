package faults

import (
	"bytes"
	"context"
	"fmt"
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
	seed := func(dir string) {
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
	trackerRef := func(dir string) string {
		var ref string
		readStore(t, context.Background(), filepath.Join(dir, "relay.sqlite3"), func(ctx context.Context, s *store.Store) error {
			r, e := s.One(ctx, "SELECT tracker_ref FROM fault_publications LIMIT 1")
			if e != nil {
				t.Fatalf("%s repoint: %v %v", dir, r, e)
			}
			ref = text(r, "tracker_ref")
			return nil
		})
		return ref
	}
	seed(goDir)
	// Python's answer: its reply and the tracker reference its store then holds.
	var want struct {
		Stdout     string `json:"stdout"`
		TrackerRef string `json:"trackerRef"`
	}
	pyValue(t, "relay fault-relink --limit 1", nil, pyRunPaths(t, home), &want, func() (any, error) {
		// Go seeded Python's store as well; Python runs on it after a takeover.
		seed(pyDir)
		testsupport.HandOver(t, filepath.Join(pyDir, "relay.sqlite3"), "python")
		root, e := filepath.Abs("../../..")
		if e != nil {
			return nil, e
		}
		cmd := exec.Command("uv", "run", "--no-sync", "codex-session-relay", "--state", pyDir, "--json", "fault-relink", "--limit", "1")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
		out, e := cmd.Output()
		if e != nil {
			return nil, fmt.Errorf("python: %v", e)
		}
		return map[string]any{"stdout": string(out), "trackerRef": trackerRef(pyDir)}, nil
	})
	var got, stderr bytes.Buffer
	code, handled := executeAsCLI(context.Background(), []string{"--state", goDir, "--json", "fault-relink", "--limit", "1"}, &got, &stderr)
	if !handled || code != 0 || want.Stdout != got.String() {
		t.Fatalf("python %s; go %d %s; stderr %s", want.Stdout, code, got.String(), stderr.String())
	}
	for dir, ref := range map[string]string{goDir: trackerRef(goDir), pyDir: want.TrackerRef} {
		if ref != "team-relay" {
			t.Fatalf("%s repoint: %v", dir, ref)
		}
	}
}
