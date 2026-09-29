package pluginwiring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
)

// The oracle is plugins/crw/wiring/crw_bridge_mcp.py itself, run by the workspace interpreter.
// Both launchers are placed in the same cache layout under one Codex home and started with the
// same record, environment and working directory; stderr and exit are compared byte for byte,
// once Python's repairs are rewritten to the Go ones (goRepairs). Where Python would exec the
// recorded bridgeExecutable, the record names a probe that prints its argv and the two policy
// variables; Go execs the real bridge, which is asked for --version in the same position, and
// the probe's view is compared with Prepare's answer.

// goRepairs are the only refusal texts in which the Go launcher departs from the Python one on
// purpose (decision 26): the repairs name the installer that writes the record since todo 38.
var goRepairs = []struct{ python, golang string }{
	{" Which command writes it depends on who owns this server. If this host registers the bridge" +
		" in its Codex configuration, run runtime_install.py register-mcp --apply and this launcher" +
		" will stand down for that registration. If the package is to own it, add --owner plugin.",
		" If the package is to own this server, run " + RepairCommand + " to write it. If this host" +
			" registers the bridge in its Codex configuration instead, this package must not start a" +
			" second one."},
	{"Rewrite it with the runtime_install.py that ships with this package rather than",
		"Rewrite it with " + RepairCommand + " rather than"},
	{" Run runtime_install.py register-mcp --owner plugin --execution-policy <file> again",
		" Run " + RepairCommand + " --execution-policy <file> again"},
}

// inGo is the Python launcher's stderr with its repairs rewritten to the Go launcher's.
func inGo(python string) string {
	for _, r := range goRepairs {
		python = strings.ReplaceAll(python, r.python, r.golang)
	}
	return python
}

// bridgeEntry is the built crw under the name the launcher execs it by, codex-thread-bridge.
var bridgeEntry = sync.OnceValues(func() (string, error) {
	built, err := crwBinary()
	if err != nil {
		return "", err
	}
	entry := filepath.Join(filepath.Dir(built), declaredServer)
	return entry, os.Symlink(filepath.Base(built), entry)
})

func workspacePython(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRoot(t), ".venv", "bin", "python")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("workspace Python missing: run uv sync --locked")
	}
	return path
}

// launcherHost is a Codex home holding a cached crw package with both launchers, and a probe.
type launcherHost struct {
	root, codexHome, version, probe, policy, digest string
}

func newLauncherHost(t *testing.T) launcherHost {
	t.Helper()
	root := t.TempDir()
	h := launcherHost{root: root, codexHome: filepath.Join(root, ".codex")}
	h.version = filepath.Join(h.codexHome, "plugins", "cache", "crw", "crw", "0.4.0")
	if err := os.MkdirAll(filepath.Join(h.version, "wiring"), 0o755); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(packageRoot(t), "wiring", "crw_bridge_mcp.py"))
	if err != nil {
		t.Fatal(err)
	}
	// Placed under the native launcher's file name: codex_home() names its own __file__ in one
	// failure text, and this makes that the same path on both sides rather than rewriting text.
	write(t, filepath.Join(h.version, "wiring", launcherName), string(source), 0o644)
	h.probe = filepath.Join(root, "probe")
	write(t, h.probe, "#!/bin/sh\nprintf 'argv=%s\\n' \"$*\"\nprintf 'policy=%s\\n' \"${CODEX_THREAD_BRIDGE_EXECUTION_POLICY-<unset>}\"\nprintf 'digest=%s\\n' \"${CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST-<unset>}\"\n", 0o755)
	h.policy = filepath.Join(root, "execution-policy.json")
	text := "{\"roles\": {\"child\": {\"model\": \"probe\", \"reasoningEffort\": \"probe\"}}}\n"
	h.digest = writePolicy(t, h.policy, text)
	return h
}

// writePolicy writes an execution policy and returns its digest.
func writePolicy(t *testing.T, path, text string) string {
	t.Helper()
	write(t, path, text, 0o644)
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func (h launcherHost) record(t *testing.T, text string) {
	t.Helper()
	path := filepath.Join(h.codexHome, RecordName)
	_ = os.Remove(path)
	if text != "" {
		write(t, path, text, 0o600)
	}
}

func (h launcherHost) env(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(h.root, "home"), "CODEX_HOME=" + h.codexHome}, extra...)
}

func (h launcherHost) python(t *testing.T, env []string, args ...string) outcome {
	return run(t, h.version, env, "", append([]string{workspacePython(t), "./wiring/" + launcherName}, args...)...)
}

