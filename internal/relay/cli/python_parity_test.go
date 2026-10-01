package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// fixedHome isolates HOME, XDG_* and CODEX_HOME in one directory of the test's own, so the
// relay cannot reach the live relay state or ~/.codex. The directory is a fixedTree: an answer
// given there carries digests of paths under it (a socket's scope key, an artifact's revision
// and event id), which only the same path gives again.
func fixedHome(t *testing.T) string {
	t.Helper()
	return isolateHome(t, fixedTree(t, goldenKey(t, "home "+t.Name())))
}

// isolateHome is fixedHome at a home the caller made.
func isolateHome(t *testing.T, home string) string {
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

// answerKey is the golden key of the answer to argv (the label the recorded answer was filed
// under).
func answerKey(t *testing.T, argv []string) string {
	t.Helper()
	return goldenKey(t, "python "+keyLabel(argv...))
}

// expectSame checks an answer to argv run in dir ("" for this process's directory) - its exit and
// its stdout without the store file's own identity - against the golden under key.
func expectSame(t *testing.T, key, dir string, argv []string, got run) {
	t.Helper()
	expectOver(t, key, dir, argv, map[string]any{"code": got.code, "stdout": identityNeutral(got.stdout)})
}

// expectOver checks value, what a test compares of an answer to argv run in dir ("" for this
// process's directory), against the golden under key: the run's directories and identities are
// placeholders, and so is the id of the store argv selects.
func expectOver(t *testing.T, key, dir string, argv []string, value any) {
	t.Helper()
	anchors := argv
	if dir != "" {
		anchors = append([]string{dir}, argv...)
	}
	options := goldenOptions(t, anchors...)
	if id := selectedStoreID(t, dir, argv); id != "" {
		options = append(options, golden.Substitute(id, "<store-id>"))
	}
	inPackageDirectory(t, func() { golden.CheckJSON(t, key, value, options...) })
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
	home := tempHome(t)
	state := filepath.Join(home, "state")
	// Given: a store as the real Python relay's absent-store initializer creates it.
	pythonCreates(t, state)

	// When: the Go build diagnoses it as its owner (restamped).
	argv := []string{"--state", state, "doctor"}
	restamp(t, state, "go")
	got := golang(t, home, argv...)

	// Then: the whole report is byte-identical once the documented runtime block is removed,
	// and that block is the report's last key.
	if !strings.HasSuffix(strings.TrimSpace(got.stdout), "}\n}") {
		t.Fatalf("runtime is not the last key:\n%s", got.stdout)
	}
	expectSame(t, answerKey(t, argv), home, argv, run{got.code, asPythonReport(t, got.stdout, "python"), ""})
	runtimeBlock := decode(t, got.stdout)["runtime"].(map[string]any)
	if runtimeBlock["language"] != "go" || runtimeBlock["version"] != cli.Version {
		t.Fatalf("runtime %v", runtimeBlock)
	}
}

// A store the answering runtime does not own is diagnosed the same way by both: identity from
// a disposable copy, no live open and no probe file beside it, and the fence's refusal in the
// access detail (store.py probe's check_start branch).
func TestDoctor_matches_python_on_a_store_the_other_runtime_owns(t *testing.T) {
	home := tempHome(t)
	_, alias := packageBinary(t)
	state := filepath.Join(home, "state")
	pythonCreates(t, state)
	// The store is not handed to Go first: Go diagnoses the fence's store, and its golden is the
	// fence's diagnosis of the same store in Go's hands (asPythonReport words it so), which the
	// restamp below leaves for the mirror without its database.
	argv := []string{"--state", state, "doctor"}
	got := binaryRun(t, alias, argv...)
	restamp(t, state, "go")
	expectSame(t, goldenKey(t, "fence "+keyLabel(argv...)), "", argv, run{got.code, asPythonReport(t, got.stdout, "go"), ""})
	access := decode(t, got.stdout)["access"].(map[string]any)
	if access["directoryWritable"] != false || access["dbWritable"] != false || access["dbReadable"] != true ||
		access["detail"] != "store_owned_by_other: the relay store belongs to another runtime" {
		t.Fatalf("access %v", access)
	}

	// A mirror without its database is refused the same way by both, before any probe file.
	if err := os.Remove(filepath.Join(state, "relay.sqlite3")); err != nil {
		t.Fatal(err)
	}
	got = binaryRun(t, alias, argv...)
	expectSame(t, goldenKey(t, "fence "+keyLabel(argv...)), "", argv, run{got.code, asPythonReport(t, got.stdout, "go"), ""})
	if detail := decode(t, got.stdout)["access"].(map[string]any)["detail"].(string); !strings.HasSuffix(detail, "; store_owned_by_other: missing or unsupported writer protocol") {
		t.Fatalf("detail %q", detail)
	}
}

func TestDoctor_expect_inode_mismatch_refuses_with_python_reason(t *testing.T) {
	home := tempHome(t)
	state := filepath.Join(home, "state")
	pythonCreates(t, state)
	argv := []string{"--state", state, "doctor", "--expect-inode", "1:2"}
	restamp(t, state, "go")
	got := golang(t, home, argv...)
	if got.code != 2 {
		t.Fatalf("exit %d", got.code)
	}
	expectSame(t, answerKey(t, argv), home, argv, run{got.code, asPythonReport(t, got.stdout, "python"), ""})
	if detail := decode(t, got.stdout)["detail"].(string); !strings.Contains(detail, "is not 1:2") {
		t.Fatalf("detail %q", detail)
	}
}

// asPythonReport is a Go doctor report as the Python runtime would word the same observation,
// which the goldens hold: without Go's trailing runtime block, with the owner the Python run saw,
// and with the Python build as runtime_build, which names the answering runtime (decisions.md
// 31).
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
	home := tempHome(t)
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
		argv = append([]string{"--state", state, "doctor"}, argv...)
		restamp(t, state, "go")
		got := golang(t, home, argv...)
		expectSame(t, answerKey(t, argv), home, argv, run{got.code, asPythonReport(t, got.stdout, "python"), ""})
		return decode(t, got.stdout)
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
	home := tempHome(t)
	state := filepath.Join(home, "state")
	for _, argv := range [][]string{
		{"--kind-module", "nosuch.mod", "status"},
		{"--kind-module", "a", "--kind-module", "b.c", "doctor"},
		{"--kind-module", "", "status"},
		{"--kind-module", "..x", "store-identity"},
		// Parsing comes first: an unknown command is the parser's exit 2 before any import.
		{"--kind-module", "nosuch", "bogus"},
	} {
		argv := append([]string{"--state", state}, argv...)
		key := answerKey(t, argv)
		got := golang(t, home, argv...)
		expectJSON(t, key, map[string]any{"code": got.code, "stdout": got.stdout, "stderr": lastLine(got.stderr)}, append([]string{home}, argv...)...)
	}
}

