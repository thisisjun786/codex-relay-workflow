package role

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// These tests drive 'crw role helper dispatch-lock-clear' through the CLI entry, with temporary workspaces and a temporary
// CRW_HOME, HOME and CODEX_HOME (home). A lock is taken for real through the pinned session directory, so its owner is
// this process; a finished owner is a child the test started, whose start it read, then killed and reaped.

const lockClearSession = "session-test"

func lockClearRun(t *testing.T, env host.LookupEnv, ws string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := CLI(append([]string{"helper", "dispatch-lock-clear", "--cwd", ws}, args...), strings.NewReader(""), &out, &errOut, env)
	return code, out.String(), errOut.String()
}

// lockClearLine decodes the one JSON line a call printed.
func lockClearLine(t *testing.T, out string) map[string]any {
	t.Helper()
	var line map[string]any
	if !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 || json.Unmarshal([]byte(out), &line) != nil {
		t.Fatalf("stdout is not one JSON line: %q", out)
	}
	return line
}

// lockClearJSON is v as it reads back from JSON, to compare with a decoded line.
func lockClearJSON(v any) any {
	var out any
	if err := json.Unmarshal(must(json.Marshal(v)), &out); err != nil {
		panic(err)
	}
	return out
}

func lockClearDir(ws string) string { return filepath.Join(ws, ".crw", "dispatches", lockClearSession) }

// lockClearHold takes the lock as a run of the ledger does and leaves it held: its owner is this process.
func lockClearHold(t *testing.T, ws, name string) string {
	t.Helper()
	dir := must(dispatchDirectory(ws, lockClearSession, nil))
	defer dir.Close()
	_, err := dir.lock(name)
	check(t, err)
	return filepath.Join(dir.path, name+".lock")
}

func lockClearOwner(pid int, start string) map[string]any {
	return map[string]any{"pid": pid, "processStart": start, "host": must(os.Hostname()), "createdAt": 1700000000000}
}

// lockClearDead is an owner whose process has finished.
func lockClearDead(t *testing.T) map[string]any {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	check(t, cmd.Start())
	start, err := dispatchProcessStart(cmd.Process.Pid)
	check(t, err)
	check(t, cmd.Process.Kill())
	_ = cmd.Wait()
	return lockClearOwner(cmd.Process.Pid, start)
}

func lockClearOwn(t *testing.T, lock string, owner map[string]any) {
	t.Helper()
	check(t, os.WriteFile(filepath.Join(lock, "owner.json"), must(json.Marshal(owner)), 0o600))
}

func lockClearExists(path string) bool { _, err := os.Lstat(path); return err == nil }

// A new lock records who holds it, whichever of the ledger's locks it is, and the release takes the record away with it.
func TestDispatchLockRecordsItsOwner(t *testing.T) {
	ws := t.TempDir()
	dir := must(dispatchDirectory(ws, lockClearSession, nil))
	defer dir.Close()
	for _, name := range []string{dispatchSessionLock, "task-one.json"} {
		before := time.Now().UnixMilli()
		release, err := dir.lock(name)
		check(t, err)
		path := filepath.Join(dir.path, name+".lock", "owner.json")
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: owner.json = %v, %v", name, info, statErr)
		}
		var owner struct {
			PID          int    `json:"pid"`
			ProcessStart string `json:"processStart"`
			Host         string `json:"host"`
			CreatedAt    int64  `json:"createdAt"`
		}
		check(t, json.Unmarshal(must(os.ReadFile(path)), &owner))
		start, err := dispatchProcessStart(os.Getpid())
		check(t, err)
		if owner.PID != os.Getpid() || owner.Host != must(os.Hostname()) || owner.ProcessStart != start || start == "" || owner.CreatedAt < before || owner.CreatedAt > time.Now().UnixMilli() {
			t.Fatalf("%s: owner = %+v, start %q", name, owner, start)
		}
		check(t, release())
		if lockClearExists(filepath.Join(dir.path, name+".lock")) {
			t.Fatalf("%s: the release left the lock", name)
		}
	}
}

