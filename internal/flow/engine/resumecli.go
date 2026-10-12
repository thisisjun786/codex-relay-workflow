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
	Close   func() error
}

// ResumeEnv opens what `crw flow resume` works on for a run.
var ResumeEnv func(ctx context.Context, req ResumeEnvRequest) (ResumeSetup, error)

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
	budget := fs_.Int64("token-budget", 0, "the run's token ceiling (0: none)")
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
	if why := notTheRuns(setup.Journal, runID, prog, argsRaw); why != "" {
		fmt.Fprintf(stderr, "crw flow resume: error: %s\n", why)
		return 2
	}
	res, err := Run(ctx, prog, Config{
		RunID: runID, Backend: setup.Backend, Policy: table, Journal: setup.Journal, Args: argsRaw, Pool: *pool, TokenBudget: *budget,
		Resume: &ResumeOptions{}, Ops: setup.Ops, OmitSeq: true, Control: controlOf(ctx),
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
	if res.Status != RunCompleted {
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
