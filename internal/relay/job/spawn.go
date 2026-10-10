// spawn.go is the Go form of CXC v0.2.40 bg-wake/src/spawn.ts: the detached start of a background job and its cancel. It sits on store.go
// (the confined files of .crw/bg) and registry.go (the record and its reconciliation) and uses their functions.
//
// On POSIX the spawned shell records its own exit code, so no watcher process exists:
//
//	( cmd ) > out 2>&1; printf %s $? > exit.tmp && mv -- exit.tmp exit
//
// behind the launch gate (launchGate), which waits for the launcher's "go" on descriptor 3.
//
// Behaviour is the oracle's. The differences are these. The workspace is the first argument and the record's cwd (store.go's rule), the
// clock is the caller's, and an error is returned where the oracle throws. Five differences come from the security checklist of the
// issue and the data-loss rule of the parity revision, each with a test:
//   - The shell is handed the absolute paths the store judged. The oracle passed OutPath and ExitPath as they were while the shell ran
//     with the workspace as its directory, so a relative workspace sent its redirects to a place the store never confined.
//   - <id>.exit.tmp is removed with the other files before the start, as the oracle removed .exit and .out only: a link planted there was
//     written through by "printf >" and truncated its target.
//   - mv takes "--", so a relative path that starts with "-" is not read as an option.
//   - Cancel signals a pid only inside 1 < pid <= MaxInt32. A record is data, and kill(-pid) of pid 0 is the caller's own process group,
//     of 1 every process the user may signal; Node rejects a larger pid before the call, but a Go int such as 2^32 or 2^32+1 is cut by
//     the kernel's 32-bit pid_t to 0 or 1.
//   - After twenty collisions the fallback id is asked again and numbered while a record has it; the oracle returned it unchecked, which
//     could replace the record of a job that has it.
//
// CRW-1155 changed the start and the cancel: the record is reserved before the shell starts, the shell waits at a launch gate until
// the record names its pid and start token, and cancel signals only a process it can prove is the job and records cancelled only
// once the job's process group is gone (RunBackground, Cancel; docs/port-cxc/known-defects/CRW-1155.md).
//
// One more difference comes from CRW-1081: the job outlives its caller, so the Go start marks every descriptor above 2 that the caller
// holds close-on-exec before the shell is started (fdsweep, the sweep of the service start) and refuses to start when it cannot.
//
// Two oracle behaviours have no Go spelling: Node starts the shell through libuv, which reaps it, so a Wait goroutine does that here,
// and a shell that cannot start is reported by an error event after spawn returns (a Start error here, settled in the same order).
// The Windows branch of buildShell is the oracle's text: this package uses syscall.Kill and O_NOFOLLOW and does not build there.

package job

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/fdsweep"
)

// ErrNULArgument is what Node's spawn throws (ERR_INVALID_ARG_VALUE) for an argument that holds a NUL byte.
const ErrNULArgument = sentinel("a command argument contains a NUL byte")

// markInherited is the sweep that keeps the caller's descriptors out of the job (internal/relay/fdsweep, shared with the service start).
// It is a variable so that a check can make it fail.
var markInherited = fdsweep.MarkInherited

// RunOptions is spawn.ts RunOptions without cwd, which is the workspace argument of RunBackground. An empty ID is no seed, as in JavaScript.
type RunOptions struct {
	SessionID *string
	Command   []string
	Note      *string
	ID        string
}

// NewID is a short id for a new job, "bg" and six hex digits, re-rolled while a record has it; the seed is tried first (newId). After
// twenty collisions it is "bg" and the time in base 36, numbered ("-2", "-3") while a record has it, which the oracle did not ask.
// It only looks: RunBackground draws its id with the same rule and publishes the record in the same step (reserve), so two runs never
// draw one id.
func NewID(ws, seed string, clock func() time.Time) string {
	return newID(ws, seed, func(b []byte) { _, _ = rand.Read(b) }, clock)
}

func newID(ws, seed string, random func([]byte), clock func() time.Time) string {
	id, _ := drawID(seed, random, clock, func(id string) (bool, error) { return RecordExists(ws, id), nil })
	return id
}