// A lock whose owner finished is removed, the removal is written down, and the same line is printed.
func TestDispatchLockClearRemovesTheLockOfAFinishedOwner(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	log := filepath.Join(lockClearDir(ws), "lock-clears.jsonl")
	var printed []string
	for _, tc := range []struct {
		flags []string
		lock  string
	}{
		{[]string{"--session-lock"}, dispatchSessionLock},
		{[]string{"--dispatch", "task-one"}, "task-one.json"},
	} {
		lock := lockClearHold(t, ws, tc.lock)
		owner := lockClearDead(t)
		lockClearOwn(t, lock, owner)
		before := time.Now().UnixMilli()
		code, out, errOut := lockClearRun(t, env, ws, append(tc.flags, "--session", lockClearSession, "--reason", "owner crashed")...)
		if code != 0 || errOut != "" {
			t.Fatalf("%s: exit %d, stdout %q, stderr %q", tc.lock, code, out, errOut)
		}
		line := lockClearLine(t, out)
		if lockClearExists(lock) {
			t.Fatalf("%s: the lock is still there", tc.lock)
		}
		at, _ := line["clearedAt"].(float64)
		if line["lock"] != tc.lock+".lock" || line["reason"] != "owner crashed" || line["clearedByPid"] != float64(os.Getpid()) || !reflect.DeepEqual(line["owner"], lockClearJSON(owner)) || int64(at) < before || int64(at) > time.Now().UnixMilli() {
			t.Fatalf("%s: line = %v", tc.lock, line)
		}
		printed = append(printed, out)
		if got := string(must(os.ReadFile(log))); got != strings.Join(printed, "") {
			t.Fatalf("%s: log = %q, want the lines printed so far %q", tc.lock, got, printed)
		}
	}
}

// The same pid with another start time is a later process that took the pid over: its lock was left by the earlier one.
func TestDispatchLockClearSamePidOtherStart(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	lock := lockClearHold(t, ws, dispatchSessionLock)
	lockClearOwn(t, lock, lockClearOwner(os.Getpid(), "0"))
	if code, out, _ := lockClearRun(t, env, ws, "--session", lockClearSession, "--session-lock", "--reason", "pid reused"); code != 0 || lockClearExists(lock) {
		t.Fatalf("exit %d, %q, lock kept %v", code, out, lockClearExists(lock))
	}
}

// lockClearOutside is a directory outside the workspace, with a dead owner's owner.json when asked, and makes the session
// directory exist.
func lockClearOutside(t *testing.T, ws string, withOwner bool) string {
	t.Helper()
	outside := dispatchPinnedOutside(t)
	if withOwner {
		check(t, os.WriteFile(filepath.Join(outside, "owner.json"), must(json.Marshal(lockClearDead(t))), 0o600))
	}
	must(dispatchDirectory(ws, lockClearSession, nil)).Close()
	return outside
}

