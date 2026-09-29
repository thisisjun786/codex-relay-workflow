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
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// editorSave replaces path the way an editor saves: a temporary sibling renamed over it,
// without the settings lock.
func editorSave(t *testing.T, path, text string) {
	t.Helper()
	temporary := path + ".editor-tmp"
	write(t, temporary, text)
	if err := os.Rename(temporary, path); err != nil {
		t.Fatal(err)
	}
}

const editorDocument = `{"editor": "saved this"}` + "\n"

// A document another writer saved over the settings - one that does not take the settings lock
// - is never removed or overwritten by a replacement settling back: neither when it lands
// between the Go document's write and its read-back (`crw install hook`), nor when it lands
// after the transition and before a failed commit puts the Python-era document back (`crw
// install update`). Both documents survive, each where the answer says.
func TestAReplacementNeverDestroysAnotherWritersDocument(t *testing.T) {
	t.Run("hook: saved before the read-back", func(t *testing.T) {
		h := newHost(t)
		h.mustInstall(t, "install", archive(t, "0.9.0", ""))
		original := h.pythonEraSettings(t)
		path := filepath.Join(h.codex, install.SettingsName)
		restore := install.ReplaceSettingsWriter(func(target string, text []byte) error {
			if err := record.AtomicWrite(target, text); err != nil {
				return err
			}
			if target == path {
				editorSave(t, path, editorDocument)
			}
			return nil
		})
		result, code := install.Hook(context.Background(), h.options(), h.hookOptions())
		restore()
		archives := must(filepath.Glob(path + ".superseded-*"))
		if code != install.Refused || at(result, "settings", "outcome") != install.ConfigAppliedUnverified || at(result, "settings", "putBack", "undone") != false {
			t.Fatalf("exit %d\n%s", code, golden.Canon(result))
		}
		if readFile(t, path) != editorDocument || len(archives) != 1 || readFile(t, archives[0]) != original || at(result, "settings", "retired") != archives[0] {
			t.Fatalf("settings %q, archives %v", readFile(t, path), archives)
		}
	})
	t.Run("update: saved before a failed commit", func(t *testing.T) {
		h := newHost(t)
		venv, original := h.pythonEraHost(t)
		path := filepath.Join(h.codex, install.SettingsName)
		restore := install.ReplaceSelectionCommit(func(string, int, record.Delta) (reading.Reading, error) {
			editorSave(t, path, editorDocument)
			return reading.Reading{}, errNoSpace
		})
		result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: archive(t, "0.9.0", "")})
		restore()
		archives := must(filepath.Glob(path + ".superseded-*"))
		if code != install.Refused || at(result, "failedStep") != "commit the selection" || at(result, "settings", "undone", "undone") != false || h.pointerTarget(t) != venv {
			t.Fatalf("exit %d\n%s", code, golden.Canon(result))
		}
		if readFile(t, path) != editorDocument || len(archives) != 1 || readFile(t, archives[0]) != original {
			t.Fatalf("settings %q, archives %v", readFile(t, path), archives)
		}
	})
}

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
// owned pointer the runtime about to be named does not provide - the venv's python3, which a Go
// runtime does not have - is the second owner beside the plugin's declaration and would run
// nothing after the swap: the promotion refuses it and nothing moves. A rollback to a venv that
// does provide it is allowed, and the way back to the Go runtime refuses it again.
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
			venv, _ := h.pythonEraHost(t)
			if err := os.Remove(filepath.Join(h.codex, install.SettingsName)); err != nil {
				t.Fatal(err)
			}
			seed(h)
			first := archive(t, "0.9.0", "")
			result, code := install.Install(context.Background(), h.options(), "update", install.Source{From: first})
			if code != install.Refused || at(result, "failedStep") != "refuse a second owner" {
				t.Fatalf("exit %d\n%s", code, golden.Canon(result))
			}
			if !strings.Contains(golden.Canon(at(result, "steps")), "provides no bin/python3") || h.pointerTarget(t) != venv {
				t.Fatalf("pointer %s\n%s", h.pointerTarget(t), golden.Canon(at(result, "steps")))
			}
			if _, err := os.Lstat(runtimeDir(h, "0.9.0", first, t)); !os.IsNotExist(err) {
				t.Fatal("the candidate was kept")
			}
		})
	}
	t.Run("rollback", func(t *testing.T) {
		h := newHost(t)
		venv, _ := h.pythonEraHost(t)
		userSettings(h)
		h.mustInstall(t, "update", archive(t, "0.9.0", ""))
		goRuntime := h.pointerTarget(t)
		write(t, filepath.Join(h.codex, "hooks.json"), registration(h))
		if result, code := install.Rollback(context.Background(), h.options(), venv); code != install.OK || h.pointerTarget(t) != venv {
			t.Fatalf("to the venv, which provides python3: exit %d\n%s", code, golden.Canon(result))
		}
		refused, code := install.Rollback(context.Background(), h.options(), "")
		if code != install.Refused || !strings.Contains(text(at(refused, "refused")), "provides no bin/python3") || h.pointerTarget(t) != venv {
			t.Fatalf("back to %s: exit %d\n%s", goRuntime, code, golden.Canon(refused))
		}
	})
}

