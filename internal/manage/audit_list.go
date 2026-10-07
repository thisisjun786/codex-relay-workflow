package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// crw manage audit list reads what the audit surface has recorded — the ledger, the alert
// queue, the round files and the draft files — and prints it as one JSON document, without
// writing anything. It is the read side the audit commands lacked: grade, report, round and
// drafts all write, so a caller that only wants to show the audit state had to create
// records to read them.

// auditListSchema is the schema of the document this command prints.
const auditListSchema = "crw-audit-list/1"

// The four sources this listing reads, in the order the document names them.
const (
	auditListNameLedger = "ledger"
	auditListNameAlerts = "alerts"
	auditListNameRounds = "rounds"
	auditListNameDrafts = "drafts"
)

// The state each source carries. A source that is there and was read whole is ok; one that
// is not there at all is absent, with an empty list, because an audit nobody has run yet is
// not a failure; one that is there and could not be read is unknown, with a null list, so a
// caller can never read a partial list as a complete one.
const (
	auditListStateOK      = "ok"
	auditListStateAbsent  = "absent"
	auditListStateUnknown = "unknown"
)

// auditListUnknownExit is the status when a source could not be read: the document is still
// printed, so a caller can see which source failed and why.
const auditListUnknownExit = 1

// auditListOutputExit is the status of a listing that could not be written out. A truncated
// document must not read as a whole one.
const auditListOutputExit = 3

// AuditListOptions is what the caller filters with. An empty Round or Issue, and HasSince
// false, leave that field unfiltered.
type AuditListOptions struct {
	Round    string
	Issue    string
	Since    time.Time
	HasSince bool
}

// auditListSource is one source's state, as the document reports it.
type auditListSource struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// auditListRound is one round as the document reports it: the round's identity and start,
// with the progress auditRoundStatusOf folds from its packages.
type auditListRound struct {
	Round     string `json:"round"`
	StartedAt string `json:"started_at"`
	Total     int    `json:"total"`
	Audited   int    `json:"audited"`
	Pending   int    `json:"pending"`
	Failed    int    `json:"failed"`
	P0        int    `json:"p0"`
	P1        int    `json:"p1"`
	Clean     bool   `json:"clean"`
}

// AuditListing is the document crw manage audit list prints. The row lists carry the fields
// their writers already define, so a reader sees exactly what the ledger, the alert queue
// and the draft summary hold.
type AuditListing struct {
	Schema   string              `json:"schema"`
	StateDir string              `json:"state_dir"`
	ReadAt   string              `json:"read_at"`
	Sources  []auditListSource   `json:"sources"`
	Results  []auditLedgerRow    `json:"results"`
	Alerts   []auditAlertRow     `json:"alerts"`
	Rounds   []auditListRound    `json:"rounds"`
	Drafts   []auditDraftSummary `json:"drafts"`
}

// AuditList reads the audit ledger, the alert queue, the round files and the draft files
// below the configured state directory and returns them as one document. It writes nothing
// and creates nothing: each source is only stat-ed, listed and read, and a state directory
// that is not there is left that way. A source that cannot be read is reported unknown with
// the reason and a null list, and the sources beside it keep their rows.
func AuditList(_ context.Context, e *Env, cfg *Config, opts AuditListOptions) (AuditListing, error) {
	listing := AuditListing{
		Schema:   auditListSchema,
		StateDir: auditStateDir(e, cfg),
		ReadAt:   e.Now().UTC().Format(time.RFC3339),
	}
	auditDir := filepath.Join(listing.StateDir, "audit")
	results, resultsSource := auditListRead(auditListNameLedger, filepath.Join(auditDir, auditLedgerFile),
		func() ([]auditLedgerRow, error) {
			rows, err := auditReportLedger(e, cfg)
			if err != nil {
				return nil, err
			}
			return auditListFilterResults(rows, opts), nil
		})
	alerts, alertsSource := auditListRead(auditListNameAlerts, filepath.Join(auditDir, auditAlertFile),
		func() ([]auditAlertRow, error) {
			rows, err := auditListAlerts(e, cfg)
			if err != nil {
				return nil, err
			}
			return auditListFilterAlerts(rows, opts), nil
		})
	rounds, roundsSource := auditListRead(auditListNameRounds, auditRoundDir(e, cfg),
		func() ([]auditListRound, error) {
			rows, err := auditListRounds(e, cfg)
			if err != nil {
				return nil, err
			}
			return auditListFilterRounds(rows, opts), nil
		})
	drafts, draftsSource := auditListRead(auditListNameDrafts, auditDraftDir(e, cfg),
		func() ([]auditDraftSummary, error) { return auditListDrafts(e, cfg) })
	listing.Results, listing.Alerts, listing.Rounds, listing.Drafts = results, alerts, rounds, drafts
	listing.Sources = []auditListSource{resultsSource, alertsSource, roundsSource, draftsSource}
	return listing, nil
}

