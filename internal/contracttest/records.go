package contracttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
)

// statusDocument is the `status` run with `document: true`: the settings document the
// installer writes, built by the Go install's own builder, printed as the scenario's stdout.
func statusDocument(s Scenario) (map[string]any, error) {
	home, err := os.MkdirTemp("", "crw-hc-doc-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(home) }()
	text := func(key, fallback string) string {
		if value, ok := s.Run[key].(string); ok {
			return value
		}
		return fallback
	}
	settings := install.HookSettings{
		Relay: text("relay", "/opt/relay"), MarkerRoot: text("marker_root", "/markers"), Socket: text("socket", ""),
		Mode: "observe", Timeout: 5, CodexHome: home, Destination: filepath.Join(home, "runtime"),
	}
	document, err := settings.Document(nil)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := contract.Emit(&out, document); err != nil {
		return nil, err
	}
	var parsed any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return nil, err
	}
	return map[string]any{"exit": float64(0), "stdout": out.String(), "stderr": "", "stdout_json": parsed, "status": map[string]any{}, "files": map[string]any{}, "observed": map[string]any{}}, nil
}

// agreementVolatile are the journal record fields that differ between two runs of the same
// Stop for reasons other than the answer: where the settings were read from, which journal
// slot the row took, and the clocks. contract/runner/files.py:agreement excludes exactly these.
var agreementVolatile = []string{"configuration", "journalledAs", "at", "elapsedMs", "guardElapsedMs", "identityScanMs"}

// runAgreement drives `crw hook` once under the checkout settings document (no owner, the
// Python-era checkout hook's shape) and once under the plugin-owned document (the packaged
// adapter's shape), each in its own home with its own guard peer answering what given.relay
// says, and requires both to return the same answer and journal the same record apart from
// agreementVolatile. It observes what contract/runner/files.py:agreement returns: the answer
// printed (null when nothing was), the checkout run's records and both guard calls.
func runAgreement(t *testing.T, s Scenario) (map[string]any, error) {
	type side struct {
		label    string
		kind     RunKind
		returned any
		records  []any
		call     any
	}
	sides := []*side{{label: "checkout", kind: "hook"}, {label: "packaged", kind: "stop"}}
	for _, one := range sides {
		run := map[string]any{"kind": string(one.kind)}
		if stdin, present := s.Run["stdin"]; present {
			run["stdin"] = stdin
		}
		actual, err := runHook(t, Scenario{ID: s.ID + "/" + one.label, Domain: s.Domain, Path: s.Path, Kind: one.kind, Given: s.Given, Run: run})
		if err != nil {
			return nil, fmt.Errorf("%s adapter: %w", one.label, err)
		}
		if exit := number(actual["exit"]); exit != 0 {
			return nil, fmt.Errorf("%s adapter exited %v: %v", one.label, exit, actual["stderr"])
		}
		if stdout, _ := actual["stdout"].(string); stdout != "" {
			one.returned = stdout
		}
		one.records, _ = actual["rows"].([]any)
		one.call = actual["call"]
	}
	left, right := sides[0], sides[1]
	if !reflect.DeepEqual(left.returned, right.returned) {
		return nil, fmt.Errorf("checkout and packaged adapters disagree: returned %q and %q", left.returned, right.returned)
	}
	if l, r := withoutVolatile(left.records), withoutVolatile(right.records); !reflect.DeepEqual(l, r) {
		lj, _ := json.MarshalIndent(l, "", " ")
		rj, _ := json.MarshalIndent(r, "", " ")
		return nil, fmt.Errorf("checkout and packaged adapters disagree on the records\ncheckout %s\npackaged %s", lj, rj)
	}
	return map[string]any{"exit": float64(0), "returned": left.returned, "records": left.records,
		"calls": map[string]any{"checkout": left.call, "packaged": right.call}}, nil
}

func withoutVolatile(records []any) []any {
	out := make([]any, len(records))
	for i, record := range records {
		row, ok := record.(map[string]any)
		if !ok {
			out[i] = record
			continue
		}
		kept := maps.Clone(row)
		for _, key := range agreementVolatile {
			delete(kept, key)
		}
		out[i] = kept
	}
	return out
}

