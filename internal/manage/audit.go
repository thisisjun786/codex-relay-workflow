package manage

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The names inside a bundle directory: what the bundle declares itself to be, the prompt
// this product writes there, and the file the grader must leave behind.
const (
	auditBundleFile = "bundle.json"
	auditPromptFile = "prompt.md"
	auditGradeFile  = "grade.json"
)

// The two schemas of the audit surface: what a bundle declares, and what a grader writes.
const (
	auditBundleSchema = "crw-audit-bundle/1"
	auditResultSchema = "crw-audit-result/1"
)

// The two modes a bundle can be built and graded in.
const (
	auditModePR      = "pr"
	auditModePackage = "package"
)

// The status one graded result carries. The ledger row writes the status through, and only
// an ok result carries a score, so a reader can tell an ungraded run from one that scored
// zero.
const (
	auditStatusOK      = "ok"
	auditStatusInvalid = "invalid"
	auditStatusTimeout = "timeout"
)

// The ledger and the alert queue below the configured state directory. The ledger records
// every graded result; the alert queue carries only the results a person must act on.
const (
	auditLedgerFile = "ledger.jsonl"
	auditAlertFile  = "alerts.jsonl"
)

// auditPromptTemplate is the built-in grader prompt. It is embedded rather than read from
// the host so the product works with no files of its own installed, and it carries no
// private path and no model name, because the grader is blind to both.
//
//go:embed audit_prompt.md
var auditPromptTemplate string

// AuditJob is one bundle to grade, with the ledger metadata the caller attaches. The
// bundle itself carries no pair or model, because the grader is blind to them.
type AuditJob struct {
	Bundle string // the bundle directory
	Pair   string
	Phase  string
	Round  string
}

// AuditCriterion is one acceptance criterion's outcome in a graded result, in the shape
// crw-audit-result/1 fixes.
type AuditCriterion struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Note    string `json:"note"`
}

// AuditDefect is one defect a grader reported, in the shape crw-audit-result/1 fixes.
type AuditDefect struct {
	Severity string `json:"severity"`
	What     string `json:"what"`
	Where    string `json:"where"`
	Repro    string `json:"repro"`
}

// AuditResult is one graded bundle: the ledger row's fields and the defects it found. It
// is what a mode issue reads to decide what to report.
type AuditResult struct {
	Mode     string           `json:"mode"`
	Subject  string           `json:"subject"`
	Head     string           `json:"head"`
	Issue    string           `json:"issue"`
	Pair     string           `json:"pair"`
	Phase    string           `json:"phase"`
	Round    string           `json:"round"`
	Status   string           `json:"status"`
	Score    int              `json:"score"`
	GradedAt string           `json:"graded_at"`
	Bundle   string           `json:"bundle"`
	Criteria []AuditCriterion `json:"criteria"`
	Defects  []AuditDefect    `json:"defects"`
}

// auditLedgerRow is one line of the ledger, in the key order the issue fixes. The score is
// a pointer so a run that left no usable result carries null rather than a misleading zero,
// and the counts are the P0-P3 tally of the defects the result carries.
type auditLedgerRow struct {
	Mode     string `json:"mode"`
	Subject  string `json:"subject"`
	Head     string `json:"head"`
	Issue    string `json:"issue"`
	Pair     string `json:"pair"`
	Phase    string `json:"phase"`
	Round    string `json:"round"`
	Status   string `json:"status"`
	Score    *int   `json:"score"`
	P0       int    `json:"p0"`
	P1       int    `json:"p1"`
	P2       int    `json:"p2"`
	P3       int    `json:"p3"`
	GradedAt string `json:"graded_at"`
	Bundle   string `json:"bundle"`
}

// auditAlertDefect is a defect as an alert line carries it: where it is and what it is,
// without the reproduction steps, which stay in the bundle the ledger row points at.
type auditAlertDefect struct {
	Severity string `json:"severity"`
	What     string `json:"what"`
	Where    string `json:"where"`
}