// goLaunch starts the Go launcher as wiring/crw-bridge.sh execs it: codex-thread-bridge --plugin-launch.
func (h launcherHost) goLaunch(t *testing.T, env []string, args ...string) outcome {
	t.Helper()
	entry, err := bridgeEntry()
	if err != nil {
		t.Fatal(err)
	}
	return run(t, h.version, env, "", append([]string{entry, Flag}, args...)...)
}

// recordCase is one record and what both launchers are expected to do with it.
type recordCase struct {
	name   string
	record func(h launcherHost) string
	env    func(h launcherHost) []string
	starts bool // true when Python execs the probe and Go starts the bridge
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func v2(h launcherHost, overrides map[string]any) string {
	record := map[string]any{"recordVersion": 2, "owner": "plugin", "serverName": "codex-thread-bridge",
		"bridgeExecutable": h.probe, "args": []string{}, "executionPolicy": map[string]any{"path": h.policy, "digest": h.digest}}
	for k, v := range overrides {
		if v == nil {
			delete(record, k)
		} else {
			record[k] = v
		}
	}
	return jsonText(record)
}

func v1(h launcherHost, overrides map[string]any) string {
	record := map[string]any{"recordVersion": 1, "owner": "plugin", "serverName": "codex-thread-bridge",
		"bridgeExecutable": h.probe, "args": []string{}}
	for k, v := range overrides {
		if v == nil {
			delete(record, k)
		} else {
			record[k] = v
		}
	}
	return jsonText(record)
}

func literal(text string) func(launcherHost) string { return func(launcherHost) string { return text } }

var recordCases = []recordCase{
	{name: "record missing", record: literal("")},
	{name: "record not JSON", record: literal("{not json")},
	{name: "record not UTF-8", record: literal("{\"a\": \"\xff\"}")},
	{name: "record a directory", record: nil},
	{name: "record not an object", record: literal("[1, 2]")},
	{name: "version 3", record: func(h launcherHost) string { return v1(h, map[string]any{"recordVersion": 3}) }},
	{name: "version absent", record: func(h launcherHost) string { return v1(h, map[string]any{"recordVersion": nil}) }},
	{name: "version string", record: func(h launcherHost) string { return v1(h, map[string]any{"recordVersion": "1"}) }},
	{name: "owner user (stand-down)", record: func(h launcherHost) string { return v1(h, map[string]any{"owner": "user"}) }},
	{name: "owner absent", record: func(h launcherHost) string { return v1(h, map[string]any{"owner": nil}) }},
	{name: "server name mismatch", record: func(h launcherHost) string { return v1(h, map[string]any{"serverName": "other"}) }},
	{name: "executable relative", record: func(h launcherHost) string { return v1(h, map[string]any{"bridgeExecutable": "bin/crw"}) }},
	{name: "executable absent", record: func(h launcherHost) string { return v1(h, map[string]any{"bridgeExecutable": nil}) }},
	{name: "args false", record: func(h launcherHost) string { return v1(h, map[string]any{"args": false}) }},
	{name: "args empty string", record: func(h launcherHost) string { return v1(h, map[string]any{"args": ""}) }},
	{name: "args object", record: func(h launcherHost) string { return v1(h, map[string]any{"args": map[string]any{}}) }},
	{name: "args holding a number", record: func(h launcherHost) string { return v1(h, map[string]any{"args": []any{"--socket", 1}}) }},
	{name: "v1 carrying a policy", record: func(h launcherHost) string {
		return v1(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy, "digest": h.digest}})
	}},
	{name: "v2 policy absent", record: func(h launcherHost) string { return v2(h, map[string]any{"executionPolicy": nil}) }},
	{name: "v2 policy extra key", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy, "digest": h.digest, "x": 1}})
	}},
	{name: "v2 policy relative path", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": "policy.json", "digest": h.digest}})
	}},
	{name: "v2 policy padded path", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy + " ", "digest": h.digest}})
	}},
	{name: "v2 policy bad digest", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy, "digest": strings.ToUpper(h.digest)}})
	}},
	{name: "v2 policy missing file", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy + ".gone", "digest": h.digest}})
	}},
	{name: "v2 policy a directory", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.root, "digest": h.digest}})
	}},
	{name: "v2 policy changed", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy, "digest": strings.Repeat("0", 64)}})
	}},
	{name: "v2 inherited other policy file", record: func(h launcherHost) string { return v2(h, nil) },
		env: func(h launcherHost) []string {
			return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY=" + h.root + "/other.json")
		}},
	{name: "v2 inherited other digest", record: func(h launcherHost) string { return v2(h, nil) },
		env: func(h launcherHost) []string {
			return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST=" + strings.Repeat("1", 64))
		}},
	{name: "CODEX_HOME unset, home derived from the cache location", record: literal(""),
		env: func(h launcherHost) []string { return h.env()[:2] }},
	{name: "CODEX_HOME unset, derived home starts", record: func(h launcherHost) string { return v1(h, nil) },
		env: func(h launcherHost) []string { return h.env()[:2] }, starts: true},
	{name: "v1 starts", record: func(h launcherHost) string { return v1(h, nil) }, starts: true},
	{name: "v1 serverName absent starts", record: func(h launcherHost) string { return v1(h, map[string]any{"serverName": nil}) }, starts: true},
	{name: "v1 args null starts", record: func(h launcherHost) string { return v1(h, map[string]any{"args": nil}) }, starts: true},
	{name: "v2 starts under the policy", record: func(h launcherHost) string { return v2(h, nil) }, starts: true},
	{name: "v2 same inherited policy starts", record: func(h launcherHost) string { return v2(h, nil) },
		env: func(h launcherHost) []string {
			return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY= "+h.policy+" ", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST="+h.digest)
		}, starts: true},
	{name: "v2 empty inherited variable starts", record: func(h launcherHost) string { return v2(h, nil) },
		env: func(h launcherHost) []string { return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY=") }, starts: true},
}

// recordCheck is one row of the evidence the orchestrator asked for.
type recordCheck struct {
	Case   string `json:"case"`
	Python string `json:"python"`
	Go     string `json:"go"`
	Equal  bool   `json:"equal"`
}

func TestBridgeLaunch_matches_the_python_launcher_record_by_record(t *testing.T) {
	var checks []recordCheck
	for _, c := range recordCases {
		t.Run(c.name, func(t *testing.T) {
			h := newLauncherHost(t)
			if c.record == nil {
				if err := os.MkdirAll(filepath.Join(h.codexHome, RecordName), 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				h.record(t, c.record(h))
			}
			env := h.env()
			if c.env != nil {
				env = c.env(h)
			}
			py := h.python(t, env)
			if !c.starts {
				got := h.goLaunch(t, env)
				equal := py.code == got.code && inGo(py.stderr) == got.stderr && py.stdout == got.stdout
				checks = append(checks, recordCheck{c.name, summary(py), summary(got), equal})
				if !equal {
					t.Fatalf("python: exit %d stdout %q stderr %q\ngo:     exit %d stdout %q stderr %q", py.code, py.stdout, inGo(py.stderr), got.code, got.stdout, got.stderr)
				}
				if py.code != 2 || py.stderr == "" {
					t.Fatalf("the oracle did not refuse: exit %d stderr %q", py.code, py.stderr)
				}
				return
			}
			// Python exec'd the probe. Go's arguments and variables must be what the probe saw,
			// and the real bridge must start under them.
			if py.code != 0 || py.stderr != "" {
				t.Fatalf("the oracle did not start the probe: exit %d stderr %q", py.code, py.stderr)
			}
			goArgs, goEnv := prepared(t, h, env)
			want := "argv=" + strings.Join(goArgs, " ") + "\npolicy=" + orUnset(goEnv, "CODEX_THREAD_BRIDGE_EXECUTION_POLICY") +
				"\ndigest=" + orUnset(goEnv, "CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST") + "\n"
			equal := want == py.stdout
			checks = append(checks, recordCheck{c.name, strings.TrimSpace(py.stdout), strings.TrimSpace(want), equal})
			if !equal {
				t.Fatalf("python's bridge saw %q\ngo prepares %q", py.stdout, want)
			}
			got := h.goLaunch(t, env, "--version")
			if got.code != 0 || got.stdout != mcp.PackageVersion+"\n" {
				t.Fatalf("crw bridge --plugin-launch --version: exit %d stdout %q stderr %q", got.code, got.stdout, got.stderr)
			}
		})
	}
	if path := os.Getenv("CRW_TASK34_RECORD_CHECKS"); path != "" {
		raw, _ := json.MarshalIndent(checks, "", "  ")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func summary(o outcome) string {
	return "exit " + strconv.Itoa(o.code) + ": " + strings.TrimSpace(o.stderr+o.stdout)
}

func orUnset(env map[string]string, name string) string {
	if value, ok := env[name]; ok {
		return value
	}
	return "<unset>"
}

// prepared is Prepare run from the version directory the host starts the launcher in.
func prepared(t *testing.T, h launcherHost, env []string) ([]string, map[string]string) {
	t.Helper()
	t.Chdir(h.version)
	values := map[string]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	args, environment, err := Prepare(values, nil)
	if err != nil {
		t.Fatal(err)
	}
	return args, environment
}

// The record's args reach the bridge as its command line, so a recorded --socket selects the
// App Server the bridge connects to. The Python launcher dropped its own argv (the declaration
// passed none); the native launcher's "$@" is appended after the record's words.
func TestBridgeLaunch_passes_the_record_args_to_the_bridge(t *testing.T) {
	h := newLauncherHost(t)
	socket := filepath.Join(h.root, "app server.sock")
	h.record(t, v1(h, map[string]any{"args": []string{"--socket", socket}}))
	py := h.python(t, h.env())
	if want := "argv=--socket " + socket; !strings.HasPrefix(py.stdout, want+"\n") {
		t.Fatalf("python probe saw %q, want %q", py.stdout, want)
	}
	t.Chdir(h.version)
	args, _, err := Prepare(map[string]string{"HOME": h.root, "CODEX_HOME": h.codexHome}, []string{"--state-dir", "/x"})
	if err != nil || strings.Join(args, "|") != "--socket|"+socket+"|--state-dir|/x" {
		t.Fatalf("Prepare args %q err %v", args, err)
	}
	// Through the real bridge: a record argument it does not know is refused as argparse would,
	// which shows the recorded words are parsed as its command line.
	h.record(t, v1(h, map[string]any{"args": []string{"--no-such-flag"}}))
	got := h.goLaunch(t, h.env())
	if got.code != 2 || !strings.Contains(got.stderr, "no-such-flag") {
		t.Fatalf("exit %d stderr %q; want the bridge's usage refusal naming the recorded flag", got.code, got.stderr)
	}
}

// Under a version-2 record the running bridge reports the recorded policy's digest as the
// policy it enforces: the variables reached the bridge's own process environment.
func TestBridgeLaunch_starts_the_bridge_under_the_recorded_policy(t *testing.T) {
	h := newLauncherHost(t)
	h.digest = writePolicy(t, h.policy, "{\"roles\": 5}")
	h.record(t, v2(h, nil))
	// A policy the bridge's own parser refuses: only a bridge that was handed the file refuses it.
	socket := filepath.Join(h.root, "app-server.sock")
	got := h.goLaunch(t, h.env(), "--socket", socket, "--state-dir", filepath.Join(h.root, "state"))
	want := "execution_policy_unreadable: roles must be a JSON object keyed by role id\n"
	if got.code != 1 || got.stderr != want {
		t.Fatalf("exit %d stderr %q; want the bridge refusing the recorded policy", got.code, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(h.root, "state")); !os.IsNotExist(err) {
		t.Fatal("the bridge opened its ledger before refusing the policy")
	}
	// Control: the same bridge with no record policy starts, opens its ledger and ends on EOF.
	h.record(t, v1(h, nil))
	got = h.goLaunch(t, h.env(), "--socket", socket, "--state-dir", filepath.Join(h.root, "state"))
	if got.code != 0 {
		t.Fatalf("v1 control: exit %d stderr %q", got.code, got.stderr)
	}
}

// The policy refusal's repair names the Go installer's command, not runtime_install.py
// (docs/port/refactor-backlog.md, the todo 38 rebase gate). That goRepairs rewrites only what
// the Python launcher still says is the record-by-record test's to show: an entry that no longer
// matches leaves Python's text unrewritten there, and the comparison fails.
func TestBridgeLaunch_repairs_name_crw_install(t *testing.T) {
	h := newLauncherHost(t)
	h.record(t, v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy, "digest": strings.Repeat("0", 64)}}))
	got := h.goLaunch(t, h.env())
	want := "Run crw install register-mcp --owner plugin --execution-policy <file> again after moving " + filepath.Join(h.codexHome, RecordName) + " aside"
	if got.code != 2 || !strings.Contains(got.stderr, want) || strings.Contains(got.stderr, "runtime_install") {
		t.Fatalf("exit %d stderr %q; want exit 2 naming %q", got.code, got.stderr, want)
	}
}

// A record whose arguments begin with the plugin-launch flag would have the exec start this
// launcher again rather than the bridge; it is refused instead of looping.
func TestBridgeLaunch_refuses_arguments_that_would_start_the_launcher_again(t *testing.T) {
	h := newLauncherHost(t)
	h.record(t, v1(h, map[string]any{"args": []string{Flag}}))
	got := h.goLaunch(t, h.env())
	if got.code != 2 || !strings.Contains(got.stderr, "crw bridge launcher: the bridge's arguments begin with "+Flag) {
		t.Fatalf("exit %d stderr %q", got.code, got.stderr)
	}
	h.record(t, v1(h, nil))
	if got := h.goLaunch(t, h.env(), Flag); got.code != 2 || !strings.Contains(got.stderr, "would start this launcher again") {
		t.Fatalf("launcher argument: exit %d stderr %q", got.code, got.stderr)
	}
}