// The release workflow's step grammar, as scripts/ci/tests/release_steps.py reads it.
var (
	releaseJob      = regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*):\n`)
	releaseStep     = regexp.MustCompile(`(?m)^      - id: ([a-z][a-z0-9-]+)\n`)
	releaseRun      = regexp.MustCompile(`(?m)^        run: \|\n((?:          .*\n|\n)*)`)
	releaseNextItem = regexp.MustCompile(`(?m)^      - `)
	releaseIndent   = regexp.MustCompile(`(?m)^          `)
)

// releaseStepScript is release_steps.step_block(job, step)["script"]: the one literal shell
// block of the named step of the named job, dedented.
func releaseStepScript(workflow, job, step string) (string, error) {
	jobs := releaseJob.FindAllStringSubmatchIndex(workflow, -1)
	var body string
	found := 0
	for i, match := range jobs {
		if workflow[match[2]:match[3]] != job {
			continue
		}
		found++
		end := len(workflow)
		if i+1 < len(jobs) {
			end = jobs[i+1][0]
		}
		body = workflow[match[1]:end]
	}
	if found != 1 {
		return "", fmt.Errorf("expected one %s job, found %d", job, found)
	}
	steps := releaseStep.FindAllStringSubmatchIndex(body, -1)
	start, end, found := 0, len(body), 0
	for i, match := range steps {
		if body[match[2]:match[3]] != step {
			continue
		}
		found++
		start = match[1]
		end = len(body)
		if i+1 < len(steps) {
			end = steps[i+1][0]
		}
	}
	if found != 1 {
		return "", fmt.Errorf("expected one %s step, found %d", step, found)
	}
	// A following step may omit an id: stop at the next step list item too.
	if next := releaseNextItem.FindStringIndex(body[start:]); next != nil && start+next[0] < end {
		end = start + next[0]
	}
	section := body[start:end]
	run := releaseRun.FindStringSubmatchIndex(section)
	if run == nil || strings.TrimSpace(section[run[1]:]) != "" {
		return "", fmt.Errorf("expected one literal shell block on %s", step)
	}
	return releaseIndent.ReplaceAllString(section[run[2]:run[3]], ""), nil
}

// runRelease is contract/runner/release.py: the named workflow step's shell block run by bash in
// a fresh clone of a real bare remote, with testdata/release's fake gh and git first on PATH.
// given.{ci,tag,release,push} script the fakes' state, one file per key (the fakes read it with
// cat, not a JSON parser); calls are the gh invocations and remote_main the remote's main after.
func runRelease(t *testing.T, s Scenario) (map[string]any, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	job, _ := s.Run["job"].(string)
	step, _ := s.Run["step"].(string)
	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		return nil, err
	}
	script, err := releaseStepScript(string(workflow), job, step)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFixture, err)
	}
	gitReal, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	dir := t.TempDir()
	remote, checkout, bin, state := filepath.Join(dir, "remote.git"), filepath.Join(dir, "checkout"), filepath.Join(dir, "bin"), filepath.Join(dir, "state")
	defaults := map[string]map[string]any{"ci": {"case": "success"}, "tag": {"case": "missing"}, "release": {"case": "missing"}, "push": {"fail_main": false, "lie_main": false}}
	for name, values := range defaults {
		given, _ := s.Given[name].(map[string]any)
		merged := maps.Clone(values)
		maps.Copy(merged, given)
		if err := writeReleaseState(filepath.Join(state, name), merged); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"gh", "git"} {
		raw, err := os.ReadFile(filepath.Join(root, "internal", "contracttest", "testdata", "release", "fake_"+name+".sh"))
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(bin, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(bin, name), raw, 0o755); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"gh.log", "summary"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			return nil, err
		}
	}
	env := map[string]string{"HOME": dir, "XDG_CONFIG_HOME": dir,
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.hooksPath", "GIT_CONFIG_VALUE_0": "/dev/null",
		"GIT_AUTHOR_NAME": "Release Fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
		"GIT_COMMITTER_NAME": "Release Fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid",
		"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH"), "GH_LOG": filepath.Join(dir, "gh.log"), "GH_STATE": state,
		"GITHUB_STEP_SUMMARY": filepath.Join(dir, "summary"), "GITHUB_REPOSITORY": "fixture/repository", "GITHUB_REF": "refs/heads/dev",
		"ACTOR": "owner", "TRIGGERING_ACTOR": "owner", "OWNER": "owner",
		"RELEASE_TAG": "v0.1.0", "RELEASE_NOTES": "Fixture notes", "RELEASE_TOKEN": "fixture-only", "GH_TOKEN": "fixture-read",
		"GIT_REAL": gitReal}
	environ := func(extra map[string]any) []string {
		all := map[string]string{}
		for _, kv := range os.Environ() {
			if key, value, ok := strings.Cut(kv, "="); ok {
				all[key] = value
			}
		}
		maps.Copy(all, env)
		for key, value := range extra {
			all[key] = fmt.Sprint(value)
		}
		out := make([]string, 0, len(all))
		for key, value := range all {
			out = append(out, key+"="+value)
		}
		return out
	}
	command := func(cwd string, args ...string) (string, error) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir, cmd.Env = cwd, environ(nil)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("%v: %w: %s", args, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	for _, args := range [][]string{
		{"git", "init", "--bare", "--initial-branch=main", remote}, {"git", "clone", remote, checkout},
	} {
		if _, err := command(dir, args...); err != nil {
			return nil, err
		}
	}
	for _, args := range [][]string{
		{"git", "commit", "--allow-empty", "-m", "base"}, {"git", "push", "origin", "main"},
		{"git", "switch", "-c", "dev"}, {"git", "commit", "--allow-empty", "-m", "candidate"},
	} {
		if _, err := command(checkout, args...); err != nil {
			return nil, err
		}
	}
	sha, err := command(checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if _, err := command(checkout, "git", "push", "origin", "dev"); err != nil {
		return nil, err
	}
	env["RELEASE_SHA"] = sha
	extra, _ := s.Run["env"].(map[string]any)
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir, cmd.Env = checkout, environ(extra)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	exit := 0
	if err := cmd.Run(); err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			return nil, err
		}
		exit = exited.ExitCode()
	}
	log, err := os.ReadFile(filepath.Join(dir, "gh.log"))
	if err != nil {
		return nil, err
	}
	calls := []any{}
	for _, line := range strings.Split(strings.TrimSuffix(string(log), "\n"), "\n") {
		if line != "" {
			calls = append(calls, line)
		}
	}
	main, err := command(dir, "git", "--git-dir", remote, "rev-parse", "main")
	if err != nil {
		return nil, err
	}
	return map[string]any{"exit": float64(exit), "stdout": stdout.String(), "stderr": stderr.String(), "calls": calls, "remote_main": main}, nil
}

// writeReleaseState writes one fake's state as <dir>/<key>, holding what the Python fakes'
// state_get printed for the key: true/false for booleans, integers without a fraction.
func writeReleaseState(dir string, values map[string]any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for key, value := range values {
		var text string
		switch v := value.(type) {
		case nil:
			text = "None"
		case bool:
			text = strconv.FormatBool(v)
		case float64:
			text = strconv.FormatFloat(v, 'f', -1, 64)
		case string:
			text = v
		default:
			return fmt.Errorf("%w: release state %s is %T", ErrFixture, key, value)
		}
		if err := os.WriteFile(filepath.Join(dir, key), []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// nativeDivergence is a corpus scenario whose expectation names what only the Python-era process
// adapter could answer: a guard run as a relay subprocess, with an exit status, a signal and an
// exec error of its own. The Go hook asks the owner over its control socket instead (docs/port
// decisions 22 and 32), so these few checks are held to the native answer. The scenario still runs
// in full and every other check stands; the fixture is left as the Python corpus run reads it.
type nativeDivergence struct {
	why    string
	checks []divergentCheck
}

// divergentCheck replaces the fixture's eq check at path, which must still say python, with native.
type divergentCheck struct {
	path   []any
	python any
	native Check
}

func nativeEq(value any) Check { return Check{Kind: "eq", Value: value} }

var nativeDivergences = func() map[string]nativeDivergence {
	row := func(field string) []any { return []any{"rows", float64(0), field} }
	const stem = "test_adapter_agreement__"
	out := map[string]nativeDivergence{
		stem + "test_a_signalled_runtime_is_signalled_in_both": {
			why: "no guard process exists for a signal to end: the guard runs inside the owner, and an owner that dies " +
				"mid-request leaves the connection unanswered, which both settings documents record as guard_said_nothing",
			checks: []divergentCheck{{path: []any{"records", float64(0), "adapterOutcome"}, python: "guard_signalled", native: nativeEq("guard_said_nothing")}},
		},
	}
	for _, document := range []string{"checkout", "packaged"} {
		out[stem+"test_a_runtime_that_cannot_be_run_is_unreachable_in_both__"+document] = nativeDivergence{
			why: "the guard is the owner's control socket, not relayExecutable: a connect that fails is journalled before " +
				"any transcript scan or claim (decision 22), so acceptance and eventIdentity stay unobserved and the detail names the socket",
			checks: []divergentCheck{
				{path: row("acceptance"), python: "unestablished", native: nativeEq(nil)},
				{path: row("eventIdentity"), python: map[string]any{"answerItem": nil, "established": false, "reason": "identity_fields_incomplete", "scannedBytes": float64(0), "scannedLines": float64(0), "transcriptPath": nil}, native: nativeEq(nil)},
				{path: row("detail"), python: "the configured runtime could not be run: [Errno 2] No such file or directory: '/nonexistent/crw-contract-absent/relay'",
					native: Check{Kind: "regex", Value: `^the configured runtime could not be run: \[Errno 2\] No such file or directory: '/.+/control\.sock'$`}},
			},
		}
		out[stem+"test_an_error_record_at_each_exit_code_is_its_own_outcome_in_both__exit_7__"+document] = nativeDivergence{
			why: "the owner answers over a socket and has no exit status: an error record of no known kind is " +
				"guard_ended_unexpectedly with exitCode 0",
			checks: []divergentCheck{{path: row("exitCode"), python: float64(7), native: nativeEq(float64(0))}},
		}
		out[stem+"test_silence_at_two_and_at_zero_are_different_outcomes_in_both__exit_9__"+document] = nativeDivergence{
			why: "an owner that answers nothing has no exit status to tell a crash from silence: it is guard_said_nothing " +
				"with exitCode 0, as at exit 0 (the rejected call, exit 2 with silence, stays its own outcome)",
			checks: []divergentCheck{
				{path: row("adapterOutcome"), python: "guard_ended_unexpectedly", native: nativeEq("guard_said_nothing")},
				{path: row("exitCode"), python: float64(9), native: nativeEq(float64(0))},
			},
		}
	}
	return out
}()

// withNativeExpectations returns the scenario with its native divergence applied. Each replaced
// check must still say what the Python runtime answered, so a declaration cannot outlive the
// fixture it describes.
func withNativeExpectations(t *testing.T, scenario Scenario) Scenario {
	t.Helper()
	divergence, declared := nativeDivergences[scenario.ID]
	if !declared {
		return scenario
	}
	checks := slices.Clone(scenario.Expect.Checks)
	for _, replacement := range divergence.checks {
		replaced := 0
		for i, check := range checks {
			if check.Kind == "eq" && reflect.DeepEqual(check.Path, replacement.path) && reflect.DeepEqual(check.Value, replacement.python) {
				checks[i].Kind, checks[i].Value = replacement.native.Kind, replacement.native.Value
				replaced++
			}
		}
		if replaced != 1 {
			t.Fatalf("%s: a native divergence declares %v == %#v, which the fixture no longer checks once", scenario.ID, replacement.path, replacement.python)
		}
	}
	t.Logf("native divergence (%d checks): %s", len(divergence.checks), divergence.why)
	scenario.Expect.Checks = checks
	return scenario
}
