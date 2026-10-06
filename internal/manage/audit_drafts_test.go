package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// auditDraftHome points HOME, CODEX_HOME, CRW_HOME and XDG_STATE_HOME at a fresh temporary
// tree and returns the manage state directory below it, so no test reaches the real ones.
func auditDraftHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, "state")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
	t.Setenv("XDG_STATE_HOME", state)
	return filepath.Join(state, "crw", "manage")
}

// auditDraftFixtureRow is one graded audit result: the ledger row and the grade.json its
// bundle carries.
type auditDraftFixtureRow struct {
	mode     string
	subject  string
	head     string
	round    string
	gradedAt string
	status   string
	defects  []AuditDefect
	criteria []auditGradeCriterion
}

// auditDraftFixture appends one ledger row and writes the grade.json beside it for every
// row, below the audit directory of the given state directory. A row that names no status
// is recorded as ok.
func auditDraftFixture(t *testing.T, state string, rows ...auditDraftFixtureRow) {
	t.Helper()
	dir := filepath.Join(state, "audit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, auditLedgerFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	for i, row := range rows {
		bundle := filepath.Join(t.TempDir(), fmt.Sprintf("bundle-%d", i))
		if err := os.MkdirAll(bundle, 0o700); err != nil {
			t.Fatal(err)
		}
		score := 7
		doc := auditGradeDoc{Schema: auditResultSchema, Score: &score, Criteria: row.criteria, Defects: row.defects}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, auditGradeFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		// The bundle declares the row's own audit, so a reader can tell that the grade.json
		// beside it is that audit's and not another audit's that reused the directory.
		decl := map[string]any{"schema": auditBundleSchema, "mode": row.mode, "subject": row.subject, "head": row.head, "issue": "CRW-1"}
		body, err := json.Marshal(decl)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundle, auditBundleFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
		status := row.status
		if status == "" {
			status = auditStatusOK
		}
		ledger := auditLedgerRow{
			Mode: row.mode, Subject: row.subject, Head: row.head, Issue: "CRW-1", Pair: "p",
			Phase: "live", Round: row.round, Status: status, Score: &score,
			GradedAt: row.gradedAt, Bundle: bundle,
		}
		line, err := json.Marshal(ledger)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// auditDraftSectionOfState is a configuration whose audit section carries the given owners
// and cap, with its state directory at the given tree.
func auditDraftSectionOfState(t *testing.T, state string, owners map[string]string, maxNewDrafts int) *Config {
	t.Helper()
	section := map[string]any{"owners": owners}
	if maxNewDrafts > 0 {
		section["max_new_drafts"] = maxNewDrafts
	}
	return auditSectionConfig(t, state, section)
}

// auditDraftRunOf runs the drafts surface over the given configuration and fails the test
// when the run reports an error.
func auditDraftRunOf(t *testing.T, cfg *Config, scope auditDraftScope) auditDraftReport {
	t.Helper()
	e, _, _ := auditEnv(t)
	report, err := auditDraftsRun(e, cfg, scope)
	if err != nil {
		t.Fatalf("audit drafts: %v", err)
	}
	return report
}

// auditDraftLoadAt reads one draft file from the state directory.
func auditDraftLoadAt(t *testing.T, state, fingerprint string) *auditDraft {
	t.Helper()
	doc, err := auditDraftLoad(filepath.Join(state, "drafts", fingerprint+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// auditDraftSorted copies and sorts strings without depending on a package-level helper.
func auditDraftSorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// C1: at the default threshold a defect below P1 leaves no draft at all.
func TestAuditDraftDefaultThresholdCreatesNoDraftBelowP1(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{
			{Severity: "P0", What: "a blocker", Where: "a.go:1"},
			{Severity: "P1", What: "a serious defect", Where: "b.go:2"},
			{Severity: "P2", What: "a weak test", Where: "c_test.go:3"},
			{Severity: "P3", What: "a nit", Where: "d.go:4"},
		},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 2 {
		t.Fatalf("the default threshold created %d drafts %+v, want the two at P0 and P1", len(report.Created), report.Created)
	}
	for _, summary := range report.Created {
		if summary.Severity == "P2" || summary.Severity == "P3" {
			t.Errorf("a defect below P1 reached a draft: %+v", summary)
		}
	}
	entries, err := os.ReadDir(filepath.Join(state, "drafts"))
	if err != nil {
		t.Fatal(err)
	}
	names := auditDraftJSONNames(entries)
	if len(names) != 3 || !strings.Contains(strings.Join(names, ","), auditDraftIndexFile) {
		t.Errorf("the drafts directory holds %v, want two drafts and the index", names)
	}
}

// A threshold the caller names includes everything at or above it.
func TestAuditDraftSeverityThresholdIsInclusive(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{
			{Severity: "P1", What: "one", Where: "a.go:1"},
			{Severity: "P2", What: "two", Where: "b.go:2"},
			{Severity: "P3", What: "three", Where: "c.go:3"},
		},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	if report := auditDraftRunOf(t, cfg, auditDraftScope{Severity: "P3"}); len(report.Created) != 3 {
		t.Errorf("the P3 threshold created %d drafts, want all three", len(report.Created))
	}
}

// C2: one defect reported by two audits is one draft whose seen list carries both.
func TestAuditDraftSameDefectTwiceHasTwoSeenEntries(t *testing.T) {
	state := t.TempDir()
	defect := AuditDefect{Severity: "P1", What: "  The  Same   Defect ", Where: "internal/manage/a.go:9", Repro: "run it"}
	auditDraftFixture(t, state,
		auditDraftFixtureRow{mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: []AuditDefect{defect}},
		auditDraftFixtureRow{mode: auditModePackage, subject: "s2", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z", defects: []AuditDefect{defect}},
	)
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 || len(report.Updated) != 0 {
		t.Fatalf("the run created %d and updated %d, want one draft", len(report.Created), len(report.Updated))
	}
	draft := auditDraftLoadAt(t, state, report.Created[0].Fingerprint)
	if len(draft.Seen) != 2 {
		t.Fatalf("the draft carries %d seen entries, want two", len(draft.Seen))
	}
	if draft.Seen[0].Mode != auditModePR || draft.Seen[0].Subject != "s1" || draft.Seen[0].Head != "h1" || draft.Seen[0].At != "2026-01-01T00:00:00Z" {
		t.Errorf("the first seen entry is %+v", draft.Seen[0])
	}
	if draft.Seen[1].Mode != auditModePackage || draft.Seen[1].Subject != "s2" {
		t.Errorf("the second seen entry is %+v", draft.Seen[1])
	}
}

// Running the same ledger twice does not grow the seen list: a sighting already recorded is
// not a new one.
func TestAuditDraftRerunDoesNotDuplicateSeen(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "w", Where: "a.go:1"}},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{})
	second := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(second.Created) != 0 || len(second.Updated) != 0 {
		t.Fatalf("the second run created %d and updated %d, want neither", len(second.Created), len(second.Updated))
	}
	draft := auditDraftLoadAt(t, state, first.Created[0].Fingerprint)
	if len(draft.Seen) != 1 {
		t.Errorf("the seen list holds %d entries, want one", len(draft.Seen))
	}
}

// A defect the whitespace and the case differ in only is the same defect.
func TestAuditDraftFingerprintNormalizesTheWhat(t *testing.T) {
	a := auditDraftFingerprint("a.go:1", "The Same Defect")
	b := auditDraftFingerprint("a.go:2", "  the   same\tdefect ")
	if a != b {
		t.Errorf("the two fingerprints differ: %q and %q", a, b)
	}
	if len(a) != auditDraftFingerprintChars || strings.ToLower(a) != a {
		t.Errorf("the fingerprint %q is not %d lower-case characters", a, auditDraftFingerprintChars)
	}
	if other := auditDraftFingerprint("b.go:1", "The Same Defect"); other == a {
		t.Error("two paths share a fingerprint")
	}
}

// The where's path drops the line number the grader appended.
func TestAuditDraftWherePathDropsTheLineNumber(t *testing.T) {
	for _, tc := range []struct{ where, want string }{
		{"internal/manage/a.go:12", "internal/manage/a.go"},
		{"a.go", "a.go"},
		{"internal/manage/a.go:12:3", "internal/manage/a.go"},
		{"a.go:notaline", "a.go:notaline"},
		{"", ""},
	} {
		if got := auditDraftWherePath(tc.where); got != tc.want {
			t.Errorf("auditDraftWherePath(%q) = %q, want %q", tc.where, got, tc.want)
		}
	}
}

// C3: the longest owners prefix wins, a path no prefix claims is owner_unknown.
func TestAuditDraftOwnerLongestPrefixAndUnknown(t *testing.T) {
	longest := map[string]string{"internal": "project-internal", "internal/manage": "project-manage"}
	if got := auditDraftOwner(longest, "internal/manage/audit.go"); got != "project-manage" {
		t.Errorf("the longest prefix chose %q, want project-manage", got)
	}
	if got := auditDraftOwner(longest, "internal/relay/x.go"); got != "project-internal" {
		t.Errorf("the shorter prefix chose %q, want project-internal", got)
	}
	if got := auditDraftOwner(longest, "cmd/crw/main.go"); got != "" {
		t.Errorf("an unclaimed path chose %q, want the empty project", got)
	}
	boundary := map[string]string{"internal/manage": "project-manage"}
	if got := auditDraftOwner(boundary, "internal/manager/x.go"); got != "" {
		t.Errorf("a prefix that only shares characters chose %q, want the empty project", got)
	}
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{
			{Severity: "P1", What: "owned", Where: "internal/manage/a.go:1"},
			{Severity: "P1", What: "unowned", Where: "cmd/crw/main.go:2"},
		},
	})
	cfg := auditDraftSectionOfState(t, state, longest, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 2 {
		t.Fatalf("the run created %d drafts, want two", len(report.Created))
	}
	var owned, unowned *auditDraft
	for _, summary := range report.Created {
		draft := auditDraftLoadAt(t, state, summary.Fingerprint)
		if draft.Project == "" {
			unowned = draft
		} else {
			owned = draft
		}
	}
	if owned == nil || owned.Project != "project-manage" {
		t.Fatalf("the owned draft reads %+v", owned)
	}
	if unowned == nil {
		t.Fatalf("no draft carries the empty project: %+v", report.Created)
	}
	if len(report.OwnerUnknown) != 1 || report.OwnerUnknown[0] != unowned.Fingerprint {
		t.Errorf("the report names owner_unknown %v, want the unowned draft", report.OwnerUnknown)
	}
	if !strings.Contains(unowned.Body, auditDraftOwnerUnknown) {
		t.Errorf("the unowned body does not name owner_unknown:\n%s", unowned.Body)
	}
	if strings.Join(unowned.Labels, ",") != "audit,P1" {
		t.Errorf("the unowned labels are %v, want the fixed pair", unowned.Labels)
	}
}

// C4: more new drafts than the cap leaves the rest uncut and the count is reported.
func TestAuditDraftCapCutsAndReportsTheRemaining(t *testing.T) {
	state := t.TempDir()
	var defects []AuditDefect
	for i := 0; i < 12; i++ {
		defects = append(defects, AuditDefect{Severity: "P1", What: fmt.Sprintf("defect %02d", i), Where: fmt.Sprintf("a%02d.go:1", i)})
	}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: defects,
	})
	cfg := auditDraftSectionOfState(t, state, nil, 3)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 3 {
		t.Fatalf("the cap created %d drafts, want three", len(report.Created))
	}
	if report.Remaining != 9 {
		t.Errorf("the report says %d remain, want nine", report.Remaining)
	}
	entries, err := os.ReadDir(filepath.Join(state, "drafts"))
	if err != nil {
		t.Fatal(err)
	}
	if names := auditDraftJSONNames(entries); len(names) != 4 {
		t.Errorf("the drafts directory holds %v, want three drafts and the index", names)
	}
}

