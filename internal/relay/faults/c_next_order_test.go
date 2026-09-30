package faults

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
)

func TestFaultNextOfferablePublicationOrderMatchesPython(t *testing.T) {
	home := t.TempDir()
	goDir := filepath.Join(home, "go")
	// Seeded at fixed times: the answer echoes the times the store holds.
	if code, reply := seedCLI(t, 100000, goDir, "fault-target", "--product", "crw", "--project", "CRW", "--team", "relay", "--project-ref", "CRW"); code != 0 {
		t.Fatal(reply)
	}
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	code, reply := seedCLI(t, 100001, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	var stdout, stderr bytes.Buffer
	gotCode, handled := executeAsCLI(context.Background(), []string{"--state", goDir, "--json", "fault-next"}, &stdout, &stderr)
	if !handled {
		t.Fatal("unhandled")
	}
	checkGolden(t, "relay --json fault-next", []string{"--json", "fault-next"}, runPathsOf(t, home), cliGolden{Code: gotCode, Stdout: stdout.String(), Stderr: stderr.String()})
}
