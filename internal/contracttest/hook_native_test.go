package contracttest

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
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
			home := t.TempDir()
			cmd := exec.Command(python, filepath.Join(root, "internal/contracttest/testdata/hook_native_python.py"), scenario.Path, home, root)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Python fixture %v\n%s", err, out)
			}
			var expected map[string]any
			if err = json.Unmarshal(out, &expected); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			// Compare every field of rows/claim bodies. Only process/time identities and
			// temporary roots are normalized; outputs and fixed diagnostics are untouched.
			for _, field := range []string{"outcomes", "rows", "claims", "ledger_outcomes", "host_claims", "acceptances"} {
				got := normalizeHookParity(actual[field], actual["home"].(string), "")
				want := normalizeHookParity(expected[field], home, "")
				if !reflect.DeepEqual(got, want) {
					g, _ := json.MarshalIndent(got, "", "  ")
					w, _ := json.MarshalIndent(want, "", "  ")
					t.Fatalf("%s mismatch\nGo %s\nPython %s", field, g, w)
				}
			}
			if len(actual["calls"].([]any)) != len(expected["calls"].([]any)) {
				t.Fatal("guard invocation count differs")
			}
			t.Logf("Python/native %s: stdout, exit, signal, full journal rows, accepted/outcome files and host claims equal", name)
		})
	}
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