// C4: the cap keeps the most severe first and then the ones seen most often.
func TestAuditDraftCapPrefersSeverityThenOccurrences(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state,
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
			defects: []AuditDefect{
				{Severity: "P1", What: "rare", Where: "rare.go:1"},
				{Severity: "P1", What: "often", Where: "often.go:1"},
			}},
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r2", gradedAt: "2026-02-01T00:00:00Z",
			defects: []AuditDefect{{Severity: "P1", What: "often", Where: "often.go:1"}}},
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r3", gradedAt: "2026-03-01T00:00:00Z",
			defects: []AuditDefect{{Severity: "P1", What: "third", Where: "third.go:1"}}},
	)
	cfg := auditDraftSectionOfState(t, state, nil, 2)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 2 {
		t.Fatalf("the cap created %d drafts, want two", len(report.Created))
	}
	if !strings.Contains(report.Created[0].Title, "often") {
		t.Errorf("the draft seen twice is not first: %+v", report.Created)
	}
	if report.Remaining != 1 {
		t.Errorf("the report says %d remain, want one", report.Remaining)
	}
}

// C4: a more severe defect outranks a defect seen more often.
func TestAuditDraftCapPrefersSeverityOverOccurrences(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state,
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
			defects: []AuditDefect{{Severity: "P1", What: "often", Where: "often.go:1"}}},
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r2", gradedAt: "2026-02-01T00:00:00Z",
			defects: []AuditDefect{{Severity: "P1", What: "often", Where: "often.go:1"}}},
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r3", gradedAt: "2026-03-01T00:00:00Z",
			defects: []AuditDefect{{Severity: "P0", What: "blocker", Where: "blocker.go:1"}}},
	)
	cfg := auditDraftSectionOfState(t, state, nil, 1)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 || report.Created[0].Severity != "P0" {
		t.Errorf("the cap kept %+v, want the P0", report.Created)
	}
	if report.Remaining != 1 {
		t.Errorf("the report says %d remain, want one", report.Remaining)
	}
}

