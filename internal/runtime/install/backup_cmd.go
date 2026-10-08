package install

// backup_cmd.go is `crw install backup-state --to <dir>`: the operator route to the stopped byte copy
// outside an install (CRW-862, docs/port/decisions.md section 83 item 4 and slice S7). The install route reaches
// backupState only when the swap actually introduces the additive DAG zone or an ordinary index
// (swapgate.AdditiveArrivalOnly), so an operator cannot take the default artifact on demand; this command takes
// the same artifact through the same routine. It is routed before the generic install options, like features,
// config and migrate-state: it has its own flag and prints its own JSON report.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/definition"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// backupStateUsage is this command's own help. The legacy `crw install` usage line is a frozen contract and
// does not name this command.
const backupStateUsage = `usage: crw install backup-state --to <dir> [--state <dir>] [--socket <path>]

Copy the whole relay state directory to <dir> outside an install: the same stopped byte copy the install
route takes before a swap that brings the additive DAG zone or an ordinary index, with the same manifest
beside it and the same integrity_check gate. The relay service must be stopped: this command refuses while
the relay's own service reading reports it running, and it holds the store's write gate exclusively while
it copies, so no relay write reaches the store under the copy. <dir> and its manifest must not exist and
must not lie inside the state directory or the runtime destination tree, exactly as on the install route.

The manifest records the copy's integrityCheck and whether it is a restoreCandidate (true only when
PRAGMA integrity_check passed on a scratch duplicate of the copy, never on the copy itself).
`

// serviceReading is the relay's own service reading, the same signal the swap gate takes
// (swapgate.DaemonCell over the relay's `service status`). It is a seam: a test fakes a running service
// without starting a daemon, which the packet requires (the "service running" case is a faked reading).
var serviceReading = func(ctx context.Context, o Options) Object {
	rec, _ := record.Load(o.RecordPath, definition.Version).Value.(Object)
	executable := filepath.Join(o.Dest, "current", "bin", definition.Relay)
	if install := selectedInstall(rec, definition.Relay); install != nil {
		if entry, ok := record.Get(install, "entryPoint").(string); ok && entry != "" {
			executable = entry
		}
	}
	return swapgate.DaemonCell(scope.Relay(ctx, []string{"service", "status"}, executable, o.Socket, o.State, o.Env, false, 0))
}

// runBackupState is `crw install backup-state`. It parses its own flags, resolves the same paths the other
// commands read, and takes the backup.
func runBackupState(ctx context.Context, args []string, env scope.Env, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw install backup-state", flag.ContinueOnError)
	flags.SetOutput(stderr)
	to := flags.String("to", "", "the directory the whole relay state directory is copied to (required)")
	codexHome := flags.String("codex-home", "", "the Codex home (default $CODEX_HOME or ~/.codex)")
	recordPath := flags.String("record", "", "the host record (default $XDG_STATE_HOME/codex-relay-workflow/host-record.json)")
	issue := flags.String("issue", DefaultIssue, "the issue recorded as the evidence of this run")
	socket := flags.String("socket", "", "the App Server socket the relay service reading is made against")
	state := flags.String("state", "", "the relay state directory to copy")
	for _, arg := range args {
		if arg == "--help" || arg == "-h" || arg == "help" {
			fmt.Fprint(stdout, backupStateUsage)
			return OK
		}
	}
	positional, err := parse(flags, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return OK
		}
		fmt.Fprintln(stderr, "crw install backup-state: error: "+err.Error())
		return Usage
	}
	if len(positional) > 0 {
		fmt.Fprintln(stderr, "crw install backup-state: error: unrecognized arguments: "+positional[0])
		return Usage
	}
	if *to == "" {
		fmt.Fprintln(stderr, "crw install backup-state: error: argument --to: the directory the state directory is copied to is required")
		return Usage
	}
	if why := notUTF8([]namedValue{{"--to", *to}, {"--state", *state}, {"--socket", *socket}, {"--codex-home", *codexHome}, {"--record", *recordPath}}); why != "" {
		fmt.Fprintln(stderr, "crw install backup-state: error: "+why)
		return Usage
	}
	o, err := options(env, *codexHome, *recordPath, *issue, *socket, *state)
	if err != nil {
		fmt.Fprintln(stderr, "crw install backup-state: error: "+err.Error())
		return Usage
	}
	if refused := foreignDestination(o, "backup-state"); refused != nil {
		if err := contract.Emit(stdout, refused); err != nil {
			fmt.Fprintln(stderr, "crw install backup-state: "+err.Error())
		}
		return Refused
	}
	dest, err := absolute(*to)
	if err != nil {
		fmt.Fprintln(stderr, "crw install backup-state: error: --to: "+err.Error())
		return Usage
	}
	result, code := BackupState(ctx, o, dest)
	if err := contract.Emit(stdout, result); err != nil {
		fmt.Fprintln(stderr, "crw install backup-state: "+err.Error())
		return Refused
	}
	return code
}

