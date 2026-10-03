// spawn.go is the Go form of CXC v0.2.40 bg-wake/src/spawn.ts: the detached start of a background job and its cancel. It sits on store.go
// (the confined files of .crw/bg) and registry.go (the record and its reconciliation) and uses their functions.
//
// On POSIX the spawned shell records its own exit code, so no watcher process exists:
//
//	( cmd ) > out 2>&1; printf %s $? > exit.tmp && mv -- exit.tmp exit
//
// Behaviour is the oracle's. The differences are these. The workspace is the first argument and the record's cwd (store.go's rule), the
// clock is the caller's, and an error is returned where the oracle throws. Four differences come from the security checklist of the
// issue, each with a test:
//   - The shell is handed the absolute paths the store judged. The oracle passed OutPath and ExitPath as they were while the shell ran
//     with the workspace as its directory, so a relative workspace sent its redirects to a place the store never confined.
//   - <id>.exit.tmp is removed with the other files before the start, as the oracle removed .exit and .out only: a link planted there was
//     written through by "printf >" and truncated its target.
//   - mv takes "--", so a relative path that starts with "-" is not read as an option.
//   - Cancel signals a pid only inside 1 < pid <= MaxInt32. A record is data, and kill(-pid) of pid 0 is the caller's own process group,
//     of 1 every process the user may signal; the kernel's pid_t is 32 bits, so a larger number is cut to one of them.
//
// Two oracle behaviours have no Go spelling: Node starts the shell through libuv, which reaps it, so a Wait goroutine does that here,
// and a shell that cannot start is reported by an error event after spawn returns (a Start error here, settled in the same order).
// The Windows branch of buildShell is the oracle's text: this package uses syscall.Kill and O_NOFOLLOW and does not build there.

package job

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrNULArgument is what Node's spawn throws (ERR_INVALID_ARG_VALUE) for an argument that holds a NUL byte.
const ErrNULArgument = sentinel("a command argument contains a NUL byte")

// RunOptions is spawn.ts RunOptions without cwd, which is the workspace argument of RunBackground. An empty ID is no seed, as in JavaScript.
type RunOptions struct {
	SessionID *string
	Command   []string
	Note      *string
	ID        string
}

// NewID is a short id for a new job, "bg" and six hex digits, re-rolled while a record has it; the seed is tried first (newId). After
// twenty collisions it is "bg" and the time in base 36.
func NewID(ws, seed string, clock func() time.Time) string {
	return newID(ws, seed, func(b []byte) { _, _ = rand.Read(b) }, clock)
}

func newID(ws, seed string, random func([]byte), clock func() time.Time) string {
	for i := 0; i < 20; i++ {
		candidate := seed
		if i > 0 || seed == "" {
			var b [3]byte
			random(b[:])
			candidate = "bg" + hex.EncodeToString(b[:])
		}
		if !RecordExists(ws, candidate) {
			return candidate
		}
	}
	return "bg" + strconv.FormatInt(clock().UnixMilli(), 36)
}

