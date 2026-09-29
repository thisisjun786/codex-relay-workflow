package install

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// DefaultIssue is recorded as the evidence of who placed the pointer and wrote a record when
// --issue names nothing else: the installer's own issue.
const DefaultIssue = "CRW-158"

// Commands are `crw install`'s subcommands.
var Commands = []string{"install", "update", "rollback", "remove", "status", "register-mcp", "hook"}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw install {"+strings.Join(Commands, ",")+"} ...")
}

// Run is `crw install ...` over this process's environment.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return Main(ctx, args, scope.Env(os.Environ()), stdout, stderr)
}

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// parse is flag parsing that, like argparse, accepts positionals between flags.
func parse(flags *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := flags.Parse(args); err != nil {
			return nil, err
		}
		if flags.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}
}

// Main is `crw install` with an explicit environment.
func Main(ctx context.Context, args []string, env scope.Env, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw install: error: the following arguments are required: command")
		return Usage
	}
	command, rest := args[0], args[1:]
	flags := flag.NewFlagSet("crw install "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dest := flags.String("dest", "", "the runtime destination (default ~/.local/share/crw-runtime)")
	codexHome := flags.String("codex-home", "", "the Codex home (default $CODEX_HOME or ~/.codex)")
	recordPath := flags.String("record", "", "the host record (default $XDG_STATE_HOME/codex-relay-workflow/host-record.json)")
	issue := flags.String("issue", DefaultIssue, "the issue recorded as the evidence of this run")
	socket := flags.String("socket", "", "the App Server socket the runtime is exercised and gated against")
	state := flags.String("state", "", "the relay state directory the gate reads")
	var from, sums, release, releaseURL, owner, name, bridgeCommand, policy, adapter, event, relayCommand, markerRoot, dbPath, journalRoot, mode, isolation *string
	var bridgeArgs repeated
	var dryRun *bool
	var guardTimeout, timeout *int64
	switch command {
	case "install", "update":
		from = flags.String("from", "", "a release archive crw_<version>_<os>_<arch>.tar.gz")
		sums = flags.String("sums", "", "the SHA256SUMS that lists it (default: beside the archive)")
		release = flags.String("release", "", "a release tag whose archive and SHA256SUMS are fetched")
		releaseURL = flags.String("release-url", ReleaseURL, "where release assets are fetched from")
	case "rollback", "remove", "status":
	case "register-mcp":
		owner = flags.String("owner", "", "who registers the bridge: plugin (the only supported owner)")
		name = flags.String("name", ServerName, "the server name the plugin declares")
		bridgeCommand = flags.String("bridge-command", "", "the bridge executable (default <dest>/current/bin/codex-thread-bridge)")
		flags.Var(&bridgeArgs, "bridge-arg", "an argument the launcher passes the bridge (repeatable)")
		policy = flags.String("execution-policy", "", "the host's execution policy file, named by path and digest in the record")
		dryRun = flags.Bool("dry-run", false, "report what would be written and write nothing")
	case "hook":
		owner = flags.String("owner", "", "who registers the Stop adapter: plugin (the only supported owner)")
		adapter = flags.String("adapter", "completion", "the adapter: completion")
		event = flags.String("event", "Stop", "the hook event: Stop")
		relayCommand = flags.String("relay-command", "", "an explicit relay executable (default <dest>/current/bin/codex-session-relay)")
		markerRoot = flags.String("marker-root", "", "the intent marker root (default: the relay's own resolution)")
		dbPath = flags.String("db-path", "", "the relay database the guard reads")
		journalRoot = flags.String("journal-root", "", "the firing journal root (default <codex home>/crw-completion-hook/journal)")
		mode = flags.String("mode", "observe", "observe or hold")
		isolation = flags.String("isolation-asserted-by", "", "who established that a held child cannot write the facts the decision reads (hold only)")
		guardTimeout = flags.Int64("guard-timeout", DefaultGuardTimeout, "the adapter's own budget for one guard call, in seconds")
		timeout = flags.Int64("timeout", RegisteredTimeout, "the registered hook timeout, in seconds")
		dryRun = flags.Bool("dry-run", false, "report what would be written and write nothing")
	case "help", "-h", "--help":
		usage(stdout)
		return OK
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw install: error: argument command: invalid choice: %s (choose from %s)\n", store.PythonRepr(command), "'"+strings.Join(Commands, "', '")+"'")
		return Usage
	}
	positional, err := parse(flags, rest)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return OK
		}
		return Usage
	}
	wantPositional := 0
	switch command {
	case "rollback":
		wantPositional = 1
	case "remove":
		wantPositional = 1
		if len(positional) != 1 {
			fmt.Fprintln(stderr, "crw install remove: error: the following arguments are required: directory")
			return Usage
		}
	}
	if len(positional) > wantPositional {
		fmt.Fprintf(stderr, "crw install %s: error: unrecognized arguments: %s\n", command, strings.Join(positional[wantPositional:], " "))
		return Usage
	}
	o, err := options(env, *dest, *codexHome, *recordPath, *issue, *socket, *state)
	if err != nil {
		fmt.Fprintln(stderr, "crw install "+command+": "+err.Error())
		return Usage
	}
	var result Object
	var code int
	switch command {
	case "install", "update":
		result, code = Install(ctx, o, command, Source{From: *from, Sums: *sums, Release: *release, BaseURL: *releaseURL})
	case "rollback":
		named := ""
		if len(positional) == 1 {
			named = positional[0]
		}
		result, code = Rollback(ctx, o, named)
	case "remove":
		result, code = Remove(ctx, o, positional[0])
	case "status":
		result, code = Status(ctx, o)
	case "register-mcp":
		result, code = RegisterMCP(ctx, o, RegisterOptions{Owner: *owner, Name: *name, BridgeCommand: *bridgeCommand, BridgeArgs: bridgeArgs, ExecutionPolicy: *policy, DryRun: *dryRun})
	case "hook":
		if *adapter != "completion" {
			fmt.Fprintf(stderr, "crw install hook: error: argument --adapter: invalid choice: %s (choose from 'completion')\n", store.PythonRepr(*adapter))
			return Usage
		}
		result, code = Hook(ctx, o, HookOptions{Owner: *owner, Event: *event, Relay: *relayCommand, MarkerRoot: *markerRoot, Database: *dbPath, Socket: *socket,
			JournalRoot: *journalRoot, Mode: *mode, Isolation: *isolation, GuardTimeout: *guardTimeout, Timeout: *timeout, DryRun: *dryRun})
	}
	if err := contract.Emit(stdout, result); err != nil {
		fmt.Fprintln(stderr, "crw install "+command+": "+err.Error())
		return Refused
	}
	return code
}

// options settles the paths every command reads: absolute, "~" expanded, defaults from env.
func options(env scope.Env, dest, codexHome, recordPath, issue, socket, state string) (Options, error) {
	if dest == "" {
		dest = doctor.DefaultDestination(env)
	}
	if codexHome == "" {
		codexHome = doctor.CodexHome(env)
	}
	if recordPath == "" {
		recordPath = record.Path(env.Get)
	}
	var err error
	for _, path := range []*string{&dest, &codexHome, &recordPath} {
		if *path, err = absolute(*path); err != nil {
			return Options{}, err
		}
	}
	return Options{Env: env, Dest: dest, CodexHome: codexHome, RecordPath: recordPath, Issue: issue, Socket: socket, State: state}, nil
}

func absolute(path string) (string, error) {
	expanded, err := store.ExpandUser(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}
