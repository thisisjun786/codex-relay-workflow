package manage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditReportLedgerLine is one ledger row as JSON, with the fields the report reads.
func auditReportLedgerLine(t *testing.T, mode, subject, pair, phase, status string, score *int, p0, p1 int) string {
	t.Helper()
	row := map[string]any{
		"mode": mode, "subject": subject, "head": "h", "issue": "CRW-1", "pair": pair,
		"phase": phase, "round": "", "status": status, "score": score,
		"p0": p0, "p1": p1, "p2": 0, "p3": 0, "graded_at": "t", "bundle": "b",
	}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// auditReportLedgerFixture writes a ledger under a temporary state directory and returns a
// configuration over it.
func auditReportLedgerFixture(t *testing.T, lines ...string) *Config {
	t.Helper()
	state := t.TempDir()
	dir := filepath.Join(state, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, auditLedgerFile), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Config{StateDir: state, raw: map[string]json.RawMessage{}}
}

func auditReportScore(n int) *int { return &n }

// C4: the aggregation is pinned to the injected ledger. Only the ok rows of mode pr are
// counted, grouped by pair and phase, and the P0 and P1 columns count the pull requests
// that carry at least one defect of that severity rather than the defects themselves.
func TestAuditReportAggregatesTheInjectedLedger(t *testing.T) {
	cfg := auditReportLedgerFixture(t,
		auditReportLedgerLine(t, auditModePR, "pr-1", "sol", "live", auditStatusOK, auditReportScore(8), 0, 0),
		auditReportLedgerLine(t, auditModePR, "pr-2", "sol", "live", auditStatusOK, auditReportScore(6), 1, 2),
		auditReportLedgerLine(t, auditModePR, "pr-3", "sol", "live", auditStatusOK, auditReportScore(4), 0, 1),
		auditReportLedgerLine(t, auditModePR, "pr-4", "if-deepseek", "live", auditStatusOK, auditReportScore(10), 0, 0),
		auditReportLedgerLine(t, auditModePR, "pr-5", "if-deepseek", "baseline", auditStatusOK, auditReportScore(2), 0, 0),
		auditReportLedgerLine(t, auditModePR, "pr-6", "sol", "live", auditStatusInvalid, nil, 0, 0),
		auditReportLedgerLine(t, auditModePackage, "pkg-x", "sol", "live", auditStatusOK, auditReportScore(9), 0, 0),
	)
	e, _, _ := auditTestEnv(t)
	rows, err := auditReportLedger(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	groups := auditReportGroups(rows)
	if len(groups) != 3 {
		t.Fatalf("the report has %d groups, want 3: %+v", len(groups), groups)
	}
	want := []auditReportGroup{
		{Pair: "if-deepseek", Phase: "baseline", PRs: 1, ScoreSum: 2, P0: 0, P1: 0},
		{Pair: "if-deepseek", Phase: "live", PRs: 1, ScoreSum: 10, P0: 0, P1: 0},
		{Pair: "sol", Phase: "live", PRs: 3, ScoreSum: 18, P0: 1, P1: 2},
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Errorf("group %d = %+v, want %+v", i, groups[i], want[i])
		}
	}
	markdown := auditReportMarkdown(rows, e.Now())
	for _, needle := range []string{"| sol | live | 3 | 6.00 | 1 | 2 |", "| if-deepseek | baseline | 1 | 2.00 | 0 | 0 |", "| if-deepseek | live | 1 | 10.00 | 0 | 0 |"} {
		if !strings.Contains(markdown, needle) {
			t.Errorf("the report does not carry %q:\n%s", needle, markdown)
		}
	}
	if strings.Contains(markdown, "pkg-x") || strings.Contains(markdown, "pr-6") {
		t.Errorf("the report counted a package row or an ungraded one:\n%s", markdown)
	}
}

// A ledger that is not there is no rows rather than an error, so a report can be built
// before anything was graded; a line that is not a document is an error, because an
// unreadable line would silently drop a result from the aggregation.
func TestAuditReportLedgerRefusesAnUnreadableLine(t *testing.T) {
	e, _, _ := auditTestEnv(t)
	missing := &Config{StateDir: t.TempDir(), raw: map[string]json.RawMessage{}}
	rows, err := auditReportLedger(e, missing)
	if err != nil || len(rows) != 0 {
		t.Fatalf("a missing ledger answered %d rows and %v, want no rows and no error", len(rows), err)
	}
	broken := auditReportLedgerFixture(t, "{oops")
	if _, err := auditReportLedger(e, broken); err == nil {
		t.Fatal("an unreadable ledger line was accepted")
	}
}

// C4: the report is written atomically, so the file is either the previous report or the
// new one and never a half-written document, and a rebuild with the same ledger is the
// same bytes.
func TestAuditReportIsWrittenAtomically(t *testing.T) {
	cfg := auditReportLedgerFixture(t,
		auditReportLedgerLine(t, auditModePR, "pr-1", "sol", "live", auditStatusOK, auditReportScore(7), 1, 0),
	)
	e, out, errOut := auditTestEnv(t)
	if code := auditReportRunWith(context.Background(), e, cfg, nil); code != 0 {
		t.Fatalf("audit report: exit %d %q", code, errOut.String())
	}
	path := auditReportPath(e, cfg)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "| sol | live | 1 | 7.00 | 1 | 0 |") {
		t.Errorf("the report does not carry the aggregate:\n%s", first)
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("the command did not name the report it wrote: %q", out.String())
	}
	if code := auditReportRunWith(context.Background(), e, cfg, nil); code != 0 {
		t.Fatalf("the second run: exit %d", code)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("two runs over one ledger wrote different reports")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != auditReportFile && entry.Name() != auditLedgerFile {
			t.Errorf("the atomic write left %q behind", entry.Name())
		}
	}
}

// The report command takes no arguments: a stray positional is a usage error at exit 2, and
// the help flags print the usage to stdout.
func TestAuditReportCommandUsage(t *testing.T) {
	e, out, errOut := auditTestEnv(t)
	for _, arg := range []string{"-h", "--help", "help"} {
		out.Reset()
		errOut.Reset()
		if code := auditReportRun(context.Background(), e, []string{arg}); code != 0 || errOut.Len() != 0 {
			t.Errorf("%s: exit %d %q %q", arg, code, out.String(), errOut.String())
		}
		if !strings.Contains(out.String(), auditReportUsage) {
			t.Errorf("%s: the usage is missing: %q", arg, out.String())
		}
	}
	out.Reset()
	errOut.Reset()
	if code := auditReportRun(context.Background(), e, []string{"nope"}); code != usageExit {
		t.Errorf("a stray argument: exit %d, want %d", code, usageExit)
	}
	if !strings.Contains(errOut.String(), auditReportUsage) {
		t.Errorf("the usage is missing: %q", errOut.String())
	}
}