// drawID is the rule of newId with the question "is this id taken" as an argument; an error from it ends the draw.
func drawID(seed string, random func([]byte), clock func() time.Time, taken func(string) (bool, error)) (string, error) {
	for i := 0; i < 20; i++ {
		candidate := seed
		if i > 0 || seed == "" {
			var b [3]byte
			random(b[:])
			candidate = "bg" + hex.EncodeToString(b[:])
		}
		if t, err := taken(candidate); err != nil || !t {
			return candidate, err
		}
	}
	base := "bg" + strconv.FormatInt(clock().UnixMilli(), 36)
	id := base
	for n := 2; ; n++ {
		t, err := taken(id)
		if err != nil || !t {
			return id, err
		}
		id = base + "-" + strconv.Itoa(n)
	}
}

// reserve draws the id of a new job and publishes its first record under it in one step: the record is written to a temporary file
// and linked to <id>.json, which fails when that name exists, so the existence check and the publication cannot be split by another
// run (CRW-1155). The record says running with no pid: nothing has been started under it yet.
func reserve(ws, seed string, rec BgRecord, clock func() time.Time) (BgRecord, error) {
	id, err := drawID(seed, func(b []byte) { _, _ = rand.Read(b) }, clock, func(id string) (bool, error) {
		rec.ID = id
		err := publishExclusive(ws, rec, clock)
		if errors.Is(err, os.ErrExist) {
			return true, nil
		}
		return false, err
	})
	rec.ID = id
	return rec, err
}

