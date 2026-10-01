package contracttest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func Test33NativeCrashAndRejectionPython(t *testing.T) {
	root, err := Root()
	if err != nil {
		t.Fatal(err)
	}
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
			// Every field of rows/claim bodies is held to the golden (first taken as the Python
			// adapter's answer to the same fixture). Only process/time identities and temporary
			// roots are normalized; outputs and fixed diagnostics are untouched.
			observed := map[string]any{"calls": len(actual["calls"].([]any))}
			for _, field := range hookParityFields {
				observed[field] = normalizeHookParity(actual[field], actual["home"].(string), "")
			}
			golden.CheckJSON(t, "observed", observed, golden.Substitute(root, "<ROOT>"), golden.Substitute(stopTranscript, "<STOP_TRANSCRIPT>"), golden.Substitute(stopCWD, "<STOP_CWD>"))
		})
	}
}

// stopTranscript and stopCWD are the default Stop payload's fixed paths (contract/runner/core.py
// STOP); a golden names them by placeholder so it carries no absolute temporary path.
const (
	stopTranscript = "/tmp/transcript.jsonl"
	stopCWD        = "/tmp/workspace"
)

// hookParityFields are the observation fields held to the golden.
var hookParityFields = []string{"outcomes", "rows", "claims", "ledger_outcomes", "host_claims", "acceptances"}

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
