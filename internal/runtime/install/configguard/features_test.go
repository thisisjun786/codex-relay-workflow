package configguard

import (
	"reflect"
	"slices"
	"testing"
)

// Pinned CXC v0.2.40 config-guard/test/features.test.ts:18-26.
const realFeaturesList = "default_mode_request_user_input  under-development  false\n" +
	"goals                            stable             true\n" +
	"hooks                            stable             true\n" +
	"multi_agent                      stable             true\n" +
	"multi_agent_v2                   under-development  false\n" +
	"plugin_hooks                     removed            false\n" +
	"web_search                       stable             true"

func TestDeclaredFeatures(t *testing.T) {
	want := []DeclaredFeature{"multi_agent", "goals", "hooks", "default_mode_request_user_input"}
	if got := DeclaredFeatures(); !slices.Equal(got, want) {
		t.Fatalf("declared = %v, want %v", got, want)
	}
}

func TestParseFeaturesListRealTable(t *testing.T) {
	want := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": false}
	if got := ParseFeaturesList(realFeaturesList); !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %v, want %v", got, want)
	}
}

func TestParseFeaturesListSiblingKeys(t *testing.T) {
	got := ParseFeaturesList(realFeaturesList)
	if !got["multi_agent"] || !got["hooks"] {
		t.Fatalf("sibling keys clobbered declared values: %v", got)
	}
	for _, key := range []string{"multi_agent_v2", "plugin_hooks", "web_search"} {
		if _, ok := got[key]; ok {
			t.Fatalf("undeclared key recorded: %s", key)
		}
	}
}

func TestReadDeclaredStateRealTable(t *testing.T) {
	// Call count, argv and ignored success stderr are anchored to features.ts:80-84.
	calls := 0
	got, err := ReadDeclaredState(func(args []string) CodexRunResult {
		calls++
		if !slices.Equal(args, []string{"features", "list"}) {
			t.Fatalf("runner args = %v", args)
		}
		return CodexRunResult{Stdout: realFeaturesList, Stderr: "success warning"}
	})
	want := map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": false}
	if err != nil || calls != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %v, err = %v, calls = %d", got, err, calls)
	}
}

func TestReadDeclaredStateUnseenFlags(t *testing.T) {
	got, err := ReadDeclaredState(func([]string) CodexRunResult {
		return CodexRunResult{Stdout: "multi_agent stable true"}
	})
	want := map[string]bool{"multi_agent": true, "goals": false, "hooks": false, "default_mode_request_user_input": false}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %v, err = %v", got, err)
	}
}

func TestReadDeclaredStateNonzeroExit(t *testing.T) {
	// Error strings were recorded from the pinned oracle with a fake runner.
	for _, tc := range []struct {
		code int
		want string
	}{
		{1, "codex features list failed (exit 1): boom"},
		{-1, "codex features list failed (exit -1): boom"},
		{127, "codex features list failed (exit 127): boom"},
	} {
		got, err := ReadDeclaredState(func([]string) CodexRunResult {
			return CodexRunResult{Stdout: "hooks true", Stderr: " \ufeffboom\r\n ", ExitCode: tc.code}
		})
		if got != nil || err == nil || err.Error() != tc.want {
			t.Fatalf("exit %d: state = %v, err = %v, want %q", tc.code, got, err, tc.want)
		}
	}
}

func TestFeaturesToEnableMixed(t *testing.T) {
	got := FeaturesToEnable(map[string]bool{"multi_agent": true, "goals": true, "hooks": false, "default_mode_request_user_input": false})
	if !slices.Equal(got, []DeclaredFeature{"hooks", "default_mode_request_user_input"}) {
		t.Fatalf("pending = %v", got)
	}
}

func TestFeaturesToEnableRealTable(t *testing.T) {
	state, err := ReadDeclaredState(func([]string) CodexRunResult { return CodexRunResult{Stdout: realFeaturesList} })
	if err != nil || !slices.Equal(FeaturesToEnable(state), []DeclaredFeature{"default_mode_request_user_input"}) {
		t.Fatalf("state = %v, err = %v", state, err)
	}
}

