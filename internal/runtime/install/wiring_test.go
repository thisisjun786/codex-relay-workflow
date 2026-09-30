package install_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/exercise"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

func wiring(parts ...string) string {
	return filepath.Join(append([]string{golden.Root(), "plugins", "crw", "wiring"}, parts...)...)
}

// preNativeWiring is the wiring the package declared before the native commands (todo 34), with
// the two Python launchers it shipped until todo 43, kept as testdata because a turn or session
// that cached it still runs it, and the <CODEX_HOME>/crw-stop-hook.py a host may still hold is
// a copy of its crw_stop_hook.py, until the operator removes that copy.
func preNativeWiring(parts ...string) string {
	return filepath.Join(append([]string{golden.Root(), "internal", "pluginwiring", "testdata", "pre-native-wiring"}, parts...)...)
}

// preNativePayload is a cached version directory of a pre-native payload, as far as its Stop
// bootstrap reads it: the packaged launcher at ${PLUGIN_ROOT}/wiring/crw_stop_hook.py.
func preNativePayload(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "wiring", "crw_stop_hook.py"), readFile(t, preNativeWiring("crw_stop_hook.py")))
	return dir
}

// stopCommandIn is the Stop command a declaration file registers, as Codex caches it.
func stopCommandIn(t *testing.T, path string) string {
	t.Helper()
	var manifest struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Hooks["Stop"][0].Hooks[0].Command
}

