package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// Expectations marked "oracle" are what Node v24 printed running the unmodified CXC v0.2.40 bg-wake/src/spawn.ts. The cases that start
// a job use only workspaces of this test and stop what they start by the pid of their own record (a process group of its own, CRW-414);
// nothing is signalled by pattern. The deliberate differences from the oracle (the security checklist of the issue) have no oracle
// counterpart.

// oracleHelper is buildWindowsHelper() of the oracle, byte for byte.
func oracleHelper() string {
	return "'use strict';\n" +
		"const { spawn } = require('node:child_process');\n" +
		"const fs = require('node:fs');\n" +
		"const [outPath, exitPath, ...cmd] = process.argv.slice(2);\n" +
		"function record(code) {\n" +
		"  try {\n" +
		"    fs.writeFileSync(exitPath + '.tmp', String(code));\n" +
		"    fs.renameSync(exitPath + '.tmp', exitPath);\n" +
		"  } catch (err) {}\n" +
		"}\n" +
		"let fd = null;\n" +
		"try { fd = fs.openSync(outPath, 'a'); } catch (err) {}\n" +
		"const sink = fd === null ? 'ignore' : fd;\n" +
		"let child = null;\n" +
		"try {\n" +
		"  child = spawn(cmd[0], cmd.slice(1), { stdio: ['ignore', sink, sink], windowsHide: true });\n" +
		"} catch (err) {\n" +
		"  // A synchronous throw would otherwise kill the helper before any listener runs,\n" +
		"  // leaving no exit file at all.\n" +
		"  record(127);\n" +
		"  process.exit(0);\n" +
		"}\n" +
		"if (fd !== null) { try { fs.closeSync(fd); } catch (err) {} }\n" +
		"let done = false;\n" +
		"function finish(code) { if (done) return; done = true; record(code); process.exit(0); }\n" +
		"// ENOENT arrives here. A cmd builtin is not an executable, but Windows does not\n" +
		"// always report it as ENOENT: the measured `echo` came back as a normal exit 1.\n" +
		"child.on('error', function () { finish(127); });\n" +
		"child.on('exit', function (code, signal) { finish(code === null ? (signal ? 129 : 1) : code); });\n" +
		""
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func needPS(t *testing.T) {
	t.Helper()
	if _, ok := ProcessStartToken(os.Getpid()); !ok {
		t.Skip("ps is not available")
	}
}

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

// start runs a job and stops its process group, by the pid it recorded, when the test ends.
func start(t *testing.T, ws string, opts RunOptions) BgRecord {
	t.Helper()
	rec, err := RunBackground(ws, opts, ticking())
	if err != nil {
		t.Fatal(err)
	}
	if rec.PID != nil {
		pid := *rec.PID
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	}
	return rec
}

// settled waits until the job has left running and returns the reconciled record. It waits for the exit file first: Reconcile reads the
// exit file and only then asks whether the pid is alive, so a shell that finishes between the two reads is settled failed with no code
// (a known defect of the oracle, docs/port-cxc/known-defects.md), and these cases are about the spawn, not about that window.
func settled(t *testing.T, ws, id string) BgRecord {
	t.Helper()
	until(t, "the exit file of "+id, func() bool { b, err := os.ReadFile(ExitPath(ws, id)); return err == nil && len(b) > 0 })
	var rec BgRecord
	until(t, "job "+id+" to end", func() bool {
		cur, ok := ReadRecord(ws, id)
		if !ok {
			t.Fatalf("no record of %s", id)
		}
		var err error
		if rec, err = Reconcile(ws, cur, time.Now); err != nil {
			t.Fatal(err)
		}
		return rec.Status != StatusRunning
	})
	return rec
}

// pending keeps a record running whatever its pid: an empty exit file is a write in progress while its mtime is inside the grace window of
// the injected clock, so Reconcile leaves the record alone and Cancel's own checks decide.
func pending(t *testing.T, ws, id string) {
	t.Helper()
	put(t, ExitPath(ws, id), "")
	if err := os.Chtimes(ExitPath(ws, id), noon(), noon()); err != nil {
		t.Fatal(err)
	}
}

func TestShellQuotePosix(t *testing.T) { // oracle: each argument comes back as one word
	dir := workspace(t)
	for _, arg := range []string{"it's", "a b", "two\nlines", "$(touch pwned)", "`touch pwned`", ";touch pwned;&|<>*?", "-n", "--", "", "'", "''"} {
		cmd := exec.Command("sh", "-c", "printf %s "+shellQuotePosix(arg))
		cmd.Dir = dir
		if out, err := cmd.Output(); err != nil || string(out) != arg {
			t.Errorf("%q came back as %q (%v)", arg, out, err)
		}
	}
	if exists(filepath.Join(dir, "pwned")) {
		t.Error("an argument ran as a command")
	}
	if got := shellQuotePosix("it's"); got != "'it'\\''s'" {
		t.Errorf("quote: %s", got)
	}
}

func TestBuildShellPosix(t *testing.T) { // oracle registry.test.ts: the shell records its own exit code; the new part is mv --
	file, args, err := BuildShell([]string{"echo", "hi there"}, "/tmp/o", "/tmp/e", "")
	want := "( 'echo' 'hi there' ) > '/tmp/o' 2>&1; printf %s $? > '/tmp/e.tmp' && mv -- '/tmp/e.tmp' '/tmp/e'"
	if err != nil || file != "/bin/sh" || !reflect.DeepEqual(args, []string{"-c", want}) {
		t.Errorf("%s %q %v", file, args, err)
	}
}

func TestBuildShellKeepsADashedRelativePathAnOperand(t *testing.T) {
	dir := workspace(t)
	mkdir(t, filepath.Join(dir, "-r"))
	_, args, err := buildShell("linux", "node", []string{"true"}, "-r/o", "-r/e", "")
	cmd := exec.Command("sh", args...)
	cmd.Dir = dir
	if err != nil || cmd.Run() != nil || get(t, filepath.Join(dir, "-r", "e")) != "0" {
		t.Errorf("the exit code was not published: %v / %v", err, cmd.ProcessState)
	}
}

func TestBuildWindowsHelperAndShell(t *testing.T) {
	if got := BuildWindowsHelper(); got != oracleHelper() {
		t.Errorf("helper text differs from the oracle's:\n%s", got)
	}
	dir := workspace(t)
	helper := filepath.Join(dir, "h.cjs")
	file, args, err := buildShell("windows", "node", []string{"npm", "run", "build"}, `C:\o.txt`, `C:\e.txt`, helper)
	if want := []string{helper, `C:\o.txt`, `C:\e.txt`, "npm", "run", "build"}; err != nil || file != "node" || !reflect.DeepEqual(args, want) || get(t, helper) != oracleHelper() {
		t.Errorf("%s %q %v", file, args, err)
	}
	out := filepath.Join(dir, "o")
	if _, args, err = buildShell("windows", "node", []string{"x"}, out, "e", ""); err != nil || len(args) == 0 || args[0] != out+".helper.cjs" || get(t, out+".helper.cjs") != oracleHelper() {
		t.Errorf("default helper path: %q %v", args, err)
	}
	if _, _, err = buildShell("windows", "node", nil, out, "e", filepath.Join(dir, "missing", "h.cjs")); err == nil {
		t.Error("a helper that cannot be written is an error")
	}
}

func TestNewID(t *testing.T) { // oracle registry.test.ts: ids do not collide with existing records
	ws := workspace(t)
	save(t, ws, mk(ws, "taken"))
	shape := regexp.MustCompile("^bg[0-9a-f]{6}$")
	if got := NewID(ws, "mine", time.Now); got != "mine" {
		t.Errorf("a free seed is used: %s", got)
	}
	for _, seed := range []string{"taken", ""} { // an empty seed is no seed, as in JavaScript
		if got := NewID(ws, seed, time.Now); !shape.MatchString(got) || RecordExists(ws, got) {
			t.Errorf("seed %q: %s", seed, got)
		}
	}
	save(t, ws, mk(ws, "bg010203"))
	reads := 0
	same := func(b []byte) { reads++; copy(b, []byte{1, 2, 3}) }
	if got := newID(ws, "", same, func() time.Time { return time.UnixMilli(1790000000000) }); got != "bg"+strconv.FormatInt(1790000000000, 36) || reads != 20 {
		t.Errorf("after %d collisions: %s", reads, got) // oracle: twenty tries, then "bg" + Date.now().toString(36)
	}
}

func TestRunBackgroundRecordsItsOwnExitCode(t *testing.T) { // oracle cli.test.ts: a real command runs detached and records its own exit code
	for _, c := range []struct {
		argv []string
		code float64
		out  string
	}{{[]string{"sh", "-c", "echo hello; exit 3"}, 3, "hello"}, {[]string{"exit", "7"}, 7, ""}, {[]string{"definitely-not-a-real-binary-xyz"}, 127, "not found"}} {
		ws, session, note := workspace(t), "S1", "smoke"
		rec := start(t, ws, RunOptions{SessionID: &session, Command: c.argv, Note: &note})
		onDisk, ok := ReadRecord(ws, rec.ID)
		if rec.Status != StatusRunning || rec.PID == nil || rec.Cwd != ws || !ok || !reflect.DeepEqual(onDisk, rec) || !regexp.MustCompile("^bg[0-9a-f]{6}$").MatchString(rec.ID) ||
			*rec.SessionID != session || *rec.Note != note || !reflect.DeepEqual(rec.Command, c.argv) || rec.StartedAt != "2026-09-09T00:10:00.000Z" ||
			rec.AdoptedBy != nil || rec.ExitCode != nil || rec.EndedAt != nil || rec.DeliveredAt != nil {
			t.Fatalf("%v: started as %+v (file %+v)", c.argv, rec, onDisk)
		}
		got := settled(t, ws, rec.ID)
		if got.Status != StatusFailed || *got.ExitCode != c.code || !strings.Contains(get(t, OutPath(ws, rec.ID)), c.out) {
			t.Errorf("%v: %+v out %q", c.argv, got, get(t, OutPath(ws, rec.ID)))
		}
		command, _ := json.Marshal(c.argv)
		wantRow := fmt.Sprintf("\"event\":\"registered\",\"id\":%q,\"pid\":%d,\"command\":%s}", rec.ID, *rec.PID, command)
		if rows := ledger(t, ws); len(rows) != 2 || !strings.HasSuffix(rows[0], wantRow) || !strings.Contains(rows[1], "\"event\":\"completed\"") {
			t.Errorf("%v: ledger %q, want a registered row ending %s", c.argv, rows, wantRow)
		}
		pid := *rec.PID
		until(t, "the shell to be reaped", func() bool { return !PidAlive(pid) }) // Go has no reaper of its own: a zombie still answers signal 0
	}
}

func TestRunBackgroundArgumentsSurviveTheShell(t *testing.T) { // oracle cli.test.ts: an argument with spaces and quotes survives the shell
	for _, arg := range []string{"it's a test", "two\nlines", "$(echo x) `echo y` ;|&", "\\n"} {
		ws := workspace(t)
		rec := start(t, ws, RunOptions{Command: []string{"printf", "%s", arg}})
		if got := settled(t, ws, rec.ID); got.Status != StatusComplete || get(t, OutPath(ws, rec.ID)) != arg {
			t.Errorf("%q: %+v out %q", arg, got, get(t, OutPath(ws, rec.ID)))
		}
	}
}

func TestRunBackgroundRefusesWhatItCannotRun(t *testing.T) {
	for name, c := range map[string]struct {
		opts  RunOptions
		stale bool
		want  error
	}{
		"an id that leaves the store":   {RunOptions{ID: "../x", Command: []string{"true"}}, false, ErrOutsideStore},
		"a NUL byte in an argument":     {RunOptions{Command: []string{"echo", "a\x00b"}}, false, ErrNULArgument}, // Node's spawn throws ERR_INVALID_ARG_VALUE
		"a NUL byte after the removals": {RunOptions{ID: "stale1", Command: []string{"echo", "a\x00b"}}, true, ErrNULArgument},
	} {
		ws := workspace(t)
		store(t, ws)
		if c.stale {
			put(t, ExitPath(ws, "stale1"), "9")
		}
		starts := 0
		_, err := runBackground(ws, c.opts, time.Now, func(*exec.Cmd) error { starts++; return errors.New("must not start") })
		if !errors.Is(err, c.want) || starts != 0 || len(ListRecordIDs(ws)) != 0 || exists(filepath.Join(BGDir(ws), LedgerFile)) ||
			exists(filepath.Join(ws, crwdir.DirName, "x.out")) || c.stale && exists(ExitPath(ws, "stale1")) {
			t.Errorf("%s: %v, started %d times", name, err, starts) // the removals come first, then the refusal, then the start
		}
	}
}

func TestRunBackgroundSpawnFailureSettlesFailed(t *testing.T) { // oracle spawn.ts:114-125: the error event comes after the running record
	ws := workspace(t)
	var seen *exec.Cmd
	rec, err := runBackground(ws, RunOptions{Command: []string{"true"}}, ticking(), func(cmd *exec.Cmd) error {
		seen = cmd
		return &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.ENOENT} // what a missing shell is
	})
	onDisk, _ := ReadRecord(ws, rec.ID)
	if seen == nil || seen.Dir != ws || seen.SysProcAttr == nil || !seen.SysProcAttr.Setsid || seen.Stdin != nil || seen.Stdout != nil || seen.Stderr != nil {
		t.Errorf("the shell is not started detached, in the workspace, with no stdio: %+v", seen)
	}
	if err != nil || rec.Status != StatusRunning || rec.PID != nil || rec.StartToken != nil || rec.StartedAt != "2026-09-09T00:10:00.000Z" ||
		onDisk.Status != StatusFailed || onDisk.ExitCode != nil || onDisk.EndedAt == nil || *onDisk.EndedAt != "2026-09-09T00:10:00.003Z" {
		t.Fatalf("%+v %v file %+v", rec, err, onDisk)
	}
	// oracle order of the clock: startedAt, record write, ledger, endedAt, record write, ledger
	rows := ledger(t, ws)
	if len(rows) != 2 || !strings.HasPrefix(rows[0], "{\"at\":\"2026-09-09T00:10:00.002Z\",\"event\":\"registered\"") || !strings.HasSuffix(rows[0], ",\"pid\":null,\"command\":[\"true\"]}") ||
		rows[1] != "{\"at\":\"2026-09-09T00:10:00.005Z\",\"event\":\"completed\",\"id\":\""+rec.ID+"\",\"exitCode\":null,\"detail\":\"spawn failed\"}" {
		t.Errorf("ledger %q", rows)
	}
}

