package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCSeededCLIOracle(t *testing.T) {
	goldenParent(t)
	home, err := os.MkdirTemp("/dev/shm", "fault-c-seed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	goDir := filepath.Join(home, "go")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	// Seeded at a fixed time: the answers echo the times the store holds.
	code, reply := seedCLI(t, 100000, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	id := reply["faultId"].(string)
	publication := publicationID(id, openRecord, triggerOpen)
	for _, args := range [][]string{{"fault-show"}, {"fault-show", "--fault", id}, {"fault-show", "--publication", publication}, {"fault-next"}, {"fault-retry", "--publication", publication}, {"fault-stage", "--fault", id, "--stage", "accepted", "--ref", "r"}, {"fault-queue", "--fault", id, "--kind", "append_comment", "--trigger", "extra"}} {
		t.Run(args[0]+"_"+args[len(args)-1], func(t *testing.T) {
			var got, stderr bytes.Buffer
			goCode := executeAsCLI(context.Background(), append([]string{"--state", goDir, "--json"}, args...), &got, &stderr)
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: goCode, Stdout: got.String()})
		})
	}
}