// publishExclusive writes the record to a temporary file beside it and links that to the record's name, which fails when the name is
// taken (os.ErrExist); the temporary file is removed either way.
func publishExclusive(ws string, rec BgRecord, clock func() time.Time) error {
	b, err := encode(rec)
	if err != nil {
		return err
	}
	path, err := confined(ws, RecordPath(ws, rec.ID))
	if err != nil {
		return err
	}
	tmp, err := writeTemp(path, string(b), os.Getpid(), clock().UnixMilli())
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	return os.Link(tmp, path)
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
// listener did, and that failed record is returned (CRW-1134); any other start failure is the error, with no record, as Node throws it.
//
// The launch is a handshake (CRW-1155). The record is reserved under its id before anything starts, the shell starts and waits on a
// pipe, its pid and start token are published in the record, and only then is it told to run the command. A start whose identity
// cannot be read, a record that cannot be published and a reservation that was cancelled in the meantime end the shell before it
// has run anything, and a caller that dies before it has published closes the pipe, so the shell ends the same way. No command runs
// under a process no record names.
func RunBackground(ws string, opts RunOptions, clock func() time.Time) (BgRecord, error) {
	return runBackground(ws, opts, clock, func(cmd *exec.Cmd) error { return cmd.Start() })
}

// ErrEmptyCommand is a job without a command: the oracle built "( ) > out", which the shell refuses as a syntax error (CRW-1134).
const ErrEmptyCommand = sentinel("a background job needs a command")

// ErrNoStartIdentity is a started shell whose start token cannot be read: its record could never prove later that a pid is still the
// job, so the shell is ended before it runs the command (CRW-1155).
const ErrNoStartIdentity = sentinel("the start identity of the job's shell cannot be read, so the job was not run")

// ErrLaunchWithdrawn is a reservation that another writer changed (a cancel) before the start was published; nothing was run.
const ErrLaunchWithdrawn = sentinel("the job's record changed before its start was published, so the job was not run")

// launchGate is what the shell runs before the command: it waits for "go" on descriptor 3, which the launcher writes once the record
// names the shell, and closes it; end of input or anything else ends the shell, with no exit file, before the command runs.
const launchGate = "IFS= read -r crw_launch <&3 && [ \"$crw_launch\" = go ] || exit 125; exec 3<&-; "

// runBackground is RunBackground with the call that starts the shell as an argument, so a check can see that it comes after the
// removals and the NUL check, and can make the shell fail to start.
func runBackground(ws string, opts RunOptions, clock func() time.Time, start func(*exec.Cmd) error) (BgRecord, error) {
	if len(opts.Command) == 0 {
		return BgRecord{}, ErrEmptyCommand
	}
	if _, err := EnsureDir(ws); err != nil {
		return BgRecord{}, err
	}
	rec, err := reserve(ws, opts.ID, BgRecord{SessionID: opts.SessionID, Cwd: ws, Command: opts.Command, Note: opts.Note, Status: StatusRunning,
		StartedAt: clock().UTC().Format(isoLayout), Extra: []Member{{launchingKey, true}}}, clock)
	if err != nil {
		return BgRecord{}, err
	}
	id := rec.ID
	withdraw := func(err error) (BgRecord, error) { _ = RemovePath(ws, RecordPath(ws, id)); return BgRecord{}, err }
	out, outErr := filepath.Abs(OutPath(ws, id))
	exit, exitErr := filepath.Abs(ExitPath(ws, id))
	if err := errors.Join(outErr, exitErr); err != nil {
		return withdraw(err)
	}
	// A stale exit file from a reused id would be read as an instant completion, and a stale output file would be appended to on Windows
	// while POSIX truncates. A path outside the store is refused here, before anything is started.
	for _, stale := range []string{exit, exit + ".tmp", out} {
		if err := RemovePath(ws, stale); err != nil {
			return withdraw(err)
		}
	}
	file, args, err := BuildShell(opts.Command, out, exit, "")
	if err != nil {
		return withdraw(err)
	}
	if slices.ContainsFunc(append([]string{file}, args...), func(arg string) bool { return strings.IndexByte(arg, 0) >= 0 }) {
		return withdraw(ErrNULArgument)
	}
	args[len(args)-1] = launchGate + args[len(args)-1]
	cmd := exec.Command(file, args...)
	cmd.Dir = ws
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detached: a session and a process group of its own, which Cancel signals
	// The caller's descriptors without close-on-exec would reach the job, which outlives the caller (CRW-1081): they are marked here, and a
	// start that cannot be shown to keep them out is refused. ExtraFiles, set up by the fork itself, are not touched.
	if err := markInherited(); err != nil {
		return withdraw(fmt.Errorf("start refused, the caller's open descriptors cannot all be kept out of the job: %w", err))
	}
	gateR, gateW, err := os.Pipe() // both ends close-on-exec; the fork hands the read end to the shell as descriptor 3
	if err != nil {
		return withdraw(err)
	}
	defer func() { _ = gateW.Close() }()
	cmd.ExtraFiles = []*os.File{gateR}
	startErr := start(cmd)
	_ = gateR.Close()
	if startErr != nil && !deferredStartError(startErr) {
		return withdraw(startErr) // Node throws these from spawn itself, before any record
	}
	if startErr != nil || cmd.Process == nil {
		// The oracle's error listener: a shell that never started will never write an exit file.
		_ = appendLedger(ws, Event{{"event", "registered"}, {"id", id}, {"pid", nil}, {"command", opts.Command}}, clock)
		return failLaunch(ws, rec, "spawn failed", nil, clock)
	}
	pid := cmd.Process.Pid
	stopShell := func() { _ = gateW.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() } // the shell is still waiting at the gate
	token, ok := ProcessStartToken(pid)
	if !ok {
		stopShell()
		_ = appendLedger(ws, Event{{"event", "registered"}, {"id", id}, {"pid", nil}, {"command", opts.Command}}, clock)
		return failLaunch(ws, rec, "start identity unreadable", ErrNoStartIdentity, clock)
	}
	published, err := update(ws, id, clock, false, func(cur BgRecord) change {
		if cur.Status != StatusRunning || cur.PID != nil {
			return change{}
		}
		next := cur
		next.PID, next.StartToken = &pid, &token
		return change{next: next, write: true}
	})
	if err == nil && (published.PID == nil || *published.PID != pid) {
		err = ErrLaunchWithdrawn
	}
	if err != nil {
		stopShell()
		if errors.Is(err, ErrLaunchWithdrawn) {
			return published, err
		}
		_ = appendLedger(ws, Event{{"event", "registered"}, {"id", id}, {"pid", nil}, {"command", opts.Command}}, clock)
		failed, _ := failLaunch(ws, rec, "record not published", nil, clock)
		return failed, err
	}
	_ = appendLedger(ws, Event{{"event", "registered"}, {"id", id}, {"pid", pid}, {"command", opts.Command}}, clock)
	if _, err := gateW.Write([]byte("go\n")); err != nil {
		stopShell() // the shell ended before it read the gate, so the command never ran
		return failLaunch(ws, published, "launch gate not read", fmt.Errorf("the job's shell did not take its start: %w", err), clock)
	}
	_ = gateW.Close()
	go func() { _ = cmd.Wait() }() // libuv reaps what Node starts; without this a finished shell stays a zombie that still answers signal 0
	return published, nil
}

// failLaunch settles a launch that ran nothing: the record says failed with no exit code, and a completed row names why. The record
// is returned as it is on disk then, with launchErr.
func failLaunch(ws string, rec BgRecord, detail string, launchErr error, clock func() time.Time) (BgRecord, error) {
	ended := clock().UTC().Format(isoLayout)
	failed, err := update(ws, rec.ID, clock, false, func(cur BgRecord) change {
		if cur.Status != StatusRunning {
			return change{}
		}
		next := cur
		next.Status, next.ExitCode, next.EndedAt = StatusFailed, nil, &ended
		return change{next: next, event: Event{{"event", "completed"}, {"id", rec.ID}, {"exitCode", nil}, {"detail", detail}}, write: true}
	})
	if err != nil {
		return rec, errors.Join(launchErr, err) // the oracle's listener throws, which ends the process
	}
	return failed, launchErr
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

// Cancel stops a running job (cancel). It reconciles first, so a finished job keeps its real outcome. It signals only a job whose
// shell it can prove is still the recorded one: a pid in range whose start token is recorded and still matches, shown again under the
// store lock right before the signal. A record without a token (an old record, or one edited by hand) is refused with ErrOwnerUnproven
// and nothing is signalled (CRW-1155).
//
// The request is recorded first (cancellation-requested), then the job's process group is sent SIGTERM, or SIGKILL when a cancel was
// already requested. The record says cancelled only once no process of the group is left, which cancel waits for up to cancelWait;
// a group still alive then keeps the request, which Reconcile settles once the group has gone. A signal that fails is returned as
// the error, with the request recorded.
func Cancel(ws string, input BgRecord, clock func() time.Time) (BgRecord, error) {
	return cancel(ws, input, clock, syscall.Kill)
}

// cancelWait bounds how long cancel waits to see the job's process group gone.
var cancelWait = 2 * time.Second

// ErrOwnerUnproven is a cancel that signalled nothing because the record cannot prove the pid is still its job.
type ErrOwnerUnproven struct {
	ID  string
	PID *int
}

func (e ErrOwnerUnproven) Error() string {
	pid := "없음"
	if e.PID != nil {
		pid = strconv.Itoa(*e.PID)
	}
	return "작업 " + e.ID + "의 프로세스(pid " + pid + ")가 이 작업인지 확인할 수 없어 신호를 보내지 않았습니다. " +
		"`ps -o pid,pgid,lstart,args -p <pid>`로 직접 확인해 멈추면 다음 조회(`crw relay job get " + e.ID + "`)가 기록을 정리합니다."
}

// SignalError is a signal to the job's process group that failed; the request stays recorded.
type SignalError struct {
	ID     string
	Signal syscall.Signal
	Err    error
}

func (e SignalError) Error() string {
	return "작업 " + e.ID + "의 프로세스 그룹에 " + e.Signal.String() + "를 보내지 못했습니다: " + e.Err.Error()
}

func (e SignalError) Unwrap() error { return e.Err }

// cancel takes the signalling call as an argument, so the guard on the pid can be checked without signalling anything. Signal 0
// through the same call asks whether the group is still there.
func cancel(ws string, input BgRecord, clock func() time.Time, kill func(pid int, sig syscall.Signal) error) (BgRecord, error) {
	now := clock().UTC().Format(isoLayout)
	rec, err := Reconcile(ws, input, clock)
	if err != nil || IsTerminal(rec.Status) {
		return rec, err
	}
	if rec.PID == nil {
		// Nothing was started under this record yet (a reservation, or a shell that never started): there is no process to stop, and a
		// launch still in progress sees the cancel and does not run the command.
		out, err := finishCancel(ws, rec, now, clock, func(cur BgRecord) bool { return cur.PID == nil })
		if err == nil && out.PID != nil && !IsTerminal(out.Status) {
			// The launch published its pid between the read above and the lock: the record on disk is a started job now, so cancel
			// acts on that record (the pid is set, so this runs once more at most) instead of reporting a cancel that changed nothing.
			return cancel(ws, out, clock, kill)
		}
		return out, err
	}
	pid := *rec.PID
	if pid <= 1 || pid > math.MaxInt32 || rec.StartToken == nil {
		return rec, ErrOwnerUnproven{ID: rec.ID, PID: rec.PID}
	}
	// A requested cancel whose group is empty is over, whoever has the number now. Otherwise the proof is made under the store lock
	// (ownsGroup), right before the record is changed and the signal follows, so a wait for the lock cannot carry an old proof over to a
	// recycled pid.
	if rec.Status == StatusCancelRequested && errors.Is(kill(-pid, 0), syscall.ESRCH) {
		return finishCancel(ws, rec, now, clock, func(c BgRecord) bool { return c.Status == StatusCancelRequested && samePID(c.PID, rec.PID) })
	}
	sig := syscall.SIGTERM
	unproven := false
	cur, err := update(ws, rec.ID, clock, false, func(cur BgRecord) change {
		if !samePID(cur.PID, rec.PID) || IsTerminal(cur.Status) {
			return change{}
		}
		if !ownsGroup(cur) {
			unproven = true
			return change{}
		}
		if cur.Status == StatusCancelRequested {
			sig = syscall.SIGKILL // asked before, and still running
			return change{}
		}
		// The shell is shown alive now (ownsGroup matched its token): the members of its group are kept with the request, for the proof
		// of a later cancel (group.go).
		// A group that cannot be listed leaves no members, and a later cancel after the shell has ended is refused.
		next := cur
		if members, ok := observeGroup(pid, *cur.StartToken); ok {
			next = withGroup(cur, members)
		}
		next.Status = StatusCancelRequested
		return change{next: next, write: true}
	})
	if unproven {
		return cur, ErrOwnerUnproven{ID: rec.ID, PID: rec.PID}
	}
	if err != nil || cur.Status != StatusCancelRequested || !samePID(cur.PID, rec.PID) {
		return cur, err
	}
	// A negative pid is the detached process group, so the job's children are signalled with the shell.
	if err := kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = appendLedger(ws, Event{{"event", "cancel-requested"}, {"id", rec.ID}, {"signal", sig.String()}, {"error", err.Error()}}, clock)
		return cur, SignalError{ID: rec.ID, Signal: sig, Err: err}
	}
	for deadline := time.Now().Add(cancelWait); ; time.Sleep(20 * time.Millisecond) {
		if errors.Is(kill(-pid, 0), syscall.ESRCH) {
			return finishCancel(ws, cur, now, clock, func(c BgRecord) bool { return c.Status == StatusCancelRequested && samePID(c.PID, rec.PID) })
		}
		if time.Now().After(deadline) {
			break
		}
	}
	_ = appendLedger(ws, Event{{"event", "cancel-requested"}, {"id", rec.ID}, {"signal", sig.String()}}, clock)
	return cur, nil
}

// finishCancel records cancelled over the record as it is on disk, when ok says it is still the one cancel acted on.
func finishCancel(ws string, rec BgRecord, now string, clock func() time.Time, ok func(BgRecord) bool) (BgRecord, error) {
	return update(ws, rec.ID, clock, false, func(cur BgRecord) change {
		if IsTerminal(cur.Status) || !ok(cur) {
			return change{}
		}
		next := cur
		next.Status = StatusCancelled
		if next.EndedAt == nil {
			next.EndedAt = &now
		}
		return change{next: next, event: Event{{"event", "cancelled"}, {"id", rec.ID}}, write: true}
	})
}

// startsAt is whether the process with that pid started when the record says (ps prints it).
func startsAt(pid int, token string) bool {
	got, ok := ProcessStartToken(pid)
	return ok && got == token
}