// `crw install hook --owner plugin` replaces a Python-era plugin document on a Go host only by
// settings that keep every host fact it records: flags that say something else (observe instead
// of hold, no isolation, no database, no socket) answer config_differs with those fields and the
// repair, and write nothing; the same flags as the document, spelled as the repair names them,
// replace it, moving only the adapter.
func TestHookKeepsThePythonEraHostFacts(t *testing.T) {
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	path := filepath.Join(h.codex, install.SettingsName)
	database, socket := filepath.Join(h.home, "relay.db"), "/run/app.sock"
	held := `{
  "adapterEntryPoint": "/home/user/code/codex-relay-workflow/scripts/completion_hook.py",
  "adapterInterpreter": "/usr/bin/python3",
  "configVersion": 1,
  "dbPath": "` + database + `",
  "event": "Stop",
  "installedBy": "CRW-116",
  "isolationAssertedBy": "operator",
  "journalPolicy": "every_invocation",
  "journalRoot": "` + filepath.Join(h.home, "journal") + `",
  "markerRoot": "` + filepath.Join(h.home, "markers") + `",
  "mode": "hold",
  "owner": "plugin",
  "relayExecutable": "` + filepath.Join(h.dest, "current", "bin", "codex-session-relay") + `",
  "socketPath": "` + socket + `",
  "timeoutSeconds": 5
}
`
	write(t, path, held)
	refused, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	if code != install.Refused || at(refused, "settings", "outcome") != install.ConfigDiffers || at(refused, "settings", "repair") == nil {
		t.Fatalf("exit %d\n%s", code, golden.Canon(refused))
	}
	if got := strings.Join(strList(at(refused, "settings", "differingFields")), ","); got != "dbPath,isolationAssertedBy,mode,socketPath" {
		t.Fatalf("differingFields %s", got)
	}
	if readFile(t, path) != held || len(must(filepath.Glob(path+".superseded-*"))) != 0 {
		t.Fatal("a refusal changed the settings")
	}
	// The repair's flags are the command's own: rerunning `crw install hook` with each flag it
	// names, given what the document records, would replace the document (a dry run answers
	// config_would_create, archiving the Python-era document, where other flags answer
	// config_differs).
	repair := text(at(refused, "settings", "repair"))
	opening, closing := strings.Index(repair, "("), strings.Index(repair, ")")
	if opening < 0 || closing < opening {
		t.Fatalf("the repair names no flags: %s", repair)
	}
	says := map[string]string{"--mode": "hold", "--isolation-asserted-by": "operator", "--marker-root": filepath.Join(h.home, "markers"),
		"--db-path": database, "--socket": socket, "--journal-root": filepath.Join(h.home, "journal"),
		"--relay-command": filepath.Join(h.dest, "current", "bin", "codex-session-relay"), "--guard-timeout": "5"}
	rerun := []string{"hook", "--owner", "plugin", "--dry-run"}
	for _, flag := range strings.Split(repair[opening+1:closing], ", ") {
		rerun = append(rerun, flag, says[flag])
	}
	if code, stdout, stderr := h.main(t, h.env, rerun...); code != install.OK || !strings.Contains(stdout, `"outcome": "`+install.ConfigWouldCreate+`"`) ||
		!strings.Contains(stdout, "would archive the Python-era settings") {
		t.Fatalf("following the repair %q: exit %d\nstdout %s\nstderr %s", rerun, code, stdout, stderr)
	}
	if readFile(t, path) != held {
		t.Fatal("a dry run changed the settings")
	}
	same := h.hookOptions()
	same.Mode, same.Isolation, same.Database, same.Socket = "hold", "operator", database, socket
	replaced, code := install.Hook(context.Background(), h.options(), same)
	if code != install.OK || at(replaced, "settings", "outcome") != install.ConfigReplaced {
		t.Fatalf("the same host facts: exit %d\n%s", code, golden.Canon(replaced))
	}
	for _, want := range []string{`"mode": "hold"`, `"isolationAssertedBy": "operator"`, `"socketPath": "/run/app.sock"`, `"adapterInterpreter": "/usr/bin/env"`} {
		if !strings.Contains(readFile(t, path), want) {
			t.Fatalf("the replacement lacks %s:\n%s", want, readFile(t, path))
		}
	}
}

// With adapterInterpreter /usr/bin/env, an adapterEntryPoint holding '=' is read by env as an
// assignment, and every Stop would run nothing: `crw install hook` refuses to write such settings,
// and an install over Python-era settings under such a destination refuses before it moves the
// pointer, leaving the Python-era settings working where they are.
func TestAnEntryPointEnvWouldMisreadIsRefused(t *testing.T) {
	h := newHost(t)
	h.dest = filepath.Join(h.home, "a=b", "crw-runtime")
	result, code := install.Hook(context.Background(), h.options(), h.hookOptions())
	if code != install.Usage || !strings.Contains(text(at(result, "error")), "contains '='") {
		t.Fatalf("hook: exit %d\n%s", code, golden.Canon(result))
	}
	if _, err := os.Lstat(filepath.Join(h.codex, install.SettingsName)); !os.IsNotExist(err) {
		t.Fatal("hook wrote settings env would misread")
	}

	venv, original := h.pythonEraHost(t)
	refused, code := install.Install(context.Background(), h.options(), "update", install.Source{From: archive(t, "0.9.0", "")})
	if code != install.Refused || at(refused, "failedStep") != "carry the Stop settings to this runtime" || h.pointerTarget(t) != venv {
		t.Fatalf("install: exit %d\n%s", code, golden.Canon(refused))
	}
	if readFile(t, filepath.Join(h.codex, install.SettingsName)) != original {
		t.Fatal("the Python-era settings were replaced")
	}
}