func TestParseFeaturesListRecordedEdges(t *testing.T) {
	// Independent Node recordings of features.ts:61-76, including its kept quirks.
	for _, tc := range []struct {
		name, stdout string
		want         map[string]bool
	}{
		{"empty", "", map[string]bool{}},
		{"crlf_two_columns", " hooks\tTRUE\r\ngoals any FALSE\r\nmulti_agent false\r\n", map[string]bool{"hooks": true, "goals": false, "multi_agent": false}},
		{"last_valid_duplicate", "hooks stable true\nhooks unknown\ngoals true\nhooks false", map[string]bool{"hooks": false, "goals": true}},
		{"invalid_duplicate_keeps_prior", "hooks stable true\nhooks unknown", map[string]bool{"hooks": true}},
		{"js_spaces", "\ufeffhooks\u00a0stable\u3000TrUe\ufeff", map[string]bool{"hooks": true}},
		{"nel_before_name", "\u0085hooks stable true", map[string]bool{}},
		{"nel_between_fields", "hooks\u0085stable true", map[string]bool{}},
		{"nel_after_bool", "hooks stable true\u0085", map[string]bool{}},
		{"unicode_separator_joins_rows", "hooks stable true\u2028goals stable false", map[string]bool{"hooks": false}},
		{"bare_cr_joins_rows", "hooks false\rgoals true", map[string]bool{"hooks": true}},
		{"comment_tail", "hooks stable true # comment", map[string]bool{}},
		{"name_case", "HOOKS stable true", map[string]bool{}},
		{"invalid_bool", "goals stable yes", map[string]bool{}},
		{"single_field", "hooks", map[string]bool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseFeaturesList(tc.stdout); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parsed = %v, want %v", got, tc.want)
			}
			state, err := ReadDeclaredState(func([]string) CodexRunResult { return CodexRunResult{Stdout: tc.stdout} })
			if err != nil || len(state) != 4 {
				t.Fatalf("state = %v, err = %v", state, err)
			}
			for _, key := range []string{"multi_agent", "goals", "hooks", "default_mode_request_user_input"} {
				value, ok := state[key]
				if !ok || value != tc.want[key] {
					t.Fatalf("state[%s] = %v (present %v), want %v", key, value, ok, tc.want[key])
				}
			}
		})
	}
}

func TestFeaturesToEnableEmptyAndComplete(t *testing.T) {
	if got := FeaturesToEnable(nil); !slices.Equal(got, []DeclaredFeature{"multi_agent", "goals", "hooks", "default_mode_request_user_input"}) {
		t.Fatalf("nil state pending = %v", got)
	}
	if got := FeaturesToEnable(map[string]bool{"multi_agent": true, "goals": true, "hooks": true, "default_mode_request_user_input": true, "plugin_hooks": false}); got == nil || len(got) != 0 {
		t.Fatalf("all enabled pending = %#v", got)
	}
}

func TestSoftFeatureImpact(t *testing.T) {
	// Ported from soft-failure-visibility.test.ts:90-95; text is oracle runtime data.
	soft := SoftFeatures()
	impact := SoftFeatureImpact()
	if !slices.Equal(soft, []DeclaredFeature{"default_mode_request_user_input"}) || len(impact) != 1 {
		t.Fatalf("soft = %v, impact = %v", soft, impact)
	}
	want := "Default 모드에서 질문선택지 UI(request_user_input)가 모델에게 노출되지 않는다. Plan 모드에서는 계속 동작한다."
	for _, key := range soft {
		if impact[key] != want {
			t.Fatalf("impact[%s] = %q", key, impact[key])
		}
	}
}

func TestFeatureMetadataFresh(t *testing.T) {
	declared, soft, impact := DeclaredFeatures(), SoftFeatures(), SoftFeatureImpact()
	declared[0], soft[0], impact[FeatureDefaultModeRequestUserInput] = "other", "other", "other"
	if DeclaredFeatures()[0] != FeatureMultiAgent || SoftFeatures()[0] != FeatureDefaultModeRequestUserInput || SoftFeatureImpact()[FeatureDefaultModeRequestUserInput] == "other" {
		t.Fatal("caller mutation changed feature metadata")
	}
}
