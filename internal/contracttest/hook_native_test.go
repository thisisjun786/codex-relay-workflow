package contracttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test33NativeCrashAndRejectionPython(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, ".venv/bin/python")
	scenarios, err := Load("hook")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"test_stop_adapter__test_a_claim_whose_owner_died_is_not_answered_twice", "test_completion_hook__test_a_refused_request_and_a_rejected_call_are_different_answers__rejected", "test_completion_hook__test_a_refused_request_and_a_rejected_call_are_different_answers__refused"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			var scenario Scenario
			for _, candidate := range scenarios {
				if candidate.ID == name {
					scenario = candidate
					break
				}
			}
			if scenario.ID == "" {
				t.Fatal("fixture missing")
			}
			actual, err := runHook(t, scenario)
			if err != nil {
				t.Fatal(err)
			}
			if err = Assert(scenario, actual); err != nil {
				t.Fatal(err)
			}
			// The Python adapter's answer to the same fixture, normalized as below (recorded,
			// internal/testsupport/pyoracle).
			var expected map[string]any
			pyoracle.JSON(t, "python", &expected, func() (any, error) {
				home := t.TempDir()
				cmd := exec.Command(python, filepath.Join(root, "internal/contracttest/testdata/hook_native_python.py"), scenario.Path, home, root)
				out, err := cmd.CombinedOutput()
				if err != nil {
					return nil, fmt.Errorf("python fixture %w\n%s", err, out)
				}
				var live map[string]any
				if err = json.Unmarshal(out, &live); err != nil {
					return nil, fmt.Errorf("%w %s", err, out)
				}
				normalized := map[string]any{"calls": float64(len(live["calls"].([]any)))}
				for _, field := range hookParityFields {
					normalized[field] = normalizeHookParity(live[field], home, "")
				}
				return normalized, nil
			}, pyoracle.Substitute(root, "<ROOT>"), pyoracle.Substitute(stopTranscript, "<STOP_TRANSCRIPT>"), pyoracle.Substitute(stopCWD, "<STOP_CWD>"))
			// Compare every field of rows/claim bodies. Only process/time identities and
			// temporary roots are normalized; outputs and fixed diagnostics are untouched.
			for _, field := range hookParityFields {
				got := jsonShaped(t, normalizeHookParity(actual[field], actual["home"].(string), ""))
				want := expected[field]
				if !reflect.DeepEqual(got, want) {
					g, _ := json.MarshalIndent(got, "", "  ")
					w, _ := json.MarshalIndent(want, "", "  ")
					t.Fatalf("%s mismatch\nGo %s\nPython %s", field, g, w)
				}
			}
			if calls, _ := expected["calls"].(json.Number); calls.String() != fmt.Sprint(len(actual["calls"].([]any))) {
				t.Fatal("guard invocation count differs")
			}
			t.Logf("Python/native %s: stdout, exit, signal, full journal rows, accepted/outcome files and host claims equal", name)
		})
	}
}

// stopTranscript and stopCWD are the default Stop payload's fixed paths (contract/runner/core.py
// STOP); a recording names them by placeholder so it carries no absolute temporary path.
const (
	stopTranscript = "/tmp/transcript.jsonl"
	stopCWD        = "/tmp/workspace"
)

// hookParityFields are the observation fields compared with the Python adapter's.
var hookParityFields = []string{"outcomes", "rows", "claims", "ledger_outcomes", "host_claims", "acceptances"}

// jsonShaped round-trips value through JSON with numbers kept as json.Number, the shape a
// recorded answer decodes to.
func jsonShaped(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var out any
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

var paritySlot = regexp.MustCompile(`^[0-9]{8}/[0-9a-f]{32}\.json$`)

func normalizeHookParity(v any, home, key string) any {
	switch value := v.(type) {
	case string:
		if key == "attemptRow" && paritySlot.MatchString(value) {
			return "<ROW>"
		}
		return strings.ReplaceAll(value, home, "<HOME>")
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = normalizeHookParity(item, home, "")
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range value {
			switch k {
			case "pid", "at", "claimedAt", "elapsedMs", "guardElapsedMs", "identityScanMs":
				continue
			}
			out[k] = normalizeHookParity(item, home, k)
		}
		return out
	default:
		return v
	}
}

func Test33NativeCrashLeavesClaimBytes(t *testing.T) {
	scenarios, err := Load("hook")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scenarios {
		if s.ID != "test_stop_adapter__test_a_claim_whose_owner_died_is_not_answered_twice" {
			continue
		}
		actual, err := runHook(t, s)
		if err != nil {
			t.Fatal(err)
		}
		if err = Assert(s, actual); err != nil {
			t.Fatal(err)
		}
		home := actual["home"].(string)
		for _, pattern := range []string{"journal/accepted/*.json", "crw-completion-hook/stop-events/*.json"} {
			paths, err := filepath.Glob(filepath.Join(home, pattern))
			if err != nil || len(paths) != 1 {
				t.Fatal(paths, err)
			}
			info, err := os.Stat(paths[0])
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal(info, err)
			}
		}
		return
	}
	t.Fatal("fixture missing")
}