// C5: after mark posted the same fingerprint only grows its seen list.
func TestAuditDraftMarkPostedOnlyGrowsSeen(t *testing.T) {
	state := auditDraftHome(t)
	defect := AuditDefect{Severity: "P1", What: "a defect", Where: "a.go:1", Repro: "run it"}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(first.Created) != 1 {
		t.Fatalf("the first run created %d drafts", len(first.Created))
	}
	fingerprint := first.Created[0].Fingerprint
	e, _, errOut := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, []string{"mark", "--fingerprint", fingerprint, "--posted", "CRW-999"}); code != 0 {
		t.Fatalf("mark: exit %d %q", code, errOut.String())
	}
	posted := auditDraftLoadAt(t, state, fingerprint)
	if posted.State != auditDraftStatePosted || posted.Posted != "CRW-999" {
		t.Errorf("the marked draft reads %+v", posted)
	}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s2", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	second := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(second.Created) != 0 || len(second.Updated) != 1 {
		t.Fatalf("the second run created %d and updated %d, want one update", len(second.Created), len(second.Updated))
	}
	after := auditDraftLoadAt(t, state, fingerprint)
	if len(after.Seen) != 2 {
		t.Errorf("the seen list holds %d entries, want two", len(after.Seen))
	}
	if after.State != auditDraftStatePosted || after.Posted != "CRW-999" {
		t.Errorf("the update lost the posted state: %+v", after)
	}
	if second.Updated[0].State != auditDraftStatePosted || second.Updated[0].Seen != 2 {
		t.Errorf("the updated summary reads %+v", second.Updated[0])
	}
}

