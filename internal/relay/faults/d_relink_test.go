package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestDRelinkRepointsBoundedWritesAgainstPython(t *testing.T) {
	home, e := os.MkdirTemp("/dev/shm", "fault-d-relink-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	goDir := filepath.Join(home, "go")
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
	// The reply and the tracker reference the store then holds.
	var got, stderr bytes.Buffer
	code := executeAsCLI(context.Background(), []string{"--state", goDir, "--json", "fault-relink", "--limit", "1"}, &got, &stderr)
	if code != 0 {
		t.Fatalf("go %d %s; stderr %s", code, got.String(), stderr.String())
	}
	ref := trackerRef(goDir)
	checkGolden(t, "relay fault-relink --limit 1", nil, runPathsOf(t, home), map[string]any{"code": code, "stdout": got.String(), "trackerRef": ref})
	if ref != "team-relay" {
		t.Fatalf("repoint: %v", ref)
	}
}
