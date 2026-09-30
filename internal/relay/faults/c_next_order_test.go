package faults

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestFaultNextOfferablePublicationOrderMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	home := t.TempDir()
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	// Seeded at fixed times: Python is given a copy of this store and its answer echoes the
	// times it holds.
	if code, reply := seedCLI(t, 100000, goDir, "fault-target", "--product", "crw", "--project", "CRW", "--team", "relay", "--project-ref", "CRW"); code != 0 {
		t.Fatal(reply)
	}
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	code, reply := seedCLI(t, 100001, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	pythonCopy(t, goDir, pyDir)
	args := []string{"--state", pyDir, "--json", "fault-next"}
	want, _ := pythonFaultCLI(t, root, home, args...)
	var stdout, stderr bytes.Buffer
	gotCode, handled := executeAsCLI(context.Background(), []string{"--state", goDir, "--json", "fault-next"}, &stdout, &stderr)
	checkGolden(t, "relay --json fault-next", []string{"--json", "fault-next"}, runPathsOf(t, home), cliGolden{Code: gotCode, Stdout: stdout.String(), Stderr: stderr.String()})
	if !handled || gotCode != want.code || stdout.String() != want.stdout || stderr.String() != want.stderr {
		t.Fatalf("Python: %#v\nGo: code=%d stdout=%q stderr=%q", want, gotCode, stdout.String(), stderr.String())
	}
}
