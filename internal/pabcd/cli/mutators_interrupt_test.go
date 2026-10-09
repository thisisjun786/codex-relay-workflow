package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1074: loop steer, memory allow-write and scan record end on the first SIGINT, as the orchestrate
// mutations have since CRW-871. The context is read at the lock wait and once more with the lock held,
// immediately before the command's first write: a run they end writes nothing and has nothing to print; a
// run past its first write finishes and answers as before. The hooks below are function fields of the
// caller's own, never package state.

// mutatorsTree is the whole workspace, so any extra file or changed byte shows.
func mutatorsTree(t *testing.T, cwd string) map[string]string { return statusTree(t, cwd) }

func mutatorsSameTree(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("the workspace changed: %d entries before, %d after", len(before), len(after))
	}
	for name, body := range before {
		if after[name] != body {
			t.Fatalf("%s changed", name)
		}
	}
}

func TestMemoryAllowWriteInterruptEndedContextWritesNothing(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	before := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, code, err := RunMemoryCLIContext(ctx, MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd})
	if !errors.Is(err, context.Canceled) || out != "" || code != 0 {
		t.Fatalf("ended context: %q %d %v, want the context's error and nothing to print", out, code, err)
	}
	mutatorsSameTree(t, before, mutatorsTree(t, cwd))
}

func TestMemoryAllowWriteInterruptCancelledWithTheLockHeldWritesNothing(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	before := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := 0
	out, code, err := cliMemoryAllowWriteRun(ctx, MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd},
		func(cwd string, s state.State) error { writes++; return state.WriteState(cwd, s) }, cancel)
	if !errors.Is(err, context.Canceled) || out != "" || code != 0 || writes != 0 {
		t.Fatalf("cancelled with the lock held: %q %d %v, %d writes", out, code, err, writes)
	}
	mutatorsSameTree(t, before, mutatorsTree(t, cwd)) // the lock file is gone too
	if state.ReadState(cwd, "rec-s1").MemoryWriteGrant {
		t.Fatal("the cancelled run recorded the grant")
	}
}

func TestMemoryAllowWriteInterruptCancelledAfterTheWriteAnswersAsToday(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, code, err := cliMemoryAllowWriteRun(ctx, MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd},
		func(cwd string, s state.State) error { werr := state.WriteState(cwd, s); cancel(); return werr }, nil)
	if err != nil || code != 0 || !strings.Contains(out, "ONE memory write") {
		t.Fatalf("cancelled after the first write: %q %d %v", out, code, err)
	}
	if !state.ReadState(cwd, "rec-s1").MemoryWriteGrant {
		t.Fatal("the grant whose write had started did not land")
	}
}

func TestMemoryAllowWriteInterruptLiveContextIsTheControl(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd}
	wantOut, wantCode := RunMemoryCLI(args)
	out, code, err := RunMemoryCLIContext(ctx, args)
	if err != nil || out != wantOut || code != wantCode {
		t.Fatalf("live context: %q %d %v, want %q %d", out, code, err, wantOut, wantCode)
	}
}

func scanInterruptArgs(t *testing.T, cwd string) ScanCliArgs {
	t.Helper()
	p := ParseScanCliArgs([]string{"record", "--session", "s1", "--known", "goal=after-interrupt"}, cwd)
	if p.Error != "" {
		t.Fatal(p.Error)
	}
	return *p.Args
}

func scanInterruptWorkspace(t *testing.T) string {
	t.Helper()
	cwd := scanRecordWorkspace(t)
	if err := state.WriteState(cwd, state.DefaultState("s1", "")); err != nil {
		t.Fatal(err)
	}
	return cwd
}

func TestScanRecordInterruptEndedContextWritesNothing(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	before := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := RunScanCliContext(ctx, scanInterruptArgs(t, cwd))
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) {
		t.Fatalf("ended context: %+v %v, want the context's error and nothing to print", got, err)
	}
	mutatorsSameTree(t, before, mutatorsTree(t, cwd))
}

