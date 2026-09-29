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

// declaredStopCommand is the Stop command the plugin package declares, as Codex caches it.
func declaredStopCommand(t *testing.T) string {
	t.Helper()
	var manifest struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, wiring("hooks", "stop-recording-completion.json"))), &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Hooks["Stop"][0].Hooks[0].Command
}

func journalRows(t *testing.T, root string) int {
	t.Helper()
	rows, err := filepath.Glob(filepath.Join(root, "[0-9]*", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// launcherEnv is what a hook process of the cached plugin receives: HOME, CODEX_HOME, the plugin
// root and a PATH with python3, and nothing that names a settings file or a policy.
func (h *host) launcherEnv(pluginRoot string) []string {
	return []string{"HOME=" + h.home, "CODEX_HOME=" + h.codex, "PLUGIN_ROOT=" + pluginRoot, "PATH=" + os.Getenv("PATH"), "PYTHONDONTWRITEBYTECODE=1"}
}

// The launchers a turn cached before this install still runs - the packaged bootstrap and the
// <CODEX_HOME>/crw-stop-hook.py copy it falls back to - run [adapterInterpreter,
// adapterEntryPoint, settings]. With adapterInterpreter /usr/bin/env that reaches the Go hook
// through the pointer with the settings path as its argument, and the hook journals the Stop.
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
	payload := `{"session_id": "s-1", "turn_id": "t-1", "transcript_path": "` + filepath.Join(h.home, "absent.jsonl") + `", "cwd": "` + h.home + `", "hook_event_name": "Stop", "stop_hook_active": false, "last_assistant_message": "DONE"}`
	command := declaredStopCommand(t)
	for _, launcher := range []struct {
		name, pluginRoot string
		fallback         bool
	}{
		{"packaged launcher", filepath.Join(golden.Root(), "plugins", "crw"), false},
		{"CODEX_HOME copy after the cache was replaced", filepath.Join(h.home, "replaced-cache"), true},
	} {
		if launcher.fallback {
			write(t, filepath.Join(h.codex, "crw-stop-hook.py"), readFile(t, wiring("crw_stop_hook.py")))
		}
		before := journalRows(t, journal)
		cmd := exec.Command("sh", "-c", command)
		cmd.Env = h.launcherEnv(launcher.pluginRoot)
		cmd.Stdin = strings.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v\n%s", launcher.name, err, stderr.String())
		}
		if after := journalRows(t, journal); after != before+1 {
			t.Fatalf("%s: the Go hook journaled %d rows (from %d); stdout %q stderr %q", launcher.name, after, before, stdout.String(), stderr.String())
		}
	}
}

// The plugin's bridge is started with a bare environment, so the only way the host's execution
// policy reaches it is the record register-mcp wrote. With only HOME set on both sides - `crw
// install register-mcp` resolving the Codex home as ~/.codex, and the wiring launcher run from
// the plugin cache under that home, which is how it finds the home when no CODEX_HOME is set -
// the launcher reads the record and starts the Go bridge under the recorded policy
// (get_capabilities reports the allowlist and its digest, not presence_only), and refuses to
// start - exit 2, naming the record and the repair - when the policy file is missing, not a
// regular file, no longer hashes to the recorded digest, or the environment names a different
// policy.
func TestTheWiringLauncherStartsTheGoBridgeUnderTheRecordedPolicy(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
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
	cached := filepath.Join(h.codex, "plugins", "cache", "crw", "crw", "0.9.0", "wiring", "crw_bridge_mcp.py")
	write(t, cached, readFile(t, wiring("crw_bridge_mcp.py")))
	launch := func(extra ...string) exercise.Bridge {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return exercise.Argv(ctx, []string{"python3", cached}, scope.Env(append(append([]string{}, homeOnly...), extra...)))
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
	if !strings.Contains(launch().Stderr, "register-mcp") {
		t.Fatal("the refusal does not name the repair")
	}
	if err := os.Remove(policy); err != nil {
		t.Fatal(err)
	}
	refuses("a missing policy")
	if err := syscall.Mkfifo(policy, 0o600); err != nil {
		t.Fatal(err)
	}
	refuses("a policy that is not a regular file")
}