func TestDelivery_kind_module_refusal_matches_python_before_ack_proof(t *testing.T) {
	home := tempHome(t)
	args := []string{"--kind-module", "does_not_exist", "ack-proof", "--event", "0123456789abcdef0123456789abcdef", "--turn", "turn-1"}
	for _, argv := range [][]string{args, args[2:]} {
		key := answerKey(t, argv)
		got := golang(t, home, argv...)
		expectRunErr(t, key, got.code, got.stdout, got.stderr, append([]string{home}, argv...)...)
	}
}

func TestRegistry_kind_module_refusal_matches_python_before_register(t *testing.T) {
	home := tempHome(t)
	state := filepath.Join(home, "state")
	args := []string{"--state", state, "--kind-module", "does_not_exist", "register",
		"--parent-task", "p", "--parent-host", "p", "--child-task", "c", "--child-host", "c",
		"--issue", "I-1", "--artifact-root", home, "--allowed-recipient", "p",
		"--dispatch-request-id", "req"}
	key := answerKey(t, args)
	got := golang(t, home, args...)
	expectRunErr(t, key, got.code, got.stdout, got.stderr, append([]string{home}, args...)...)
	if got.code != 4 || strings.Contains(got.stdout, "relationshipId") {
		t.Fatalf("unexpected registration: %+v", got)
	}
	// Without the invalid global option the same handler still behaves as it did; the clock
	// reading each run registered at is its own.
	valid := append(append([]string{}, args[:2]...), args[4:]...)
	key = answerKey(t, valid)
	got = golang(t, home, valid...)
	got.stdout = clockReading.ReplaceAllString(got.stdout, `"<at>"`)
	expectRunErr(t, key, got.code, got.stdout, got.stderr, append([]string{home}, valid...)...)
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	return lines[len(lines)-1]
}