// A refusal removes nothing, writes no log, answers with one JSON error line on stdout and leaves stderr empty.
func TestDispatchLockClearRefusals(t *testing.T) {
	type setup func(t *testing.T, ws string) (flags []string, lock string, verify func())
	session := []string{"--session-lock"}
	held := func(owner func(t *testing.T) map[string]any) setup {
		return func(t *testing.T, ws string) ([]string, string, func()) {
			lock := lockClearHold(t, ws, dispatchSessionLock)
			if owner != nil {
				lockClearOwn(t, lock, owner(t))
			}
			return session, lock, nil
		}
	}
	otherHost := func(t *testing.T) map[string]any {
		owner := lockClearDead(t)
		owner["host"] = "another-host.invalid"
		return owner
	}
	// a dead owner's lock, then something odd in the place of the log
	logCase := func(plant func(t *testing.T, ws, log string) func()) setup {
		return func(t *testing.T, ws string) ([]string, string, func()) {
			lock := lockClearHold(t, ws, dispatchSessionLock)
			lockClearOwn(t, lock, lockClearDead(t))
			return session, lock, plant(t, ws, filepath.Join(lockClearDir(ws), "lock-clears.jsonl"))
		}
	}
	ownerCase := func(plant func(t *testing.T, owner string)) setup {
		return func(t *testing.T, ws string) ([]string, string, func()) {
			lock := lockClearHold(t, ws, dispatchSessionLock)
			_ = os.Remove(filepath.Join(lock, "owner.json")) // absent where the lock records no owner
			plant(t, filepath.Join(lock, "owner.json"))
			return session, lock, nil
		}
	}
	for _, tc := range []struct {
		name, want string
		setup      setup
	}{
		{"owner is alive", "owner process is alive", held(nil)},
		{"owner exists but is not ours to signal", "owner process is alive", held(func(t *testing.T) map[string]any {
			if syscall.Kill(1, 0) != syscall.EPERM {
				t.Skip("pid 1 can be signalled here (root)")
			}
			return lockClearOwner(1, must(dispatchProcessStart(1)))
		})},
		{"start not recorded, pid exists", "cannot confirm its owner is gone", held(func(*testing.T) map[string]any { return lockClearOwner(os.Getpid(), "") })},
		{"no owner", "cannot confirm its owner is gone", ownerCase(func(*testing.T, string) {})},
		{"owner is not JSON", "cannot confirm its owner is gone", ownerCase(func(t *testing.T, p string) { check(t, os.WriteFile(p, []byte("not json"), 0o600)) })},
		{"owner has no pid", "cannot confirm its owner is gone", held(func(*testing.T) map[string]any { return map[string]any{"host": must(os.Hostname())} })},
		{"owner is a link", "cannot confirm its owner is gone", ownerCase(func(t *testing.T, p string) {
			target := filepath.Join(t.TempDir(), "owner.json")
			check(t, os.WriteFile(target, must(json.Marshal(lockClearDead(t))), 0o600))
			check(t, os.Symlink(target, p))
		})},
		{"owner is a named pipe", "cannot confirm its owner is gone", ownerCase(func(t *testing.T, p string) { check(t, syscall.Mkfifo(p, 0o600)) })},
		{"another host", "owner host differs from this host", held(otherHost)},
		{"lock is a link", "not a real directory", func(t *testing.T, ws string) ([]string, string, func()) {
			outside := lockClearOutside(t, ws, true)
			lock := filepath.Join(lockClearDir(ws), dispatchSessionLock+".lock")
			check(t, os.Symlink(outside, lock))
			return session, lock, func() {
				if !lockClearExists(filepath.Join(outside, "owner.json")) || string(must(os.ReadFile(filepath.Join(outside, "sentinel")))) != "untouched" {
					t.Error("what the link leads to was touched")
				}
			}
		}},
		{"lock is a file", "not a real directory", func(t *testing.T, ws string) ([]string, string, func()) {
			lockClearOutside(t, ws, false)
			lock := filepath.Join(lockClearDir(ws), dispatchSessionLock+".lock")
			check(t, os.WriteFile(lock, nil, 0o600))
			return session, lock, nil
		}},
		{"no lock", "no lock to clear", func(t *testing.T, ws string) ([]string, string, func()) {
			lockClearOutside(t, ws, false)
			return []string{"--dispatch", "task-one"}, "", nil
		}},
		{"no session directory", "session directory does not exist", func(t *testing.T, ws string) ([]string, string, func()) {
			return session, "", func() {
				if entries := must(os.ReadDir(ws)); len(entries) != 0 {
					t.Errorf("the refusal created %v", entries)
				}
			}
		}},
		{"log is a link to a file outside", "must be a regular file", logCase(func(t *testing.T, ws, log string) func() {
			outside := dispatchPinnedOutside(t)
			check(t, os.Symlink(filepath.Join(outside, "sentinel"), log))
			return func() {
				if string(must(os.ReadFile(filepath.Join(outside, "sentinel")))) != "untouched" {
					t.Error("the file the log leads to was written")
				}
			}
		})},
		{"log is a link to a record", "must be a regular file", logCase(func(t *testing.T, ws, log string) func() {
			record := filepath.Join(lockClearDir(ws), "task-two.json")
			check(t, os.WriteFile(record, []byte("record"), 0o600))
			check(t, os.Symlink("task-two.json", log))
			return func() {
				if string(must(os.ReadFile(record))) != "record" {
					t.Error("the record the log leads to was written")
				}
			}
		})},
		{"log is a directory", "must be a regular file", logCase(func(t *testing.T, ws, log string) func() { check(t, os.Mkdir(log, 0o700)); return nil })},
		{"log is a named pipe", "must be a regular file", logCase(func(t *testing.T, ws, log string) func() { check(t, syscall.Mkfifo(log, 0o600)); return nil })},
		{"log cannot be written", "permission denied", logCase(func(t *testing.T, ws, log string) func() {
			if os.Geteuid() == 0 {
				t.Skip("root writes a read-only file")
			}
			check(t, os.WriteFile(log, []byte("earlier\n"), 0o400))
			return func() {
				if string(must(os.ReadFile(log))) != "earlier\n" {
					t.Error("the log changed")
				}
			}
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			env, _ := home(t)
			flags, lock, verify := tc.setup(t, ws)
			log := filepath.Join(lockClearDir(ws), "lock-clears.jsonl")
			logBefore := lockClearExists(log)
			code, out, errOut := lockClearRun(t, env, ws, append(flags, "--session", lockClearSession, "--reason", "test")...)
			message, _ := lockClearLine(t, out)["error"].(string)
			if code != 1 || errOut != "" || !strings.Contains(message, tc.want) {
				t.Fatalf("exit %d, error %q, stderr %q, want %q", code, message, errOut, tc.want)
			}
			if lock != "" && !lockClearExists(lock) {
				t.Error("the refusal removed the lock")
			}
			if lockClearExists(log) != logBefore {
				t.Error("the refusal wrote the log")
			}
			if verify != nil {
				verify()
			}
		})
	}
}

// Usage errors exit 2 with one JSON line, and nothing is touched, not even the workspace.
func TestDispatchLockClearUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--dispatch", "task-one", "--reason", "r"},
		{"--session", lockClearSession, "--reason", "r"},
		{"--session", lockClearSession, "--session-lock", "--dispatch", "task-one", "--reason", "r"},
		{"--session", lockClearSession, "--session-lock"},
		{"--session", lockClearSession, "--session-lock", "--reason", "  "},
		{"--session", lockClearSession, "--session-lock", "--reason", "r", "--force"},
		{"--session-lock", "--reason", "r", "--session"},
		{"--session", "bad/session", "--session-lock", "--reason", "r"},
		{"--session", lockClearSession, "--dispatch", "../x", "--reason", "r"},
	} {
		ws := t.TempDir()
		env, _ := home(t)
		code, out, errOut := lockClearRun(t, env, ws, args...)
		message, _ := lockClearLine(t, out)["error"].(string)
		if code != 2 || errOut != "" || message == "" {
			t.Errorf("%v: exit %d, %q, stderr %q", args, code, out, errOut)
		}
		if entries := must(os.ReadDir(ws)); len(entries) != 0 {
			t.Errorf("%v: created %v", args, entries)
		}
	}
}

