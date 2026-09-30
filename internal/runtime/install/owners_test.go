package install_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

const policyText = `{"allowed": [{"model": "gpt-5", "efforts": ["high"]}]}`

func (h *host) policy(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(h.home, "policy.json")
	write(t, path, policyText)
	sum := sha256.Sum256([]byte(policyText))
	return path, hex.EncodeToString(sum[:])
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// register-mcp --owner plugin writes the version-2 record naming the policy by path and digest,
// answers unchanged on a rerun, refuses a policy that changed after it was registered, and
// refuses the retired user owner, a second owner in config.toml and a policy the bridge's own
// parser refuses - writing nothing in every refusal.
func TestRegisterMCPWritesTheRecordAndRefusesASecondOwner(t *testing.T) {
	h := newHost(t)
	policy, digest := h.policy(t)
	o := h.options()
	result, code := install.RegisterMCP(context.Background(), o, install.RegisterOptions{Owner: install.OwnerPlugin, ExecutionPolicy: policy})
	if code != install.OK || at(result, "outcome") != install.RecordCreated || at(result, "executionPolicy", "mode") != "allowlist" {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	path := filepath.Join(h.codex, install.BridgeRecordName)
	want := string(record.Encode(install.BridgeDocument(filepath.Join(h.dest, "current", "bin", "codex-thread-bridge"), nil, install.ServerName, "CRW-158",
		record.Object{{Key: "path", Value: policy}, {Key: "digest", Value: digest}})))
	if got := readFile(t, path); got != want {
		t.Fatalf("record:\n%s\nwant\n%s", got, want)
	}
	if again, code := install.RegisterMCP(context.Background(), o, install.RegisterOptions{Owner: install.OwnerPlugin, ExecutionPolicy: policy}); code != install.OK || at(again, "outcome") != install.RecordUnchanged {
		t.Fatalf("rerun: exit %d\n%s", code, golden.Canon(again))
	}
	write(t, policy, policyText+" ")
	if _, code := install.RegisterMCP(context.Background(), o, install.RegisterOptions{Owner: install.OwnerPlugin, ExecutionPolicy: policy}); code != install.Refused {
		t.Fatalf("a record naming a different digest is not overwritten: exit %d", code)
	}
	write(t, policy, policyText)
	os.Chmod(policy, 0o644)
	if got := readFile(t, path); got != want {
		t.Fatal("a refusal changed the record")
	}
	if _, code := install.RegisterMCP(context.Background(), o, install.RegisterOptions{Owner: install.OwnerUser}); code != install.Usage {
		t.Fatalf("the user owner is retired: exit %d", code)
	}

	other := newHost(t)
	write(t, filepath.Join(other.codex, "config.toml"), "[mcp_servers.bridge-by-hand]\ncommand = \"/opt/env/bin/codex-thread-bridge\"\nargs = []\n")
	refused, code := install.RegisterMCP(context.Background(), other.options(), install.RegisterOptions{Owner: install.OwnerPlugin})
	if code != install.Refused || at(refused, "outcome") != install.Conflict || !strings.Contains(text(at(refused, "detail")), "bridge-by-hand") {
		t.Fatalf("a second owner: exit %d\n%s", code, golden.Canon(refused))
	}
	write(t, filepath.Join(other.codex, "config.toml"), "[mcp_servers\nbroken")
	if refused, code := install.RegisterMCP(context.Background(), other.options(), install.RegisterOptions{Owner: install.OwnerPlugin}); code != install.Refused || !strings.Contains(text(at(refused, "detail")), "not readable TOML") {
		t.Fatalf("an unreadable config.toml: exit %d\n%s", code, golden.Canon(refused))
	}
	os.Remove(filepath.Join(other.codex, "config.toml"))
	bad := filepath.Join(other.home, "bad-policy.json")
	write(t, bad, `{"allowed": "everything"}`)
	if refused, code := install.RegisterMCP(context.Background(), other.options(), install.RegisterOptions{Owner: install.OwnerPlugin, ExecutionPolicy: bad}); code != install.Refused || at(refused, "outcome") != install.PolicyUnreadable {
		t.Fatalf("a policy the bridge refuses: exit %d\n%s", code, golden.Canon(refused))
	}
	if _, err := os.Lstat(filepath.Join(other.codex, install.BridgeRecordName)); !os.IsNotExist(err) {
		t.Fatal("a refusal wrote a record")
	}

	saved := record.LockTimeout
	record.LockTimeout = 100 * time.Millisecond
	defer func() { record.LockTimeout = saved }()
	write(t, filepath.Join(other.codex, install.OwnershipLockName+record.LockSuffix), "4242")
	if busy, code := install.RegisterMCP(context.Background(), other.options(), install.RegisterOptions{Owner: install.OwnerPlugin}); code != install.Refused || at(busy, "outcome") != install.Busy {
		t.Fatalf("a held ownership lock (the Python writers' O_EXCL file): exit %d\n%s", code, golden.Canon(busy))
	}
}

func (h *host) hookOptions() install.HookOptions {
	return install.HookOptions{Owner: install.OwnerPlugin, GuardTimeout: 5, Timeout: 10, Mode: "observe",
		MarkerRoot: filepath.Join(h.home, "markers"), JournalRoot: filepath.Join(h.home, "journal")}
}

// hook --owner plugin writes settings the Go hook accepts, naming the relay and the Go adapter
// through the pointer with /usr/bin/env as the interpreter; it refuses a user-owned registration
// in the hook file, the settings override, an over-long budget and the retired user owner.
func TestHookWritesTheGoSettingsAndRefusesASecondOwner(t *testing.T) {
	h := newHost(t)
	result, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	if code != install.OK || at(result, "settings", "outcome") != install.ConfigCreated {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
	path := filepath.Join(h.codex, install.SettingsName)
	document, err := install.HookSettings{Destination: h.dest, MarkerRoot: filepath.Join(h.home, "markers"), JournalRoot: filepath.Join(h.home, "journal"), Mode: "observe", Timeout: 5, Issue: "CRW-158", CodexHome: h.codex}.Document(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != string(record.Encode(document)) || !strings.Contains(got, `"adapterInterpreter": "/usr/bin/env"`) || !strings.Contains(got, filepath.Join(h.dest, "current", "bin", "crw-completion-hook")) {
		t.Fatalf("settings:\n%s", got)
	}
	if again, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.OK || at(again, "settings", "outcome") != install.ConfigUnchanged {
		t.Fatalf("rerun: exit %d\n%s", code, golden.Canon(again))
	}

	other := newHost(t)
	write(t, filepath.Join(other.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/usr/bin/python3 /repo/scripts/completion_hook.py /x/crw-completion-hook.json", "timeout": 10}]}]}}`)
	if refused, code := install.Hook(context.Background(), other.options(), other.hookOptions()); code != install.Refused || len(golden.List(at(refused, "registrations"))) != 1 {
		t.Fatalf("a user-owned registration: exit %d\n%s", code, golden.Canon(refused))
	}
	os.Remove(filepath.Join(other.codex, "hooks.json"))
	o := other.options()
	o.Env = append(o.Env, install.SettingsOverride+"=/elsewhere.json")
	if _, code := install.Hook(context.Background(), o, other.hookOptions()); code != install.Usage {
		t.Fatalf("the settings override: exit %d", code)
	}
	long := other.hookOptions()
	long.GuardTimeout = 8
	if _, code := install.Hook(context.Background(), other.options(), long); code != install.Usage {
		t.Fatalf("a guard budget over 7 s: exit %d", code)
	}
	user := other.hookOptions()
	user.Owner = install.OwnerUser
	if _, code := install.Hook(context.Background(), other.options(), user); code != install.Usage {
		t.Fatalf("the user owner is retired: exit %d", code)
	}
	if _, err := os.Lstat(filepath.Join(other.codex, install.SettingsName)); !os.IsNotExist(err) {
		t.Fatal("a refusal wrote settings")
	}
}

// goEraSettings writes the plugin-owned Stop settings `crw install hook` writes for this host and
// answers their bytes.
func (h *host) goEraSettings(t *testing.T) string {
	t.Helper()
	if result, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.OK {
		t.Fatalf("hook: exit %d\n%s", code, golden.Canon(result))
	}
	return readFile(t, filepath.Join(h.codex, install.SettingsName))
}

// hostPython is this host's python3, resolved, or "" when there is none.
var hostPython = sync.OnceValue(func() string {
	found, err := exec.LookPath("python3")
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(found)
	if err != nil {
		return ""
	}
	return resolved
})

// A promotion refuses a second owner on the reading it promotes on: a config.toml table that
// starts the bridge from a path the pointer does not name, and settings owned by the plugin
// beside a user hook-file registration. The candidate is released and nothing moved.
func TestInstallRefusesASecondOwner(t *testing.T) {
	for name, seed := range map[string]func(h *host){
		"config.toml": func(h *host) {
			write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.codex-thread-bridge]\ncommand = \"/opt/elsewhere/bin/codex-thread-bridge\"\n")
		},
		"hooks.json": func(h *host) {
			h.goEraSettings(t)
			write(t, filepath.Join(h.codex, "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "python3 /repo/scripts/completion_hook.py /x.json"}]}]}}`)
		},
	} {
		h := newHost(t)
		seed(h)
		first := archive(t, "0.9.0", "")
		result, code := install.Install(context.Background(), h.options(), "install", install.Source{From: first})
		if code != install.Refused || at(result, "failedStep") != "refuse a second owner" || at(result, "retriable") != true {
			t.Fatalf("%s: exit %d\n%s", name, code, golden.Canon(result))
		}
		if _, err := os.Lstat(filepath.Join(h.dest, "current")); !os.IsNotExist(err) {
			t.Fatalf("%s: the pointer was placed", name)
		}
		if _, err := os.Lstat(runtimeDir(h, "0.9.0", first, t)); !os.IsNotExist(err) {
			t.Fatalf("%s: the candidate was kept", name)
		}
	}
}