// auditAlertRow is one line of the alert queue, which the management pump reads.
type auditAlertRow struct {
	Mode    string             `json:"mode"`
	Subject string             `json:"subject"`
	Issue   string             `json:"issue"`
	Pair    string             `json:"pair"`
	Phase   string             `json:"phase"`
	Round   string             `json:"round"`
	Score   *int               `json:"score"`
	Defects []auditAlertDefect `json:"defects"`
}

// auditBundle is a bundle.json this product has checked.
type auditBundle struct {
	Path                string
	Mode                string
	Subject             string
	Head                string
	Issue               string
	CriteriaUnavailable bool
}

// auditBundleDoc is the document a bundle declares. The identifying fields are pointers so
// a document that omits one is told apart from one that carries an empty value.
type auditBundleDoc struct {
	Schema              *string `json:"schema"`
	Mode                *string `json:"mode"`
	Subject             *string `json:"subject"`
	Head                *string `json:"head"`
	Issue               *string `json:"issue"`
	CriteriaUnavailable bool    `json:"criteria_unavailable"`
}

// auditRequiredFields is what a bundle document must carry, each named as the error names
// it. A bundle without them is not something to grade: the ledger row and the alert line
// both quote the subject, the head and the issue.
var auditRequiredFields = []string{"schema", "mode", "subject", "head", "issue"}

// auditReadBundle reads and checks a bundle's bundle.json. A missing directory, a missing
// file, malformed JSON, another schema, a mode that is neither pr nor package, or a
// required field that is absent or empty is an error naming what is wrong, because a
// bundle this product cannot trust is not something to grade.
func auditReadBundle(dir string) (*auditBundle, error) {
	if dir == "" {
		return nil, errors.New("the bundle directory is empty")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bundle %s: not a directory", dir)
	}
	data, err := os.ReadFile(filepath.Join(dir, auditBundleFile))
	if err != nil {
		return nil, fmt.Errorf("bundle %s: %w", dir, err)
	}
	var doc auditBundleDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("bundle %s: %s: %v", dir, auditBundleFile, err)
	}
	values := map[string]*string{
		"schema": doc.Schema, "mode": doc.Mode, "subject": doc.Subject,
		"head": doc.Head, "issue": doc.Issue,
	}
	for _, name := range auditRequiredFields {
		if value := values[name]; value == nil || *value == "" {
			return nil, fmt.Errorf("bundle %s: %s: the required field %q is missing or empty", dir, auditBundleFile, name)
		}
	}
	if *doc.Schema != auditBundleSchema {
		return nil, fmt.Errorf("bundle %s: schema %q is not %s", dir, *doc.Schema, auditBundleSchema)
	}
	if *doc.Mode != auditModePR && *doc.Mode != auditModePackage {
		return nil, fmt.Errorf("bundle %s: mode %q is not %q or %q", dir, *doc.Mode, auditModePR, auditModePackage)
	}
	return &auditBundle{
		Path: dir, Mode: *doc.Mode, Subject: *doc.Subject, Head: *doc.Head,
		Issue: *doc.Issue, CriteriaUnavailable: doc.CriteriaUnavailable,
	}, nil
}

// auditPrompt is the prompt written into a bundle: the built-in template with the
// guidance the bundle's mode needs, and a word about a bundle that came without criteria.
func auditPrompt(b *auditBundle) string {
	var out strings.Builder
	out.WriteString(strings.TrimRight(auditPromptTemplate, "\n"))
	out.WriteString("\n\n## Mode\n\n")
	if b.Mode == auditModePR {
		out.WriteString("This is a pull request audit. `candidate/diff.patch` is the change that was merged, `candidate/pr.md` is the description its author wrote, and `candidate/tree/` is the whole source tree as it stands after the merge, for reading the code around the change.")
	} else {
		out.WriteString("This is a package audit. There is no single change to review: `candidate/tree/` is the whole source tree, and the criteria and the issue text name what it is judged against. Read the tree as it stands and judge the package.")
	}
	if b.CriteriaUnavailable {
		out.WriteString("\n\nThis bundle declares `criteria_unavailable`. ")
		if b.Mode == auditModePR {
			out.WriteString("Judge against the issue text under `inputs/` and the description in `candidate/pr.md`, and say in each note which of the two you used.")
		} else {
			out.WriteString("There is no criteria file and no description to fall back on: judge the package against the issue text under `inputs/` and what the tree itself shows, and say in each note which file or symbol you used.")
		}
	}
	return out.String()
}