// The draft file carries exactly the keys the issue fixes.
func TestAuditDraftDocumentCarriesTheFixedKeys(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "w", Where: "a.go:1"}},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 {
		t.Fatalf("the run created %d drafts", len(report.Created))
	}
	data, err := os.ReadFile(filepath.Join(state, "drafts", report.Created[0].Fingerprint+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(doc))
	for key := range doc {
		got = append(got, key)
	}
	want := strings.Split("schema fingerprint source project title severity body labels seen state", " ")
	if strings.Join(auditDraftSorted(got), ",") != strings.Join(auditDraftSorted(want), ",") {
		t.Errorf("the draft keys are %v, want %v", got, want)
	}
	var labels []string
	if err := json.Unmarshal(doc["labels"], &labels); err != nil {
		t.Fatal(err)
	}
	if strings.Join(labels, ",") != "audit,P1" {
		t.Errorf("the labels are %v, want [audit P1]", labels)
	}
	var schema, source, stateValue string
	for _, pair := range []struct {
		key string
		dst *string
	}{{"schema", &schema}, {"source", &source}, {"state", &stateValue}} {
		if err := json.Unmarshal(doc[pair.key], pair.dst); err != nil {
			t.Fatal(err)
		}
	}
	if schema != auditDraftSchema || source != auditDraftSource || stateValue != auditDraftStateDraft {
		t.Errorf("the fixed values are %q/%q/%q", schema, source, stateValue)
	}
}