// Node reports ENOENT, EACCES, EAGAIN, EMFILE and ENFILE on the child's error event, after spawn has returned (the record is settled
// failed); any other failure, E2BIG for one, it throws from spawn itself (oracle, checked with Node 24: no record, no ledger row).
func TestRunBackgroundTellsAThrownStartFailureFromAnErrorEvent(t *testing.T) {
	for _, c := range []struct {
		errno   syscall.Errno
		settled bool
	}{{syscall.ENOENT, true}, {syscall.EACCES, true}, {syscall.EAGAIN, true}, {syscall.EMFILE, true}, {syscall.ENFILE, true},
		{syscall.E2BIG, false}, {syscall.ENOMEM, false}, {syscall.EINVAL, false}, {syscall.ENOEXEC, false}} {
		ws := workspace(t)
		rec, err := runBackground(ws, RunOptions{Command: []string{"true"}}, time.Now, func(*exec.Cmd) error { return &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: c.errno} })
		onDisk, ok := ReadRecord(ws, rec.ID)
		if c.settled && (err != nil || !ok || onDisk.Status != StatusFailed) || !c.settled && (!errors.Is(err, c.errno) || len(ListRecordIDs(ws)) != 0 || exists(filepath.Join(BGDir(ws), LedgerFile))) {
			t.Errorf("%v: %v, record %+v", c.errno, err, onDisk)
		}
	}
}

