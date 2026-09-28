package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func ledgerOracle(t *testing.T, spec map[string]any, got any) {
	t.Helper()
	repo, _ := filepath.Abs("../../..")
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/ledger_capture.py"))
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("oracle %v %s", err, out)
	}
	var want any
	if err := json.Unmarshal(out, &want); err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("Go %s Python %s", actual, expected)
	}
}
func Test28_MAL_1_LedgerPinnedAndEnvironmentSelection(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "app.sock")
	state, other := filepath.Join(root, "explicit-state"), filepath.Join(root, "env-state")
	for _, dir := range []string{state, other} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", other)
	selection, err := store.ResolveStateDir("", socket)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		pin bool
		dir string
	}{{false, selection.Path}, {true, state}} {
		a, err := Open(socket, test.dir, Options{RPC: &scriptRPC{}})
		if err != nil {
			t.Fatal(err)
		}
		first, err := a.LedgerIdentityRecord(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		ledgerOracle(t, map[string]any{"socket": socket, "directory": test.dir, "pin": test.pin}, first)
		t.Setenv("CODEX_SESSION_RELAY_STATE", filepath.Join(root, "retry-state"))
		a, err = Open(socket, test.dir, Options{RPC: &scriptRPC{}})
		if err != nil {
			t.Fatal(err)
		}
		if err := a.RequireLedger(context.Background(), first); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "retry-state")); !os.IsNotExist(err) {
			t.Fatalf("retry directory created: %v", err)
		}
		t.Setenv("CODEX_SESSION_RELAY_STATE", other)
	}
}
func Test28_MAL_2_ReplacedLedgerRefuses(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "app.sock")
	for _, replace := range []bool{false, true} {
		dir := filepath.Join(root, map[bool]string{false: "same", true: "replaced"}[replace])
		a, err := Open(socket, dir, Options{RPC: &scriptRPC{}})
		if err != nil {
			t.Fatal(err)
		}
		first, err := a.LedgerIdentityRecord(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if replace {
			path := first["realPath"].(string)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		var got any = first
		if err := a.RequireLedger(context.Background(), first); err != nil {
			got = map[string]any{"error": err.Error()}
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
		ledgerOracle(t, map[string]any{"socket": socket, "directory": dir, "pin": true, "replace": replace}, got)
	}
}