// auditCounts is how many defects of each severity a result carries.
func auditCounts(defects []AuditDefect) (p0, p1, p2, p3 int) {
	for _, defect := range defects {
		switch defect.Severity {
		case "P0":
			p0++
		case "P1":
			p1++
		case "P2":
			p2++
		case "P3":
			p3++
		}
	}
	return p0, p1, p2, p3
}

// auditStateDir is where the ledger and the alert queue live: the state_dir the
// configuration names, or the default one when it names none.
func auditStateDir(e *Env, cfg *Config) string {
	if cfg != nil && cfg.StateDir != "" {
		return cfg.StateDir
	}
	return coreDefaults(e).StateDir
}

// auditRecord appends one ledger line per result, and one alert line for every result that
// carries a P0 or a P1. Both files are opened for append, so a concurrent writer's lines are
// never rewritten. The alert file is opened only once a result is known to need one, so a
// run in which nothing reached P0 or P1 leaves no alert file at all. The directory and
// the files are private to the owner, as the relay store's own state is, because a ledger
// row names the subject, the issue and the bundle an audit covered.
func auditRecord(e *Env, cfg *Config, results []AuditResult) (err error) {
	dir := filepath.Join(auditStateDir(e, cfg), "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ledgerPath := filepath.Join(dir, auditLedgerFile)
	ledger, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// A deferred close cannot be skipped on an early return, and its error is kept: a write
	// that reached the page cache can still fail on close, and that is not a recorded row.
	defer func() { err = errors.Join(err, ledger.Close()) }()
	alerting := false
	for _, result := range results {
		if p0, p1, _, _ := auditCounts(result.Defects); p0+p1 > 0 {
			alerting = true
		}
	}
	var alerts *os.File
	alertsPath := ""
	if alerting {
		alertsPath = filepath.Join(dir, auditAlertFile)
		if alerts, err = os.OpenFile(alertsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
			return err
		}
		defer func() { err = errors.Join(err, alerts.Close()) }()
	}
	for _, result := range results {
		p0, p1, p2, p3 := auditCounts(result.Defects)
		row := auditLedgerRow{
			Mode: result.Mode, Subject: result.Subject, Head: result.Head, Issue: result.Issue,
			Pair: result.Pair, Phase: result.Phase, Round: result.Round, Status: result.Status,
			Score: auditScoreOf(result), P0: p0, P1: p1, P2: p2, P3: p3,
			GradedAt: result.GradedAt, Bundle: result.Bundle,
		}
		if err := auditAppendLine(ledger, ledgerPath, row); err != nil {
			return err
		}
		if p0+p1 == 0 {
			continue
		}
		alert := auditAlertRow{
			Mode: result.Mode, Subject: result.Subject, Issue: result.Issue, Pair: result.Pair,
			Phase: result.Phase, Round: result.Round, Score: auditScoreOf(result),
		}
		for _, defect := range result.Defects {
			if defect.Severity != "P0" && defect.Severity != "P1" {
				continue
			}
			alert.Defects = append(alert.Defects, auditAlertDefect{Severity: defect.Severity, What: defect.What, Where: defect.Where})
		}
		if err := auditAppendLine(alerts, alertsPath, alert); err != nil {
			return err
		}
	}
	return nil
}

// auditScoreOf is the score a ledger or alert line carries, and null when the run left no
// usable result to score.
func auditScoreOf(result AuditResult) *int {
	if result.Status != auditStatusOK {
		return nil
	}
	score := result.Score
	return &score
}

// auditAppendLine writes one JSON document as one line. One write per line keeps a
// concurrent writer's lines intact under O_APPEND, and a file an earlier torn write left
// without a final line feed gets one first, so the new line is never joined to the
// fragment. This is the CRW-474 guard the state ledger carries (internal/pabcd/state
// appendRow), kept alike here because that helper is not importable.
func auditAppendLine(f *os.File, path string, doc any) error {
	line, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if auditEndsMidLine(f, path) {
		line = append([]byte{'\n'}, line...)
	}
	_, err = f.Write(line)
	return err
}

// auditEndsMidLine reports whether an open file already ends in a line with no final
// line feed. A blank line costs nothing and a joined line loses a record, so an
// unreadable tail answers yes. A new, empty or non-regular file has no tail to protect.
func auditEndsMidLine(f *os.File, path string) bool {
	info, err := f.Stat()
	if err != nil {
		return true
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	r, err := os.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = r.Close() }()
	var last [1]byte
	n, _ := r.ReadAt(last[:], info.Size()-1)
	return n != 1 || last[0] != '\n'
}

// auditCommand is crw manage audit.
var auditCommand = Command{Name: "audit", Summary: "grade an audit bundle and record the result", Run: auditRun}

func init() { Register(auditCommand) }

// auditUsage is what the audit command prints: the grade line the command shipped with,
// then the package and round lines this piece adds.
const auditUsage = "usage: crw manage audit grade --bundle DIR [--pair P] [--phase X] [--round R]\n" +
	"       crw manage audit package --round R [--next N] [--head SHA]\n" +
	"       crw manage audit round {start,status} --name R"

// auditRun is crw manage audit. It dispatches the grade subcommand, which grades one bundle
// the caller already assembled, the package subcommand, which audits the packages a round
// still holds pending or failed, and the round subcommand, which starts a round and reports
// its progress. The help flags keep their own path so the usage stays reachable without a
// subcommand.
func auditRun(_ context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, auditUsage)
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(e.Stdout, auditUsage)
		return 0
	case "grade":
		return auditRunGrade(e, args[1:])
	case "package":
		return auditRunPackage(e, args[1:])
	case "round":
		return auditRunRound(e, args[1:])
	}
	fmt.Fprintln(e.Stderr, auditUsage)
	fmt.Fprintf(e.Stderr, "crw manage audit: error: invalid command %q (choose from 'grade', 'package', 'round')\n", args[0])
	return usageExit
}

