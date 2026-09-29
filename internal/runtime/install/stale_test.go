package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
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

// A cooperating writer that replaces the Python-era settings after the install read them and
// before it takes the settings lock never has its document overwritten by a Go variant built
// from the bytes read earlier: the look under the lock sees another document, nothing is
// written, and the transition is decided again from the fresh reading - so the Go variant
// written is the newer document's, and the newer document is what is archived. Checked for a
// change a replacement would move anyway (installedBy, the adapter) and for one it keeps (the
// mode, the isolation, the roots, the budget).
func TestATransitionNeverWritesAGoVariantBuiltFromStaleBytes(t *testing.T) {
	for name, change := range map[string]func(h *host, text string) string{
		"installedBy and the adapter": func(h *host, text string) string {
			text = strings.Replace(text, `"installedBy": "CRW-116"`, `"installedBy": "CRW-200"`, 1)
			return strings.Replace(text, "/home/user/code/codex-relay-workflow/scripts/completion_hook.py", "/home/user/other/scripts/completion_hook.py", 1)
		},
		"the mode, the roots and the budget": func(h *host, text string) string {
			text = strings.Replace(text, `"isolationAssertedBy": null`, `"isolationAssertedBy": "CRW-999"`, 1)
			text = strings.Replace(text, `"mode": "observe"`, `"mode": "hold"`, 1)
			text = strings.Replace(text, `"timeoutSeconds": 5`, `"timeoutSeconds": 4`, 1)
			return strings.Replace(text, filepath.Join(h.home, "markers"), filepath.Join(h.home, "other-markers"), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			_, original := h.pythonEraHost(t)
			path := filepath.Join(h.codex, install.SettingsName)
			newer := change(h, original)
			if newer == original {
				t.Fatal("the change changed nothing")
			}
			between, wrote := cooperatingWriter(t, path, newer)
			restore := install.ReplaceBeforeWriteLock(between)
			result := h.mustInstall(t, "update", archive(t, "0.9.0", ""))
			restore()
			if !*wrote || at(result, "settings", "action") != "replaced" || at(result, "settings", "rebuiltFromFreshReading") != int64(1) {
				t.Fatalf("settings: %s", golden.Canon(at(result, "settings")))
			}
			decoded, err := reading.Decode([]byte(newer))
			if err != nil {
				t.Fatal(err)
			}
			want := string(record.Encode(install.GoVariant(decoded.(record.Object), pointer.Path(h.dest))))
			if got := readFile(t, path); got != want {
				t.Fatalf("the settings are not the Go variant of the newer document:\n%s\nwant\n%s", got, want)
			}
			if archives := must(filepath.Glob(path + ".superseded-*")); len(archives) != 1 || readFile(t, archives[0]) != newer {
				t.Fatalf("the newer document is not what was archived: %v", archives)
			}
		})
	}
}

// `crw install hook` decides on one look at the settings and writes only onto that document: a
// cooperating writer's newer Python-era document, saved between the decision and the lock,
// answers config_changed_underneath with its repair, and stays exactly as that writer saved it.
func TestHookNeverWritesOverADocumentThatChangedAfterItWasRead(t *testing.T) {
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	original := h.pythonEraSettings(t)
	path := filepath.Join(h.codex, install.SettingsName)
	newer := strings.Replace(original, `"installedBy": "CRW-116"`, `"installedBy": "CRW-200"`, 1)
	between, wrote := cooperatingWriter(t, path, newer)
	restore := install.ReplaceBeforeWriteLock(between)
	result, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	restore()
	if !*wrote || code != install.Refused || at(result, "settings", "outcome") != install.ConfigChangedUnderneath || at(result, "settings", "repair") == nil {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	if readFile(t, path) != newer || len(must(filepath.Glob(path+".superseded-*"))) != 0 {
		t.Fatalf("the newer document was touched:\n%s", readFile(t, path))
	}
	if again, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.OK || at(again, "settings", "outcome") != install.ConfigReplaced {
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
