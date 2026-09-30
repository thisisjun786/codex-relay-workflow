package delivery

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
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

// captureOmitted is what testdata/omitted_capture.py captured for method, as recorded: every
// observe call's arguments and result, and the tree each call read, restored under root (a
// parityTree: the tree's paths name its marker directories) when Python is not run.
func captureOmitted(t *testing.T, method string) (string, omittedCapture) {
	t.Helper()
	root := parityTree(t)
	raw := pyAnswer(t, "capture", func() ([]byte, error) {
		repo, err := filepath.Abs("../../..")
		if err != nil {
			return nil, err
		}
		script, err := filepath.Abs("testdata/omitted_capture.py")
		if err != nil {
			return nil, err
		}
		home := filepath.Join(root, "home")
		if err := os.MkdirAll(home, 0700); err != nil {
			return nil, err
		}
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, root, method)
		cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+os.TempDir(), "PYTHONPATH="+filepath.Join(repo, "packages/codex-session-relay/src")+":"+filepath.Join(repo, "packages/codex-session-relay"))
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("python %s: %w\n%s", method, err, out)
		}
		return os.ReadFile(filepath.Join(root, "capture.json"))
	}, pyoracle.Substitute(root, "<root>"), pyoracle.Substitute(os.TempDir(), "<tmp>"))
	var capture omittedCapture
	if err := json.Unmarshal(raw, &capture); err != nil {
		t.Fatal(err)
	}
	if len(capture.Problems) != 0 {
		t.Fatal(capture.Problems)
	}
	for i, call := range capture.Calls {
		if call.Snapshot == "" {
			continue
		}
		archive := pyAnswer(t, fmt.Sprintf("snapshot %d", i), func() ([]byte, error) { return archiveSnapshot(call.Snapshot) })
		if !pyoracle.Live() {
			mustDo(t, restoreSnapshot(archive, call.Snapshot))
		}
	}
	return root, capture
}

// snapshotStoreID and snapshotStamp replace a captured store's random id and wall-clock stamps,
// and a mirror's device and inode numbers become 0: the omission reader reads none of them, and
// the archive is then the same on every capture.
const (
	snapshotStoreID = "0123456789abcdef0123456789abcdef"
	snapshotStamp   = "2026-01-01T00:00:00Z"
)

