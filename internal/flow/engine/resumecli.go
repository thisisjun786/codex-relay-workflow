package engine

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// ResumeEnvRequest is what `crw flow resume` asks ResumeEnv to open: the stored journal of the run and the backend the run
// executes on. The binary installs ResumeEnv (cmd/crw): it opens the relay store, connects to the App Server and makes the
// allocator of the run's working directories. A build that installs nothing cannot resume a run of another process and says so.
type ResumeEnvRequest struct {
	RunID string
	// State, Socket and Repo are the relay state directory, the App Server control socket and the git repository the
	// operator named; empty takes the relay's own selection, the default control socket and the repository of the working
	// directory.
	State, Socket, Repo string
}

// ResumeSetup is what ResumeEnv opened: the run's journal and the backend that executes its calls (a Prover, Placer, Forker
// and ReconHost when it is the App Server backend), and the close that ends them (it commits the rows still buffered).
type ResumeSetup struct {
	Journal Journal
	Backend Backend
	Ops     map[string]Op
	// Upward is the hook the resumed run hands what goes up from it to (the plan summary, the escalations, the completion digest); nil
	// hands nothing.
	Upward UpwardHook
	// Session is the session environment of a run its started row records on the session backend (CRW-1316); nil for one of the
	// App Server backend.
	Session *SessionConfig
	Close   func() error
}

// ResumeEnv opens what `crw flow resume` works on for a run.
var ResumeEnv func(ctx context.Context, req ResumeEnvRequest) (ResumeSetup, error)

// ResumeHold is what the caller of the engine's `crw flow resume RUN SCRIPT` holds a resumed run to beyond its journal (CRW-1348):
// the run process holds a session run the same way. Backend is the backend the run's spec names ("" for a spec written before the
// field, or no spec): a run whose started row records the other backend is a mixed resume and is refused before it is taken.
// Process is the token of the process lock the caller holds for the run (SessionConfig.Process, "" for none). Session, when set, is
// called for a session run once the script and the arguments are the run's and before the run goes on: it gets the run's owner
// thread and the pool asked for, and returns the pool the run keeps in flight, the bind the run calls once its started row is in the
// journal (SessionConfig.Started: the owner's line with the table that row pinned, CRW-1372) and the release called when the run ends
// (nil binds or releases nothing).
//
// Budget, when set, holds the resume to the token budget of the run (CRW-1278): it gets what the run's journal shows it spent
// (RunSpent) and the --token-budget of the command line, and returns the run's ceiling for this resume, or the error that refuses
// it before the run goes on (a budget with nothing left). The run counts what it spent before against that ceiling. Without it the
// resume counts the same figure (Config.SpentBefore) and a --token-budget is what this resume may spend on top of it.
type ResumeHold struct {
	Backend string
	Process string
	Session func(owner string, pool int) (inFlight int, bind func(table *Policy), release func())
	Budget  func(spent, asked int64) (int64, error)
}

type resumeHoldKey struct{}

// WithResumeHold hands the engine's resume command the hold of its caller.
func WithResumeHold(ctx context.Context, h ResumeHold) context.Context {
	return context.WithValue(ctx, resumeHoldKey{}, h)
}

func resumeHoldOf(ctx context.Context) ResumeHold {
	h, _ := ctx.Value(resumeHoldKey{}).(ResumeHold)
	return h
}

// mixed says why a run the setup resumes on its recorded backend is not the one the spec names ("" when it is, or when the spec
// names none).
func (h ResumeHold) mixed(runID string, session bool) string {
	recorded := BackendAppServer
	if session {
		recorded = BackendSession
	}
	if h.Backend == "" || h.Backend == recorded {
		return ""
	}
	return fmt.Sprintf("run %s was started on the %s backend and its spec names the %s backend: a resume goes on with the backend its started row records, and these disagree (a mixed resume is refused)", runID, recorded, h.Backend)
}

const resumeUsage = `usage: crw flow resume [--state DIR] [--socket PATH] [--repo DIR] [--args JSON | --args-file FILE] [--pool N] [--token-budget N] RUN SCRIPT`

// ResumeCommand is the command a report names for a run that has calls to go on with.
const ResumeCommand = "crw flow resume"