// The oracle's error listener writes the failed record after the running one; when that second write fails it throws.
func TestRunBackgroundReturnsAFailureToSettle(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a directory the owner cannot write is writable for root")
	}
	ws := workspace(t)
	t.Cleanup(func() { _ = os.Chmod(BGDir(ws), 0o755) }) // before TempDir's own removal
	reads := 0
	clock := func() time.Time { // the third reading is the registered ledger row: the first record is on disk, the next write must fail
		if reads++; reads == 3 {
			_ = os.Chmod(BGDir(ws), 0o555)
		}
		return noon()
	}
	rec, err := runBackground(ws, RunOptions{Command: []string{"true"}}, clock, func(*exec.Cmd) error { return &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.ENOENT} })
	if onDisk, _ := ReadRecord(ws, rec.ID); err == nil || onDisk.Status != StatusRunning {
		t.Errorf("a failure to record the failed state is the error: %v, record %+v", err, onDisk)
	}
}

func TestRunBackgroundReturnsAWriteFailure(t *testing.T) { // oracle spawn.ts:143: writeRecord throws
	ws := workspace(t)
	long := strings.Repeat("a", 300) // longer than a file name: the seed is used, and no record file can be made
	_, err := runBackground(ws, RunOptions{ID: long, Command: []string{"true"}}, time.Now, func(*exec.Cmd) error { return &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.ENOENT} })
	if err == nil || exists(filepath.Join(BGDir(ws), LedgerFile)) {
		t.Errorf("a record that cannot be written is the error and leaves no registered row: %v", err)
	}
}

