package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// ledgerOracle compares got with Python's answer to spec. ledger is the ledger file the Go
// adapter opened: its name is a digest of the socket's path, which lies in the test's temporary
// directory (asGoAnswers).
func ledgerOracle(t *testing.T, spec map[string]any, got any, ledger string) {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	repo := pyRepo(t)
	out := pyoracle.Answer(t, pyKey(t, "ledger_capture.py"), func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(repo, "internal/relay/adapter/testdata/ledger_capture.py"))
		cmd.Dir = repo
		cmd.Stdin = bytes.NewReader(raw)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("%v\n%s", err, out)
		}
		return ledgerFileIdentity(out, false)
	}, pyOptions(t, asGoAnswers(filepath.Base(ledger), "<ledger-file>"))...)
	out, err = ledgerFileIdentity(out, true)
	if err != nil {
		t.Fatal(err)
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

// ledgerFileIdentity stands for the device and inode of a ledger identity record: they are the
// file's at realPath (the ledger the test's Go adapter created, which Python opened), so the
// recording names them and a replay reads them from the file again. place writes the numbers
// back; otherwise they become placeholders, and only when they are that file's.
func ledgerFileIdentity(raw []byte, place bool) ([]byte, error) {
	var record map[string]any
	if err := decodeNumbers(raw, &record); err != nil {
		return nil, fmt.Errorf("%w: %s", err, raw)
	}
	path, ok := record["realPath"].(string)
	if !ok {
		return raw, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	for key, value := range map[string]uint64{"device": uint64(stat.Dev), "inode": stat.Ino} {
		placeholder := "<" + key + ">"
		if place {
			if record[key] == placeholder {
				record[key] = value
			}
			continue
		}
		if record[key] != json.Number(strconv.FormatUint(value, 10)) {
			return nil, fmt.Errorf("the %s of %s is %d, not the answer's %v", key, path, value, record[key])
		}
		record[key] = placeholder
	}
	return encodeJSON(record)
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
		ledgerOracle(t, map[string]any{"socket": socket, "directory": test.dir, "pin": test.pin}, first, first["realPath"].(string))
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
		ledgerOracle(t, map[string]any{"socket": socket, "directory": dir, "pin": true, "replace": replace}, got, first["realPath"].(string))
	}
}
