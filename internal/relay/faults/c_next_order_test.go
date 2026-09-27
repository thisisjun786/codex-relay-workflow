package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFaultNextOfferablePublicationOrderMatchesPython(t *testing.T) {
	root, _ := filepath.Abs("../../..")
	home := t.TempDir()
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	if code, reply := cliCall(t, goDir, "fault-target", "--product", "crw", "--project", "CRW", "--team", "relay", "--project-ref", "CRW"); code != 0 {
		t.Fatal(reply)
	}
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	code, reply := cliCall(t, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	if err := os.MkdirAll(pyDir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(goDir, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pyDir, "relay.sqlite3"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--state", pyDir, "--json", "fault-next"}
	want := pythonFaultCLI(t, root, home, args...)
	var stdout, stderr bytes.Buffer
	gotCode, handled := ExecuteAs(context.Background(), "codex-session-relay", []string{"--state", goDir, "--json", "fault-next"}, &stdout, &stderr, nil)
	if !handled || gotCode != want.code || stdout.String() != want.stdout || stderr.String() != want.stderr {
		t.Fatalf("Python: %#v\nGo: code=%d stdout=%q stderr=%q", want, gotCode, stdout.String(), stderr.String())
	}
}