func TestRunBackgroundClearsStaleFiles(t *testing.T) { // oracle spawn.ts:108-112; the links are the deliberate difference
	ws := workspace(t)
	victim, victim2 := filepath.Join(workspace(t), "victim"), filepath.Join(workspace(t), "victim2")
	put(t, victim, "KEEP")
	put(t, victim2, "KEEP2")
	put(t, ExitPath(ws, "fixed"), "9")
	symlink(t, victim2, OutPath(ws, "fixed"))
	symlink(t, victim, ExitPath(ws, "fixed")+".tmp")
	job := "echo started > ready.tmp && mv ready.tmp ready; until [ -e gate ] || [ ! -e ready ]; do sleep 0.02; done; echo new"
	rec := start(t, ws, RunOptions{ID: "fixed", Command: []string{"sh", "-c", job}})
	until(t, "the job to run", func() bool { return exists(filepath.Join(ws, "ready")) }) // its output redirect is open by then
	out, err := os.Lstat(OutPath(ws, "fixed"))
	if exists(ExitPath(ws, "fixed")) || err != nil || !out.Mode().IsRegular() || get(t, victim) != "KEEP" || get(t, victim2) != "KEEP2" {
		t.Errorf("before the end of the job: stale exit %v, output %v %v, victims %q %q", exists(ExitPath(ws, "fixed")), out, err, get(t, victim), get(t, victim2))
	}
	put(t, filepath.Join(ws, "gate"), "")
	if got := settled(t, ws, rec.ID); got.Status != StatusComplete || get(t, OutPath(ws, "fixed")) != "new\n" || get(t, ExitPath(ws, "fixed")) != "0" || get(t, victim) != "KEEP" || get(t, victim2) != "KEEP2" {
		t.Errorf("%+v out %q victims %q %q", got, get(t, OutPath(ws, "fixed")), get(t, victim), get(t, victim2))
	}
}