// auditListRead stats one source and, when it is there, runs its loader. The list it returns
// is non-nil for a source that is ok or absent and nil only when the source could not be
// read, so a null list in the document means exactly unknown and never empty. The stat
// result is not held across the read: a ledger another process removes between the two is
// reported ok with an empty list, which reads the same as absent — complete and empty.
func auditListRead[T any](name, path string, load func() ([]T, error)) ([]T, auditListSource) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []T{}, auditListSource{Name: name, State: auditListStateAbsent}
		}
		return nil, auditListSource{Name: name, State: auditListStateUnknown, Reason: err.Error()}
	}
	rows, err := load()
	if err != nil {
		return nil, auditListSource{Name: name, State: auditListStateUnknown, Reason: err.Error()}
	}
	if rows == nil {
		rows = []T{}
	}
	return rows, auditListSource{Name: name, State: auditListStateOK}
}

// auditListFilterResults keeps the ledger rows the options select. The result is always a
// non-nil list, so a filter that matches nothing is an empty list and never a null one.
func auditListFilterResults(rows []auditLedgerRow, opts AuditListOptions) []auditLedgerRow {
	out := []auditLedgerRow{}
	for _, row := range rows {
		if opts.Round != "" && row.Round != opts.Round {
			continue
		}
		if opts.Issue != "" && row.Issue != opts.Issue {
			continue
		}
		if opts.HasSince && !auditListSinceMatches(row.GradedAt, opts.Since) {
			continue
		}
		out = append(out, row)
	}
	return out
}

// auditListFilterAlerts keeps the alert rows the options select. Round and issue are fields
// an alert carries; since dates a graded result, and an alert carries no graded_at, so it
// selects nothing here.
func auditListFilterAlerts(rows []auditAlertRow, opts AuditListOptions) []auditAlertRow {
	out := []auditAlertRow{}
	for _, row := range rows {
		if opts.Round != "" && row.Round != opts.Round {
			continue
		}
		if opts.Issue != "" && row.Issue != opts.Issue {
			continue
		}
		out = append(out, row)
	}
	return out
}

// auditListFilterRounds keeps the rounds the --round option names. A round's own Round field
// is its identity; the listing is ordered by the file name, which the round writer keeps
// equal to that field.
func auditListFilterRounds(rows []auditListRound, opts AuditListOptions) []auditListRound {
	out := []auditListRound{}
	for _, row := range rows {
		if opts.Round != "" && row.Round != opts.Round {
			continue
		}
		out = append(out, row)
	}
	return out
}

// auditListSinceMatches reports whether a graded_at is at or after the threshold, compared
// as real instants, so an offset other than Z and a fractional second both compare right. A
// graded_at this build cannot read is kept: a read-only listing must not drop a row it
// cannot date.
func auditListSinceMatches(gradedAt string, since time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, gradedAt)
	if err != nil {
		return true
	}
	return !at.Before(since)
}