// shellQuotePosix is the argument as one word of a POSIX shell: inside single quotes only the quote itself is special.
func shellQuotePosix(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// BuildWindowsHelper is the Windows second hop, text for node (buildWindowsHelper): it opens the output file, runs the command with
// that file as its output and error, and records the exit code the way the POSIX shell does.
func BuildWindowsHelper() string {
	return strings.Join([]string{
		"'use strict';",
		"const { spawn } = require('node:child_process');",
		"const fs = require('node:fs');",
		"const [outPath, exitPath, ...cmd] = process.argv.slice(2);",
		"function record(code) {",
		"  try {",
		"    fs.writeFileSync(exitPath + '.tmp', String(code));",
		"    fs.renameSync(exitPath + '.tmp', exitPath);",
		"  } catch (err) {}",
		"}",
		"let fd = null;",
		"try { fd = fs.openSync(outPath, 'a'); } catch (err) {}",
		"const sink = fd === null ? 'ignore' : fd;",
		"let child = null;",
		"try {",
		"  child = spawn(cmd[0], cmd.slice(1), { stdio: ['ignore', sink, sink], windowsHide: true });",
		"} catch (err) {",
		"  // A synchronous throw would otherwise kill the helper before any listener runs,",
		"  // leaving no exit file at all.",
		"  record(127);",
		"  process.exit(0);",
		"}",
		"if (fd !== null) { try { fs.closeSync(fd); } catch (err) {} }",
		"let done = false;",
		"function finish(code) { if (done) return; done = true; record(code); process.exit(0); }",
		"// ENOENT arrives here. A cmd builtin is not an executable, but Windows does not",
		"// always report it as ENOENT: the measured `echo` came back as a normal exit 1.",
		"child.on('error', function () { finish(127); });",
		"child.on('exit', function (code, signal) { finish(code === null ? (signal ? 129 : 1) : code); });",
		"",
	}, "\n")
}

// BuildShell is the file and arguments that start the job (buildShell). An empty helperPath is the oracle's default, out + ".helper.cjs".
func BuildShell(command []string, out, exit, helperPath string) (string, []string, error) {
	return buildShell(runtime.GOOS, "node", command, out, exit, helperPath)
}

// buildShell takes the platform and the path of node (process.execPath) as arguments, so the Windows branch can be checked here.
func buildShell(goos, nodePath string, command []string, out, exit, helperPath string) (string, []string, error) {
	if goos == "windows" {
		if helperPath == "" {
			helperPath = out + ".helper.cjs"
		}
		if err := os.WriteFile(helperPath, []byte(BuildWindowsHelper()), 0o666); err != nil {
			return "", nil, err
		}
		// argv goes straight to CreateProcess, so there are no cmd quoting rules to get wrong.
		return nodePath, append([]string{helperPath, out, exit}, command...), nil
	}
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = shellQuotePosix(arg)
	}
	// tmp + mv so a reader never sees a half-written exit code. A SUBSHELL, not a brace group: a command such as "exit 3" in braces
	// would end the wrapper itself and the exit code would never be written.
	tmp := shellQuotePosix(exit + ".tmp")
	script := "( " + strings.Join(quoted, " ") + " ) > " + shellQuotePosix(out) + " 2>&1; printf %s $? > " + tmp + " && mv -- " + tmp + " " + shellQuotePosix(exit)
	return "/bin/sh", []string{"-c", script}, nil
}

// RunBackground starts the command detached and returns its record, which is already written as running (runBackground). A shell that
// cannot start for a reason Node reports on the error event is not an error: the record is settled failed, as the oracle's error
// listener did, and the running record is returned; any other start failure is the error, with no record, as Node throws it.
func RunBackground(ws string, opts RunOptions, clock func() time.Time) (BgRecord, error) {
	return runBackground(ws, opts, clock, func(cmd *exec.Cmd) error { return cmd.Start() })
}