// After a clear the created report and the stopped close that the stale lock refused work again, and the session directory,
// which now holds lock-clears.jsonl, is still scanned as before.
func TestDispatchLockClearLetsTheLedgerWorkAgain(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	h := &createdLockHost{status: "idle"}
	one := createdArchivedSession(t, ws, env, "task-one")
	report := func(dispatch, attempt string) (DispatchResult, error) {
		return CheckedDispatch(context.Background(), ws, createdArchivedReport(dispatch, attempt, "child-a"), env, h)
	}
	stale := func(name string) { lockClearOwn(t, lockClearHold(t, ws, name), lockClearDead(t)) }
	clear := func(flags ...string) {
		t.Helper()
		if code, out, _ := lockClearRun(t, env, ws, append(flags, "--session", lockClearSession, "--reason", "owner crashed")...); code != 0 {
			t.Fatalf("clear %v: exit %d, %q", flags, code, out)
		}
	}
	stale(dispatchSessionLock)
	if _, err := report("task-one", one); err == nil || !strings.Contains(err.Error(), ".session.lock: file exists") {
		t.Fatalf("created report under a stale session lock: %v", err)
	}
	clear("--session-lock")
	if out, err := report("task-one", one); err != nil || out.Action != "wait" {
		t.Fatalf("created report after the clear: %q, %v", out.Action, err)
	}
	stale("task-one.json")
	if _, err := CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, h); err == nil || !strings.Contains(err.Error(), "task-one.json.lock: file exists") {
		t.Fatalf("stopped close under a stale record lock: %v", err)
	}
	clear("--dispatch", "task-one")
	if out, err := CheckedDispatch(context.Background(), ws, createdLockStop("task-one", one, "child-a"), env, h); err != nil || out.Action != "stop" {
		t.Fatalf("stopped close after the clear: %q, %v", out.Action, err)
	}
	two := createdArchivedSession(t, ws, env, "task-two")
	if out, err := report("task-two", two); err != nil || out.Action != "wait" {
		t.Fatalf("report of the freed child with lock-clears.jsonl in the directory: %q, %v", out.Action, err)
	}
	if lines := strings.Count(string(must(os.ReadFile(filepath.Join(lockClearDir(ws), "lock-clears.jsonl")))), "\n"); lines != 2 {
		t.Fatalf("lock-clears.jsonl holds %d lines, want 2", lines)
	}
}