func TestScanRecordInterruptCancelledWithTheLockHeldWritesNothing(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	before := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	appends, writes := 0, 0
	got, err := cliScanRecordRunContext(ctx, scanInterruptArgs(t, cwd),
		func(cwd string, e state.InterviewEvent) error { appends++; return state.AppendInterviewEvent(cwd, e) },
		func(cwd string, s state.State) error { writes++; return state.WriteState(cwd, s) }, cancel)
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) || appends != 0 || writes != 0 {
		t.Fatalf("cancelled with the lock held: %+v %v, %d appends, %d writes", got, err, appends, writes)
	}
	mutatorsSameTree(t, before, mutatorsTree(t, cwd))
	if len(state.ReadInterviewEvents(cwd, "s1")) != 0 {
		t.Fatal("the cancelled run appended a scan_completed row")
	}
}

func TestScanRecordInterruptCancelledAfterTheFirstWriteAnswersAsToday(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := cliScanRecordRunContext(ctx, scanInterruptArgs(t, cwd),
		func(cwd string, e state.InterviewEvent) error {
			aerr := state.AppendInterviewEvent(cwd, e)
			cancel()
			return aerr
		},
		state.WriteState, nil)
	if err != nil || got.Code != 0 || !strings.HasPrefix(got.Output, "scan record: round 1 recorded for session s1") {
		t.Fatalf("cancelled after the first write: %+v %v", got, err)
	}
	if len(state.ReadInterviewEvents(cwd, "s1")) != 1 || state.ReadState(cwd, "s1").Interview == nil {
		t.Fatal("the round whose append had started did not land")
	}
}

func TestScanRecordInterruptLiveContextIsTheControl(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := scanInterruptArgs(t, cwd)
	got, err := RunScanCliContext(ctx, args)
	want := RunScanCli(args)
	// The second run is round 2; only the round number differs.
	if err != nil || got.Code != want.Code || strings.Replace(want.Output, "round 2", "round 1", 1) != got.Output {
		t.Fatalf("live context: %+v %v, plain run %+v", got, err, want)
	}
}

func TestScanRecordInterruptHelpPrintsUnderAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := RunScanCliContext(ctx, ScanCliArgs{Action: ScanActionHelp})
	if err != nil || got.Output != scanRecordHelp {
		t.Fatalf("help under an ended context: %+v %v", got, err)
	}
}

func TestLoopSteerInterruptEndedContextWritesNothing(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	tree := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	batch := `{"idempotencyKey":"k1","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`
	for _, source := range []string{batch, filepath.Join(cwd, "batch.json")} {
		if source != batch {
			if err := os.WriteFile(source, []byte(batch), 0o600); err != nil {
				t.Fatal(err)
			}
			tree = mutatorsTree(t, cwd)
		}
		args, err := ParseLoopCliArgs([]string{"steer", "--session", loopMutSession, "--batch-json", source, "--cwd", cwd}, cwd)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RunLoopCliContext(ctx, args)
		if !errors.Is(err, context.Canceled) || got != (LoopCliResult{}) {
			t.Fatalf("ended context with %q: %+v %v, want the context's error and nothing to print", source, got, err)
		}
		before.assertUnchanged(t, cwd, slug)
		mutatorsSameTree(t, tree, mutatorsTree(t, cwd))
	}
}