// declaredServer is the MCP server the package declares: its command, arguments and working
// directory relative to the installed version directory.
func declaredServer(t *testing.T, path string) (string, []string, string) {
	t.Helper()
	var manifest struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Cwd     string   `json:"cwd"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &manifest); err != nil {
		t.Fatal(err)
	}
	server := manifest.Servers[install.ServerName]
	return server.Command, server.Args, server.Cwd
}

// journalRows is every Stop row under root, decoded.
func journalRows(t *testing.T, root string) []map[string]any {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "[0-9]*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows := []map[string]any{}
	for _, path := range paths {
		var row map[string]any
		if err := json.Unmarshal([]byte(readFile(t, path)), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	return rows
}

// launcherEnv is what a hook process of the cached plugin receives: HOME, CODEX_HOME, the plugin
// root and a PATH with python3, and nothing that names a settings file or a policy.
func (h *host) launcherEnv(pluginRoot string) []string {
	return []string{"HOME=" + h.home, "CODEX_HOME=" + h.codex, "PLUGIN_ROOT=" + pluginRoot, "PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
}

// stopPayloadFor is a Stop payload for session and turn whose transcript is absent under h's home.
func stopPayloadFor(h *host, session, turn string) string {
	return `{"session_id": "` + session + `", "turn_id": "` + turn + `", "transcript_path": "` + filepath.Join(h.home, "absent.jsonl") + `", "cwd": "` + h.home + `", "hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": "DONE"}`
}

// runStop runs a declared Stop command the way the host does, through `sh -c`.
func runStop(t *testing.T, command string, env []string, payload string) (string, string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%q: %v\n%s", command, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// The Stop command the package declares reaches the installed runtime through the pointer: under
// the settings `crw install hook --owner plugin` writes, `crw hook --plugin-launch` journals the
// Stop with its session and turn, under the installer's settings even when the environment
// names another file. With the pointer gone (mid-rollback, say) the turn is released: exit 0,
// empty stdout and no row.
func TestTheNativeStopCommandJournalsTheStopThroughThePointer(t *testing.T) {
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	if _, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.OK {
		t.Fatal("hook settings")
	}
	journal := filepath.Join(h.home, "journal")
	command := stopCommandIn(t, wiring("hooks", "stop-recording-completion.json"))
	env := append(h.launcherEnv(filepath.Join(golden.Root(), "plugins", "crw")), "CRW_COMPLETION_HOOK_CONFIG="+filepath.Join(h.home, "elsewhere.json"))
	stdout, stderr := runStop(t, command, env, stopPayloadFor(h, "s-native", "t-native"))
	rows := journalRows(t, journal)
	if stdout != "" || len(rows) != 1 {
		t.Fatalf("stdout %q stderr %q, %d rows; want no output and one row", stdout, stderr, len(rows))
	}
	if row := rows[0]; row["sessionId"] != "s-native" || row["turnId"] != "t-native" || row["configuration"] != filepath.Join(h.codex, install.SettingsName) || row["adapterOutcome"] == nil {
		t.Fatalf("row %v", row)
	}
	current := filepath.Join(h.dest, "current")
	if err := os.Rename(current, current+".aside"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr = runStop(t, command, env, stopPayloadFor(h, "s-gone", "t-gone"))
	if rows := journalRows(t, journal); stdout != "" || len(rows) != 1 || !strings.Contains(stderr, filepath.Join(current, "bin", "crw")) {
		t.Fatalf("pointer gone: stdout %q stderr %q, %d rows; want no output, the missing runtime on stderr and no new row", stdout, stderr, len(rows))
	}
}

// The launchers a turn cached before this install still runs - the pre-native bootstrap, opening
// the pre-native payload's crw_stop_hook.py and the <CODEX_HOME>/crw-stop-hook.py copy it falls
// back to - run [adapterInterpreter, adapterEntryPoint, settings]. With adapterInterpreter
// /usr/bin/env that reaches the Go hook through the pointer with the settings path as its
// argument, and the hook journals the Stop.
func TestLegacyStopLaunchersReachTheGoHook(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 runs the legacy launchers")
	}
	h := newHost(t)
	h.mustInstall(t, "install", archive(t, "0.9.0", ""))
	if _, code := install.Hook(context.Background(), h.options(), h.hookOptions()); code != install.OK {
		t.Fatal("hook settings")
	}
	journal := filepath.Join(h.home, "journal")
	command := stopCommandIn(t, preNativeWiring("hooks", "stop-recording-completion.json"))
	for _, launcher := range []struct {
		name, pluginRoot string
		fallback         bool
	}{
		{"packaged launcher", preNativePayload(t), false},
		{"CODEX_HOME copy after the cache was replaced", filepath.Join(h.home, "replaced-cache"), true},
	} {
		session := "s-" + strings.ReplaceAll(launcher.name, " ", "-")
		if launcher.fallback {
			// Without the copy the bootstrap has nothing to open and releases the Stop unrecorded,
			// which shows the row below comes from the Python launcher and not from any runtime
			// the command could reach on its own.
			before := len(journalRows(t, journal))
			if stdout, stderr := runStop(t, command, h.launcherEnv(launcher.pluginRoot), stopPayloadFor(h, session+"-uncopied", "t-1")); len(journalRows(t, journal)) != before {
				t.Fatalf("%s: a row without the launcher copy; stdout %q stderr %q", launcher.name, stdout, stderr)
			}
			write(t, filepath.Join(h.codex, "crw-stop-hook.py"), readFile(t, preNativeWiring("crw_stop_hook.py")))
		}
		before := len(journalRows(t, journal))
		stdout, stderr := runStop(t, command, h.launcherEnv(launcher.pluginRoot), stopPayloadFor(h, session, "t-1"))
		rows := journalRows(t, journal)
		if len(rows) != before+1 {
			t.Fatalf("%s: the Go hook journaled %d rows (from %d); stdout %q stderr %q", launcher.name, len(rows), before, stdout, stderr)
		}
		found := false
		for _, row := range rows {
			found = found || row["sessionId"] == session && row["turnId"] == "t-1"
		}
		if !found {
			t.Fatalf("%s: no row for session %s: %v", launcher.name, session, rows)
		}
	}
}

// bridgeLauncher starts one of the plugin's bridge launchers from the cache layout under the
// Codex home, with exactly env.
type bridgeLauncher struct {
	name   string
	python bool
	place  func(t *testing.T, h *host) (dir string, argv []string)
	// repair is what a refusal names as the repair.
	repair string
}

var bridgeLaunchers = []bridgeLauncher{
	{name: "native crw-bridge.sh", repair: "crw install register-mcp --owner plugin --execution-policy <file>",
		place: func(t *testing.T, h *host) (string, []string) {
			// The declaration as shipped: `sh ./wiring/crw-bridge.sh` from the version directory.
			command, args, cwd := declaredServer(t, wiring("mcp.json"))
			version := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.9.0")
			write(t, filepath.Join(version, "wiring", "crw-bridge.sh"), readFile(t, wiring("crw-bridge.sh")))
			return filepath.Join(version, cwd), append([]string{command}, args...)
		}},
	// A session that loaded the pre-native declaration still starts the Python launcher, which
	// keeps naming runtime_install.py, the host's installer until the cutover (decision 38).
	{name: "legacy crw_bridge_mcp.py", python: true, repair: "register-mcp",
		place: func(t *testing.T, h *host) (string, []string) {
			cached := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.9.0", "wiring", "crw_bridge_mcp.py")
			write(t, cached, readFile(t, preNativeWiring("crw_bridge_mcp.py")))
			return "", []string{"python3", cached}
		}},
}

// The plugin's bridge is started with a bare environment, so the only way the host's execution
// policy reaches it is the record register-mcp wrote. With only HOME set on both sides - `crw
// install register-mcp` resolving the Codex home as ~/.codex, and the launcher run from the
// plugin cache under that home, which is how it finds the home when no CODEX_HOME is set - the
// launcher reads the record and starts the Go bridge under the recorded policy (get_capabilities
// reports the allowlist and its digest, not presence_only), and refuses to start - exit 2, naming
// the record and the repair - when the policy file is missing, not a regular file, no longer
// hashes to the recorded digest, or the environment names a different policy. The native
// launcher is the declared one; the legacy one serves sessions that cached the older declaration.
func TestTheWiringLaunchersStartTheGoBridgeUnderTheRecordedPolicy(t *testing.T) {
	for _, launcher := range bridgeLaunchers {
		t.Run(launcher.name, func(t *testing.T) {
			if _, err := exec.LookPath("python3"); launcher.python && err != nil {
				t.Skip("python3 runs the packaged bridge launcher")
			}
			h := newHost(t)
			h.mustInstall(t, "install", archive(t, "0.9.0", ""))
			policy, digest := h.policy(t)
			ledger := filepath.Join(h.home, "bridge-ledger")
			homeOnly := []string{"HOME=" + h.home}
			var stdout, stderr strings.Builder
			if code := install.Main(context.Background(), []string{"register-mcp", "--owner", "plugin", "--execution-policy", policy,
				"--bridge-arg=--socket", "--bridge-arg=" + h.fake.SocketPath, "--bridge-arg=--state-dir", "--bridge-arg=" + ledger}, scope.Env(homeOnly), &stdout, &stderr); code != install.OK {
				t.Fatalf("register-mcp with only HOME set: exit %d\n%s%s", code, stdout.String(), stderr.String())
			}
			recordPath := filepath.Join(h.codex, install.BridgeRecordName)
			if _, err := os.Stat(recordPath); err != nil {
				t.Fatalf("register-mcp with only HOME set did not write %s: %v\n%s", recordPath, err, stdout.String())
			}
			dir, argv := launcher.place(t, h)
			launch := func(extra ...string) exercise.Bridge {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				return exercise.ArgvIn(ctx, dir, argv, scope.Env(append(append([]string{}, homeOnly...), extra...)))
			}
			started := launch()
			if started.Err != nil {
				t.Fatalf("the launcher did not start the bridge: %v\n%s", started.Err, started.Stderr)
			}
			summary := golden.Obj(record.Get(golden.Obj(started.Connection), "executionPolicy"))
			if record.Get(summary, "mode") != "allowlist" || record.Get(summary, "digest") != digest {
				t.Fatalf("the bridge runs without the recorded policy: %s", golden.Canon(summary))
			}
			refuses := func(name string, extra ...string) {
				t.Helper()
				answer := launch(extra...)
				if answer.Err == nil || answer.ExitCode != 2 || !strings.Contains(answer.Stderr, recordPath) {
					t.Fatalf("%s: exit %d err %v stderr %q", name, answer.ExitCode, answer.Err, answer.Stderr)
				}
			}
			refuses("another policy in the environment", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY="+filepath.Join(h.home, "other.json"))
			write(t, policy, policyText+"\n")
			refuses("a policy that changed after it was registered")
			if said := launch().Stderr; !strings.Contains(said, launcher.repair) {
				t.Fatalf("the refusal does not name the repair %q: %q", launcher.repair, said)
			}
			if err := os.Remove(policy); err != nil {
				t.Fatal(err)
			}
			refuses("a missing policy")
			if err := syscall.Mkfifo(policy, 0o600); err != nil {
				t.Fatal(err)
			}
			refuses("a policy that is not a regular file")
		})
	}
}