// Clears of one stale lock take turns: one removes it and writes one line, the others find it gone.
func TestDispatchLockClearSerialisesConcurrentClears(t *testing.T) {
	ws := t.TempDir()
	env, _ := home(t)
	lockClearOwn(t, lockClearHold(t, ws, dispatchSessionLock), lockClearDead(t))
	var wg sync.WaitGroup
	codes, outs := make([]int, 8), make([]string, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], outs[i], _ = lockClearRun(t, env, ws, "--session", lockClearSession, "--session-lock", "--reason", "owner crashed")
		}()
	}
	wg.Wait()
	cleared := 0
	for i, code := range codes {
		if code == 0 {
			cleared++
		} else if message, _ := lockClearLine(t, outs[i])["error"].(string); code != 1 || !strings.Contains(message, "no lock to clear") {
			t.Errorf("clear %d: exit %d, %q", i, code, outs[i])
		}
	}
	if lines := strings.Count(string(must(os.ReadFile(filepath.Join(lockClearDir(ws), "lock-clears.jsonl")))), "\n"); cleared != 1 || lines != 1 {
		t.Fatalf("%d clears succeeded and the log holds %d lines, want one of each", cleared, lines)
	}
}

// A writer that fails takes its lock away again. One that finds the lock swapped for another entry writes nothing through it
// and leaves that entry alone: it is not the directory that was created.
func TestDispatchLockWithAbandonsOnlyItsOwnLock(t *testing.T) {
	setup := func(t *testing.T) (*dispatchPinnedDir, string) {
		dir := must(dispatchDirectory(t.TempDir(), lockClearSession, nil))
		t.Cleanup(func() { dir.Close() })
		return dir, filepath.Join(dir.path, "w.lock")
	}
	t.Run("failed writer", func(t *testing.T) {
		dir, lock := setup(t)
		injected := errors.New("injected")
		release, err := dir.lockWith("w", func(string, fs.FileInfo) error { return injected })
		if release != nil || !errors.Is(err, injected) || lockClearExists(lock) {
			t.Fatalf("release %v, error %v, lock left %v", release != nil, err, lockClearExists(lock))
		}
		if _, err = dir.lock("w"); err != nil {
			t.Fatalf("the lock cannot be taken again: %v", err)
		}
	})
	t.Run("lock swapped for a link", func(t *testing.T) {
		dir, lock := setup(t)
		inside := filepath.Join(dir.path, "inside")
		check(t, os.Mkdir(inside, 0o700))
		_, err := dir.lockWith("w", func(name string, created fs.FileInfo) error {
			check(t, os.Rename(lock, lock+".moved"))
			check(t, os.Symlink("inside", lock))
			return dir.writeOwner(name, created)
		})
		if err == nil || lockClearExists(filepath.Join(inside, "owner.json")) {
			t.Fatalf("error %v, owner.json written through the link %v", err, lockClearExists(filepath.Join(inside, "owner.json")))
		}
		if info, statErr := os.Lstat(lock); statErr != nil || info.Mode()&fs.ModeSymlink == 0 {
			t.Fatalf("the entry that took the name was removed: %v", statErr)
		}
	})
	t.Run("lock replaced by a directory", func(t *testing.T) {
		dir, lock := setup(t)
		_, err := dir.lockWith("w", func(string, fs.FileInfo) error {
			check(t, os.Rename(lock, lock+".moved"))
			check(t, os.Mkdir(lock, 0o700))
			check(t, os.WriteFile(filepath.Join(lock, "sentinel"), []byte("untouched"), 0o600))
			return errors.New("injected")
		})
		if err == nil || !strings.Contains(err.Error(), "left in place") || string(must(os.ReadFile(filepath.Join(lock, "sentinel")))) != "untouched" {
			t.Fatalf("error %v: the replacement was removed", err)
		}
	})
}

