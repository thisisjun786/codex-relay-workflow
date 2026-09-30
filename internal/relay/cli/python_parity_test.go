package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// pythonHome isolates HOME, XDG_* and CODEX_HOME for BOTH implementations in one directory of
// the test's own, so neither can reach the live relay state or ~/.codex. The directory is a
// fixedTree: an answer the Python relay gave there carries digests of paths under it (a socket's
// scope key, an artifact's revision and event id), which only the same path gives again.
func pythonHome(t *testing.T) string {
	t.Helper()
	return pythonHomeAt(t, fixedTree(t, oracleKey(t, "home "+t.Name())))
}

// pythonHomeAt is pythonHome at a home the caller made.
func pythonHomeAt(t *testing.T, home string) string {
	t.Helper()
	for key, dir := range map[string]string{
		"HOME": "", "XDG_STATE_HOME": "xdg-state", "XDG_CONFIG_HOME": "xdg-config", "XDG_DATA_HOME": "xdg-data",
		"XDG_CACHE_HOME": "xdg-cache", "CODEX_HOME": "codex-home", "CODEX_SESSION_RELAY_SCOPE_DIR": "scopes",
	} {
		t.Setenv(key, filepath.Join(home, dir))
	}
	for _, key := range []string{"CODEX_SESSION_RELAY_STATE", "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	return home
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

type run struct {
	code   int
	stdout string
	stderr string
}

// python is the answer the real Python relay CLI gave with this process's (isolated)
// environment, on the selected store as its owner (recorded: see oracleRun). The store is
// handed to Python whether or not Python runs, so the Go side meets the same store either way.
func python(t *testing.T, dir string, argv ...string) run {
	t.Helper()
	ownedArgs(t, dir, argv, "python")
	options := placeholders(append([]string{dir}, argv...)...)
	if id := selectedStoreID(t, dir, argv); id != "" {
		options = append(options, pyoracle.Substitute(id, "<store-id>"))
	}
	return oracleRun(t, oracleKey(t, "python "+oracleLabel(argv...)), func() (run, error) {
		command := exec.Command("uv", append([]string{"run", "--no-sync", "--project", repositoryRoot(t), "codex-session-relay"}, argv...)...)
		command.Dir = dir
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		code := 0
		if err := command.Run(); err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				return run{}, fmt.Errorf("python %v: %w", argv, err)
			}
			code = exitErr.ExitCode()
		}
		return run{code, stdout.String(), stderr.String()}, nil
	}, options...)
}

// golang runs the Go relay CLI in-process, as `codex-session-relay`, on the selected store as
// its owner.
func golang(t *testing.T, dir string, argv ...string) run {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(previous) }()
	ownedArgs(t, "", argv, "go")
	var stdout, stderr bytes.Buffer
	code := cli.ExecuteAs(context.Background(), "codex-session-relay", argv, &stdout, &stderr)
	return run{code, stdout.String(), stderr.String()}
}

// withoutKey drops one top-level key from a JSON document, keeping every other byte.
func withoutKey(t *testing.T, document, key string) string {
	t.Helper()
	marker := "  \"" + key + "\": "
	start := strings.Index(document, marker)
	if start < 0 {
		t.Fatalf("no %q in %s", key, document)
	}
	end := start + strings.Index(document[start:], "\n  }")
	if end < start {
		t.Fatalf("unterminated %q", key)
	}
	cut := document[:start] + document[end+len("\n  }"):]
	// The removed key was the last one: drop the comma the previous line kept.
	return strings.Replace(cut, ",\n\n}", "\n}", 1)
}

func requireSame(t *testing.T, py, got run) {
	t.Helper()
	if py.code != got.code || identityNeutral(py.stdout) != identityNeutral(got.stdout) {
		t.Fatalf("exit python=%d go=%d\npython:\n%s\ngo:\n%s", py.code, got.code, py.stdout, got.stdout)
	}
}

func decode(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, text)
	}
	return value
}

func TestDoctor_matches_python_on_a_python_created_store(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	// Given: a store as the real Python relay's absent-store initializer creates it.
	pythonCreates(t, state)

	// When: each implementation diagnoses it as its owner (the same file, restamped).
	py := python(t, home, "--state", state, "doctor")
	restamp(t, state, "go")
	got := golang(t, home, "--state", state, "doctor")

	// Then: the whole report is byte-identical once the documented runtime block is removed,
	// and that block is the report's last key.
	if !strings.HasSuffix(strings.TrimSpace(got.stdout), "}\n}") {
		t.Fatalf("runtime is not the last key:\n%s", got.stdout)
	}
	requireSame(t, py, run{got.code, asPythonReport(t, got.stdout, "python"), got.stderr})
	runtimeBlock := decode(t, got.stdout)["runtime"].(map[string]any)
	if runtimeBlock["language"] != "go" || runtimeBlock["version"] != cli.Version {
		t.Fatalf("runtime %v", runtimeBlock)
	}
}