// auditRunGrade is crw manage audit grade. It validates the flags, refuses a grader the
// configuration does not name, grades the one bundle through the engine and prints the
// graded result as JSON. The engine writes the ledger and alert rows itself, so this
// command records nothing of its own.
func auditRunGrade(e *Env, args []string) int {
	job, err := auditParseGradeArgs(args)
	if err != nil {
		fmt.Fprintln(e.Stderr, auditUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit grade: error: %v\n", err)
		return usageExit
	}
	cfg := coreDefaults(e)
	section, err := auditConfigOf(cfg)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit grade: error: %v\n", err)
		return 1
	}
	if len(section.Grader) == 0 {
		fmt.Fprintln(e.Stderr, "crw manage audit: error: grader_unconfigured: the audit section of the configuration names no grader command")
		return usageExit
	}
	results, err := AuditGrade(context.Background(), e, cfg, []AuditJob{job})
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit grade: error: %v\n", err)
		return 1
	}
	data, err := json.Marshal(results)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit grade: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	return 0
}

// auditParseGradeArgs reads the grade subcommand's flags, each written --name value or
// --name=value. A flag that is not one of the four, a flag without a value, a stray
// argument, or a missing --bundle is an error naming what is wrong.
func auditParseGradeArgs(args []string) (AuditJob, error) {
	var job AuditJob
	for i := 0; i < len(args); i++ {
		name := args[i]
		if !strings.HasPrefix(name, "--") {
			return AuditJob{}, fmt.Errorf("unexpected argument %q", name)
		}
		key, value := strings.TrimPrefix(name, "--"), ""
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, value = key[:eq], key[eq+1:]
		} else {
			i++
			// A following token that starts another option is a missing value, not the
			// value itself, so a --name=value form stays the way to pass such a string.
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return AuditJob{}, fmt.Errorf("the option %s needs a value", name)
			}
			value = args[i]
		}
		switch key {
		case "bundle":
			job.Bundle = value
		case "pair":
			job.Pair = value
		case "phase":
			job.Phase = value
		case "round":
			job.Round = value
		default:
			return AuditJob{}, fmt.Errorf("unknown option %s", name)
		}
	}
	if job.Bundle == "" {
		return AuditJob{}, errors.New("--bundle is required")
	}
	return job, nil
}