func TestRunBackgroundWritesTheConfinedPathsOfARelativeWorkspace(t *testing.T) {
	base := workspace(t)
	t.Chdir(base)
	mkdir(t, "-ws")
	rec := start(t, "-ws", RunOptions{Command: []string{"echo", "hi"}})
	got := settled(t, "-ws", rec.ID)
	bg := filepath.Join(base, "-ws", crwdir.DirName, "bg")
	if got.Status != StatusComplete || *got.ExitCode != 0 || get(t, filepath.Join(bg, rec.ID+".exit")) != "0" || get(t, filepath.Join(bg, rec.ID+".out")) != "hi\n" || exists(filepath.Join(base, "-ws", "-ws")) {
		t.Errorf("the shell wrote somewhere the store did not confine: %+v", got)
	}
}

func TestCancelKeepsAFinishedOutcome(t *testing.T) { // oracle cli.test.ts: cancel does not overwrite an already finished job
	ws := workspace(t)
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", "exit 0"}}) // the start record, which still says running, goes to Cancel
	until(t, "the exit file", func() bool { b, err := os.ReadFile(ExitPath(ws, rec.ID)); return err == nil && string(b) == "0" })
	got, err := Cancel(ws, rec, time.Now)
	if err != nil || got.Status != StatusComplete || got.ExitCode == nil || *got.ExitCode != 0 || strings.Contains(strings.Join(ledger(t, ws), "\n"), "cancelled") {
		t.Errorf("%+v %v", got, err)
	}
}