// The title stays inside the 72 characters the issue fixes.
func TestAuditDraftTitleIsAtMostSeventyTwoCharacters(t *testing.T) {
	long := strings.Repeat("a very long defect description ", 8)
	title := auditDraftTitle("P1", long)
	if len([]rune(title)) > auditDraftTitleLimit {
		t.Errorf("the title is %d characters: %q", len([]rune(title)), title)
	}
	if !strings.HasPrefix(title, "P1: ") {
		t.Errorf("the title does not name the severity: %q", title)
	}
	if short := auditDraftTitle("P0", "a short one"); short != "P0: a short one" {
		t.Errorf("the short title is %q", short)
	}
}

// The body carries what, where, the reproduction, the audit and the criteria.
func TestAuditDraftBodyCarriesWhatWhereReproAndCriteria(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects:  []AuditDefect{{Severity: "P1", What: "the defect", Where: "a.go:1", Repro: "run the thing"}},
		criteria: []auditGradeCriterion{{ID: "C1", Verdict: "FAIL", Note: "it fails"}},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	draft := auditDraftLoadAt(t, state, report.Created[0].Fingerprint)
	for _, needle := range []string{"the defect", "a.go:1", "run the thing", auditModePR, "C1", "FAIL"} {
		if !strings.Contains(draft.Body, needle) {
			t.Errorf("the body does not carry %q:\n%s", needle, draft.Body)
		}
	}
}

// The round and the since scope limit which ledger rows a run reads.
func TestAuditDraftRoundAndSinceScope(t *testing.T) {
	old := auditDraftFixtureRow{mode: auditModePR, subject: "old", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "old", Where: "a.go:1"}}}
	fresh := auditDraftFixtureRow{mode: auditModePR, subject: "new", head: "h", round: "r2", gradedAt: "2026-06-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "new", Where: "b.go:1"}}}
	state := t.TempDir()
	auditDraftFixture(t, state, old, fresh)
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	if report := auditDraftRunOf(t, cfg, auditDraftScope{Round: "r2"}); len(report.Created) != 1 || !strings.Contains(report.Created[0].Title, "new") {
		t.Errorf("the round scope created %+v", report.Created)
	}
	since, err := time.Parse(time.RFC3339, "2026-03-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	state2 := t.TempDir()
	auditDraftFixture(t, state2, old, fresh)
	cfg2 := auditDraftSectionOfState(t, state2, nil, 0)
	if report := auditDraftRunOf(t, cfg2, auditDraftScope{Since: since, HasSince: true}); len(report.Created) != 1 || !strings.Contains(report.Created[0].Title, "new") {
		t.Errorf("the since scope created %+v", report.Created)
	}
}

// A non-ok ledger row is not an input: only the ok rows carry a usable grade.json.
func TestAuditDraftIgnoresNonOKLedgerRows(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state,
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
			defects: []AuditDefect{{Severity: "P1", What: "w", Where: "a.go:1"}}},
		auditDraftFixtureRow{mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z", status: auditStatusTimeout,
			defects: []AuditDefect{{Severity: "P1", What: "ignored", Where: "b.go:1"}}},
	)
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 || strings.Contains(report.Created[0].Title, "ignored") {
		t.Errorf("a non-ok row produced %+v", report)
	}
}

