package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCSeededCLIOracle(t *testing.T) {
	home, err := os.MkdirTemp("/dev/shm", "fault-c-seed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	// Seeded at a fixed time: Python is given a copy of this store and its answers echo the
	// times it holds.
	code, reply := seedCLI(t, 100000, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	id := reply["faultId"].(string)
	publication := publicationID(id, openRecord, triggerOpen)
	pythonCopy(t, goDir, pyDir)
	for _, args := range [][]string{{"fault-show"}, {"fault-show", "--fault", id}, {"fault-show", "--publication", publication}, {"fault-next"}, {"fault-retry", "--publication", publication}, {"fault-stage", "--fault", id, "--stage", "accepted", "--ref", "r"}, {"fault-queue", "--fault", id, "--kind", "append_comment", "--trigger", "extra"}} {
		t.Run(args[0]+"_"+args[len(args)-1], func(t *testing.T) {
			answer := pyCLIRun(t, home, "", append([]string{"--state", pyDir, "--json"}, args...), false, pyHomeEnv(home)...)
			want, pyCode := []byte(answer.Stdout), answer.Code
			var got, stderr bytes.Buffer
			goCode, handled := executeAsCLI(context.Background(), append([]string{"--state", goDir, "--json"}, args...), &got, &stderr)
			if !handled {
				t.Fatal("unhandled")
			}
			if pyCode != goCode || !bytes.Equal(want, got.Bytes()) {
				t.Errorf("python (%d): %s\ngo (%d): %s\nstderr: %s", pyCode, want, goCode, got.String(), stderr.String())
			}
		})
	}
}