func TestCancelRunningKillsTheGroup(t *testing.T) {
	needPS(t)
	ws := workspace(t)
	job := `trap 'echo term > term.tmp && mv term.tmp term; exit 143' TERM; echo $$ > pids.tmp && mv pids.tmp pids; echo ready > ready.tmp && mv ready.tmp ready; while [ -e ready ]; do sleep 1 & wait $!; done`
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", job}})
	t.Cleanup(func() { // the job's own shell, by the pid it wrote, in case the start failed to isolate it
		b, _ := os.ReadFile(filepath.Join(ws, "pids"))
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 1 {
			_ = syscall.Kill(n, syscall.SIGKILL)
		}
	})
	until(t, "the job to install its trap", func() bool { return exists(filepath.Join(ws, "ready")) })
	pid := *rec.PID
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Errorf("the job is not its own process group: %d %v", pgid, err)
	}
	if sid, _, errno := syscall.Syscall(syscall.SYS_GETSID, uintptr(pid), 0, 0); errno != 0 || int(sid) != pid {
		t.Errorf("the job is not its own session: %d %v", sid, errno)
	}
	if runtime.GOOS == "linux" {
		for fd := 0; fd < 3; fd++ {
			if to, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", pid, fd)); err != nil || to != "/dev/null" {
				t.Errorf("fd %d of the job is %q (%v), want /dev/null", fd, to, err)
			}
		}
	}
	got, err := Cancel(ws, rec, time.Now)
	onDisk, _ := ReadRecord(ws, rec.ID)
	if err != nil || got.Status != StatusCancelled || got.ExitCode != nil || got.EndedAt == nil || rec.StartToken == nil || !reflect.DeepEqual(onDisk, got) {
		t.Fatalf("%+v %v", got, err)
	}
	// The trap runs in the job's own shell, so term appears only when the signal went to the group and not only to the record.
	until(t, "the job to receive SIGTERM", func() bool { return exists(filepath.Join(ws, "term")) })
	if rows := ledger(t, ws); len(rows) != 2 || !strings.Contains(rows[1], "\"event\":\"cancelled\",\"id\":\""+rec.ID+"\"}") || exists(ExitPath(ws, rec.ID)) {
		t.Errorf("ledger %q", rows)
	}
}

