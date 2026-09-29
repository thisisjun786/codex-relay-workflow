package install_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// installsOf is every environment the host record lists an install of the relay in.
func (h *host) installsOf(t *testing.T) []any {
	t.Helper()
	var out []any
	for _, raw := range golden.List(at(h.hostRecord(t), "components", "codex-session-relay", "installs")) {
		out = append(out, record.Get(golden.Obj(raw), "environment"))
	}
	return out
}

func listed(list []any, want any) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// A runtime that no process runs out of, that nothing selects and that the pointer does not
// name is still in use while a registration the host reads names a path inside it: Codex starts
// that path afresh in each new session, so nothing shows in the process table between sessions.
// Remove refuses it for every surface the host reads - the bridge record, the Stop settings'
// relayExecutable, adapterEntryPoint and adapterInterpreter (named inside the directory though
// the link resolves outside it), config.toml's mcp_servers commands and their arguments,
// hooks.json Stop commands and the settings document such a command names, and the launcher
// copy - and when a registration cannot be read. Each refusal names the registration and
// leaves the directory and its install entries in place; once nothing names it, it is removed.
func TestRemoveRefusesARuntimeARegistrationStillNames(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)
	venv := h.pythonVenv(t) // its bin/python3 is a link to the base interpreter, outside it
	write(t, filepath.Join(venv, "bin", "codex-thread-bridge"), "#!"+filepath.Join(venv, "bin", "python3")+"\n")
	if err := os.Chmod(filepath.Join(venv, "bin", "codex-thread-bridge"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(h.codex, install.SettingsName)
	bridgeRecord := filepath.Join(h.codex, install.BridgeRecordName)
	config := filepath.Join(h.codex, "config.toml")
	hooks := filepath.Join(h.codex, "hooks.json")
	elsewhere := filepath.Join(h.home, "other-settings.json")
	launcher := filepath.Join(h.codex, "crw-stop-hook.py")
	stopHooks := func(command string) string {
		return `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "` + command + `", "timeout": 10}]}]}}`
	}
	cases := []struct {
		label, directory, file, text, source, field string
		link                                        bool
	}{
		{"the plugin bridge record", old, bridgeRecord, `{"bridgeExecutable": "` + filepath.Join(old, "bin", "codex-thread-bridge") + `", "owner": "plugin", "recordVersion": 1, "serverName": "codex-thread-bridge"}`, bridgeRecord, "bridgeExecutable", false},
		{"the Stop settings' relay", old, settings, `{"configVersion": 1, "owner": "user", "relayExecutable": "` + filepath.Join(old, "bin", "codex-session-relay") + `"}`, settings, "relayExecutable", false},
		{"the Stop settings' adapter", old, settings, `{"adapterEntryPoint": "` + filepath.Join(old, "bin", "crw-completion-hook") + `", "adapterInterpreter": "/usr/bin/env", "configVersion": 1, "owner": "plugin"}`, settings, "adapterEntryPoint", false},
		{"a venv's interpreter named inside it", venv, settings, `{"adapterEntryPoint": "/nonexistent/completion_hook.py", "adapterInterpreter": "` + filepath.Join(venv, "bin", "python3") + `", "configVersion": 1, "owner": "plugin"}`, settings, "adapterInterpreter", false},
		{"a config.toml command", venv, config, "[mcp_servers.bridge]\ncommand = \"" + filepath.Join(venv, "bin", "codex-thread-bridge") + "\"\n", config, "mcp_servers.bridge", false},
		{"a config.toml argument", old, config, "[mcp_servers.bridge]\ncommand = \"/usr/bin/env\"\nargs = [\"" + filepath.Join(old, "bin", "codex-thread-bridge") + "\"]\n", config, "mcp_servers.bridge", false},
		{"a hooks.json Stop command", old, hooks, stopHooks(filepath.Join(old, "bin", "crw-completion-hook") + " " + settings), hooks, "hooks.Stop[0].hooks[0].command", false},
		{"the settings a Stop command names", old, elsewhere, `{"configVersion": 1, "owner": "user", "relayExecutable": "` + filepath.Join(old, "bin", "codex-session-relay") + `"}`, elsewhere, "relayExecutable", false},
		{"the launcher copy", old, launcher, filepath.Join(old, "LICENSE"), launcher, "file", true},
	}
	for _, c := range cases {
		if c.file == elsewhere {
			write(t, hooks, stopHooks("/usr/bin/env "+filepath.Join(h.dest, "current", "bin", "crw-completion-hook")+" "+elsewhere))
		}
		if c.link {
			if err := os.Symlink(c.text, c.file); err != nil {
				t.Fatal(err)
			}
		} else {
			write(t, c.file, c.text)
		}
		refused, code := install.Remove(context.Background(), h.options(), c.directory)
		if code != install.Refused || at(refused, "applied") != false || !strings.Contains(text(at(refused, "refused")), "names a path inside this directory") {
			t.Fatalf("%s: exit %d\n%s", c.label, code, golden.Canon(refused))
		}
		found := false
		for _, raw := range golden.List(at(refused, "registrations")) {
			one := golden.Obj(raw)
			found = found || record.Get(one, "source") == c.source && strings.HasPrefix(text(record.Get(one, "field")), c.field)
		}
		if !found {
			t.Fatalf("%s: the refusal does not name %s %s\n%s", c.label, c.source, c.field, golden.Canon(at(refused, "registrations")))
		}
		if _, err := os.Stat(c.directory); err != nil || !listed(h.installsOf(t), c.directory) {
			t.Fatalf("%s: a refused remove removed the directory or its install entries", c.label)
		}
		for _, path := range []string{c.file, hooks} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
	}

	write(t, config, "[mcp_servers.bridge\ncommand = \"x\"\n")
	if refused, code := install.Remove(context.Background(), h.options(), old); code != install.Refused || len(golden.List(at(refused, "unreadable"))) == 0 {
		t.Fatalf("an unreadable config.toml: exit %d\n%s", code, golden.Canon(refused))
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}

	// Registrations reaching the selected runtime through the pointer name neither directory.
	write(t, config, "[mcp_servers.codex-thread-bridge]\ncommand = \""+filepath.Join(h.dest, "current", "bin", "codex-thread-bridge")+"\"\n")
	for _, directory := range []string{old, venv} {
		if removed, code := install.Remove(context.Background(), h.options(), directory); code != install.OK || at(removed, "removed") != true {
			t.Fatalf("once nothing names %s: exit %d\n%s", directory, code, golden.Canon(removed))
		}
		if _, err := os.Lstat(directory); !os.IsNotExist(err) || listed(h.installsOf(t), directory) {
			t.Fatalf("%s or its install entries remain", directory)
		}
	}
}

// The directory's install entries are dropped under the host record's lock before the
// directory is removed, as runtime_install.py's release_candidate drops a candidate's before
// removing it: a drop that cannot be written (another writer holds the record's lock) refuses
// with the directory and its entries in place, and never reports entries it did not drop; a
// removal that does not finish after the drop is exit 3, naming what is left and its repair.
func TestRemoveDropsTheInstallEntriesBeforeTheDirectory(t *testing.T) {
	h := newHost(t)
	first, second := archive(t, "0.9.0", ""), archive(t, "0.9.1", "")
	old := runtimeDir(h, "0.9.0", first, t)
	h.mustInstall(t, "install", first)
	h.mustInstall(t, "update", second)

	saved := record.LockTimeout
	record.LockTimeout = 300 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	write(t, h.record+record.LockSuffix, "4242")
	refused, code := install.Remove(context.Background(), h.options(), old)
	if err := os.Remove(h.record + record.LockSuffix); err != nil {
		t.Fatal(err)
	}
	if code != install.Refused || at(refused, "applied") != false || at(refused, "droppedInstallEntries") != nil || !strings.Contains(text(at(refused, "refused")), "lock could not be taken") {
		t.Fatalf("a held record lock: exit %d\n%s", code, golden.Canon(refused))
	}
	if _, err := os.Stat(filepath.Join(old, "bin", "crw")); err != nil || !listed(h.installsOf(t), old) {
		t.Fatal("a remove whose drop could not be written removed the directory or its install entries")
	}

	if os.Geteuid() == 0 {
		t.Skip("root removes a read-only directory's entries, so the removal cannot be made to fail")
	}
	// The directory is set aside as its tombstone before anything under it is deleted, so a
	// deletion that stops part-way leaves the tombstone - named for what it is - and nothing
	// under the runtime's own name; status lists it and remove finishes it.
	grave := filepath.Join(h.dest, ".crw-removing-"+filepath.Base(old))
	bin := filepath.Join(old, "bin")
	if err := os.Chmod(bin, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(filepath.Join(grave, "bin"), 0o755) }()
	partial, code := install.Remove(context.Background(), h.options(), old)
	if code != install.Incomplete || at(partial, "removed") != false || !listed(golden.List(at(partial, "droppedInstallEntries")), old) || !listed(golden.List(at(partial, "residualPaths")), grave) || !strings.Contains(text(at(partial, "recoveryRequires")), "crw install remove "+grave) {
		t.Fatalf("a removal that does not finish: exit %d\n%s", code, golden.Canon(partial))
	}
	if listed(h.installsOf(t), old) {
		t.Fatal("exit 3 says the entries were dropped, and the record still lists them")
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatal("something is left under the runtime's own name")
	}
	status, _ := install.Status(context.Background(), h.options())
	if interrupted := golden.List(at(status, "interruptedRemovals")); len(interrupted) != 1 || at(golden.Obj(interrupted[0]), "path") != grave {
		t.Fatalf("status: %s", golden.Canon(at(status, "interruptedRemovals")))
	}
	if err := os.Chmod(filepath.Join(grave, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	finished, code := install.Remove(context.Background(), h.options(), old)
	if code != install.OK || at(finished, "finished") != grave {
		t.Fatalf("finishing the removal: exit %d\n%s", code, golden.Canon(finished))
	}
	if _, err := os.Lstat(grave); !os.IsNotExist(err) {
		t.Fatal("the tombstone is still there")
	}
}
