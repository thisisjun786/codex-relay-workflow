package manage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"io"
	"os"
	"slices"
)

// The exit status of crw manage send-parent, as the issue body fixes it: 0 for a delivery that
// was accepted or queued, 1 for one the bridge refused, and 4 for one nobody can yet place.
const (
	sendParentExitAccepted = 0
	sendParentExitRefused  = 1
	sendParentExitUnknown  = 4
	sendParentClassQueued  = "queued"
	sendParentQueueDir     = "parent-queue"
)

// sendParentArgs is the parsed command line.
type sendParentArgs struct {
	thread      string
	messageFile string
	logicalID   string
	queue       bool
	// role is the role the message is sent with: parent, the default, or supervisor (CRW-1204: the initiative lead's thread, with the
	// settings its supervision binding records). report and initiative name what a supervisor notice is about.
	role       string
	report     string
	initiative string
	// relayState and relaySocket name the relay a supervisor notice's report was written in (--state, --socket); empty: the configuration's.
	relayState, relaySocket string
}

// sendParentConfig is where the command reads its configuration: the defaults today, and the crw
// configuration file once a later issue reads it. A test replaces it to point the command at a
// fake bridge and a temporary state directory.
var sendParentConfig = coreDefaults

var sendParentCommand = Command{Name: "send-parent", Summary: "deliver one message to the 자식's thread, or with --role supervisor to the 부모's, or queue it", Run: sendParentRun}

func init() { Register(sendParentCommand) }

// sendParentRun is crw manage send-parent --thread T --message-file F [--logical-id ID] [--queue].
// It delivers with the parent's settings and the parent role, and prints the class, request id and
// logical id it settled on. With --queue it sends nothing and writes the notice where the pump
// collects it. An unconfigured bridge policy refuses rather than sending, which is the specified
// behaviour while no configuration file supplies one.
func sendParentRun(ctx context.Context, e *Env, args []string) int {
	parsed, err := sendParentParse(args, e.Stderr)
	if err != nil {
		fmt.Fprintln(e.Stderr, "usage: crw manage send-parent --thread T --message-file F [--logical-id ID] [--queue]")
		if slices.Contains(args, "--role") {
			fmt.Fprintln(e.Stderr, sendParentRoleUsage)
		}
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return usageExit
	}
	text, err := os.ReadFile(parsed.messageFile)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: read the message file: %v\n", err)
		return sendParentExitRefused
	}
	return sendParentDispatch(ctx, e, sendParentConfig(e), parsed, text)
}

// sendParentDispatch is the command with its configuration supplied: queue the notice, or hand it
// to the delivery core and map the class it settled on to an exit status.
func sendParentDispatch(ctx context.Context, e *Env, cfg *Config, parsed sendParentArgs, text []byte) int {
	logicalID := parsed.logicalID
	if logicalID == "" {
		logicalID = sendParentLogicalID(text)
	}
	if parsed.role == supervisorRole {
		return sendParentSupervisor(ctx, e, cfg, parsed, logicalID, text)
	}
	if parsed.queue && summaryIsLogical(logicalID) {
		// the notice that a plan summary entry is owed (CRW-1204) is queued once, whatever the queue holds of it already
		return sendParentSummaryQueue(e, cfg, parsed, logicalID, text)
	}
	if parsed.queue {
		if err := sendParentQueueSafe(cfg, parsed.thread, logicalID); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
			return sendParentExitRefused
		}
		dest := crwconfig.JoinRoot(cfg.StateDir, sendParentQueueDir, parsed.thread, logicalID+".txt")
		if err := deliverWriteAtomic(dest, text); err != nil {
			fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
			return sendParentExitRefused
		}
		return sendParentReport(e, sendParentClassQueued, "", logicalID)
	}
	// A plan-summary notice names its summary entry, not its words: the run composes it again on every retry, and a retry of a send an
	// earlier build made in other words settles that send instead of being refused (Message.Rewordable, CRW-1478).
	out, err := Deliver(ctx, e, cfg, Message{LogicalID: logicalID, Thread: parsed.thread, Text: string(text), Role: "parent", Settings: cfg.Settings.Parent,
		Rewordable: summaryIsLogical(logicalID)})
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
	}
	class := out.Class
	if class == "" {
		// A local refusal the delivery never classified -- an unsafe logical id, text that is not
		// UTF-8, an unreadable or unwritable ledger -- still has to report a class the output
		// contract names, or a client cannot read the exit-1 result.
		class = deliverClassRefused
	}
	return sendParentReport(e, class, out.RequestID, logicalID)
}

