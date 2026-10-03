package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// expectLedger checks got with the golden. ledger is the ledger file the
// adapter opened: its name is a digest of the socket's path, which lies in the test's temporary
// directory, and its device and inode are the file's, so the golden names them.
func expectLedger(t *testing.T, got any, ledger string) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := decodeNumbers(raw, &record); err != nil {
		t.Fatal(err)
	}
	if path, ok := record["realPath"].(string); ok {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		for key, value := range map[string]uint64{"device": uint64(stat.Dev), "inode": stat.Ino} {
			if record[key] != json.Number(strconv.FormatUint(value, 10)) {
				t.Fatalf("the %s of %s is %d, not the answer's %v", key, path, value, record[key])
			}
			record[key] = "<" + key + ">"
		}
	}
	expectJSON(t, "ledger", record, golden.Substitute(filepath.Base(ledger), "<ledger-file>"))
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
		expectLedger(t, first, first["realPath"].(string))
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
	t.Parallel()
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
		expectLedger(t, got, first["realPath"].(string))
	}
}
