package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// cooperatingWriter replaces the document at path with text once, the first time a write
// reaches the point between its decision and its lock - taking the lock the writers share, as
// a cooperating writer does - and reports whether it did.
func cooperatingWriter(t *testing.T, path, text string) (func(string), *bool) {
	t.Helper()
	wrote := false
	return func(locking string) {
		if locking != path || wrote {
			return
		}
		wrote = true
		lock, err := record.Lock(path, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Release()
		if err := record.AtomicWrite(path, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}, &wrote
}

// `crw install hook` decides on one look at the settings and writes only onto that document: a
// cooperating writer's document, saved between the decision and the lock, answers
// config_changed_underneath with its repair, and stays exactly as that writer saved it.
func TestHookNeverWritesOverADocumentThatChangedAfterItWasRead(t *testing.T) {
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	path := filepath.Join(h.codex, install.SettingsName)
	newer := h.goEraSettings(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	newer = strings.Replace(newer, `"installedBy": "CRW-158"`, `"installedBy": "CRW-200"`, 1)
	between, wrote := cooperatingWriter(t, path, newer)
	restore := install.ReplaceBeforeWriteLock(between)
	result, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	restore()
	if !*wrote || code != install.Refused || at(result, "settings", "outcome") != install.ConfigChangedUnderneath || at(result, "settings", "repair") == nil {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if readFile(t, path) != newer {
		t.Fatalf("the newer document was touched:\n%s", readFile(t, path))
	}
	if again, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.Refused || at(again, "settings", "outcome") != install.ConfigDiffers {
		t.Fatalf("the rerun decides on the file as it now stands: exit %d\n%s", code, golden.Canon(again))
	}
}

// The bridge record is written only onto the document its decision saw: a record another
// writer creates between the decision and the lock answers record_changed_underneath, and that
// record stays as it was written.
func TestRegisterMCPNeverWritesOverARecordThatChangedAfterItWasRead(t *testing.T) {
	h := newHost(t)
	path := filepath.Join(h.codex, install.BridgeRecordName)
	theirs := string(record.Encode(install.BridgeDocument("/opt/elsewhere/bin/codex-thread-bridge", nil, install.ServerName, "CRW-200", nil)))
	between, wrote := cooperatingWriter(t, path, theirs)
	restore := install.ReplaceBeforeWriteLock(between)
	result, code := install.RegisterMCP(context.Background(), h.options(), install.RegisterOptions{Owner: install.OwnerPlugin})
	restore()
	if !*wrote || code != install.Refused || at(result, "outcome") != install.RecordChangedUnderneath || at(result, "repair") == nil || readFile(t, path) != theirs {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
}

// On a platform with no procfs (darwin) `crw install remove` cannot establish that no relay or
// bridge runs out of a directory, so it keeps refusing, and says so plainly with the recovery by
// hand; the directory is left in place.
func TestRemoveWithoutAProcessTableSaysWhyAndHowToRecover(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	o := h.options()
	o.Proc = t.TempDir()
	result, code := install.Remove(context.Background(), o, old)
	if code != install.Refused || !strings.Contains(text(at(result, "refused")), "has no process table this command can read") {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	recovery := text(at(result, "recoveryRequires"))
	for _, want := range []string{"service stop", "delete " + old, "crw install status"} {
		if !strings.Contains(recovery, want) {
			t.Fatalf("the recovery lacks %q: %s", want, recovery)
		}
	}
	if _, err := os.Stat(old); err != nil {
		t.Fatal("a refused remove removed the directory")
	}
}