// A lock that takes the place of the one that was judged gone, after the judgment, is not removed: the clear puts it back, or
// leaves it under the tombstone name when the name was taken again meanwhile, and writes no line.
func TestDispatchLockClearKeepsALockReplacedAfterTheCheck(t *testing.T) {
	for _, retaken := range []bool{false, true} {
		t.Run(map[bool]string{false: "put back", true: "left at the tombstone"}[retaken], func(t *testing.T) {
			ws := t.TempDir()
			lock := lockClearHold(t, ws, dispatchSessionLock)
			lockClearOwn(t, lock, lockClearDead(t))
			var dir *dispatchPinnedDir
			after := func(point string) {
				switch {
				case point == "judged": // a live process takes the lock the clear is about to remove
					check(t, os.RemoveAll(lock))
					_, err := dir.lock(dispatchSessionLock)
					check(t, err)
				case point == "claimed" && retaken: // and the name is taken again before the clear can put it back
					check(t, os.Mkdir(lock, 0o700))
				}
			}
			dir = must(dispatchDirectory(ws, lockClearSession, after))
			defer dir.Close()
			_, err := dispatchLockClear(dir, dispatchSessionLock+".lock", "test")
			want, kept := "put back", lock
			if retaken {
				want, kept = "left at", ""
				if tombs := must(filepath.Glob(lock + ".clearing-*")); len(tombs) == 1 {
					kept = tombs[0]
				}
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("clear of a replaced lock: %v, want %q", err, want)
			}
			owner, _ := os.ReadFile(filepath.Join(kept, "owner.json"))
			if kept == "" || !strings.Contains(string(owner), "\"pid\":"+strconv.Itoa(os.Getpid())+",") {
				t.Fatalf("the live lock is not at %q: %q", kept, owner)
			}
			if retaken && len(must(os.ReadDir(lock))) != 0 {
				t.Error("the directory that took the name was touched")
			}
			if line, _ := os.ReadFile(filepath.Join(lockClearDir(ws), "lock-clears.jsonl")); len(line) != 0 {
				t.Errorf("a clear that did not happen was written down: %q", line)
			}
		})
	}
}