// resumeCmd is `crw flow resume RUN SCRIPT`: the run goes on from its stored journal with the same script and arguments. Every
// call whose last row is an intent, or whose hold was cleared, or that ended held, is looked up on the host before the script
// runs again (a call that `crw flow recover` has not settled stays in its hold); the script then replays every position that
// still holds and runs the rest live. It prints the run's result as JSON and exits 0 when the script completed, 1 when it ended
// in script_error, and 2 for a command line, a store, or a script or arguments that are not the run's.
func resumeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs_ := flag.NewFlagSet("crw flow resume", flag.ContinueOnError)
	fs_.SetOutput(stderr)
	stateDir := fs_.String("state", "", "the relay state directory the journal is stored in (default: the relay's own selection)")
	socket := fs_.String("socket", "", "the App Server control socket the run's host listens on (default: the default control socket)")
	repo := fs_.String("repo", "", "the git repository the run's worktrees are made from (default: the working directory's)")
	argsJSON := fs_.String("args", "", "the script's args, as JSON")
	argsFile := fs_.String("args-file", "", "a file holding the script's args as JSON")
	pool := fs_.Int("pool", 0, "agent calls in flight at once (default 6)")
	budget := fs_.Int64("token-budget", 0, "the tokens this resume may spend, at most what the budget of the run's spec leaves (0: that remainder, or none without one)")
	fs_.Usage = func() { fmt.Fprintln(stderr, resumeUsage) }
	var positional []string
	rest := args
	for {
		if err := fs_.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if fs_.NArg() == 0 {
			break
		}
		positional = append(positional, fs_.Arg(0))
		rest = fs_.Args()[1:]
	}
	switch {
	case len(positional) != 2:
		fmt.Fprintln(stderr, resumeUsage)
		fmt.Fprintln(stderr, "crw flow resume: error: exactly a run id and a script file are required")
		return 2
	case *argsJSON != "" && *argsFile != "":
		fmt.Fprintln(stderr, "crw flow resume: error: --args and --args-file exclude each other")
		return 2
	case ResumeEnv == nil:
		fmt.Fprintln(stderr, "crw flow resume: error: this build installs no journal store and no host, so a run of another process cannot be resumed")
		return 2
	}
	var argsRaw json.RawMessage
	switch {
	case *argsJSON != "":
		argsRaw = json.RawMessage(*argsJSON)
	case *argsFile != "":
		data, err := os.ReadFile(*argsFile)
		if err != nil {
			fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
			return 2
		}
		argsRaw = data
	}
	if len(argsRaw) > 0 && !json.Valid(argsRaw) {
		fmt.Fprintln(stderr, "crw flow resume: error: args is not valid JSON")
		return 2
	}
	runID, file := positional[0], positional[1]
	text, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	root, err := os.OpenRoot(filepath.Dir(file))
	if err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	defer root.Close()
	dir := root.FS()
	table, _, err := LoadPolicy()
	if err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	prog, err := script.Load(script.Source{Name: filepath.Base(file), Text: text, Dir: dir})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	setup, err := ResumeEnv(ctx, ResumeEnvRequest{RunID: runID, State: *stateDir, Socket: *socket, Repo: *repo})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	closeSetup := func() {
		if setup.Close != nil {
			if cerr := setup.Close(); cerr != nil {
				fmt.Fprintf(stderr, "crw flow resume: error: the rows of the run could not be stored: %v\n", cerr)
			}
		}
	}
	defer closeSetup()
	// a legacy issue-child run is refused as one, before the check of its started row: the run of an old issue-lane process that ended
	// before that row has none (CRW-1415)
	if why := LegacyRun(setup.Journal.Events(), runID, ProgramWorkflow(prog)); why != "" {
		fmt.Fprintf(stderr, "crw flow resume: error: %s\n", why)
		return 2
	}
	if why := notTheRuns(setup.Journal, runID, prog, argsRaw); why != "" {
		fmt.Fprintf(stderr, "crw flow resume: error: %s\n", why)
		return 2
	}
	hold := resumeHoldOf(ctx)
	if why := hold.mixed(runID, setup.Session != nil); why != "" {
		fmt.Fprintf(stderr, "crw flow resume: error: %s\n", why)
		return 2
	}
	// the run's figure is what its journal shows it spent, whatever holds the resume (CRW-1278): a hold with the run's spec judges the
	// ceiling from it, and without one a --token-budget is what this resume may spend on top of it
	spentBefore := RunSpent(setup.Journal.Events(), runID).Total
	ceiling := *budget
	switch {
	case hold.Budget != nil:
		if ceiling, err = hold.Budget(spentBefore, *budget); err != nil {
			fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
			return 2
		}
	case *budget > 0:
		ceiling = spentBefore + *budget
	}
	inFlight := *pool
	if setup.Session != nil {
		setup.Session.Process = hold.Process
		if hold.Session != nil {
			var release func()
			inFlight, setup.Session.Started, release = hold.Session(setup.Session.Owner, inFlight)
			if release != nil {
				defer release()
			}
		}
	}
	res, err := Run(ctx, prog, Config{
		RunID: runID, Backend: setup.Backend, Policy: table, Journal: setup.Journal, Args: argsRaw, Pool: inFlight, TokenBudget: ceiling, SpentBefore: spentBefore,
		Resume: &ResumeOptions{}, Ops: setup.Ops, OmitSeq: true, Control: controlOf(ctx), Upward: setup.Upward, Session: setup.Session,
		Workflows: func(name, scriptPath string) (*script.Program, error) {
			return loadSibling(fs.FS(dir), name, scriptPath)
		},
	})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintf(stderr, "crw flow resume: error: %v\n", err)
		return 2
	}
	// a run the operator paused ended as asked (CRW-1487): a resume goes on with it
	if res.Status != RunCompleted && res.Status != RunPaused {
		return 1
	}
	return 0
}

// notTheRuns says why a script and arguments are not those of the run the journal holds ("" when they are): a resume replays the
// journal against the same script and arguments, and a run with another script is not this run's resume.
func notTheRuns(j Journal, runID string, prog *script.Program, args json.RawMessage) string {
	var started *Event
	for _, ev := range j.Events() {
		if ev.Kind == KindRun && ev.RunID == runID && ev.Event == "started" {
			ev := ev
			started = &ev
			break
		}
	}
	if started == nil {
		return fmt.Sprintf("the store holds no run %s", runID)
	}
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	switch {
	case rowStr(started.Data, "script_digest") != prog.Digest:
		return fmt.Sprintf("the script is not the one run %s ran (digest %s, the run's %s)", runID, prog.Digest, rowStr(started.Data, "script_digest"))
	case rowStr(started.Data, "args_digest") != digestJSON(args):
		return fmt.Sprintf("the arguments are not the ones run %s ran", runID)
	}
	return ""
}