// auditListAlerts reads the alert queue, one JSON document per line, with the same rules as
// the ledger reader: a blank line is skipped and a line that is not a whole document is an
// error naming its line, because an unread alert line would silently drop an alert.
func auditListAlerts(e *Env, cfg *Config) ([]auditAlertRow, error) {
	path := filepath.Join(auditStateDir(e, cfg), "audit", auditAlertFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	rows := []auditAlertRow{}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row auditAlertRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("the audit alert line %d: %w", i+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// auditListRounds reads every round file below the rounds directory and summarises each. It
// takes no lock: a round file is replaced by an atomic rename, so a reader sees a whole
// document either way, and the lock file another process holds is not a round. os.ReadDir
// returns the entries in name order, which is the order the document lists them in.
func auditListRounds(e *Env, cfg *Config) ([]auditListRound, error) {
	dir := auditRoundDir(e, cfg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := []auditListRound{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		doc, err := auditRoundLoad(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		status := auditRoundStatusOf(doc)
		out = append(out, auditListRound{
			Round: status.Round, StartedAt: doc.StartedAt, Total: status.Total,
			Audited: status.Audited, Pending: status.Pending, Failed: status.Failed,
			P0: status.P0, P1: status.P1, Clean: status.Clean,
		})
	}
	return out, nil
}

// auditListDrafts reads every draft below the drafts directory, skipping the index and any
// document of another schema. A file whose JSON cannot be read at all, or that is not a
// draft this build knows, is corruption rather than a foreign schema, so it makes the whole
// source unknown instead of disappearing quietly. The result is ordered by fingerprint.
func auditListDrafts(e *Env, cfg *Config) ([]auditDraftSummary, error) {
	dir := auditDraftDir(e, cfg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := []auditDraftSummary{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || name == auditDraftIndexFile {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var head struct {
			Schema string `json:"schema"`
		}
		if err := json.Unmarshal(data, &head); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if head.Schema != auditDraftSchema {
			// A document that names another schema is a file this command does not own and
			// is skipped. A document that names no schema at all is not a foreign schema but
			// a draft this build cannot read, so it is refused rather than dropped: the
			// drafts source must never report ok while leaving a draft out.
			if head.Schema == "" {
				return nil, fmt.Errorf("%s: the draft names no schema", path)
			}
			continue
		}
		doc, err := auditDraftLoad(path)
		if err != nil {
			return nil, err
		}
		out = append(out, auditDraftSummaryOf(doc))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out, nil
}

// auditListUsage is the line the list subcommand prints.
const auditListUsage = "usage: crw manage audit list [--round R] [--issue K] [--since T]"

// auditListRun is crw manage audit list. It validates the flags, reads the listing and
// prints it as one JSON document. A source that could not be read is reported inside the
// document and exits 1; a failed output write is exit 3, because a truncated document must
// not read as a whole one.
func auditListRun(ctx context.Context, e *Env, args []string) int {
	if auditListHelpRequested(args) {
		fmt.Fprintln(e.Stdout, auditListUsage)
		return 0
	}
	opts, err := auditListParseArgs(args)
	if err != nil {
		fmt.Fprintln(e.Stderr, auditListUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit list: error: %v\n", err)
		return usageExit
	}
	listing, err := AuditList(ctx, e, coreDefaults(e), opts)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit list: error: %v\n", err)
		return auditListOutputExit
	}
	data, err := json.Marshal(listing)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit list: error: %v\n", err)
		return auditListOutputExit
	}
	if _, err := fmt.Fprintf(e.Stdout, "%s\n", data); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit list: error: %v\n", err)
		return auditListOutputExit
	}
	for _, source := range listing.Sources {
		if source.State == auditListStateUnknown {
			return auditListUnknownExit
		}
	}
	return 0
}

// auditListHelpRequested reports whether the arguments ask for the usage. Help is the first
// argument being "help", or -h or --help standing where an option is expected. The token an
// option consumes is a value whatever it is, so --round help names the round help and
// --round --help is a missing value rather than a help request.
func auditListHelpRequested(args []string) bool {
	if len(args) > 0 && args[0] == "help" {
		return true
	}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-h" || arg == "--help":
			return true
		case strings.HasPrefix(arg, "--") && !strings.ContainsRune(arg, '='):
			i++ // this option takes the next token as its value
		}
	}
	return false
}

// auditListParseArgs reads the three filter flags. A flag that is present with an empty
// value, including one only spaces pad, names no filter and is refused rather than silently
// read as unfiltered.
func auditListParseArgs(args []string) (AuditListOptions, error) {
	values, err := auditPkgParseArgs(args, map[string]bool{"round": true, "issue": true, "since": true})
	if err != nil {
		return AuditListOptions{}, err
	}
	for _, key := range []string{"round", "issue", "since"} {
		if raw, ok := values[key]; ok && strings.TrimSpace(raw) == "" {
			return AuditListOptions{}, fmt.Errorf("the option --%s needs a value", key)
		}
	}
	opts := AuditListOptions{Round: values["round"], Issue: values["issue"]}
	if raw := values["since"]; raw != "" {
		if opts.Since, err = time.Parse(time.RFC3339Nano, raw); err != nil {
			return AuditListOptions{}, fmt.Errorf("--since %q is not an RFC 3339 time", raw)
		}
		opts.HasSince = true
	}
	return opts, nil
}
