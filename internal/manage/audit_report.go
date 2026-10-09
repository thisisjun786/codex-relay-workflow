package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"os"
	"sort"
	"strings"
	"time"
)

// auditReportFile is the report below the audit state directory.
const auditReportFile = "report.md"

// auditReportGroup is one pair and phase's aggregate: how many ok results it holds, their
// score total, and how many of them carry a P0 or a P1.
type auditReportGroup struct {
	Pair     string
	Phase    string
	PRs      int
	ScoreSum int
	P0       int
	P1       int
}

// auditReportLedger reads the audit ledger, one JSON document per line. A ledger that is not
// there is no rows rather than an error, because a run that graded nothing has nothing to
// report; a line that is not a document is an error, because an unread ledger line would
// silently drop a result from the aggregation.
func auditReportLedger(e *Env, cfg *Config) ([]auditLedgerRow, error) {
	path := crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditLedgerFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var rows []auditLedgerRow
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row auditLedgerRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("the audit ledger line %d: %w", i+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// auditReportPath is the report file: <state_dir>/audit/report.md.
func auditReportPath(e *Env, cfg *Config) string {
	return crwconfig.JoinRoot(auditStateDir(e, cfg), "audit", auditReportFile)
}

// auditReportGroups folds the ok rows of the pull request mode into one entry per pair and
// phase, in pair then phase order. A row of another mode or another status is not part of
// this report, and only an ok row carries a score to average.
func auditReportGroups(rows []auditLedgerRow) []auditReportGroup {
	index := map[[2]string]int{}
	var out []auditReportGroup
	for _, row := range rows {
		if row.Mode != auditModePR || row.Status != auditStatusOK || row.Score == nil {
			continue
		}
		key := [2]string{row.Pair, row.Phase}
		at, ok := index[key]
		if !ok {
			out = append(out, auditReportGroup{Pair: row.Pair, Phase: row.Phase})
			at = len(out) - 1
			index[key] = at
		}
		out[at].PRs++
		out[at].ScoreSum += *row.Score
		if row.P0 > 0 {
			out[at].P0++
		}
		if row.P1 > 0 {
			out[at].P1++
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Pair != out[j].Pair {
			return out[i].Pair < out[j].Pair
		}
		return out[i].Phase < out[j].Phase
	})
	return out
}

// auditReportMarkdown renders the report: the ok rows of mode pr, one line per pair and
// phase. It is English product text, and it names no model, pair or child id of its own.
//
// A pair or phase label is configuration text, so it is escaped before it reaches the table: a
// label carrying a pipe or a newline would otherwise forge rows or columns in the report.
func auditReportMarkdown(rows []auditLedgerRow, now time.Time) string {
	return auditReportMarkdownWith(rows, nil, now)
}

// auditReportMarkdownWith is the report with the pull request targets the failure record holds
// out of the selection named under the table: a person reading the report learns that a
// target is not being audited and at which head. The section is absent when there are none.
func auditReportMarkdownWith(rows []auditLedgerRow, skips []auditPRSkip, now time.Time) string {
	groups := auditReportGroups(rows)
	var out strings.Builder
	out.WriteString("# Post-merge pull request audit\n\n")
	out.WriteString("Generated " + now.UTC().Format(time.RFC3339) + " from the audit ledger. Only the `ok` results of mode `pr` are counted; a pull request is counted once per severity it carries.\n\n")
	out.WriteString("| pair | phase | PRs | average score | PRs with P0 | PRs with P1 |\n")
	out.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, group := range groups {
		fmt.Fprintf(&out, "| %s | %s | %d | %.2f | %d | %d |\n",
			auditReportLabel(group.Pair), auditReportLabel(group.Phase), group.PRs,
			float64(group.ScoreSum)/float64(group.PRs), group.P0, group.P1)
	}
	if len(skips) > 0 {
		out.WriteString("\n## Skipped pull requests\n\n")
		for _, skip := range skips {
			fmt.Fprintf(&out, "- %s: skipped: failed %d times at %s\n", auditReportLabel(skip.Subject), skip.Failures, auditReportLabel(skip.Head))
		}
	}
	return out.String()
}

// auditReportLabel makes a pair or phase safe inside a Markdown table cell. Both are
// configuration text, so a label carrying a pipe or a line break would otherwise forge rows
// or columns in the report a person reads.
func auditReportLabel(label string) string {
	label = strings.NewReplacer("|", "\\|", "\r\n", " ", "\r", " ", "\n", " ").Replace(label)
	return label
}

// auditReportWrite rebuilds the report from the ledger and writes it atomically, so a reader
// never sees a half-written report and a failure leaves the previous one in place.
func auditReportWrite(e *Env, cfg *Config) error {
	return auditReportWriteWith(e, cfg, nil)
}

// auditReportWriteWith writes the report with the skipped pull requests judged at the heads the
// caller knows (a nil map judges each at the head of its latest failure).
func auditReportWriteWith(e *Env, cfg *Config, heads map[string]string) error {
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		return err
	}
	failures, err := auditPRReadFailures(e, cfg)
	if err != nil {
		return err
	}
	skips := auditPRSkips(failures, auditPRAudited(rows), heads)
	return deliverWriteAtomic(auditReportPath(e, cfg), []byte(auditReportMarkdownWith(rows, skips, e.Now())))
}

// auditReportUsage is the line the report subcommand prints.
const auditReportUsage = "usage: crw manage audit report"

// auditReportRun is crw manage audit report.
func auditReportRun(ctx context.Context, e *Env, args []string) int {
	return auditReportRunWith(ctx, e, coreDefaults(e), args)
}

// auditReportRunWith does the work auditReportRun validated the arguments for.
func auditReportRunWith(_ context.Context, e *Env, cfg *Config, args []string) int {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			fmt.Fprintln(e.Stdout, auditReportUsage)
			return 0
		}
	}
	if len(args) > 0 {
		fmt.Fprintln(e.Stderr, auditReportUsage)
		fmt.Fprintf(e.Stderr, "crw manage audit report: error: unexpected argument %q\n", args[0])
		return usageExit
	}
	if err := auditReportWrite(e, cfg); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage audit report: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "%s\n", auditReportPath(e, cfg))
	return 0
}
