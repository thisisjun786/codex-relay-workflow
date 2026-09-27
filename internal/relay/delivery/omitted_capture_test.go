package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type omittedCapture struct {
	Calls []struct {
		Args     []any          `json:"args"`
		Result   map[string]any `json:"result"`
		Error    string         `json:"error"`
		Snapshot string         `json:"snapshot"`
		Writes   []string       `json:"writes"`
	} `json:"calls"`
	Problems []string `json:"problems"`
}

func captureOmitted(t *testing.T, method string) (string, omittedCapture) {
	t.Helper()
	root, err := os.MkdirTemp("", "crw-omitted-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs("testdata/omitted_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, root, method)
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src")+":"+filepath.Join(repo, "packages/codex-session-relay"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python %s: %v\n%s", method, err, out)
	}
	raw, err := os.ReadFile(filepath.Join(root, "capture.json"))
	if err != nil {
		t.Fatal(err)
	}
	var capture omittedCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Problems) != 0 {
		t.Fatal(capture.Problems)
	}
	return root, capture
}

func normalizeOmitted(v any, oldRoot, newRoot string) any {
	raw, _ := json.Marshal(v)
	text := strings.ReplaceAll(string(raw), oldRoot, newRoot)
	var out any
	_ = json.Unmarshal([]byte(text), &out)
	return out
}

func replayOmittedCalls(t *testing.T, method string) {
	t.Helper()
	root, capture := captureOmitted(t, method)
	if len(capture.Calls) == 0 {
		t.Fatal("Python fixture captured no observe calls")
	}
	// Restore the input for each call, not the fixture's final state.
	for i, call := range capture.Calls {
		if call.Error != "" {
			t.Fatalf("unexpected Python exception in call %d: %s", i+1, call.Error)
		}
		if len(call.Args) != 8 {
			t.Fatalf("call %d args=%v", i, call.Args)
		}
		selection := call.Args[0].(map[string]any)
		pythonTree := filepath.Join(root, "tree")
		if err := os.RemoveAll(pythonTree); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(pythonTree, 0700); err != nil {
			t.Fatal(err)
		}
		copy := exec.Command("cp", "-a", call.Snapshot+"/.", pythonTree)
		if out, err := copy.CombinedOutput(); err != nil {
			t.Fatalf("restore call %d: %v %s", i, err, out)
		}
		markerRoot := call.Args[1].(string)
		workspace := call.Args[2].(string)
		assignment := call.Args[3].(string)
		session := call.Args[4].(string)
		turn := call.Args[5].(string)
		now := call.Args[6].(string)
		kwargs := call.Args[7].(map[string]any)
		grace, _ := kwargs["grace"].(float64)
		state := store.StateSelection{Path: selection["path"].(string), Source: selection["source"].(string)}
		var got Obj
		if len(call.Writes) == 0 {
			got = ObserveOmission(context.Background(), state, markerRoot, workspace, assignment, session, turn, now, grace)
		} else {
			// Replay the captured writer at the exact _current boundary, never by a sleep.
			writer, err := sql.Open("sqlite", state.DBPath())
			if err != nil {
				t.Fatal(err)
			}
			got = observeOmission(context.Background(), state, markerRoot, workspace, assignment, session, turn, now, grace, func() {
				for _, statement := range call.Writes {
					if _, err := writer.Exec(statement); err != nil {
						t.Fatal(err)
					}
				}
			})
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(plainOmitted(got))
		var gotMap map[string]any
		if err := json.Unmarshal(raw, &gotMap); err != nil {
			t.Fatal(err)
		}
		want := normalizeOmitted(call.Result, root, root)
		if !reflect.DeepEqual(gotMap, want) {
			t.Fatalf("%s observe call %d differs\ngo=%s\npython=%s", method, i+1, jsonTextDelivery(gotMap), jsonTextDelivery(want))
		}
	}
}

func plainOmitted(v any) any {
	switch value := v.(type) {
	case Obj:
		out := map[string]any{}
		for _, field := range value {
			out[field.Key] = plainOmitted(field.Value)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i := range value {
			out[i] = plainOmitted(value[i])
		}
		return out
	default:
		return value
	}
}

func jsonTextDelivery(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func Test24_OMI_1_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_actual_omission_plus_independent_settlement_is_unreported_and_read_only")
}
func Test24_OMI_4_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_empty_queue_is_not_a_stop_observation")
}
func Test24_OMI_5_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_stop_is_not_terminal_evidence")
}
func Test24_OMI_6_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_duplicate_stop_and_later_declaration_preserve_original_warning")
}
func Test24_OMI_9_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_foreign_global_observation_does_not_settle_this_assignment")
}
func Test24_OMI_11_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_legacy_admission_needs_fresh_binding_before_omission_is_proven")
}
func Test24_OMI_12_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_paused_relationship_is_preserved_without_writes")
}
func Test24_OMI_13_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_marker_absent_is_unmanaged")
}
func Test24_OMI_17_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_conflicting_terminal_observations_remain_unknown")
}
func Test24_OMI_20_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_unregistered_warning_is_visible_without_claiming_omitted_assignment")
}
func Test24_OMI_21_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_reader_requires_actual_omission_not_only_an_ended_turn")
}
func Test24_OMI_22_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_more_than_bounded_stop_history_does_not_pick_a_convenient_subset")
}

func Test24_OMI_2_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_a_registration_naming_another_generation_is_not_read_as_receipted")
}
func Test24_OMI_7_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_staged_ready_stays_staged")
}
func Test24_OMI_8_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_failed_settlement_without_receipt_is_not_a_report")
}
func Test24_OMI_10_WholeOutput(t *testing.T) {
	for _, method := range []string{
		"test_wrong_dispatch_claim_on_bound_session_is_unmeasured",
		"test_exact_assignment_does_not_switch_to_more_recent_claim",
		"test_unadmitted_business_is_not_inferred_from_claim_or_time",
		"test_stale_generation_and_foreign_session_are_not_reports",
	} {
		t.Run(method, func(t *testing.T) { replayOmittedCalls(t, method) })
	}
}
func Test24_OMI_14_WholeOutput(t *testing.T) {
	for _, method := range []string{
		"test_bad_stop_identity_is_not_an_omission",
		"test_symlink_to_foreign_marker_file_is_unmeasured",
		"test_corrupt_and_oversized_stop_records_are_unmeasured",
		"test_selected_marker_cannot_alias_another_assignment",
		"test_special_marker_file_is_refused_before_a_blocking_read",
	} {
		t.Run(method, func(t *testing.T) { replayOmittedCalls(t, method) })
	}
}
func Test24_OMI_15_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_changed_registry_during_read_does_not_mix_facts")
}
func Test24_OMI_16_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_missing_managed_schema_is_not_absence_of_bootstrap")
}
func Test24_OMI_18_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_normal_managed_start_standby_is_not_business")
}
func Test24_OMI_19_WholeOutput(t *testing.T) {
	for _, method := range []string{
		"test_store_and_issue_provenance_are_independent_of_same_session",
		"test_missing_selected_database_stays_missing",
	} {
		t.Run(method, func(t *testing.T) { replayOmittedCalls(t, method) })
	}
}