// An ok ledger row whose bundle left no usable grade.json is named in the report and the
// rows the run can read still count, because an append-only ledger must not let one lost
// bundle stop every later run.
func TestAuditDraftSkipsARowWithNoUsableGrade(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "w", Where: "a.go:1"}},
	})
	ledger := filepath.Join(state, "audit", auditLedgerFile)
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var row auditLedgerRow
	if err := json.Unmarshal(data[:len(data)-1], &row); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(row.Bundle, auditGradeFile)); err != nil {
		t.Fatal(err)
	}
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Errorf("a row with no grade produced %+v", report)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, auditGradeFile) {
		t.Errorf("the skipped row is not named with its reason: %+v", report.Skipped)
	}
	if report.Skipped[0].Subject != "s" || report.Skipped[0].Head != "h" {
		t.Errorf("the skipped row is not identified: %+v", report.Skipped[0])
	}
}

// A bundle a later audit graded into again holds that audit's grade.json, so the row that
// named the old audit is skipped by name instead of drafting another audit's defects.
func TestAuditDraftSkipsARowWhoseBundleNowHoldsAnotherAudit(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "old", head: "oldhead", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "the old defect", Where: "a.go:1"}},
	})
	ledger := filepath.Join(state, "audit", auditLedgerFile)
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	var row auditLedgerRow
	if err := json.Unmarshal(data[:len(data)-1], &row); err != nil {
		t.Fatal(err)
	}
	// The bundle is graded again for another audit: its bundle.json now names the new subject
	// and head, and its grade.json carries the new defects.
	decl := map[string]any{"schema": auditBundleSchema, "mode": auditModePR, "subject": "new", "head": "newhead", "issue": "CRW-1"}
	declBody, err := json.Marshal(decl)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(row.Bundle, auditBundleFile), declBody, 0o600); err != nil {
		t.Fatal(err)
	}
	score := 9
	doc := auditGradeDoc{Schema: auditResultSchema, Score: &score,
		Defects: []AuditDefect{{Severity: "P1", What: "the new defect", Where: "b.go:1"}}}
	docBody, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(row.Bundle, auditGradeFile), docBody, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 0 || len(report.Updated) != 0 {
		t.Fatalf("the reused bundle drafted another audit's defects: %+v", report)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].Subject != "old" {
		t.Fatalf("the reused row is not named: %+v", report.Skipped)
	}
	if !strings.Contains(report.Skipped[0].Reason, "another audit") {
		t.Errorf("the reason reads %q", report.Skipped[0].Reason)
	}
}

// An index that names a draft the directory no longer holds does not stop the run: the file
// decides whether a draft exists, and the index is rewritten from what the directory holds.
func TestAuditDraftIndexNamingAMissingFileDoesNotBlock(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "w", Where: "a.go:1"}},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{})
	fingerprint := first.Created[0].Fingerprint
	dir := filepath.Join(state, "drafts")
	if err := os.Remove(filepath.Join(dir, fingerprint+".json")); err != nil {
		t.Fatal(err)
	}
	second := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(second.Created) != 1 {
		t.Fatalf("the index naming a missing file stopped the run: %+v", second)
	}
	if draft := auditDraftLoadAt(t, state, fingerprint); draft.Fingerprint != fingerprint {
		t.Fatalf("the draft was not recreated: %+v", draft)
	}
	saved, err := auditDraftIndexLoad(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(saved.Drafts, ",") != fingerprint {
		t.Errorf("the index reads %v, want the recreated draft", saved.Drafts)
	}
}

