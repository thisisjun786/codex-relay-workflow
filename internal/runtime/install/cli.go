package install

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// DefaultIssue is recorded as the evidence of who placed the pointer and wrote a record when
// --issue names nothing else: the installer's own issue.
const DefaultIssue = "CRW-158"

// backupHelp is the flag that acknowledges the additive DAG zone arriving (D-01, OPS-4.5).
const backupHelp = "the directory the whole relay state directory is copied to (copy only, byte for byte, recorded) before a swap that brings the additive DAG zone to a store that predates it; the acknowledgement that route needs"

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

// given is a string flag that knows whether it was given at all: argparse's default None,
// which an empty value is not. The last one given wins, as argparse's store does.
type given struct {
	value string
	set   bool
}

func (g *given) String() string     { return g.value }
func (g *given) Set(v string) error { g.value, g.set = v, true; return nil }

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
	codexHome := flags.String("codex-home", "", "the Codex home (default $CODEX_HOME or ~/.codex)")
	recordPath := flags.String("record", "", "the host record (default $XDG_STATE_HOME/codex-relay-workflow/host-record.json)")
	issue := flags.String("issue", DefaultIssue, "the issue recorded as the evidence of this run")
	socket := flags.String("socket", "", "the App Server socket the runtime is exercised and gated against")
	state := flags.String("state", "", "the relay state directory the gate reads")
	var from, sums, release, releaseURL, owner, name, bridgeCommand, relayCommand, markerRoot, dbPath, journalRoot, mode, isolation *string
	var bridgeArgs repeated
	var policy, backup given
	var dryRun *bool
	var guardTimeout, timeout *int64
	switch command {
	case "install", "update":
		from = flags.String("from", "", "a release archive crw_<version>_<os>_<arch>.tar.gz")
		sums = flags.String("sums", "", "the SHA256SUMS that lists it (default: beside the archive)")
		release = flags.String("release", "", "a release tag whose archive and SHA256SUMS are fetched")
		releaseURL = flags.String("release-url", ReleaseURL, "where release assets are fetched from")
		flags.Var(&backup, "backup-state-to", backupHelp)
	case "rollback":
		flags.Var(&backup, "backup-state-to", backupHelp)
	case "remove", "status":
	case "register-mcp":
		owner = flags.String("owner", OwnerPlugin, "who registers the bridge: plugin (the only supported owner)")
		name = flags.String("name", ServerName, "the server name the plugin declares")
		bridgeCommand = flags.String("bridge-command", "", "the bridge executable (default ~/.local/share/crw-runtime/current/bin/codex-thread-bridge)")
		flags.Var(&bridgeArgs, "bridge-arg", "an argument the launcher passes the bridge (repeatable)")
		flags.Var(&policy, "execution-policy", "the host's execution policy file, named by path and digest in the record")
		dryRun = flags.Bool("dry-run", false, "report what would be written and write nothing")
	case "hook":
		owner = flags.String("owner", OwnerPlugin, "who registers the Stop adapter: plugin (the only supported owner)")
		relayCommand = flags.String("relay-command", "", "an explicit relay executable (default ~/.local/share/crw-runtime/current/bin/codex-session-relay)")
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
		fmt.Fprintf(stderr, "crw install: error: argument command: invalid choice: %q (choose from %s)\n", command, "'"+strings.Join(Commands, "', '")+"'")
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
	if command == "rollback" && len(positional) == 1 && strings.TrimSpace(positional[0]) == "" {
		// An unset variable spelled as the directory is not a bare rollback: the operator named a
		// runtime, and the outgoing selection may be another one.
		fmt.Fprintln(stderr, "crw install rollback: error: argument directory: an empty directory names no runtime; name one, or give no directory to return to the outgoing selection")
		return Usage
	}
	if backup.set && strings.TrimSpace(backup.value) == "" {
		fmt.Fprintln(stderr, "crw install "+command+": error: argument --backup-state-to: an empty value names no directory; name the directory the state directory is to be copied to")
		return Usage
	}
	var paths []namedValue
	if backup.set {
		paths = append(paths, namedValue{"--backup-state-to", backup.value})
	}
	for _, one := range []struct {
		flag  string
		value *string
	}{{"--codex-home", codexHome}, {"--record", recordPath}, {"--socket", socket}, {"--state", state}, {"--bridge-command", bridgeCommand},
		{"--relay-command", relayCommand}, {"--marker-root", markerRoot}, {"--db-path", dbPath}, {"--journal-root", journalRoot}} {
		if one.value != nil {
			paths = append(paths, namedValue{one.flag, *one.value})
		}
	}
	for _, arg := range bridgeArgs {
		paths = append(paths, namedValue{"--bridge-arg", arg})
	}
	for _, arg := range positional {
		paths = append(paths, namedValue{"argument directory", arg})
	}
	if why := notUTF8(paths); why != "" {
		fmt.Fprintln(stderr, "crw install "+command+": error: "+why)
		return Usage
	}
	o, err := options(env, *codexHome, *recordPath, *issue, *socket, *state)
	if err != nil {
		fmt.Fprintln(stderr, "crw install "+command+": error: "+err.Error())
		return Usage
	}
	if command != "status" {
		if refused := foreignDestination(o, command); refused != nil {
			if err := contract.Emit(stdout, refused); err != nil {
				fmt.Fprintln(stderr, "crw install "+command+": "+err.Error())
			}
			return Refused
		}
	}
	if backup.set {
		if o.StateBackup, err = absolute(backup.value); err != nil {
			fmt.Fprintln(stderr, "crw install "+command+": error: --backup-state-to: "+err.Error())
			return Usage
		}
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
		result, code = RegisterMCP(ctx, o, RegisterOptions{Owner: *owner, Name: *name, BridgeCommand: *bridgeCommand, BridgeArgs: bridgeArgs, ExecutionPolicy: policy.value, PolicyGiven: policy.set, DryRun: *dryRun})
	case "hook":
		result, code = Hook(ctx, o, HookOptions{Owner: *owner, Relay: *relayCommand, MarkerRoot: *markerRoot, Database: *dbPath, Socket: *socket,
			JournalRoot: *journalRoot, Mode: *mode, Isolation: *isolation, GuardTimeout: *guardTimeout, Timeout: *timeout, DryRun: *dryRun})
	}
	if err := contract.Emit(stdout, result); err != nil {
		fmt.Fprintln(stderr, "crw install "+command+": "+err.Error())
		return Refused
	}
	return code
}