func TestCancelNeverSignalsAnUnsafePid(t *testing.T) {
	big := int64(1) << 32 // the kernel's pid_t is 32 bits: kill would take these for 0 (the caller's group) and 1 (every process)
	for _, pid := range []int{0, 1, int(big), int(big) + 1, -(1 << 30)} {
		ws := workspace(t)
		r := mk(ws, "u")
		r.PID = &pid
		save(t, ws, r)
		pending(t, ws, "u")
		var calls []int
		got, err := cancel(ws, r, noonClock, func(p int, _ syscall.Signal) error { calls = append(calls, p); return nil })
		if err != nil || got.Status != StatusCancelled || len(calls) != 0 {
			t.Errorf("pid %d: %+v %v signalled %v", pid, got, err, calls)
		}
	}
}

func TestCancelSignalsOnlyTheProcessItStarted(t *testing.T) { // oracle spawn.ts:153-160
	needPS(t)
	live := child(t).Process.Pid
	token, _ := ProcessStartToken(live)
	for name, c := range map[string]struct {
		token  *string
		exit   string
		calls  []int
		status BgStatus
	}{
		"another process owns the pid":    {sp("not its token"), "", nil, StatusCancelled},
		"the start token matches":         {&token, "", []int{-live}, StatusCancelled},
		"a finished job is not signalled": {nil, "0", nil, StatusComplete},
	} {
		ws := workspace(t)
		r := mk(ws, "t")
		r.PID, r.StartToken = &live, c.token
		save(t, ws, r)
		put(t, ExitPath(ws, "t"), c.exit)
		if err := os.Chtimes(ExitPath(ws, "t"), noon(), noon()); err != nil {
			t.Fatal(err)
		}
		var calls []int
		got, err := cancel(ws, r, noonClock, func(p int, _ syscall.Signal) error { calls = append(calls, p); return nil })
		if err != nil || got.Status != c.status || !reflect.DeepEqual(calls, c.calls) {
			t.Errorf("%s: %+v %v signalled %v, want %v %v", name, got, err, calls, c.status, c.calls)
		}
	}
}

func TestCancelActsOnTheWorkspaceNotTheRecordsCwd(t *testing.T) { // registry.go: the caller's workspace, never the cwd field of a record
	ws, other := workspace(t), workspace(t)
	save(t, other, mk(other, "w"))
	put(t, filepath.Join(BGDir(other), LedgerFile), "{\"at\":\"x\"}\n")
	record, rows := get(t, RecordPath(other, "w")), get(t, filepath.Join(BGDir(other), LedgerFile))
	r := mk(other, "w") // its cwd names the other workspace
	save(t, ws, r)
	pending(t, ws, "w")
	got, err := cancel(ws, r, noonClock, func(int, syscall.Signal) error { t.Error("signalled"); return nil })
	if err != nil || got.Status != StatusCancelled || !strings.Contains(strings.Join(ledger(t, ws), "\n"), "\"event\":\"cancelled\"") ||
		get(t, RecordPath(other, "w")) != record || get(t, filepath.Join(BGDir(other), LedgerFile)) != rows {
		t.Errorf("%+v %v", got, err)
	}
	if onDisk, _ := ReadRecord(ws, "w"); onDisk.Status != StatusCancelled {
		t.Errorf("the calling workspace's record is %+v", onDisk)
	}
}

func TestCancelFallsBackToThePid(t *testing.T) { // oracle spawn.ts:164-170
	ws, live := workspace(t), child(t).Process.Pid
	r := mk(ws, "f")
	r.PID = &live
	save(t, ws, r)
	type call struct {
		pid int
		sig syscall.Signal
	}
	var calls []call
	got, err := cancel(ws, r, noonClock, func(p int, sig syscall.Signal) error {
		calls = append(calls, call{p, sig})
		if p < 0 {
			return syscall.EPERM
		}
		return nil
	})
	if err != nil || got.Status != StatusCancelled || !reflect.DeepEqual(calls, []call{{-live, syscall.SIGTERM}, {live, syscall.SIGTERM}}) {
		t.Errorf("%+v %v signalled %v", got, err, calls)
	}
}