// A later audit that raises a defect's severity escalates the stored draft, keeping the
// posted state and the issue key.
func TestAuditDraftEscalatesSeverityOnALaterAudit(t *testing.T) {
	state := auditDraftHome(t)
	defect := AuditDefect{Severity: "P1", What: "a crash", Where: "a.go:3"}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{})
	fingerprint := first.Created[0].Fingerprint
	e, _, errOut := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, []string{"mark", "--fingerprint", fingerprint, "--posted", "CRW-999"}); code != 0 {
		t.Fatalf("mark: exit %d %q", code, errOut.String())
	}
	raised := defect
	raised.Severity = "P0"
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s2", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z", defects: []AuditDefect{raised},
	})
	second := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(second.Updated) != 1 {
		t.Fatalf("the escalation was not reported as an update: %+v", second)
	}
	after := auditDraftLoadAt(t, state, fingerprint)
	if after.Severity != "P0" || strings.Join(after.Labels, ",") != "audit,P0" || !strings.HasPrefix(after.Title, "P0: ") {
		t.Errorf("the escalated draft reads %+v", after)
	}
	if after.State != auditDraftStatePosted || after.Posted != "CRW-999" {
		t.Errorf("the escalation lost the posted state: %+v", after)
	}
}

// A later sighting reaches the body the management session reads, not only the seen list.
func TestAuditDraftBodyNamesALaterSighting(t *testing.T) {
	state := t.TempDir()
	defect := AuditDefect{Severity: "P1", What: "a defect", Where: "a.go:1"}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "auditA", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{})
	fingerprint := first.Created[0].Fingerprint
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePackage, subject: "auditB", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	auditDraftRunOf(t, cfg, auditDraftScope{})
	after := auditDraftLoadAt(t, state, fingerprint)
	if !strings.Contains(after.Body, "auditB") {
		t.Errorf("the body does not name the later audit:\n%s", after.Body)
	}
	if !strings.Contains(after.Body, "auditA") {
		t.Errorf("the body lost the first audit:\n%s", after.Body)
	}
	if !strings.Contains(after.Body, "a.go:1") {
		t.Errorf("the body lost the defect's where:\n%s", after.Body)
	}
}

// The subcommand prints its usage and refuses a line it cannot use.
func TestAuditDraftCommandsUsage(t *testing.T) {
	auditDraftHome(t)
	e, out, errOut := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, []string{"--help"}); code != 0 || !strings.Contains(out.String(), auditDraftUsage) {
		t.Errorf("drafts --help: exit %d %q", code, out.String())
	}
	for _, args := range [][]string{
		{"nope"},
		{"--nope", "x"},
		{"--round"},
		{"--severity", "P9"},
		{"--round", "r", "--since", "2026-01-01T00:00:00Z"},
		{"--since", "yesterday"},
		{"mark"},
		{"mark", "--fingerprint", "../escape", "--posted", "CRW-1"},
		{"mark", "--fingerprint", "0123456789abcdef"},
		{"mark", "--posted", "CRW-1"},
	} {
		out.Reset()
		errOut.Reset()
		if code := auditRunDrafts(context.Background(), e, args); code != usageExit {
			t.Errorf("%v: exit %d, want %d", args, code, usageExit)
		}
	}
	// A state directory with no ledger yet holds no ok rows and no drafts.
	out.Reset()
	errOut.Reset()
	if code := auditRunDrafts(context.Background(), e, nil); code != 0 {
		t.Fatalf("an empty state: exit %d %q", code, errOut.String())
	}
	var report auditDraftReport
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if len(report.Created) != 0 || len(report.Updated) != 0 || report.Remaining != 0 {
		t.Errorf("an empty state reported %+v", report)
	}
}

// A fingerprint the index does not hold is refused by mark rather than creating a file.
func TestAuditDraftMarkRefusesAnUnknownFingerprint(t *testing.T) {
	auditDraftHome(t)
	e, _, _ := auditEnv(t)
	if code := auditRunDrafts(context.Background(), e, []string{"mark", "--fingerprint", "0123456789abcdef", "--posted", "CRW-1"}); code != 1 {
		t.Errorf("mark of an unknown fingerprint: exit %d, want 1", code)
	}
}

// The drafts subcommand is reachable from the audit command's dispatch.
func TestAuditDraftIsDispatchedFromTheAuditCommand(t *testing.T) {
	auditDraftHome(t)
	e, _, errOut := auditEnv(t)
	if code := auditRun(context.Background(), e, []string{"drafts", "nope"}); code != usageExit {
		t.Errorf("audit drafts with a stray argument: exit %d, want %d", code, usageExit)
	}
	if !strings.Contains(errOut.String(), auditDraftUsage) {
		t.Errorf("the drafts usage is missing: %q", errOut.String())
	}
}

