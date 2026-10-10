package job

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// CRW-1134, post-evaluation round (f24d11cd): a dangling link is an entry that is there, and a store that exists keeps its modes.

// A link whose target is missing is there: reading through it fails as a missing file does, and that is not the entry's absence.
func TestADanglingLinkIsNotAMissingEntry(t *testing.T) {
	ws := workspace(t)
	hookDone(t, ws, "due", "S1")
	if err := os.Symlink(filepath.Join(ws, "nowhere"), DisabledPath(ws)); err != nil {
		t.Fatal(err)
	}
	if state := ReadDisabledState(ws); !state.Disabled || state.Err == nil {
		t.Errorf("a dangling off switch enables the wake: %+v", state)
	}
	if out := HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon); out != "" {
		t.Errorf("a Stop hook woke against a dangling off switch: %q", out)
	}
	if got := cliResult(t, ws, "on"); got.Out == "bg wake 이미 ON" {
		t.Errorf("on reports the wake already on: %+v", got)
	}
	if exists(DisabledPath(ws)) {
		t.Errorf("on left the dangling switch")
	}
	// A dangling record is a broken record, and the store is not reported empty.
	if err := os.Symlink(filepath.Join(ws, "nowhere"), RecordPath(ws, "bad")); err != nil {
		t.Fatal(err)
	}
	if broken := BrokenRecords(ws); len(broken) != 1 || broken[0].ID != "bad" {
		t.Errorf("broken records %+v", broken)
	}
	if got := cliResult(t, ws, "list", "--json"); got.Code != 1 {
		t.Errorf("list --json of a store with a dangling record: %+v", got)
	}
	if got := cliResult(t, ws, "get", "bad"); got.Code != 1 || !strings.Contains(got.Out.(string), "손상된 기록") {
		t.Errorf("get of a dangling record: %+v", got)
	}
}

// The directory and the ledger of a store that exists are not changed (nothing is chmodded): a job started in it appends its row,
// command included, to a ledger as readable as it was.
func TestAnExistingReadableLedgerStaysAsItWas(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	ws := workspace(t)
	mkdir(t, BGDir(ws))
	ledgerPath := filepath.Join(BGDir(ws), LedgerFile)
	put(t, ledgerPath, "")
	if err := os.Chmod(ledgerPath, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", "echo hidden-argument"}})
	settled(t, ws, rec.ID)
	if m := mode(t, ledgerPath); m != 0o644 {
		t.Errorf("the ledger of an existing store changed to %v", m)
	}
	if m := mode(t, RecordPath(ws, rec.ID)); m != 0o600 {
		t.Errorf("the new record is %v", m)
	}
}