// TestLoopSteerInterruptCancelledWithTheLockHeldWritesNothing cancels from the lock's Now seam, which runs
// right after the goalplan lock is taken.
func TestLoopSteerInterruptCancelledWithTheLockHeldWritesNothing(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	tree := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args, err := ParseLoopCliArgs([]string{"steer", "--session", loopMutSession, "--cwd", cwd, "--batch-json",
		`{"idempotencyKey":"k1","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loopSteer(ctx, args, &goalplan.GoalplanWriteLockOptions{Now: func() string { cancel(); return "2026-03-03T00:00:00.000Z" }}, nil)
	if !errors.Is(err, context.Canceled) || got != (LoopCliResult{}) {
		t.Fatalf("cancelled with the lock held: %+v %v", got, err)
	}
	before.assertUnchanged(t, cwd, slug)
	mutatorsSameTree(t, tree, mutatorsTree(t, cwd))
}

// TestLoopSteerInterruptLiveContextIsTheControl: a context that can end but has not, over the file form, which
// reads in a goroutine, answers exactly as the plain call does.
func TestLoopSteerInterruptLiveContextIsTheControl(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	file := filepath.Join(cwd, "batch.json")
	if err := os.WriteFile(file, []byte(`{"idempotencyKey":"k2","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args, err := ParseLoopCliArgs([]string{"steer", "--session", loopMutSession, "--batch-json", file, "--cwd", cwd}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RunLoopCliContext(ctx, args)
	if err != nil || got.Code != 0 || got.Output != "loop steer: applied k2 (1 op(s): annotate)" {
		t.Fatalf("live context: %+v %v", got, err)
	}
	if n := len(loopMutPlan(t, cwd, slug).SteeringLog); n != 1 {
		t.Fatalf("steering log has %d entries, want 1", n)
	}
	if !bytes.Contains([]byte(loopMutFile(t, cwd, slug, "ledger.jsonl")), []byte("steered")) {
		t.Fatal("the ledger holds no steered row")
	}
}

// TestLoopSteerInterruptEndedContextRefusalsAreSilent: the argument and batch refusals that precede the lock are
// answers of a process the signal has already ended, so an ended context takes precedence over each of them.
func TestLoopSteerInterruptEndedContextRefusalsAreSilent(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	before := loopMutTake(t, cwd, slug)
	tree := mutatorsTree(t, cwd)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, argv := range map[string][]string{
		"malformed inline batch": {"steer", "--session", loopMutSession, "--batch-json", "{", "--cwd", cwd},
		"missing batch":          {"steer", "--session", loopMutSession, "--cwd", cwd},
		"missing session":        {"steer", "--batch-json", "{}", "--cwd", cwd},
		"unreadable batch file":  {"steer", "--session", loopMutSession, "--batch-json", "absent.json", "--cwd", cwd},
		"unbound session":        {"steer", "--session", "00000000-0000-4000-8000-000000000001", "--batch-json", "{}", "--cwd", cwd},
	} {
		args, err := ParseLoopCliArgs(argv, cwd)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := RunLoopCliContext(ctx, args)
		if !errors.Is(err, context.Canceled) || got != (LoopCliResult{}) {
			t.Fatalf("%s under an ended context: %+v %v, want the context's error and nothing to print", name, got, err)
		}
		before.assertUnchanged(t, cwd, slug)
		mutatorsSameTree(t, tree, mutatorsTree(t, cwd))
	}
}

// Fix round 2: a cancellation that lands after the lock is taken and before the first write takes precedence
// over every refusal or error that run reaches before that write, not only over a clean pre-write answer.
// mutatorsLockedThenEnded is a context that is live through the session lock's own checks and ended for every
// check after them, the shape of a SIGINT that arrives once the lock is held.
type mutatorsLockedThenEnded struct {
	context.Context
	live  int
	calls int
}

func (c *mutatorsLockedThenEnded) Err() error {
	c.calls++
	if c.calls > c.live {
		return context.Canceled
	}
	return nil
}

func newMutatorsLockedThenEnded(t *testing.T) *mutatorsLockedThenEnded {
	t.Helper()
	probe := &mutatorsLockedThenEnded{Context: context.Background(), live: 1 << 30}
	if err := state.WithSessionLockContext(probe, t.TempDir(), "probe", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return &mutatorsLockedThenEnded{Context: context.Background(), live: probe.calls}
}

func mutatorsStillFIFO(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the session state was replaced: %v %v", info, err)
	}
}

func TestMemoryAllowWriteInterruptCancelledDuringARefusedStateReadIsSilent(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	ctx := newMutatorsLockedThenEnded(t)
	path := state.StatePath(cwd, "rec-s1")
	fifoStateWithOpenWriter(t, path)
	writes := 0
	out, code, err := cliMemoryAllowWriteRun(ctx, MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd},
		func(cwd string, s state.State) error { writes++; return state.WriteState(cwd, s) }, nil)
	if !errors.Is(err, context.Canceled) || out != "" || code != 0 || writes != 0 {
		t.Fatalf("cancelled during the refused state read: %q %d %v, %d writes; want the context's error and nothing to print", out, code, err, writes)
	}
	mutatorsStillFIFO(t, path)
}

func TestMemoryAllowWriteInterruptCancelledOnceTheWriteBeganKeepsItsFailure(t *testing.T) {
	cwd, _, _ := cliSeed(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, code, err := cliMemoryAllowWriteRun(ctx, MemoryAllowWriteArgs{Verb: "allow-write", SessionID: "rec-s1", Cwd: cwd},
		func(string, state.State) error { cancel(); return errors.New("disk full") }, nil)
	if err != nil || code != 1 || out != "memory allow-write: could not record the grant (disk full)" {
		t.Fatalf("a write that began and failed: %q %d %v, want its own failure", out, code, err)
	}
}

func TestScanRecordInterruptCancelledDuringARefusedStateReadIsSilent(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	ctx := newMutatorsLockedThenEnded(t)
	path := state.StatePath(cwd, "s1")
	fifoStateWithOpenWriter(t, path)
	appends, writes := 0, 0
	got, err := cliScanRecordRunContext(ctx, scanInterruptArgs(t, cwd),
		func(cwd string, e state.InterviewEvent) error { appends++; return state.AppendInterviewEvent(cwd, e) },
		func(cwd string, s state.State) error { writes++; return state.WriteState(cwd, s) }, nil)
	if !errors.Is(err, context.Canceled) || got != (CliResult{}) || appends != 0 || writes != 0 {
		t.Fatalf("cancelled during the refused state read: %+v %v, %d appends, %d writes; want the context's error and nothing to print", got, err, appends, writes)
	}
	mutatorsStillFIFO(t, path)
	if len(state.ReadInterviewEvents(cwd, "s1")) != 0 {
		t.Fatal("the cancelled run appended a scan_completed row")
	}
}

func TestScanRecordInterruptCancelledOnceTheAppendBeganKeepsItsFailure(t *testing.T) {
	cwd := scanInterruptWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := cliScanRecordRunContext(ctx, scanInterruptArgs(t, cwd),
		func(string, state.InterviewEvent) error { cancel(); return errors.New("disk full") }, state.WriteState, nil)
	if err != nil || got.Code != 1 || got.Output != "scan record failed: disk full" {
		t.Fatalf("an append that began and failed: %+v %v, want its own failure", got, err)
	}
}

// TestLoopSteerInterruptEndedContextOutranksPreWriteErrors: an error the goalplan lock path returns before the
// steering transaction's first write (here the plan file is a symlink) is the answer of a process the signal has
// already ended.
func TestLoopSteerInterruptEndedContextOutranksPreWriteErrors(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	if err := os.Rename(planPath, planPath+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("goalplan.json.real", planPath); err != nil {
		t.Fatal(err)
	}
	tree := mutatorsTree(t, cwd)
	args, err := ParseLoopCliArgs([]string{"steer", "--session", loopMutSession, "--cwd", cwd, "--batch-json",
		`{"idempotencyKey":"k1","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunLoopCli(args); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("control: the live run's error is %v, want the symlink refusal", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := RunLoopCliContext(ctx, args)
	if !errors.Is(err, context.Canceled) || got != (LoopCliResult{}) {
		t.Fatalf("ended context over a pre-write error: %+v %v, want the context's error and nothing to print", got, err)
	}
	mutatorsSameTree(t, tree, mutatorsTree(t, cwd))
}

// TestLoopSteerInterruptCancelledOnceTheWriteBeganAnswersAsToday: a cancellation that lands as the plan write
// begins does not take precedence; the transaction finishes and answers applied.
func TestLoopSteerInterruptCancelledOnceTheWriteBeganAnswersAsToday(t *testing.T) {
	cwd, slug := loopMutWorkspace(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args, err := ParseLoopCliArgs([]string{"steer", "--session", loopMutSession, "--cwd", cwd, "--batch-json",
		`{"idempotencyKey":"k3","rationale":"r","evidence":"e","ops":[{"kind":"annotate","note":"n"}]}`}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := loopSteer(ctx, args, nil, cancel)
	if err != nil || got.Code != 0 || got.Output != "loop steer: applied k3 (1 op(s): annotate)" {
		t.Fatalf("cancelled as the write began: %+v %v, want applied", got, err)
	}
	if ctx.Err() == nil {
		t.Fatal("the write-begin seam did not run")
	}
	if n := len(loopMutPlan(t, cwd, slug).SteeringLog); n != 1 {
		t.Fatalf("steering log has %d entries, want 1", n)
	}
}