// The command reads the ledger and the audit section through the configuration and prints
// the report the run produced.
func TestAuditDraftCommandPrintsTheReport(t *testing.T) {
	state := auditDraftHome(t)
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "w", Where: "internal/manage/a.go:1"}},
	})
	e, out, errOut := auditEnv(t)
	if code := auditRun(context.Background(), e, []string{"drafts"}); code != 0 {
		t.Fatalf("audit drafts: exit %d %q", code, errOut.String())
	}
	var report auditDraftReport
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out.String())
	}
	if len(report.Created) != 1 {
		t.Fatalf("the command created %d drafts, want one", len(report.Created))
	}
	if report.Created[0].File != report.Created[0].Fingerprint+".json" {
		t.Errorf("the summary names %q", report.Created[0].File)
	}
}

// Two runs must not create or grow one draft at once: a second caller is refused while the
// first holds the drafts lock, and a later run succeeds once the lock is released.
func TestAuditDraftLockRefusesTheSecondRun(t *testing.T) {
	state := t.TempDir()
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	e, _, _ := auditEnv(t)
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auditDraftLock(e, cfg); err == nil {
		t.Error("a second run took the drafts lock")
	} else if !strings.Contains(err.Error(), "drafts_locked") {
		t.Errorf("the refusal reads %q, want it to name drafts_locked", err)
	}
	release()
	if _, err := auditDraftLock(e, cfg); err != nil {
		t.Errorf("the lock was not released: %v", err)
	}
}

// An update must not drop a field the draft file carries that this product does not name: a
// key the record does not read is refused rather than silently rewritten away.
func TestAuditDraftUpdateRefusesAnUnknownField(t *testing.T) {
	state := t.TempDir()
	defect := AuditDefect{Severity: "P1", What: "a defect", Where: "a.go:1"}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s1", head: "h1", round: "r1", gradedAt: "2026-01-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	first := auditDraftRunOf(t, cfg, auditDraftScope{})
	path := filepath.Join(state, "drafts", first.Created[0].Fingerprint+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["a_field_from_a_later_version"] = json.RawMessage(`"keep me"`)
	patched, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(patched, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s2", head: "h2", round: "r2", gradedAt: "2026-02-01T00:00:00Z", defects: []AuditDefect{defect},
	})
	e, _, _ := auditEnv(t)
	if _, err := auditDraftsRun(e, cfg, auditDraftScope{}); err == nil {
		t.Fatal("a draft carrying an unread field was rewritten")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "a_field_from_a_later_version") {
		t.Error("the update dropped the field it does not read")
	}
}

// auditDraftJSONNames is the JSON artifacts a drafts directory holds: the drafts and the
// index. The lock file the run takes is not one of them.
func auditDraftJSONNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

// A torn ledger tail is not a whole row; the rows after it are, and a run still drafts from
// them instead of failing at the fragment.
func TestAuditDraftSkipsATornLedgerLine(t *testing.T) {
	state := t.TempDir()
	auditDraftFixture(t, state, auditDraftFixtureRow{
		mode: auditModePR, subject: "s", head: "h", round: "r1", gradedAt: "2026-01-01T00:00:00Z",
		defects: []AuditDefect{{Severity: "P1", What: "whole row", Where: "a.go:1"}},
	})
	ledger := filepath.Join(state, "audit", auditLedgerFile)
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	fragment := []byte("{\"mode\":\"pr\",\"subject\":\"half\"" + "\n")
	if err := os.WriteFile(ledger, append(fragment, data...), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := auditDraftSectionOfState(t, state, nil, 0)
	report := auditDraftRunOf(t, cfg, auditDraftScope{})
	if len(report.Created) != 1 || !strings.Contains(report.Created[0].Title, "whole row") {
		t.Fatalf("the torn tail hid the whole row: %+v", report)
	}
	if report.TornLines != 1 {
		t.Errorf("the report says %d torn lines, want one", report.TornLines)
	}
}