// A store the answering runtime does not own is diagnosed the same way by both: identity from
// a disposable copy, no live open and no probe file beside it, and the fence's refusal in the
// access detail (store.py probe's check_start branch).
func TestDoctor_matches_python_on_a_store_the_other_runtime_owns(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	state := filepath.Join(home, "state")
	pythonCreates(t, state)
	// Neither run hands the store to its runtime first: each diagnoses the other's store.
	got := binaryRun(t, alias, "--state", state, "doctor")
	restamp(t, state, "go")
	py := fence(t, "--state", state, "doctor")
	requireSame(t, py, run{got.code, asPythonReport(t, got.stdout, "go"), got.stderr})
	access := decode(t, got.stdout)["access"].(map[string]any)
	if access["directoryWritable"] != false || access["dbWritable"] != false || access["dbReadable"] != true ||
		access["detail"] != "store_owned_by_other: the relay store belongs to another runtime" {
		t.Fatalf("access %v", access)
	}

	// A mirror without its database is refused the same way by both, before any probe file.
	if err := os.Remove(filepath.Join(state, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	py = fence(t, "--state", state, "doctor")
	got = binaryRun(t, alias, "--state", state, "doctor")
	requireSame(t, py, run{got.code, asPythonReport(t, got.stdout, "go"), got.stderr})
	if detail := decode(t, got.stdout)["access"].(map[string]any)["detail"].(string); !strings.HasSuffix(detail, "; store_owned_by_other: missing or unsupported writer protocol") {
		t.Fatalf("detail %q", detail)
	}
}

func TestDoctor_expect_inode_mismatch_refuses_with_python_reason(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	pythonCreates(t, state)
	py := python(t, home, "--state", state, "doctor", "--expect-inode", "1:2")
	restamp(t, state, "go")
	got := golang(t, home, "--state", state, "doctor", "--expect-inode", "1:2")
	if got.code != 2 {
		t.Fatalf("exit %d", got.code)
	}
	requireSame(t, py, run{got.code, asPythonReport(t, got.stdout, "python"), got.stderr})
	if detail := decode(t, got.stdout)["detail"].(string); !strings.Contains(detail, "is not 1:2") {
		t.Fatalf("detail %q", detail)
	}
}

// asPythonReport is a Go doctor report as the Python runtime would word the same observation:
// without Go's trailing runtime block, with the owner the Python run saw, and with the Python
// build as runtime_build, which names the answering runtime (decisions.md 31).
func asPythonReport(t *testing.T, stdout, pythonSawOwner string) string {
	t.Helper()
	report := withoutKey(t, stdout, "runtime")
	report = regexp.MustCompile(`\n    "owner": "[a-z]+",\n`).ReplaceAllString(report, "\n    \"owner\": \""+pythonSawOwner+"\",\n")
	return regexp.MustCompile(`"runtime_build": "[^"]*"`).ReplaceAllString(report, `"runtime_build": "`+ownership.CompatibilityBuild+`"`)
}

// A declared execution policy, read through the bridge's parser, from each source doctor
// reports: this process's environment, the service's launch-policy.json, both naming one
// file, a policy that declares no roles, and a worker-policy requirement. Every report is
// byte-identical to Python's once the documented runtime block is removed.
func TestDoctor_matches_python_with_a_declared_execution_policy(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	pythonCreates(t, state)
	policy := filepath.Join(home, "policy.json")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(policy, `{"allowed": [{"model": "gpt-5.5", "efforts": ["xhigh", "high"]}], "roles": {"supervisor": {"expectation": "record"}, "parent": {"model": "gpt-5.5", "reasoningEffort": "xhigh"}, "child": {"model": "gpt-5.5", "reasoningEffort": "high"}}}`)
	noRoles := filepath.Join(home, "noroles.json")
	write(noRoles, `{"allowed": [{"model": "m", "efforts": ["e"]}]}`)
	declaration := filepath.Join(state, "launch-policy.json")
	compare := func(t *testing.T, argv ...string) map[string]any {
		t.Helper()
		restamp(t, state, "python")
		py := python(t, home, append([]string{"--state", state, "doctor"}, argv...)...)
		restamp(t, state, "go")
		got := golang(t, home, append([]string{"--state", state, "doctor"}, argv...)...)
		requireSame(t, py, run{got.code, asPythonReport(t, got.stdout, "python"), got.stderr})
		return decode(t, py.stdout)
	}
	t.Run("environment", func(t *testing.T) {
		t.Setenv(policyEnv, policy)
		report := compare(t)
		role, launch := report["rolePolicy"].(map[string]any), report["launchPolicy"].(map[string]any)
		if role["state"] != "declared" || role["digest"] == nil || launch["source"] != "environment" || launch["state"] != "declared" || launch["digest"] != role["digest"] {
			t.Fatalf("rolePolicy %v launchPolicy %v", role, launch)
		}
	})
	t.Run("worker requirement", func(t *testing.T) {
		t.Setenv(policyEnv, policy)
		report := compare(t, "--require-worker-policy", `[{"role":"parent","model":"gpt-5.5","reasoningEffort":"xhigh"}]`)
		if report["workerReadiness"].(map[string]any)["ready"] != false {
			t.Fatalf("no worker runs here: %v", report["workerReadiness"])
		}
	})
	t.Run("no roles", func(t *testing.T) {
		t.Setenv(policyEnv, noRoles)
		if role := compare(t)["rolePolicy"].(map[string]any); role["state"] != "unresolved" {
			t.Fatalf("rolePolicy %v", role)
		}
	})
	t.Run("declared launch-policy.json", func(t *testing.T) {
		write(declaration, `{"schemaVersion": 1, "path": "`+policy+`", "declaredAt": "2026-09-26T00:00:00Z", "declaredBy": "cli"}`)
		defer os.Remove(declaration)
		launch := compare(t)["launchPolicy"].(map[string]any)
		if launch["source"] != "record" || launch["state"] != "declared" || launch["persisted"] != true || launch["digest"] == nil {
			t.Fatalf("launchPolicy %v", launch)
		}
	})
	t.Run("declaration and environment name one file", func(t *testing.T) {
		write(declaration, `{"schemaVersion": 1, "path": "`+policy+`", "declaredAt": "2026-09-26T00:00:00Z", "declaredBy": "cli"}`)
		defer os.Remove(declaration)
		t.Setenv(policyEnv, filepath.Join(home, ".", "policy.json"))
		if role := compare(t)["rolePolicy"].(map[string]any); role["state"] != "declared" {
			t.Fatalf("rolePolicy %v", role)
		}
	})
}

const policyEnv = "CODEX_THREAD_BRIDGE_EXECUTION_POLICY"

// --kind-module names a Python module, which this build cannot import: the answer is the one
// Python gives for a module it cannot import, including the empty and relative spellings.
func TestKindModule_matches_python_for_an_unimportable_module(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	for _, argv := range [][]string{
		{"--kind-module", "nosuch.mod", "status"},
		{"--kind-module", "a", "--kind-module", "b.c", "doctor"},
		{"--kind-module", "", "status"},
		{"--kind-module", "..x", "store-identity"},
		// Parsing comes first: an unknown command is argparse's exit 2 before any import. The
		// choices listed differ only by the commands this build does not register yet.
		{"--kind-module", "nosuch", "bogus"},
	} {
		argv := append([]string{"--state", state}, argv...)
		py, got := python(t, home, argv...), golang(t, home, argv...)
		pyErr, goErr := lastLine(py.stderr), lastLine(got.stderr)
		if strings.Contains(pyErr, "invalid choice") {
			pyErr, goErr = pyErr[:strings.Index(pyErr, "(choose")], goErr[:max(0, strings.Index(goErr, "(choose"))]
		}
		if py.code != got.code || py.stdout != got.stdout || pyErr != goErr {
			t.Fatalf("%v: python %d %q %q\ngo %d %q %q", argv, py.code, py.stdout, lastLine(py.stderr), got.code, got.stdout, lastLine(got.stderr))
		}
	}
}

func TestDelivery_kind_module_refusal_matches_python_before_ack_proof(t *testing.T) {
	home := pythonHome(t)
	args := []string{"--kind-module", "does_not_exist", "ack-proof", "--event", "0123456789abcdef0123456789abcdef", "--turn", "turn-1"}
	for _, argv := range [][]string{args, args[2:]} {
		py, got := python(t, home, argv...), golang(t, home, argv...)
		if py.code != got.code || py.stdout != got.stdout || py.stderr != got.stderr {
			t.Fatalf("%v: python %+v; go %+v", argv, py, got)
		}
	}
}

func TestRegistry_kind_module_refusal_matches_python_before_register(t *testing.T) {
	home := pythonHome(t)
	state := filepath.Join(home, "state")
	args := []string{"--state", state, "--kind-module", "does_not_exist", "register",
		"--parent-task", "p", "--parent-host", "p", "--child-task", "c", "--child-host", "c",
		"--issue", "I-1", "--artifact-root", home, "--allowed-recipient", "p",
		"--dispatch-request-id", "req"}
	py, got := python(t, home, args...), golang(t, home, args...)
	if py.code != got.code || py.stdout != got.stdout || py.stderr != got.stderr {
		t.Fatalf("python %+v; go %+v", py, got)
	}
	if py.code != 4 || strings.Contains(py.stdout, "relationshipId") {
		t.Fatalf("unexpected registration: %+v", py)
	}
	// Without the invalid global option the same handler still behaves as Python's; the clock
	// reading each run registered at is its own.
	valid := append(append([]string{}, args[:2]...), args[4:]...)
	py, got = python(t, home, valid...), golang(t, home, valid...)
	py.stdout, got.stdout = clockReading.ReplaceAllString(py.stdout, `"<at>"`), clockReading.ReplaceAllString(got.stdout, `"<at>"`)
	if py.code != got.code || py.stdout != got.stdout || py.stderr != got.stderr {
		t.Fatalf("valid: python %+v; go %+v", py, got)
	}
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}

// pythonSnippet runs `python -c script args...` in the relay's uv environment. Only a capture
// closure calls it (see oracleRun).
func pythonSnippet(dir, script string, args ...string) (string, error) {
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..", "..")
	command := exec.Command("uv", append([]string{"run", "--no-sync", "--project", repo, "python", "-c", script}, args...)...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	return string(out), err
}
