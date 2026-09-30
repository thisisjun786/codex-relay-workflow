package delivery

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// omittedCall is one observe call an omission test makes: its arguments, the statements a writer
// runs at the reader's _current boundary, and the tree the call reads.
type omittedCall struct {
	Args     []any    `json:"args"`
	Snapshot string   `json:"snapshot"`
	Writes   []string `json:"writes"`
}

// omittedCalls are method's observe calls (testdata/fixtures/omitted/<method>.json), with the tree
// each call reads (<method>-<i>.tar.gz) restored under root. The calls and trees are inputs the
// Python test module built (testdata/omitted_capture.py captured them before the Python
// implementation left); root is a parityTree because the trees' stores and marker files name
// paths under it.
func omittedCalls(t *testing.T, method string) (string, []omittedCall) {
	t.Helper()
	root := parityTree(t)
	var fixture struct {
		Calls []omittedCall `json:"calls"`
	}
	raw := strings.ReplaceAll(string(golden.Fixture(t, "omitted/"+method+".json")), "<root>", root)
	mustDo(t, json.Unmarshal([]byte(raw), &fixture))
	for i, call := range fixture.Calls {
		if call.Snapshot != "" {
			mustDo(t, restoreSnapshot(golden.Fixture(t, fmt.Sprintf("omitted/%s-%d.tar.gz", method, i)), call.Snapshot))
		}
	}
	return root, fixture.Calls
}

// restoreSnapshot writes a gzip-compressed tar of a tree back at dir, keeping each entry's type
// (FIFOs and symlinks included) and mode.
func restoreSnapshot(archive []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var dirs []*tar.Header
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		path := filepath.Join(dir, filepath.FromSlash(header.Name))
		mode := fs.FileMode(header.Mode).Perm()
		switch header.Typeflag {
		case tar.TypeDir:
			// Written 0700 and given its mode once its entries exist.
			err = os.Mkdir(path, 0o700)
			dirs = append(dirs, header)
		case tar.TypeSymlink:
			err = os.Symlink(header.Linkname, path)
		case tar.TypeFifo:
			if err = syscall.Mkfifo(path, uint32(mode)); err == nil {
				err = os.Chmod(path, mode)
			}
		case tar.TypeReg:
			var content []byte
			if content, err = io.ReadAll(tr); err == nil {
				if err = os.WriteFile(path, content, 0o600); err == nil {
					err = os.Chmod(path, mode)
				}
			}
		default:
			err = fmt.Errorf("%s: unexpected entry type %c", header.Name, header.Typeflag)
		}
		if err != nil {
			return err
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(dirs[i].Name)), fs.FileMode(dirs[i].Mode).Perm()); err != nil {
			return err
		}
	}
	return nil
}

func replayOmittedCalls(t *testing.T, method string) {
	t.Helper()
	root, calls := omittedCalls(t, method)
	if len(calls) == 0 {
		t.Fatal("the fixture holds no observe calls")
	}
	// Restore the input for each call, not the fixture's final state.
	for i, call := range calls {
		if len(call.Args) != 8 {
			t.Fatalf("call %d args=%v", i, call.Args)
		}
		selection := call.Args[0].(map[string]any)
		tree := filepath.Join(root, "tree")
		if err := os.RemoveAll(tree); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(tree, 0700); err != nil {
			t.Fatal(err)
		}
		copy := exec.Command("cp", "-a", call.Snapshot+"/.", tree)
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
		golden.CheckJSON(t, fmt.Sprintf("call %d", i), gotMap, golden.Substitute(root, "<root>"), golden.Substitute(os.TempDir(), "<tmp>"))
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

// A frozen copy nested past json.loads's depth leaves guard.deliverable_state as a
// RecursionError, which the reader answers as unreadable evidence with the exception's words.
func Test24_OMI_7b_WholeOutput(t *testing.T) {
	replayOmittedCalls(t, "test_a_frozen_copy_nested_past_the_decoder_is_unreadable_evidence")
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
