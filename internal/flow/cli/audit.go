package cli

// `crw flow audit` (CRW-1206, F13): the audit of the reviews behind the acceptances of a flow run, read back from the reviewer
// threads on the host (internal/flow/audit). The audit a run makes at a milestone close is the same audit; this command makes it
// again whenever somebody asks - after a close, from another process, on a run that is over - and leaves its result in the journal
// beside the rows of the run.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// auditUsage is the usage line of `crw flow audit` (Usage holds it among the other commands).
const auditUsage = `usage: crw flow audit (--run RUN | --acceptance ID) [--milestone M] [--state DIR] [--socket PATH] [--json] [--no-record]`

// AuditRequest is what `crw flow audit` asks the environment to open.
type AuditRequest struct {
	// RunID is the run named with --run; an acceptance given alone is searched for in the store.
	RunID, AcceptanceID string
	// State and Socket are the relay state directory and the App Server control socket the operator named; empty takes the relay's
	// own selection and the default control socket.
	State, Socket string
}

// AuditSetup is what the environment opened: the journal of the run, the reader of the host, and the close that ends them (it commits
// the rows still buffered).
type AuditSetup struct {
	RunID   string
	Journal engine.Journal
	Reader  audit.Reader
	// Finished is whether the journal holds the end of the run: only then does the command append a row, because a run that is
	// still writing owns its journal.
	Finished bool
	Close    func() error
}

// AuditEnv opens the journal of a run and the host it ran on. The binary installs it (OpenAuditStore); a build that installs nothing cannot
// audit a run of another process.
var AuditEnv func(ctx context.Context, req AuditRequest) (AuditSetup, error)

// AuditNow is the clock of the row the command appends; tests replace it.
var AuditNow = time.Now