// runBackground is RunBackground with the call that starts the shell as an argument, so a check can see that it comes after the
// removals and the NUL check, and can make the shell fail to start.
func runBackground(ws string, opts RunOptions, clock func() time.Time, start func(*exec.Cmd) error) (BgRecord, error) {
	if _, err := EnsureDir(ws); err != nil {
		return BgRecord{}, err
	}
	id := NewID(ws, opts.ID, clock)
	out, outErr := filepath.Abs(OutPath(ws, id))
	exit, exitErr := filepath.Abs(ExitPath(ws, id))
	if err := errors.Join(outErr, exitErr); err != nil {
		return BgRecord{}, err
	}
	// A stale exit file from a reused id would be read as an instant completion, and a stale output file would be appended to on Windows
	// while POSIX truncates. A path outside the store is refused here, before anything is started.
	for _, stale := range []string{exit, exit + ".tmp", out} {
		if err := RemovePath(ws, stale); err != nil {
			return BgRecord{}, err
		}
	}
	file, args, err := BuildShell(opts.Command, out, exit, "")
	if err != nil {
		return BgRecord{}, err
	}
	if slices.ContainsFunc(append([]string{file}, args...), func(arg string) bool { return strings.IndexByte(arg, 0) >= 0 }) {
		return BgRecord{}, ErrNULArgument
	}
	cmd := exec.Command(file, args...)
	cmd.Dir = ws
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detached: a session and a process group of its own, which Cancel signals
	var pid *int
	var token *string
	startErr := start(cmd)
	if startErr != nil && !deferredStartError(startErr) {
		return BgRecord{}, startErr // Node throws these from spawn itself, before any record
	}
	if startErr == nil && cmd.Process != nil {
		p := cmd.Process.Pid
		pid = &p
		if t, ok := ProcessStartToken(p); ok {
			token = &t
		}
		go func() { _ = cmd.Wait() }() // libuv reaps what Node starts; without this a finished shell stays a zombie that still answers signal 0
	}
	rec := BgRecord{ID: id, SessionID: opts.SessionID, Cwd: ws, Command: opts.Command, Note: opts.Note, PID: pid, StartToken: token,
		Status: StatusRunning, StartedAt: clock().UTC().Format(isoLayout)}
	if err := writeRecord(ws, rec, clock); err != nil {
		return BgRecord{}, err
	}
	_ = appendLedger(ws, Event{{"event", "registered"}, {"id", id}, {"pid", opt(pid)}, {"command", opts.Command}}, clock)
	if pid == nil {
		// The oracle's error listener ran after all this: a shell that never started will never write an exit file.
		if cur, ok := ReadRecord(ws, id); ok && cur.Status == StatusRunning {
			ended := clock().UTC().Format(isoLayout)
			cur.Status, cur.ExitCode, cur.EndedAt = StatusFailed, nil, &ended
			if err := writeRecord(ws, cur, clock); err != nil {
				return rec, err // the oracle's listener throws, which ends the process
			}
			_ = appendLedger(ws, Event{{"event", "completed"}, {"id", id}, {"exitCode", nil}, {"detail", "spawn failed"}}, clock)
		}
	}
	return rec, nil
}

// deferredStartError is whether Node reports this start failure on the child's error event, after spawn has returned (ENOENT for a
// missing shell or working directory, EACCES, EAGAIN, EMFILE, ENFILE). Any other (E2BIG, ENOMEM, ...) it throws from spawn itself.
func deferredStartError(err error) bool {
	for _, errno := range []syscall.Errno{syscall.ENOENT, syscall.EACCES, syscall.EAGAIN, syscall.EMFILE, syscall.ENFILE} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// Cancel stops a running job and records it cancelled (cancel). It reconciles first, so a finished job keeps its real outcome, and it
// signals only the process it started: the start token stops a recycled pid's group from being signalled.
func Cancel(ws string, input BgRecord, clock func() time.Time) (BgRecord, error) {
	return cancel(ws, input, clock, syscall.Kill)
}

// cancel takes the signalling call as an argument, so the guard on the pid can be checked without signalling anything.
func cancel(ws string, input BgRecord, clock func() time.Time, kill func(pid int, sig syscall.Signal) error) (BgRecord, error) {
	now := clock().UTC().Format(isoLayout)
	rec, err := Reconcile(ws, input, clock)
	if err != nil || rec.Status != StatusRunning {
		return rec, err
	}
	if pid := rec.PID; pid != nil && *pid > 1 && *pid <= math.MaxInt32 && PidAlive(*pid) && (rec.StartToken == nil || startsAt(*pid, *rec.StartToken)) {
		// A negative pid is the detached process group, so the job's children die with the shell.
		if kill(-*pid, syscall.SIGTERM) != nil {
			_ = kill(*pid, syscall.SIGTERM) // the group is gone or not ours to signal: the leader alone, or already gone
		}
	}
	next := rec
	next.Status = StatusCancelled
	if next.EndedAt == nil {
		next.EndedAt = &now
	}
	if err := writeRecord(ws, next, clock); err != nil {
		return rec, err
	}
	_ = appendLedger(ws, Event{{"event", "cancelled"}, {"id", rec.ID}}, clock)
	return next, nil
}

// startsAt is whether the process with that pid started when the record says (ps prints it).
func startsAt(pid int, token string) bool {
	got, ok := ProcessStartToken(pid)
	return ok && got == token
}
