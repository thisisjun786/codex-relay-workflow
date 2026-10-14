package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/fdsweep"
)

// ProcessCommand is the hidden command of the run process: `crw flow _run [--state DIR] [--resume] RUN`. `crw flow start`,
// `crw flow resume --detach` and `crw flow answer` start it detached; it is not part of the usage.
const ProcessCommand = "_run"

// Exit statuses of the run process (besides the 0 and 1 of crw flow run: completed and script_error).
const (
	exitUnusable = 2
	exitLive     = 3
)

// SpawnRequest is the process to start.
type SpawnRequest struct {
	Spec   Spec
	State  string
	Resume bool
	// Dir is the working directory of the process: the one the command was run in (a run resolves its repository from it).
	Dir string
}

// DetachedCommand is the command SpawnDetached starts: the same executable, in a session of its own with no descriptor of the
// caller's, no input, and its output in <state>/flow/<run>/process.log. It is exported so that the way a run is detached is a
// thing a test reads.
func DetachedCommand(self string, req SpawnRequest, log *os.File) *exec.Cmd {
	args := []string{"flow", ProcessCommand, "--state", req.State}
	if req.Resume {
		args = append(args, "--resume")
	}
	args = append(args, req.Spec.RunID)
	cmd := exec.Command(self, args...)
	cmd.Dir = req.Dir
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

// executable is the program a run process is started from: this one. A variable so that a test starts the built binary instead of
// the test binary.
var executable = os.Executable

// markInherited keeps the caller's descriptors out of the process (internal/relay/fdsweep): a descriptor the caller holds, a lock
// it took with `exec 9>lock` for one, would be held by the run for as long as it lives. A variable so that a check can make it fail.
var markInherited = fdsweep.MarkInherited

// SpawnDetached starts the process of a run and lets go of it. The caller's descriptors are marked close-on-exec first; when that
// cannot be done the run is not started.
func SpawnDetached(ctx context.Context, req SpawnRequest) (int, error) {
	self, err := executable()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(RunDir(req.State, req.Spec.RunID), 0o700); err != nil {
		return 0, err
	}
	log, err := os.OpenFile(filepath.Join(RunDir(req.State, req.Spec.RunID), "process.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return 0, err
	}
	defer log.Close()
	if err := markInherited(); err != nil {
		return 0, fmt.Errorf("the run is not started: %w", err)
	}
	cmd := DetachedCommand(self, req, log)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// AwaitLock waits until the process of a run holds the run lock, or has ended after leaving a result (a run that finished before
// the first look), and says so when the process ended without either. since is the time the process was started at.
func AwaitLock(ctx context.Context, state, run string, pid int, since time.Time) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		if LockHeld(state, run) {
			return nil
		}
		if processGone(pid) {
			// the lock may have been taken and released between two looks: a result written since the start says so
			if info, err := os.Stat(resultPath(state, run)); err == nil && !info.ModTime().Before(since.Add(-time.Second)) {
				return nil
			}
			return fmt.Errorf("process %d ended before it held the run: %s", pid, logTail(state, run))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process %d did not take the run lock in time: %s", pid, logTail(state, run))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// processGone says whether a process has ended: it does not exist, or it is a zombie its parent has not waited for (the command
// that started a run does not wait for it).
func processGone(pid int) bool {
	if pid <= 1 {
		return false
	}
	if raw, err := os.ReadFile("/proc/" + itoa(pid) + "/stat"); err == nil {
		// the state is the field after the command's closing parenthesis
		if i := bytes.LastIndexByte(raw, ')'); i >= 0 && i+2 < len(raw) {
			return raw[i+2] == 'Z' || raw[i+2] == 'X'
		}
	}
	return syscall.Kill(pid, 0) != nil
}

func logTail(state, run string) string {
	raw, err := os.ReadFile(filepath.Join(RunDir(state, run), "process.log"))
	if err != nil || len(raw) == 0 {
		return "(no log)"
	}
	if len(raw) > 600 {
		raw = raw[len(raw)-600:]
	}
	return string(bytes.TrimSpace(raw))
}

// process is `crw flow _run`: the process of a run. It holds the run lock for as long as it works, runs the workflow of the run's
// spec on the engine over the world it opens (a new run, or with --resume the journal an earlier process left), pauses on SIGUSR1
// and at the run's deadline, ends the run on SIGTERM, SIGINT and SIGHUP, and leaves the result in <state>/flow/<run>/result.json.
func (c *command) process(args []string) int {
	fs, cm := c.flags()
	resume := fs.Bool("resume", false, "go on with the journal an earlier process of the run left")
	positional, code, ok := parse(fs, args)
	if !ok {
		return code
	}
	if len(positional) != 1 {
		return c.usage("exactly one run id is required")
	}
	sel, err := ResolveState(cm.state, cm.socket)
	if err != nil {
		return c.fail("%v", err)
	}
	spec, err := ReadSpec(sel.Path, positional[0])
	if err != nil {
		return c.fail("%v", err)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(sigs)
	return c.execute(spec, sel.Path, *resume, sigs)
}

// execute runs a spec as this process: the lock, the world, the engine, the result. sigs delivers the signals the process answers.
func (c *command) execute(spec Spec, state string, resume bool, sigs <-chan os.Signal) int {
	// a session run's process holds its process lock from before the run lock until after it (its started row names the token)
	process := c.processLock(state, spec)
	defer process.Release()
	lock, err := TakeLock(state, spec.RunID, "_run", c.now())
	if err != nil {
		fmt.Fprintf(c.stderr, "crw flow %s: %v\n", c.name, err)
		if errors.Is(err, ErrRunLive) {
			return exitLive
		}
		return exitUnusable
	}
	defer lock.Release()
	src, err := spec.Load()
	if err != nil {
		return c.fail("the workflow of run %s cannot be loaded: %v", spec.RunID, err)
	}
	if c.World == nil {
		return c.fail("this build installs no world for a run process")
	}
	world, err := c.World(c.ctx, WorldRequest{Spec: spec, State: state, Resume: resume})
	if err != nil {
		return c.fail("run %s: %v", spec.RunID, err)
	}
	closeWorld := func() {
		if world.Close != nil {
			if err := world.Close(); err != nil {
				fmt.Fprintf(c.stderr, "crw flow %s: error: the rows of the run could not be stored: %v\n", c.name, err)
			}
		}
	}
	defer closeWorld()
	table := world.Policy
	if table == nil {
		if table, _, err = engine.LoadPolicy(); err != nil {
			return c.fail("%v", err)
		}
	}
	args := compactJSON(spec.Args)
	budget, spent := spec.TokenBudget, int64(0)
	if resume {
		if why := notTheRuns(world.Journal, spec.RunID, src.Program.Digest, args); why != "" {
			return c.fail("%s", why)
		}
		// the budget is the run's: what its earlier processes spent counts, and a run with nothing left is not resumed (CRW-1278)
		spent = engine.RunSpent(world.Journal.Events(), spec.RunID).Total
		if budget, err = runBudget(spec, spent, spec.TokenBudget); err != nil {
			return c.fail("%v", err)
		}
	}
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	ctl := &engine.Control{}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-sigs:
				if !ok {
					return
				}
				if sig == syscall.SIGUSR1 {
					ctl.Pause("operator", "crw flow pause")
					continue
				}
				cancel()
				return
			}
		}
	}()
	if spec.Deadline != "" {
		if at, err := time.Parse(time.RFC3339, spec.Deadline); err == nil {
			timer := time.AfterFunc(at.Sub(c.now()), func() { ctl.Pause("deadline", "the run's deadline "+spec.Deadline+" passed") })
			defer timer.Stop()
		}
	}
	cfg := engine.Config{
		RunID: spec.RunID, Backend: world.Backend, Policy: table, Journal: world.Journal, Args: args, Pool: spec.Pool, TokenBudget: budget, SpentBefore: spent,
		Ops: world.Ops, Prover: world.Prover, InputSnapshot: world.InputSnapshot, Workflows: src.Workflows, Control: ctl, OmitSeq: true,
		Upward: world.Upward, Session: world.Session,
	}
	if resume {
		cfg.Resume = &engine.ResumeOptions{}
	}
	if s := world.Session; s != nil {
		// a session run keeps at most agents.max_threads - 1 calls in flight (the owner's own helper takes a slot), and its owner
		// thread holds the run's orchestrator line while the run lives (CRW-1316 §9). This process is the one that writes the line
		// while it holds the run, from its own record: right after its started row pinned the table it runs on, it writes the line
		// again when it is not the last one (after closing the open line of a run whose process is gone); `crw flow next` only reads
		// it (CRW-1372). The start wrote the first line before any process existed.
		cfg.Pool = sessionPool(spec.Pool, readCodexAgents(c.getenv).MaxThreads)
		s.Process = process.Token()
		s.Started = c.bindFromStarted(state, spec.RunID, s.Owner)
		defer func() {
			if _, err := closeOwner(spec.RunID, s.Owner); err != nil {
				fmt.Fprintf(c.stderr, "crw flow %s: warning: the orchestrator line of thread %s could not be closed: %v\n", c.name, s.Owner, err)
			}
		}()
	}
	res, err := engine.Run(ctx, src.Program, cfg)
	if err != nil {
		return c.fail("run %s: %v", spec.RunID, err)
	}
	closeWorld()
	world.Close = nil
	if err := writeResult(state, spec.RunID, res, c.now()); err != nil {
		fmt.Fprintf(c.stderr, "crw flow %s: error: the result of run %s could not be written: %v\n", c.name, spec.RunID, err)
	}
	if res.Status != engine.RunCompleted {
		return 1
	}
	return 0
}

// compactJSON is a JSON value without insignificant white space, "" and "null" for no value: the form the run's arguments are
// digested in, the same at the start of a run and at every resume.
func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

// Result is what a run process leaves in result.json: how the latest process of the run ended, with the value its script returned.
type Result struct {
	Run       string          `json:"run"`
	Status    string          `json:"status"`
	ErrorKind string          `json:"error_kind,omitempty"`
	Error     string          `json:"error,omitempty"`
	Value     json.RawMessage `json:"value"`
	Tokens    int64           `json:"tokens"`
	EndedAt   string          `json:"ended_at"`
}

func resultPath(state, run string) string { return filepath.Join(RunDir(state, run), "result.json") }

func writeResult(state, run string, res *engine.Result, now time.Time) error {
	out := Result{Run: run, Status: res.Status, ErrorKind: res.ErrorKind, Error: res.Error, Value: res.Value, Tokens: res.Spent, EndedAt: now.UTC().Format(time.RFC3339)}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	tmp := resultPath(state, run) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, resultPath(state, run))
}

// ReadResult reads the result a run process left; ok is false while no process has ended.
func ReadResult(state, run string) (Result, bool) {
	raw, err := os.ReadFile(resultPath(state, run))
	if err != nil {
		return Result{}, false
	}
	var r Result
	if json.Unmarshal(raw, &r) != nil {
		return Result{}, false
	}
	return r, true
}

// notTheRuns says why a script and arguments are not those of the run the journal holds ("" when they are): a resume replays the
// journal against the same script and arguments, and a run with another script is not this run's resume.
func notTheRuns(j engine.Journal, run, scriptDigest string, args json.RawMessage) string {
	for _, ev := range j.Events() {
		if ev.Kind != engine.KindRun || ev.RunID != run || ev.Event != "started" {
			continue
		}
		if len(args) == 0 {
			args = json.RawMessage("null")
		}
		switch {
		case str(ev.Data, "script_digest") != scriptDigest:
			return fmt.Sprintf("the script is not the one run %s ran (digest %s, the run's %s)", run, scriptDigest, str(ev.Data, "script_digest"))
		case str(ev.Data, "args_digest") != argsDigest(args):
			return fmt.Sprintf("the arguments are not the ones run %s ran", run)
		}
		return ""
	}
	return fmt.Sprintf("the store holds no run %s", run)
}

// argsDigest is the digest the engine records for a run's arguments.
func argsDigest(args []byte) string {
	sum := sha256.Sum256(args)
	return hex.EncodeToString(sum[:])
}