// archiveSnapshot neutralizes dir in place (see snapshotStoreID) and returns it as a gzip'd tar
// with every entry's type, mode, content and link target, and nothing a rerun changes.
func archiveSnapshot(dir string) ([]byte, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		if err := neutralSnapshotFile(path); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	tw := tar.NewWriter(zw)
	err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		var content []byte
		switch mode := info.Mode(); {
		case mode.IsDir():
			header.Typeflag = tar.TypeDir
		case mode&fs.ModeSymlink != 0:
			header.Typeflag = tar.TypeSymlink
			if header.Linkname, err = os.Readlink(path); err != nil {
				return err
			}
		case mode&fs.ModeNamedPipe != 0:
			header.Typeflag = tar.TypeFifo
		case mode.IsRegular():
			header.Typeflag = tar.TypeReg
			if content, err = os.ReadFile(path); err != nil {
				return err
			}
			header.Size = int64(len(content))
		default:
			return fmt.Errorf("%s: cannot archive mode %v", path, mode)
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		_, err = tw.Write(content)
		return err
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// neutralSnapshotFile rewrites a captured store's identity and a mirror's volatile fields.
func neutralSnapshotFile(path string) error {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A store's -wal or -shm, gone once the store was rewritten and closed.
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case bytes.HasPrefix(content, []byte("SQLite format 3\x00")):
		db, err := sql.Open("sqlite", path)
		if err != nil {
			return err
		}
		err = neutralStore(db)
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	case filepath.Base(path) == "takeover.json":
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		var mirror map[string]any
		if decoder.Decode(&mirror) != nil {
			return nil
		}
		for key, value := range map[string]any{"storeId": snapshotStoreID, "updatedAt": snapshotStamp} {
			if _, ok := mirror[key]; ok {
				mirror[key] = value
			}
		}
		if database, ok := mirror["database"].(map[string]any); ok {
			for _, key := range []string{"device", "inode", "walDirectoryDevice", "walDirectoryInode"} {
				if _, ok := database[key]; ok {
					database[key] = 0
				}
			}
		}
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(mirror); err != nil {
			return err
		}
		return os.WriteFile(path, bytes.TrimSuffix(out.Bytes(), []byte("\n")), 0o600)
	}
	return nil
}

// physicalIdentity is a device or inode number in JSON a store row holds.
var physicalIdentity = regexp.MustCompile(`("(?:device|inode|walDirectoryDevice|walDirectoryInode)": ?)\d+`)

// neutralStore rewrites a captured store's id and creation stamp, the device and inode numbers its
// JSON rows name, and each managed start's request fingerprint (a digest over those) wherever the
// store holds it; then rebuilds the file (VACUUM), so the same rows are the same bytes.
func neutralStore(db *sql.DB) error {
	if err := neutralRows(db); err != nil {
		return err
	}
	_, err := db.Exec("VACUUM")
	return err
}

func neutralRows(db *sql.DB) error {
	names := map[string]bool{}
	rows, err := db.Query("SELECT name, COALESCE(sql, '') FROM sqlite_master WHERE type = 'table'")
	if err != nil {
		return err
	}
	rowid := map[string]bool{}
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			_ = rows.Close()
			return err
		}
		names[name] = true
		rowid[name] = !strings.Contains(strings.ToUpper(definition), "WITHOUT ROWID")
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, table := range slices.Sorted(maps.Keys(names)) {
		if !rowid[table] || strings.HasPrefix(table, "sqlite_") {
			continue
		}
		columns, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		for _, column := range columns {
			if err := neutralColumn(db, table, column); err != nil {
				return err
			}
		}
	}
	if names["schema_meta"] {
		if _, err := db.Exec("UPDATE schema_meta SET value = CASE key WHEN 'store_id' THEN ? ELSE ? END WHERE key IN ('store_id', 'store_created_at')", snapshotStoreID, snapshotStamp); err != nil {
			return err
		}
	}
	if !names["managed_start_requests"] {
		return nil
	}
	fingerprints := map[string]string{}
	rows, err = db.Query("SELECT request_id, request_fingerprint FROM managed_start_requests")
	if err != nil {
		return err
	}
	for rows.Next() {
		var request, fingerprint string
		if err := rows.Scan(&request, &fingerprint); err != nil {
			_ = rows.Close()
			return err
		}
		sum := sha256.Sum256([]byte("neutral fingerprint " + request))
		fingerprints[fingerprint] = hex.EncodeToString(sum[:])
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// In one order: a rewritten overflow page is allocated where the previous rewrite left room.
	for _, table := range slices.Sorted(maps.Keys(names)) {
		columns, err := tableColumns(db, table)
		if err != nil {
			return err
		}
		for _, column := range columns {
			for _, old := range slices.Sorted(maps.Keys(fingerprints)) {
				neutral := fingerprints[old]
				statement := fmt.Sprintf(`UPDATE "%s" SET "%s" = replace("%s", ?, ?) WHERE typeof("%s") = 'text' AND instr("%s", ?) > 0`, table, column, column, column, column)
				if _, err := db.Exec(statement, old, neutral, old); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		columns = append(columns, name)
	}
	return columns, rows.Err()
}

// neutralColumn zeroes the device and inode numbers in one column's JSON text.
func neutralColumn(db *sql.DB, table, column string) error {
	rows, err := db.Query(fmt.Sprintf(`SELECT rowid, "%s" FROM "%s" WHERE typeof("%s") = 'text' AND (instr("%s", '"device"') > 0 OR instr("%s", '"inode"') > 0)`, column, table, column, column, column))
	if err != nil {
		return err
	}
	type change struct {
		rowid int64
		value string
	}
	var changes []change
	for rows.Next() {
		var c change
		if err := rows.Scan(&c.rowid, &c.value); err != nil {
			_ = rows.Close()
			return err
		}
		if neutral := physicalIdentity.ReplaceAllString(c.value, "${1}0"); neutral != c.value {
			changes = append(changes, change{c.rowid, neutral})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, c := range changes {
		if _, err := db.Exec(fmt.Sprintf(`UPDATE "%s" SET "%s" = ? WHERE rowid = ?`, table, column), c.value, c.rowid); err != nil {
			return err
		}
	}
	return nil
}

// restoreSnapshot writes an archiveSnapshot back at dir.
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