// BackupState takes the stopped byte copy outside an install. It refuses while the relay service runs, holds the
// store's write gate exclusively for the whole copy, and takes the same artifact backupState makes.
func BackupState(ctx context.Context, o Options, dest string) (Object, int) {
	reading := serviceReading(ctx, o)
	if record.Get(reading, "readable") != true {
		return refusedResult("backup-state", "whether the relay service is running could not be established: "+record.Text(reading, "detail"),
			"nothing was read and nothing was copied: a backup is taken only while the service is known stopped.")
	}
	if record.Get(reading, "answer") == scope.Running {
		return refusedResult("backup-state", "the relay service is running ("+record.Text(reading, "detail")+")",
			"nothing was read and nothing was copied: stop the relay service and run this command again.")
	}
	selection, err := store.ResolveStateDir(o.State, o.Socket)
	if err != nil {
		return refusedResult("backup-state", "the state directory could not be resolved: "+err.Error(), "nothing was read and nothing was copied.")
	}
	// The relay's write gate sits beside the database as SQLite resolves it (holdGate), which is the state
	// directory's own relay.sqlite3 in the ordinary layout and the linked file's directory otherwise.
	resolved, err := resolveStorePath(selection.DBPath())
	if err != nil {
		return refusedResult("backup-state", "the store's database path could not be resolved: "+err.Error(), "nothing was read and nothing was copied.")
	}
	gatePath := filepath.Join(filepath.Dir(resolved), "write-gate.lock")
	if _, err := os.Lstat(gatePath); errors.Is(err, fs.ErrNotExist) {
		return refusedResult("backup-state", "no relay store write gate exists at "+gatePath+", so the store cannot be held still for a copy",
			"nothing was read and nothing was copied.")
	} else if err != nil {
		return refusedResult("backup-state", gatePath+" could not be examined: "+err.Error(), "nothing was read and nothing was copied.")
	}
	gate, err := ownership.Lock(gatePath, true, false)
	if err != nil {
		return refusedResult("backup-state", "the store's write gate could not be taken exclusively ("+err.Error()+"), so a relay writer may be reaching the store",
			"nothing was read and nothing was copied: stop the relay service and run this command again.")
	}
	defer gate.Close()
	backup, err := backupState(ctx, o, dest, backupForOperator)
	if err != nil {
		return Object{field("command", "backup-state"), field("applied", false), field("refused", err.Error()), field("stateBackup", backup),
			field("note", "nothing but what is reported under stateBackup was written: a partial copy stays where it is and is never vouched for by a manifest it did not write.")}, Refused
	}
	return Object{field("command", "backup-state"), field("applied", true), field("stateBackup", backup),
		field("note", "the state directory was copied byte for byte with the relay service stopped and the store's write gate held exclusively; the copy and its manifest stay where they are, and a rerun needs a new destination.")}, OK
}