// RunAudit serves `crw flow audit`. It prints the result of each acceptance - ok, mismatch or unobserved - and exits 0 when every
// acceptance audited is ok, 1 when one is a mismatch or is unobserved (unobserved is not a pass), and 2 for a command line, a
// store or a run it cannot use. The result is appended to the journal of the run as a crw.audit row (kind audit) unless
// --no-record is given or the run has not finished.
func RunAudit(ctx context.Context, args []string, stdout, stderr io.Writer) (code int) {
	if len(args) == 0 || args[0] != "audit" {
		fmt.Fprintln(stderr, auditUsage)
		return 2
	}
	fs := flag.NewFlagSet("crw flow audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, auditUsage) }
	run := fs.String("run", "", "audit every agent-path acceptance of this run")
	acceptance := fs.String("acceptance", "", "audit this acceptance (its run is found in the store unless --run names it)")
	milestone := fs.String("milestone", "", "audit the acceptances of this milestone only")
	state := fs.String("state", "", "the relay state directory the journal is stored in (default: the relay's own selection)")
	socket := fs.String("socket", "", "the App Server control socket the run's host listens on (default: the default control socket)")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	noRecord := fs.Bool("no-record", false, "do not append the result to the journal of the run")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch {
	case fs.NArg() != 0:
		fmt.Fprintln(stderr, "crw flow audit: error: takes no positional argument")
		return 2
	case *run == "" && *acceptance == "":
		fmt.Fprintln(stderr, auditUsage)
		fmt.Fprintln(stderr, "crw flow audit: error: one of --run and --acceptance is required")
		return 2
	case AuditEnv == nil:
		fmt.Fprintln(stderr, "crw flow audit: error: this build installs no journal store and no host, so the run cannot be audited")
		return 2
	}
	setup, err := AuditEnv(ctx, AuditRequest{RunID: *run, AcceptanceID: *acceptance, State: *state, Socket: *socket})
	if err != nil {
		fmt.Fprintf(stderr, "crw flow audit: error: %v\n", err)
		return 2
	}
	// closing the store, the host client and the journal is part of the command: an error of it is reported and is exit 2, whatever
	// the audit found (the exit code is the named result, so the deferred close sets it)
	defer func() {
		if setup.Close == nil {
			return
		}
		if err := setup.Close(); err != nil {
			fmt.Fprintf(stderr, "crw flow audit: error: %v\n", err)
			code = 2
		}
	}()
	events := setup.Journal.Events()
	var results []audit.Result
	for _, s := range audit.Subjects(events) {
		if *acceptance != "" && s.AcceptanceID != *acceptance {
			continue
		}
		if *milestone != "" && s.Milestone != *milestone {
			continue
		}
		results = append(results, audit.Audit(ctx, setup.Reader, events, s))
	}
	if len(results) == 0 {
		fmt.Fprintf(stderr, "crw flow audit: error: run %s holds no agent-path acceptance to audit%s\n", setup.RunID, filterNote(*acceptance, *milestone))
		return 2
	}
	recorded, why := false, ""
	switch {
	case *noRecord:
		why = "--no-record"
	case engine.LegacyIssueChild(events, setup.RunID) != "":
		// a legacy issue-child run is read only (CRW-1415): its acceptances are audited and the result is not recorded in its journal
		why = RefusedLegacyRun + ": " + engine.LegacyIssueChild(events, setup.RunID)
	case !setup.Finished:
		why = "the run has not finished: it owns its journal"
	default:
		setup.Journal.Append(engine.Event{RunID: setup.RunID, Kind: engine.KindCall, CallID: rowCallID(setup.RunID, results), Path: "audit", Op: audit.Op, Label: audit.Op,
			Event: engine.EvCompleted, Data: audit.RowData(*milestone, results)})
		recorded = true
		if f, ok := setup.Journal.(engine.FailingJournal); ok && f.Err() != nil {
			fmt.Fprintf(stderr, "crw flow audit: error: the result could not be stored: %v\n", f.Err())
			return 2
		}
	}
	report := struct {
		Run      string         `json:"run"`
		Results  []audit.Result `json:"results"`
		Summary  audit.Summary  `json:"summary"`
		Recorded bool           `json:"recorded"`
		NotSaved string         `json:"not_recorded_because,omitempty"`
	}{setup.RunID, results, audit.Summarize(results), recorded, why}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		printText(stdout, report.Run, results, report.Summary, recorded, why)
	}
	if report.Summary.Mismatch > 0 || report.Summary.Unobserved > 0 {
		return 1
	}
	return 0
}

func filterNote(acceptance, milestone string) string {
	var parts []string
	if acceptance != "" {
		parts = append(parts, "acceptance "+acceptance)
	}
	if milestone != "" {
		parts = append(parts, "milestone "+milestone)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// rowCallID is the call id of the row the command appends: it names the run, the acceptances and the results, and the moment, so
// two audits of the same acceptances are two rows.
func rowCallID(run string, results []audit.Result) string {
	ids := make([]string, 0, len(results))
	for _, r := range results {
		ids = append(ids, r.AcceptanceID+":"+r.State)
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(run + "\x00" + strings.Join(ids, "\x00") + "\x00" + AuditNow().UTC().Format(time.RFC3339Nano)))
	return "audit_" + hex.EncodeToString(sum[:])[:20]
}

func printText(w io.Writer, run string, results []audit.Result, sum audit.Summary, recorded bool, why string) {
	fmt.Fprintf(w, "run %s: %d ok, %d mismatch, %d unobserved\n", run, sum.OK, sum.Mismatch, sum.Unobserved)
	for _, r := range results {
		line := fmt.Sprintf("  %-10s %s  node %s  head %.12s", r.State, r.AcceptanceID, r.Node, r.Head)
		if r.Reason != "" {
			line += "  " + r.Reason
		}
		fmt.Fprintln(w, line)
	}
	if recorded {
		fmt.Fprintln(w, "recorded in the journal as a crw.audit row")
	} else {
		fmt.Fprintf(w, "not recorded: %s\n", why)
	}
}
