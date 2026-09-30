package contracttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
