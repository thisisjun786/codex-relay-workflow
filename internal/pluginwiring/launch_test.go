package pluginwiring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
)

// The oracle is crw_bridge_mcp.py itself, run by the workspace interpreter: the launcher the
// package shipped until todo 43, kept byte for byte in testdata/pre-native-wiring
// (preNativeLauncher).
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
	source, err := os.ReadFile(preNativeLauncher("crw_bridge_mcp.py"))
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
	// The same policy under a name that is not UTF-8. A record names it as runtime_install.py's
	// json.dumps writes a surrogate-escaped path: "p[U+DC80].json" (escapes).
	writePolicy(t, filepath.Join(root, "p\x80.json"), text)
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

// char is the character r, for the non-printing ones Python's repr() escapes.
func char(r rune) string { return string(r) }

var escapeMark = regexp.MustCompile(`\[U\+([0-9A-F]{4})\]`)

// escapes spells each [U+XXXX] in a record's JSON text as the escape json.dumps writes for that
// code point, which is how a lone surrogate reaches a record: runtime_install.py records a byte
// of a path or argument that is not UTF-8 as its surrogateescape code point, U+DC80..U+DCFF.
func escapes(text string) string {
	return escapeMark.ReplaceAllStringFunc(text, func(mark string) string {
		return `\` + "u" + strings.ToLower(mark[3:7])
	})
}

// policyAt is a version-2 record naming the host's policy text under path, spelled as escapes reads it.
func policyAt(path string) func(h launcherHost) string {
	return func(h launcherHost) string {
		return escapes(v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.root + path, "digest": h.digest}}))
	}
}

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

	// A path or argument that is not UTF-8, recorded surrogate-escaped: os.open and os.execve
	// fs-encode it back to its bytes, so the policy is found, the environment names those bytes,
	// and the bridge's argv carries them.
	{name: "v2 policy path not UTF-8 starts", record: policyAt("/p[U+DC80].json"), starts: true},
	{name: "v2 policy path not UTF-8, same inherited variable starts", record: policyAt("/p[U+DC80].json"),
		env: func(h launcherHost) []string {
			return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY=" + h.root + "/p\x80.json")
		}, starts: true},
	{name: "v1 args not UTF-8 start", record: func(h launcherHost) string {
		return escapes(v1(h, map[string]any{"args": []string{"--state-dir", h.root + "/st[U+DC80]te", "--socket", h.root + "/s[U+DCFF].sock"}}))
	}, starts: true},
	// The same path missing: str() of it and the OSError's repr() both carry the surrogate, which
	// the stream writes as its escape.
	{name: "v2 policy path not UTF-8, missing", record: policyAt("/gone[U+DC80].json")},
	{name: "v2 policy path with a surrogate outside the escape range", record: policyAt("/p[U+D800].json")},
	{name: "v2 inherited policy variable not UTF-8, another file", record: func(h launcherHost) string { return v2(h, nil) },
		env: func(h launcherHost) []string {
			return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY=" + h.root + "/other\x80.json")
		}},

	// What repr() escapes: every character str.isprintable() rejects.
	{name: "owner holding a no-break space", record: func(h launcherHost) string {
		return v1(h, map[string]any{"owner": "a" + char(0xa0) + "b"})
	}},
	{name: "owner holding a zero-width space between printables", record: func(h launcherHost) string {
		return v1(h, map[string]any{"owner": char(0xe9) + char(0x200b) + char(0x1f600)})
	}},
	{name: "owner holding private-use, unassigned and astral format characters", record: func(h launcherHost) string {
		return v1(h, map[string]any{"owner": char(0xe000) + char(0x378) + char(0xe0001) + char(0x3000)})
	}},
	{name: "owner a lone surrogate", record: func(h launcherHost) string {
		return escapes(v1(h, map[string]any{"owner": "[U+D800]"}))
	}},
	{name: "server name holding a Mongolian vowel separator", record: func(h launcherHost) string {
		return v1(h, map[string]any{"serverName": "x" + char(0x180e)})
	}},
	{name: "v2 policy path padded with a no-break space", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.policy + char(0xa0), "digest": h.digest}})
	}},
	{name: "v2 policy path holding a next-line character, missing", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.root + "/nonexistent" + char(0x85) + "x", "digest": h.digest}})
	}},
	{name: "v2 policy path holding a line separator, missing", record: func(h launcherHost) string {
		return v2(h, map[string]any{"executionPolicy": map[string]any{"path": h.root + "/nonexistent" + char(0x2028) + "x", "digest": h.digest}})
	}},
	{name: "v2 inherited policy variable holding a zero-width space", record: func(h launcherHost) string { return v2(h, nil) },
		env: func(h launcherHost) []string {
			return h.env("CODEX_THREAD_BRIDGE_EXECUTION_POLICY=" + h.root + "/other" + char(0x200b) + ".json")
		}},
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

// Where the record names an executable or argument os.execv cannot encode (a lone surrogate
// outside U+DC80..U+DCFF, or a NUL), the Python launcher starts nothing: execv raises, and the
// traceback exits 1. The Go launcher does not exec bridgeExecutable, but refuses that record too,
// with exit 2 naming the record, rather than starting a bridge Python never started.
func TestBridgeLaunch_refuses_a_record_python_cannot_exec(t *testing.T) {
	for _, c := range []struct {
		name, executable, args, python, golang string
	}{
		{"argument with a surrogate outside the escape range", "", `["--state-dir", "ROOT/st[U+D800]te"]`,
			"UnicodeEncodeError", "lists the argument 'ROOT/st" + `\` + "ud800te'"},
		{"argument holding a NUL", "", `["--state-dir", "ROOT/st[U+0000]te"]`,
			"embedded null", `lists the argument 'ROOT/st\x00te'`},
		{"executable with a surrogate outside the escape range", "/probe[U+DBFF]", `[]`,
			"UnicodeEncodeError", "names bridgeExecutable '/probe" + `\` + "udbff'"},
		{"executable holding a NUL", "/probe[U+0000]", `[]`,
			"embedded null", `names bridgeExecutable '/probe\x00'`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newLauncherHost(t)
			executable := c.executable
			if executable == "" {
				executable = h.probe
			}
			// Under h.root, so a launcher that started the bridge anyway writes nothing elsewhere.
			args := strings.ReplaceAll(c.args, "ROOT", h.root)
			h.record(t, escapes(`{"recordVersion": 1, "owner": "plugin", "bridgeExecutable": "`+executable+`", "args": `+args+`}`))
			py := h.python(t, h.env())
			if py.code != 1 || py.stdout != "" || !strings.Contains(py.stderr, c.python) {
				t.Fatalf("python: exit %d stdout %q stderr %q; want the exec to raise %s", py.code, py.stdout, py.stderr, c.python)
			}
			got := h.goLaunch(t, h.env())
			want := "crw bridge launcher: the record at " + filepath.Join(h.codexHome, RecordName) + " " + strings.ReplaceAll(c.golang, "ROOT", h.root) +
				", which this system cannot pass to exec. Rewrite it with " + RepairCommand + ".\n"
			if got.code != 2 || got.stdout != "" || got.stderr != want {
				t.Fatalf("go: exit %d stdout %q stderr %q\nwant exit 2 stderr %q", got.code, got.stdout, got.stderr, want)
			}
		})
	}
}
