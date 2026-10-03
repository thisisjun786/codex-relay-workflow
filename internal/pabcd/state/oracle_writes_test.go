package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// testdata/oracle-writes.json holds what the CXC v0.2.40 oracle's write path did in each case, recorded once by
// testdata/record-writes.mjs under Node 24 (no Node runs here): file bytes, listings, modes and the error it threw. This test
// replays the cases one process can run; concurrency_test.go replays concurrent_writers, concurrent_ensure_state,
// lock_contention_between_processes, killed_between_temp_write_and_rename, lock_runs_and_releases, lock_released_when_fn_throws and
// lock_held_exhausts. The ledger_ and interview_ cases are the ledger issue's.

type recorded map[string]any

func (r recorded) str(k string) string { s, _ := r[k].(string); return s }
func (r recorded) strs(k string) (out []string) {
	for _, v := range r[k].([]any) {
		out = append(out, v.(string))
	}
	return out
}

func TestWritesMatchTheRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/oracle-writes.json")
	var cases []recorded
	if err != nil || json.Unmarshal(data, &cases) != nil || len(cases) < 55 {
		t.Fatalf("%d recorded cases, %v", len(cases), err)
	}
	errno := map[string]syscall.Errno{"EPERM": syscall.EPERM, "ENOTSUP": syscall.ENOTSUP, "EXDEV": syscall.EXDEV, "EEXIST": syscall.EEXIST, "EIO": syscall.EIO, "EACCES": syscall.EACCES}
	for _, c := range cases {
		id, cwd := c.str("id"), t.TempDir()
		// The ledger_ and interview_ cases are recorded for the ledger issue, which owns LedgerEntry, appendLedger, the interview scan
		// events and readInterviewEvents and replays them from this file; they are skipped here by prefix.
		if strings.HasPrefix(id, "ledger_") || strings.HasPrefix(id, "interview_") {
			continue
		}
		state := StatePath(cwd, "rec-s1")
		same := func(what string, got, want any) {
			t.Helper()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s:\n got %#v\nwant %#v", id, what, got, want)
			}
		}
		switch {
		case id == "ensure_fresh":
			first, _ := ensureState(cwd, "rec-s1", at, os.Link)
			file := fileText(t, state)
			second, _ := ensureState(cwd, "rec-s1", at, os.Link)
			same("results", []any{first, second, file, fileText(t, state) == file, sessionFiles(cwd)}, []any{c["first"].(map[string]any)["returned"], c["second"].(map[string]any)["returned"], c.str("file"), true, c.strs("listing")})
		case id == "ensure_noncanonical":
			created, err := ensureState(cwd, c.str("sessionId"), at, os.Link)
			_, statErr := os.Stat(filepath.Join(cwd, crwdir.DirName))
			same("outcome", []any{created, errors.Is(err, ErrNonCanonicalSessionID), err.Error(), statErr == nil}, []any{false, true, c.str("message"), c["stateDirCreated"]})
		case strings.HasPrefix(id, "ensure_link_"):
			e := errno[strings.TrimPrefix(id, "ensure_link_")]
			created, err := ensureState(cwd, "rec-s1", at, linkFails(e))
			want := []any{c["returned"] == true, c.str("threw") != ""}
			if c.str("threw") == "" {
				want = []any{c["returned"] == true, false}
			}
			same("outcome", []any{created, err != nil && errors.Is(err, e)}, []any{want[0], want[1]})
			same("listing", sessionFiles(cwd), c.strs("listing"))
		case id == "ensure_fallback_restamps": // the fallback file carries the clock's next reading, as the oracle's second defaultState does
			n := 0
			clock := func() time.Time { n++; return at().Add(time.Duration(n-1) * time.Second) }
			created, err := ensureState(cwd, "rec-s1", clock, linkFails(syscall.EPERM))
			same("outcome", []any{created, err, fileText(t, state)}, []any{true, nil, c.str("file")})
		case id == "ensure_final_is_directory":
			_ = os.MkdirAll(state, 0o777)
			created, err := ensureState(cwd, "rec-s1", at, os.Link)
			same("outcome", []any{created, err, sessionFiles(cwd)}, []any{false, nil, c.strs("listing")})
		case strings.HasPrefix(id, "ensure_tmp_replaced_by_directory_"):
			// the link step swaps the temp file for a directory: the removal refuses it (ERR_FS_EISDIR) and its error replaces the result
			swap := func(tmp, final string) error {
				_ = os.Remove(tmp)
				_ = os.Mkdir(tmp, 0o777)
				if strings.Contains(id, "_full_") {
					_ = os.WriteFile(filepath.Join(tmp, "x"), []byte("1"), 0o644)
				}
				if strings.HasSuffix(id, "_eexist") {
					return linkFails(syscall.EEXIST)(tmp, final)
				}
				return nil
			}
			created, err := ensureState(cwd, "rec-s1", at, swap)
			same("outcome", []any{created, errors.Is(err, syscall.EISDIR), c.str("threw"), len(sessionFiles(cwd)) - btoi(slices.Contains(sessionFiles(cwd), "rec-s1.json"))}, []any{false, true, "ERR_FS_EISDIR", int(c["tmpLeft"].(float64))})
		case strings.HasPrefix(id, "write_") && c["state"] != nil:
			var s State
			_ = json.Unmarshal(jsonOf(c["state"]), &s)
			same("write", writeState(cwd, s, at(), crwdir.Rename), error(nil))
			files := map[string]any{}
			for _, n := range sessionFiles(cwd) {
				files[n] = fileText(t, filepath.Join(filepath.Dir(state), n))
			}
			same("files", files, c["files"].(map[string]any))
		case id == "write_alias_ids":
			for _, s := range []State{{Phase: PhaseP, SessionID: "a/b"}, {Phase: PhaseB, SessionID: "a-b"}} {
				s.InjectedTurns, s.UnverifiedSubagents = []string{}, []UnverifiedSubagent{}
				_ = WriteState(cwd, s)
			}
			same("alias", []any{sessionFiles(cwd), string(ReadState(cwd, "a/b").Phase)}, []any{c.strs("listing"), c.str("phase")})
		case id == "write_final_is_directory":
			_ = os.MkdirAll(state, 0o777)
			// os.Rename refuses a directory target itself, with EEXIST where rename(2) and Node say EISDIR; the failure, the removed temp
			// file and the untouched directory are the same
			same("outcome", []any{errors.Is(WriteState(cwd, DefaultState("rec-s1", "")), fs.ErrExist), sessionFiles(cwd)}, []any{c.str("threw") == "EISDIR", c.strs("listing")})
		case id == "write_orphan_tmp_kept":
			orphan := "rec-s1.json.12345.1767225600000.tmp"
			putIn(t, cwd, "rec-s1", "garbage")
			_ = os.WriteFile(filepath.Join(filepath.Dir(state), orphan), []byte("half"), 0o644)
			s := DefaultState("rec-s1", "")
			s.Phase = PhaseP
			same("outcome", []any{WriteState(cwd, s), sessionFiles(cwd), fileText(t, filepath.Join(filepath.Dir(state), orphan)), string(ReadState(cwd, "rec-s1").Phase)}, []any{nil, c.strs("listing"), c.str("orphan"), c.str("phase")})
		case id == "write_state_dir_is_file":
			_ = os.WriteFile(filepath.Join(cwd, crwdir.DirName), []byte("a file"), 0o644)
			same("outcome", []any{errors.Is(WriteState(cwd, DefaultState("rec-s1", "")), syscall.ENOTDIR), fileText(t, filepath.Join(cwd, crwdir.DirName))}, []any{true, c.str("dir")})
		case id == "modes_umask_022" || id == "modes_umask_077":
			mask, _ := strconv.ParseInt(strings.TrimPrefix(id, "modes_umask_"), 8, 32)
			defer syscall.Umask(syscall.Umask(int(mask)))
			_, _ = EnsureState(cwd, "ens")
			_ = WriteState(cwd, DefaultState("wr", ""))
			mode := func(p string) string { info, _ := os.Stat(p); return fmt.Sprintf("%o", info.Mode().Perm()) }
			lock := ""
			_ = WithSessionLock(cwd, "wr", func() error { lock = mode(StatePath(cwd, "wr") + ".lock"); return nil })
			same("modes", []string{mode(StatePath(cwd, "ens")), mode(StatePath(cwd, "wr")), lock}, []string{c.str("ensure"), c.str("write"), c.str("lock")})
		case id == "lock_release_removes_a_foreign_lock" || strings.HasPrefix(id, "lock_release_over_a_directory_"):
			lock := StatePath(cwd, "rec-s1") + ".lock"
			err := WithSessionLock(cwd, "rec-s1", func() error { // the lock is released by path: whatever stands there goes, or stays when it is a directory
				if id == "lock_release_removes_a_foreign_lock" {
					return os.WriteFile(lock, []byte("another holder"), 0o644)
				}
				_ = os.Remove(lock)
				_ = os.Mkdir(lock, 0o777)
				if strings.HasSuffix(id, "_full") {
					_ = os.WriteFile(filepath.Join(lock, "x"), []byte("1"), 0o644)
				}
				return nil
			})
			info, statErr := os.Stat(lock)
			same("outcome", []any{err, statErr == nil, statErr == nil && info.IsDir()}, []any{nil, id != "lock_release_removes_a_foreign_lock", id != "lock_release_removes_a_foreign_lock"})
		case id == "lock_key_is_sanitised":
			_ = makeSessionsDir(cwd)
			_ = os.WriteFile(StatePath(cwd, "x")+".lock", []byte("1"), 0o644)
			for _, key := range []string{"x", "x/"} { // "x/" sanitises to "x": the same state file, so the same lock
				same(key, errors.Is(withSessionLock(cwd, key, func() error { return nil }, func(time.Duration) {}), fs.ErrExist), true)
			}
		case slices.Contains([]string{"concurrent_writers", "concurrent_ensure_state", "lock_contention_between_processes", "killed_between_temp_write_and_rename", "lock_runs_and_releases", "lock_released_when_fn_throws", "lock_held_exhausts"}, id):
			// replayed by concurrency_test.go
		default:
			t.Errorf("%s is replayed nowhere", id)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonOf(v any) []byte { b, _ := json.Marshal(v); return b }