// sendParentQueueSafe refuses a queue path that would not stay under the state directory. The
// thread names a directory and the logical id names a file, both from a command line, and a
// planted symlink where either belongs would redirect the notice out of the state directory even
// though every component is a single safe name.
func sendParentQueueSafe(cfg *Config, thread, logicalID string) error {
	// The thread names a directory and is never used as a retry id, so it takes the bridge's own
	// thread bound rather than the shorter one a request id leaves room for.
	if err := deliverPathComponentLimit(thread, "thread", deliverThreadLimit); err != nil {
		return err
	}
	if err := deliverPathComponent(logicalID, "logical id"); err != nil {
		return err
	}
	queue := crwconfig.JoinRoot(cfg.StateDir, sendParentQueueDir)
	if err := sendParentNoSymlink(queue, "자식 queue directory"); err != nil {
		return err
	}
	return sendParentNoSymlink(crwconfig.JoinRoot(queue, thread), "자식 queue thread directory")
}

// sendParentNoSymlink refuses a directory that is a symlink. A path that does not exist yet is
// fine: deliverWriteAtomic creates it. The outbox has the same guard in deliver.go.
func sendParentNoSymlink(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("crw manage: read the %s: %w", what, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("crw manage: the %s is a symlink; refusing to write through it", what)
	}
	return nil
}

// sendParentReport prints the one JSON object the command answers with, and returns the status
// that object's class means.
func sendParentReport(e *Env, class, requestID, logicalID string) int {
	data, err := json.Marshal(map[string]any{"class": class, "requestId": requestID, "logicalId": logicalID})
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage send-parent: error: %v\n", err)
		return sendParentExitRefused
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	switch class {
	case deliverClassAccepted, sendParentClassQueued:
		return sendParentExitAccepted
	case deliverClassUnknown:
		return sendParentExitUnknown
	default:
		return sendParentExitRefused
	}
}

// sendParentParse reads the command line. A missing or unknown flag is a usage error.
func sendParentParse(args []string, out io.Writer) (sendParentArgs, error) {
	var parsed sendParentArgs
	set := flag.NewFlagSet("send-parent", flag.ContinueOnError)
	set.SetOutput(out)
	set.StringVar(&parsed.thread, "thread", "", "the thread to deliver to: the 자식's, or the 부모's with --role supervisor")
	set.StringVar(&parsed.messageFile, "message-file", "", "the file holding the message text")
	set.StringVar(&parsed.logicalID, "logical-id", "", "the logical message id (default: the message sha256, first 16 characters)")
	set.BoolVar(&parsed.queue, "queue", false, "write the notice for the pump instead of sending it")
	set.StringVar(&parsed.role, "role", "", "the role to send with: 'parent' (자식), the default, or 'supervisor' (부모)")
	set.StringVar(&parsed.report, "report", "", "the flow report a 부모 notice is about (--role supervisor)")
	set.StringVar(&parsed.initiative, "initiative", "", "the initiative a 부모 notice is for (--role supervisor)")
	set.StringVar(&parsed.relayState, "state", "", "the relay state directory the report was written in (--role supervisor; default: the configuration's)")
	set.StringVar(&parsed.relaySocket, "socket", "", "the App Server control socket of that relay (--role supervisor; default: the configuration's)")
	if err := set.Parse(args); err != nil {
		return sendParentArgs{}, err
	}
	// The flag's own default stays empty so -h prints no unglossed role id (flag.PrintDefaults quotes a
	// non-empty default); a --role the command line did not give is the parent role, and an explicit empty
	// one is refused below.
	roleGiven := false
	set.Visit(func(f *flag.Flag) { roleGiven = roleGiven || f.Name == "role" })
	if !roleGiven {
		parsed.role = "parent"
	}
	if set.NArg() > 0 {
		return sendParentArgs{}, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if parsed.thread == "" || parsed.messageFile == "" {
		return sendParentArgs{}, fmt.Errorf("--thread and --message-file are required")
	}
	if parsed.role != "parent" && parsed.role != supervisorRole {
		return sendParentArgs{}, fmt.Errorf("--role is 'parent' (자식) or 'supervisor' (부모), not %q", parsed.role)
	}
	if parsed.role == "parent" && (parsed.report != "" || parsed.initiative != "") {
		return sendParentArgs{}, fmt.Errorf("--report and --initiative apply to --role supervisor")
	}
	if parsed.role == "parent" && (parsed.relayState != "" || parsed.relaySocket != "") {
		return sendParentArgs{}, fmt.Errorf("--state and --socket apply to --role supervisor")
	}
	return parsed, nil
}

// sendParentLogicalID is the default logical id: the first 16 characters of the message sha256,
// so the same text names the same logical message without an operator choosing one.
func sendParentLogicalID(text []byte) string {
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:])[:16]
}