// options settles the paths every command reads. The destination is fixed: crw install acts
// only on <home>/.local/share/crw-runtime, the directory whose current/bin the plugin wiring runs
// (docs/port/decisions.md 11, 38), with the home as pathlib expands it. The Codex home and the
// host record default from the environment as runtime_install.py's do, except that a path the
// installer cannot spell as UTF-8, and a state home that is not absolute, are refused naming the
// variable rather than written with a replacement character or read against the working
// directory.
func options(env scope.Env, codexHome, recordPath, issue, socket, state string) (Options, error) {
	lookup := environOf(env)
	home, err := record.Home(lookup)
	if err != nil {
		return Options{}, fmt.Errorf("the home the destination lives under could not be established: %w", err)
	}
	if why := fixedHome(home); why != "" {
		return Options{}, errors.New(why)
	}
	dest := filepath.Join(home, ".local", "share", "crw-runtime")
	source := "--codex-home"
	if codexHome == "" {
		if named, set := lookup("CODEX_HOME"); set && named != "" {
			codexHome, source = named, "CODEX_HOME"
		} else {
			codexHome, source = filepath.Join(home, ".codex"), "HOME"
		}
	}
	if !utf8.ValidString(codexHome) {
		return Options{}, errors.New(notUTF8Detail(source, codexHome))
	}
	if recordPath == "" {
		source = "XDG_STATE_HOME"
		if named, _ := lookup("XDG_STATE_HOME"); named == "" {
			source = "HOME"
		}
		if recordPath, err = record.PathOf(lookup); err != nil {
			return Options{}, fmt.Errorf("the host record's state home (%s) is not one this command reads: %w; runtime_install.py would read it against the working directory, which is another directory's record wherever it runs", source, err)
		}
		if !utf8.ValidString(recordPath) {
			return Options{}, errors.New(notUTF8Detail(source, recordPath))
		}
	}
	for _, path := range []*string{&codexHome, &recordPath} {
		if *path, err = absolute(*path); err != nil {
			return Options{}, err
		}
	}
	return Options{Env: env, Dest: dest, CodexHome: codexHome, RecordPath: recordPath, Issue: issue, Socket: socket, State: state}, nil
}

// fixedHome is why home cannot carry the fixed destination: it must be absolute, UTF-8, and read
// alike lexically and by the kernel - no ".." (which a lexical join folds, naming another
// directory behind a symbolic link) and no leading "//" (implementation-defined in POSIX).
func fixedHome(home string) string {
	switch {
	case !utf8.ValidString(home):
		return notUTF8Detail("HOME", home)
	case !filepath.IsAbs(home):
		return "HOME is " + strconv.Quote(home) + ", which is not an absolute path, so the destination under it would be read wherever this command runs"
	case reading.Spelling(home) != filepath.Clean(home) || strings.HasPrefix(home, "//") && !strings.HasPrefix(home, "///"):
		return "HOME is " + strconv.Quote(home) + ", whose \"..\" or leading \"//\" a lexical join and the kernel can read as different directories, so the destination under it would not be one directory; set HOME to its plain absolute spelling"
	}
	return ""
}

// environOf is os.environ.get with presence over an explicit environment, the last entry winning.
func environOf(env scope.Env) record.Environ {
	return func(key string) (string, bool) {
		for i := len(env) - 1; i >= 0; i-- {
			if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
				return v, true
			}
		}
		return "", false
	}
}

type namedValue struct{ name, value string }

// notUTF8 is why a path given on the command line cannot be recorded, or "".
func notUTF8(values []namedValue) string {
	for _, v := range values {
		if !utf8.ValidString(v.value) {
			return notUTF8Detail(v.name, v.value)
		}
	}
	return ""
}

// notUTF8Detail refuses a path holding a byte that is not UTF-8. runtime_install.py would record
// it surrogate-escaped; crw install refuses it instead (docs/port/decisions.md 38), because a
// record, a settings document or a pointer naming it would otherwise carry a replacement
// character and name a file that does not exist. The execution policy path is the one path
// recorded surrogate-escaped, as runtime_install.py records it.
func notUTF8Detail(name, value string) string {
	return name + " holds a byte that is not UTF-8 (" + strconv.Quote(value) + "), and crw install records only paths it can spell as UTF-8, so nothing was read or written; use a path whose name is UTF-8"
}

func absolute(path string) (string, error) {
	expanded, err := store.ExpandUser(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(expanded)
}
