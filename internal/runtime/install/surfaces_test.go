package install_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// The execution policy path is recorded as Python records it, Path(value).expanduser()
// .absolute(): '..' kept, so the record names, and its digest hashes, the file the kernel opens
// for that spelling - here through a symbolic link - which is the file the bridge enforces.
func TestTheExecutionPolicyPathIsSpelledAsPythonRecordsIt(t *testing.T) {
	h := newHost(t)
	named := `{"allowed": [{"model": "gpt-5", "efforts": ["high"]}]}`
	other := `{"allowed": [{"model": "gpt-4", "efforts": ["low"]}]}`
	write(t, filepath.Join(h.home, "real", "policy.json"), named)
	write(t, filepath.Join(h.home, "policy.json"), other)
	if err := os.MkdirAll(filepath.Join(h.home, "real", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.home, "real", "sub"), filepath.Join(h.home, "link")); err != nil {
		t.Fatal(err)
	}
	spelled := h.home + "/link/./../policy.json"
	result, code := install.RegisterMCP(context.Background(), h.options(), install.RegisterOptions{Owner: install.OwnerPlugin, ExecutionPolicy: spelled})
	sum := sha256.Sum256([]byte(named))
	if code != install.OK || at(result, "executionPolicy", "path") != h.home+"/link/../policy.json" || at(result, "executionPolicy", "digest") != hex.EncodeToString(sum[:]) {
		t.Fatalf("exit %d\n%s", code, golden.Canon(result))
	}
}

// A user-owned Stop registration, or a Codex configuration table, that runs something through the
// owned pointer a Go runtime does not provide - a bin/python3 - is the second owner beside the
// plugin's declaration and would run nothing after the swap: an update refuses it and nothing
// moves, and so does a rollback to the runtime the update replaced.
func TestAUserRegistrationThroughThePointerIsASecondOwner(t *testing.T) {
	userSettings := func(h *host) {
		write(t, filepath.Join(h.codex, install.SettingsName), `{
  "configVersion": 1,
  "event": "Stop",
  "journalRoot": "`+filepath.Join(h.home, "journal")+`",
  "markerRoot": "`+filepath.Join(h.home, "markers")+`",
  "mode": "observe",
  "relayExecutable": "`+filepath.Join(h.dest, "current", "bin", "codex-session-relay")+`",
  "timeoutSeconds": 5
}
`)
	}
	registration := func(h *host) string {
		return `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "` + filepath.Join(h.dest, "current", "bin", "python3") + ` /repo/scripts/completion_hook.py ` + filepath.Join(h.codex, install.SettingsName) + `", "timeout": 10}]}]}}`
	}
	for name, seed := range map[string]func(h *host){
		"hooks.json": func(h *host) {
			userSettings(h)
			write(t, filepath.Join(h.codex, "hooks.json"), registration(h))
		},
		"config.toml": func(h *host) {
			write(t, filepath.Join(h.codex, "config.toml"), "[mcp_servers.relay-by-hand]\ncommand = \""+filepath.Join(h.dest, "current", "bin", "python3")+"\"\nargs = [\"-m\", \"codex_session_relay\"]\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHost(t)
			first, second := archive(t, "0.8.0", ""), archive(t, "0.9.0", "")
			previous := runtimeDir(h, "0.8.0", first, t)
			h.mustInstall(t, "install", first)
			seed(h)
			result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: second})
			if code != install.Refused || at(result, "failedStep") != "refuse a second owner" {
				t.Fatalf("exit %d\n%s", code, golden.Canon(result))
			}
			if !strings.Contains(golden.Canon(at(result, "steps")), "provides no bin/python3") || h.pointerTarget(t) != previous {
				t.Fatalf("pointer %s\n%s", h.pointerTarget(t), golden.Canon(at(result, "steps")))
			}
			if _, err := os.Lstat(runtimeDir(h, "0.9.0", second, t)); !os.IsNotExist(err) {
				t.Fatal("the candidate was kept")
			}
		})
	}
	t.Run("rollback", func(t *testing.T) {
		h := newHost(t)
		previous, _ := h.goEraHost(t)
		if err := os.Remove(filepath.Join(h.codex, install.SettingsName)); err != nil {
			t.Fatal(err)
		}
		userSettings(h)
		h.mustInstall(t, "update", archive(t, "0.9.0", ""))
		current := h.pointerTarget(t)
		write(t, filepath.Join(h.codex, "hooks.json"), registration(h))
		refused, code := install.Rollback(context.Background(), h.options(), previous)
		if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "provides no bin/python3") || h.pointerTarget(t) != current {
			t.Fatalf("back to %s: exit %d\n%s", previous, code, golden.Canon(refused))
		}
	})
}
