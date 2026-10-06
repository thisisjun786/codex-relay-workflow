package manage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay store a checkpoint reads and the directory a checkpoint record lives in. The store
// is the one the manage relay helper resolves; the record is the management session's own
// state, which is never the relay zone.
const (
	checkpointStoreFile    = "relay.sqlite3"
	checkpointBusyMillis   = 5000
	checkpointSummaryLimit = 2000
	checkpointDirName      = "checkpoint"
)

// The due signals and the one reading that is not a signal. A signal that holds is a reason;
// a reading the store cannot support is unmeasured and decides nothing.
const (
	checkpointSignalMilestone    = "milestone_integrated"
	checkpointSignalMerges       = "merges_since_checkpoint"
	checkpointSignalRunningHours = "running_hours"
	checkpointSignalNeedsChanges = "needs_changes_or_split_2h"
	checkpointSignalPairEval     = "pair_eval_p0p1_since_checkpoint"
	checkpointSignalBacklog      = "backlog_net_4h"
	checkpointSignalIntegrations = "integrations_since_checkpoint"
)

// checkpointDueExit is the status of a reading that found a due project; a clean reading exits
// 0 and one that could not read the store exits checkpointStoreExit.
const (
	checkpointDueExit   = 1
	checkpointStoreExit = 3
)

// The two rolling windows the signal names carry, and the instant form the relay writes.
const (
	checkpointNeedsChangesWindow = 2 * time.Hour
	checkpointBacklogWindow      = 4 * time.Hour
	checkpointInstantLayout      = "2006-01-02T15:04:05.000000+00:00"
)

// checkpointThresholds is the point at which each signal is due.
type checkpointThresholds struct {
	Merges              int
	RunningHours        int
	NeedsChangesOrSplit int
	PairEvalP0P1        int
	BacklogNet          int
}

// checkpointDefaultThresholds is the issue's own set.
var checkpointDefaultThresholds = checkpointThresholds{
	Merges: 20, RunningHours: 8, NeedsChangesOrSplit: 3, PairEvalP0P1: 2, BacklogNet: 15,
}

// checkpointSectionDoc is the checkpoint settings section: one optional override per threshold,
// so a section that names one key leaves the others at their default.
type checkpointSectionDoc struct {
	Merges              *int `json:"merges_since_checkpoint"`
	RunningHours        *int `json:"running_hours"`
	NeedsChangesOrSplit *int `json:"needs_changes_or_split_2h"`
	PairEvalP0P1        *int `json:"pair_eval_p0p1_since_checkpoint"`
	BacklogNet          *int `json:"backlog_net_4h"`
}

// CheckpointOptions is what one reading is asked: the project to narrow to (empty means every
// project the store records) and the two files the management session exported.
type CheckpointOptions struct {
	Project      string
	LinearExport string
	PairEval     string
}

// CheckpointCounts is what a reading counted for one project. A field is a pointer exactly
// when its reading is unmeasured, so a reader can tell "none" from "not measured".
type CheckpointCounts struct {
	MergesSinceCheckpoint         int     `json:"merges_since_checkpoint"`
	IntegrationsSinceCheckpoint   *int    `json:"integrations_since_checkpoint"`
	NeedsChangesSinceCheckpoint   int     `json:"needs_changes_since_checkpoint"`
	SplitDecisionsSinceCheckpoint int     `json:"split_decisions_since_checkpoint"`
	RunningHours                  float64 `json:"running_hours"`
	NeedsChangesOrSplit2h         int     `json:"needs_changes_or_split_2h"`
	PairEvalP0P1SinceCheckpoint   *int    `json:"pair_eval_p0p1_since_checkpoint"`
	BacklogNet4h                  *int    `json:"backlog_net_4h"`
	MilestoneIntegrated           *int    `json:"milestone_integrated"`
}

// CheckpointReport is one project's reading, in the shape the issue fixes.
type CheckpointReport struct {
	Project    string           `json:"project"`
	Due        bool             `json:"due"`
	Reasons    []string         `json:"reasons"`
	Counts     CheckpointCounts `json:"counts"`
	Unmeasured []string         `json:"unmeasured"`
	Since      string           `json:"since"`
}

// checkpointStore is the read-only handle on the relay store: mode=ro with query_only under the
// store's own no-sidecar rule, so a reading can neither create a table nor write a row, and a
// table a store predates is an unmeasured reading rather than a repair.
type checkpointStore struct{ db *sql.DB }

// checkpointOpenStore opens the store at state read-only. A missing file is an error, because a
// reading of nothing is not a clean reading.
func checkpointOpenStore(ctx context.Context, state string) (*checkpointStore, error) {
	if state == "" {
		return nil, errors.New("the relay state directory is not configured")
	}
	path := filepath.Join(state, checkpointStoreFile)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("relay store: %w", err)
	}
	// store.InPlaceRead is the store's own no-sidecar rule: it resolves the path the way SQLite
	// does and returns immutable=1 when the write-ahead log holds no frame, so a reading never
	// creates -wal or -shm, and it refuses a log whose index is missing rather than rebuilding it.
	resolved, params, err := store.InPlaceRead(path)
	if err != nil {
		return nil, fmt.Errorf("relay store: %w", err)
	}
	u := url.URL{Scheme: "file", Path: resolved}
	q := params
	q.Set("_busy_timeout", fmt.Sprint(checkpointBusyMillis))
	q.Set("_pragma", "query_only(1)")
	u.RawQuery = q.Encode()
	connector, err := sqlite.NewConnector(u.String())
	if err != nil {
		return nil, fmt.Errorf("relay store: %w", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("relay store: %w", err)
	}
	return &checkpointStore{db: db}, nil
}

// Close releases the read-only handle.
func (s *checkpointStore) Close() error { return s.db.Close() }

// checkpointHasTable reports whether the store carries a table. The DAG zone is additive, so a
// store written before it lacks tables this reading would otherwise read.
func (s *checkpointStore) checkpointHasTable(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// checkpointRows runs one query and scans every row through scan.
func checkpointRows[T any](ctx context.Context, db *sql.DB, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		value, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// checkpointInstant parses a stored relay timestamp; the second result is false when the value
// is empty or in a form this build does not know, which is not a failure to read.
func checkpointInstant(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, checkpointInstantLayout, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// checkpointProjects is every project the store records, sorted. The frozen tables are the
// source; dag_plans joins them only when the store carries the DAG zone.
func (s *checkpointStore) checkpointProjects(ctx context.Context) ([]string, error) {
	query := "SELECT project_key FROM merge_turns WHERE project_key <> ''" +
		" UNION SELECT project_key FROM relationship_scope WHERE project_key <> ''"
	zoned, err := s.checkpointHasTable(ctx, "dag_plans")
	if err != nil {
		return nil, err
	}
	if zoned {
		query += " UNION SELECT project_key FROM dag_plans WHERE project_key <> ''"
	}
	return checkpointRows(ctx, s.db, query+" ORDER BY 1", nil, func(rows *sql.Rows) (string, error) {
		var project string
		return project, rows.Scan(&project)
	})
}

// checkpointProjectInstant is one project-scoped instant of the store.
type checkpointProjectInstant struct{ project, at string }

// checkpointMerges reads the landed merge turns, with the instant each landed.
func (s *checkpointStore) checkpointMerges(ctx context.Context) ([]checkpointProjectInstant, error) {
	return checkpointRows(ctx, s.db,
		"SELECT project_key, COALESCE(NULLIF(closed_at, ''), updated_at) FROM merge_turns WHERE state = 'landed' ORDER BY turn_id",
		nil, func(rows *sql.Rows) (checkpointProjectInstant, error) {
			var row checkpointProjectInstant
			return row, rows.Scan(&row.project, &row.at)
		})
}

// checkpointVerdicts reads the needs_changes rulings, with the instant each was decided.
func (s *checkpointStore) checkpointVerdicts(ctx context.Context) ([]checkpointProjectInstant, error) {
	return checkpointRows(ctx, s.db,
		"SELECT s.project_key, v.decided_at FROM verdicts v"+
			" JOIN events e ON e.event_id = v.event_id"+
			" JOIN relationship_scope s ON s.relationship_id = e.relationship_id"+
			" WHERE v.verdict = 'needs_changes' ORDER BY v.event_id",
		nil, func(rows *sql.Rows) (checkpointProjectInstant, error) {
			var row checkpointProjectInstant
			return row, rows.Scan(&row.project, &row.at)
		})
}

// checkpointSplitDecisions reads the decision replies and keeps the split approvals, whose
// instant is the one the receipt recorded.
func (s *checkpointStore) checkpointSplitDecisions(ctx context.Context) ([]checkpointProjectInstant, error) {
	type raw struct{ project, receipt, firstSeen string }
	rows, err := checkpointRows(ctx, s.db,
		"SELECT s.project_key, e.receipt, e.first_seen_at FROM events e"+
			" JOIN relationship_scope s ON s.relationship_id = e.relationship_id"+
			" WHERE e.outcome = 'decision_reply' ORDER BY e.event_id",
		nil, func(rows *sql.Rows) (raw, error) {
			var row raw
			return row, rows.Scan(&row.project, &row.receipt, &row.firstSeen)
		})
	if err != nil {
		return nil, err
	}
	var out []checkpointProjectInstant
	for _, row := range rows {
		var receipt struct {
			Decision  string `json:"decision"`
			DecidedAt string `json:"decidedAt"`
		}
		if err := json.Unmarshal([]byte(row.receipt), &receipt); err != nil {
			continue
		}
		if receipt.Decision != "split_approval" {
			continue
		}
		at := receipt.DecidedAt
		if _, ok := checkpointInstant(at); !ok {
			at = row.firstSeen
		}
		out = append(out, checkpointProjectInstant{project: row.project, at: at})
	}
	return out, nil
}

// checkpointIntegrations reads the effective integration observations, joined to the plan that
// carries the project. The caller asks only when the store carries the zone.
func (s *checkpointStore) checkpointIntegrations(ctx context.Context) ([]checkpointProjectInstant, error) {
	return checkpointRows(ctx, s.db,
		"SELECT p.project_key, o.observed_at FROM dag_integration_observations o"+
			" JOIN dag_acceptances a ON a.acceptance_id = o.acceptance_id"+
			" JOIN dag_plans p ON p.plan_id = a.plan_id"+
			" WHERE o.is_ancestor = 1 AND COALESCE(o.reverted_by, '') = '' ORDER BY o.observation_id",
		nil, func(rows *sql.Rows) (checkpointProjectInstant, error) {
			var row checkpointProjectInstant
			return row, rows.Scan(&row.project, &row.at)
		})
}

// checkpointRecordInstants reads the checkpoint records below the manage state directory: the
// instant of every line, by project. A directory that does not exist yet has no records, which
// is not an error.
func checkpointRecordInstants(stateDir string) (map[string][]time.Time, error) {
	dir := filepath.Join(stateDir, checkpointDirName)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string][]time.Time{}, nil
		}
		return nil, err
	}
	out := map[string][]time.Time{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var row struct {
				Project string `json:"project"`
				At      string `json:"at"`
			}
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				continue
			}
			instant, ok := checkpointInstant(row.At)
			if !ok || row.Project == "" {
				continue
			}
			out[row.Project] = append(out[row.Project], instant)
		}
	}
	return out, nil
}

// checkpointExport is the linear export the management session writes. The four fields the
// issue fixes keep their meaning; milestone and completedAt are the optional additions the
// milestone and backlog signals need, and an export without them still measures the backlog
// and reports the milestone unmeasured.
type checkpointExport struct {
	Issues []checkpointExportIssue `json:"issues"`
}

type checkpointExportIssue struct {
	Identifier  string `json:"identifier"`
	Project     string `json:"project"`
	CreatedAt   string `json:"createdAt"`
	State       string `json:"state"`
	Milestone   string `json:"milestone"`
	CompletedAt string `json:"completedAt"`
}

// checkpointReadExport reads the linear export file.
func checkpointReadExport(path string) (checkpointExport, error) {
	var export checkpointExport
	data, err := os.ReadFile(path)
	if err != nil {
		return export, err
	}
	if err := json.Unmarshal(data, &export); err != nil {
		return export, fmt.Errorf("%s: %w", path, err)
	}
	return export, nil
}

// checkpointPairEval is one line of the pair evaluation the management session exports.
type checkpointPairEval struct {
	Project  string `json:"project"`
	Severity string `json:"severity"`
	At       string `json:"at"`
}

// checkpointReadPairEval reads the pair-eval JSONL. A line whose instant cannot be parsed is not
// counted; a file with no countable line leaves its signal unmeasured.
func checkpointReadPairEval(path string) ([]checkpointPairEval, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []checkpointPairEval
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row checkpointPairEval
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		if _, ok := checkpointInstant(row.At); !ok {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// checkpointCompletedState reports whether an exported issue state means the issue left the
// backlog.
func checkpointCompletedState(state string) bool {
	lower := strings.ToLower(strings.TrimSpace(state))
	return strings.Contains(lower, "done") || strings.Contains(lower, "complet")
}

// checkpointCountsAfter counts the instants strictly after a baseline; an empty baseline counts
// the whole history, because a project with no record has not had a checkpoint yet rather than
// having had nothing happen.
func checkpointCountsAfter(instants []checkpointProjectInstant, project string, since time.Time, hasSince bool) int {
	count := 0
	for _, row := range instants {
		if row.project != project {
			continue
		}
		instant, ok := checkpointInstant(row.at)
		if !ok {
			continue
		}
		if hasSince && !instant.After(since) {
			continue
		}
		count++
	}
	return count
}

// checkpointCountsInWindow counts the instants inside a rolling window ending at now.
func checkpointCountsInWindow(instants []checkpointProjectInstant, project string, from, now time.Time) int {
	count := 0
	for _, row := range instants {
		if row.project != project {
			continue
		}
		instant, ok := checkpointInstant(row.at)
		if !ok {
			continue
		}
		if instant.Before(from) || instant.After(now) {
			continue
		}
		count++
	}
	return count
}

// checkpointThresholdsOf is the thresholds a reading runs with: the defaults, overridden by the
// checkpoint settings section.
func checkpointThresholdsOf(cfg *Config) (checkpointThresholds, error) {
	thresholds := checkpointDefaultThresholds
	var doc checkpointSectionDoc
	if cfg != nil {
		if err := cfg.Section("checkpoint", &doc); err != nil {
			return thresholds, fmt.Errorf("the checkpoint section: %w", err)
		}
	}
	for _, override := range []struct {
		value *int
		into  *int
	}{
		{doc.Merges, &thresholds.Merges},
		{doc.RunningHours, &thresholds.RunningHours},
		{doc.NeedsChangesOrSplit, &thresholds.NeedsChangesOrSplit},
		{doc.PairEvalP0P1, &thresholds.PairEvalP0P1},
		{doc.BacklogNet, &thresholds.BacklogNet},
	} {
		if override.value != nil {
			*override.into = *override.value
		}
	}
	return thresholds, nil
}

// Checkpoint is crw manage checkpoint: it reads the relay store read-only and reports, per
// project, whether a mid-project checkpoint is due and why. It never writes to the store or to
// any host state. It is exported because a later caller reads the same judgment.
func Checkpoint(ctx context.Context, e *Env, cfg *Config, opts CheckpointOptions) ([]CheckpointReport, error) {
	thresholds, err := checkpointThresholdsOf(cfg)
	if err != nil {
		return nil, err
	}
	state, err := e.relayHelperState(ctx, cfg)
	if err != nil {
		return nil, err
	}
	st, err := checkpointOpenStore(ctx, state)
	if err != nil {
		return nil, err
	}
	defer st.Close()

	projects, err := st.checkpointProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	merges, err := st.checkpointMerges(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the merge turns: %w", err)
	}
	verdicts, err := st.checkpointVerdicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the verdicts: %w", err)
	}
	decisions, err := st.checkpointSplitDecisions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the decisions: %w", err)
	}
	zoned, err := st.checkpointHasTable(ctx, "dag_plans")
	if err != nil {
		return nil, fmt.Errorf("read the DAG zone: %w", err)
	}
	var integrations []checkpointProjectInstant
	if zoned {
		if integrations, err = st.checkpointIntegrations(ctx); err != nil {
			return nil, fmt.Errorf("read the integration observations: %w", err)
		}
	}

	records, err := checkpointRecordInstants(auditStateDir(e, cfg))
	if err != nil {
		return nil, fmt.Errorf("read the checkpoint records: %w", err)
	}

	var export *checkpointExport
	if opts.LinearExport != "" {
		read, err := checkpointReadExport(opts.LinearExport)
		if err != nil {
			return nil, fmt.Errorf("read the linear export: %w", err)
		}
		export = &read
	}
	var pairEval []checkpointPairEval
	if opts.PairEval != "" {
		if pairEval, err = checkpointReadPairEval(opts.PairEval); err != nil {
			return nil, fmt.Errorf("read the pair evaluation: %w", err)
		}
	}

	if opts.Project != "" {
		projects = []string{opts.Project}
	}
	now := e.Now()
	reports := make([]CheckpointReport, 0, len(projects))
	for _, project := range projects {
		reports = append(reports, checkpointBuildReport(checkpointInput{
			project: project, now: now, thresholds: thresholds,
			merges: merges, verdicts: verdicts, decisions: decisions, integrations: integrations,
			zoned: zoned, records: records[project],
			export: export, pairEval: pairEval,
		}))
	}
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].Project < reports[j].Project })
	return reports, nil
}

// checkpointInput is everything one project's report is built from.
type checkpointInput struct {
	project      string
	now          time.Time
	thresholds   checkpointThresholds
	merges       []checkpointProjectInstant
	verdicts     []checkpointProjectInstant
	decisions    []checkpointProjectInstant
	integrations []checkpointProjectInstant
	zoned        bool
	records      []time.Time
	export       *checkpointExport
	pairEval     []checkpointPairEval
}

// checkpointBuildReport builds one project's reading.
func checkpointBuildReport(in checkpointInput) CheckpointReport {
	report := CheckpointReport{Project: in.project, Reasons: []string{}, Unmeasured: []string{}}
	since, hasSince := time.Time{}, false
	if len(in.records) > 0 {
		earliest, latest := in.records[0], in.records[0]
		for _, instant := range in.records {
			if instant.Before(earliest) {
				earliest = instant
			}
			if instant.After(latest) {
				latest = instant
			}
		}
		since, hasSince = latest, true
		report.Since = latest.UTC().Format(checkpointInstantLayout)
		report.Counts.RunningHours = in.now.Sub(earliest).Hours()
	} else {
		report.Unmeasured = append(report.Unmeasured, checkpointSignalRunningHours)
	}

	report.Counts.MergesSinceCheckpoint = checkpointCountsAfter(in.merges, in.project, since, hasSince)
	report.Counts.NeedsChangesSinceCheckpoint = checkpointCountsAfter(in.verdicts, in.project, since, hasSince)
	report.Counts.SplitDecisionsSinceCheckpoint = checkpointCountsAfter(in.decisions, in.project, since, hasSince)
	if in.zoned {
		count := checkpointCountsAfter(in.integrations, in.project, since, hasSince)
		report.Counts.IntegrationsSinceCheckpoint = &count
	} else {
		report.Unmeasured = append(report.Unmeasured, checkpointSignalIntegrations)
	}

	windowStart := in.now.Add(-checkpointNeedsChangesWindow)
	report.Counts.NeedsChangesOrSplit2h = checkpointCountsInWindow(in.verdicts, in.project, windowStart, in.now) +
		checkpointCountsInWindow(in.decisions, in.project, windowStart, in.now)

	if in.export != nil {
		milestone, unmeasured := checkpointMilestone(in, since, hasSince)
		report.Counts.MilestoneIntegrated = milestone
		report.Unmeasured = append(report.Unmeasured, unmeasured...)
		report.Counts.BacklogNet4h = checkpointBacklogNet(in)
	} else {
		report.Unmeasured = append(report.Unmeasured, checkpointSignalMilestone, checkpointSignalBacklog)
	}
	if in.pairEval != nil {
		count := checkpointPairEvalCount(in, since, hasSince)
		report.Counts.PairEvalP0P1SinceCheckpoint = &count
	} else {
		report.Unmeasured = append(report.Unmeasured, checkpointSignalPairEval)
	}

	report.Reasons = checkpointReasons(in.thresholds, &report)
	report.Due = len(report.Reasons) > 0
	return report
}

// checkpointReasons is every signal that holds, in the fixed order the issue lists them.
func checkpointReasons(thresholds checkpointThresholds, report *CheckpointReport) []string {
	reasons := []string{}
	if report.Counts.MilestoneIntegrated != nil && *report.Counts.MilestoneIntegrated > 0 {
		reasons = append(reasons, checkpointSignalMilestone)
	}
	if report.Counts.MergesSinceCheckpoint >= thresholds.Merges {
		reasons = append(reasons, checkpointSignalMerges)
	}
	if report.Counts.RunningHours >= float64(thresholds.RunningHours) {
		reasons = append(reasons, checkpointSignalRunningHours)
	}
	if report.Counts.NeedsChangesOrSplit2h >= thresholds.NeedsChangesOrSplit {
		reasons = append(reasons, checkpointSignalNeedsChanges)
	}
	if report.Counts.PairEvalP0P1SinceCheckpoint != nil && *report.Counts.PairEvalP0P1SinceCheckpoint >= thresholds.PairEvalP0P1 {
		reasons = append(reasons, checkpointSignalPairEval)
	}
	if report.Counts.BacklogNet4h != nil && *report.Counts.BacklogNet4h > thresholds.BacklogNet {
		reasons = append(reasons, checkpointSignalBacklog)
	}
	return reasons
}

// checkpointMilestone counts the project's integrated milestones and reports whether the
// reading is unmeasured: an export that carries no milestone information cannot measure it.
func checkpointMilestone(in checkpointInput, since time.Time, hasSince bool) (*int, []string) {
	type milestone struct {
		total     int
		completed int
		latest    time.Time
	}
	seen := map[string]*milestone{}
	for _, issue := range in.export.Issues {
		if issue.Project != in.project || strings.TrimSpace(issue.Milestone) == "" {
			continue
		}
		entry, ok := seen[issue.Milestone]
		if !ok {
			entry = &milestone{}
			seen[issue.Milestone] = entry
		}
		entry.total++
		if !checkpointCompletedState(issue.State) {
			continue
		}
		entry.completed++
		if completed, ok := checkpointInstant(issue.CompletedAt); ok && completed.After(entry.latest) {
			entry.latest = completed
		}
	}
	if len(seen) == 0 {
		return nil, []string{checkpointSignalMilestone}
	}
	count := 0
	for _, name := range checkpointSortedKeys(seen) {
		entry := seen[name]
		if entry.total == 0 || entry.completed != entry.total {
			continue
		}
		if hasSince && !entry.latest.After(since) {
			continue
		}
		count++
	}
	return &count, nil
}

// checkpointBacklogNet is the backlog's net change over the rolling window: the issues created
// in it that have not left the backlog, minus the issues that left it in the window.
func checkpointBacklogNet(in checkpointInput) *int {
	from := in.now.Add(-checkpointBacklogWindow)
	net := 0
	for _, issue := range in.export.Issues {
		if issue.Project != in.project {
			continue
		}
		if created, ok := checkpointInstant(issue.CreatedAt); ok && !created.Before(from) && !created.After(in.now) {
			if !checkpointCompletedState(issue.State) {
				net++
			}
		}
		if completed, ok := checkpointInstant(issue.CompletedAt); ok && !completed.Before(from) && !completed.After(in.now) {
			net--
		}
	}
	return &net
}

// checkpointPairEvalCount counts the P0 and P1 findings after the baseline.
func checkpointPairEvalCount(in checkpointInput, since time.Time, hasSince bool) int {
	count := 0
	for _, finding := range in.pairEval {
		if finding.Project != in.project {
			continue
		}
		if finding.Severity != "P0" && finding.Severity != "P1" {
			continue
		}
		instant, ok := checkpointInstant(finding.At)
		if !ok {
			continue
		}
		if hasSince && !instant.After(since) {
			continue
		}
		count++
	}
	return count
}

// checkpointSortedKeys is a map's keys in a stable order, so a report does not move between
// runs.
func checkpointSortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// checkpointProjectKey checks that a project key is one file name below the checkpoint
// directory. A key that is empty, names a directory, or carries a path separator is refused, so
// a record can never be written outside the state directory it belongs to.
func checkpointProjectKey(project string) error {
	if project == "" {
		return errors.New("--project is required")
	}
	if project == "." || project == ".." || strings.ContainsAny(project, "/\\") {
		return fmt.Errorf("the project key %q is not a plain name", project)
	}
	if filepath.Base(project) != project {
		return fmt.Errorf("the project key %q is not a plain name", project)
	}
	return nil
}

// checkpointRecord appends one checkpoint line for a project. The record is the baseline the
// next reading counts after, and it is the management session's own state: the relay zone is
// never written.
func checkpointRecord(e *Env, cfg *Config, project, summaryFile string) (string, error) {
	if err := checkpointProjectKey(project); err != nil {
		return "", err
	}
	if summaryFile == "" {
		return "", errors.New("--summary-file is required")
	}
	summary, err := os.ReadFile(summaryFile)
	if err != nil {
		return "", fmt.Errorf("the summary file: %w", err)
	}
	sum := sha256.Sum256(summary)
	dir := filepath.Join(auditStateDir(e, cfg), checkpointDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, project+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	runes := []rune(string(summary))
	if len(runes) > checkpointSummaryLimit {
		runes = runes[:checkpointSummaryLimit]
	}
	row := map[string]any{
		"project":        project,
		"at":             e.Now().UTC().Format(checkpointInstantLayout),
		"summary_sha256": hex.EncodeToString(sum[:]),
		"summary":        string(runes),
	}
	if err := auditAppendLine(file, path, row); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// checkpointCommand is crw manage checkpoint.
var checkpointCommand = Command{Name: "checkpoint", Summary: "report whether a mid-project checkpoint is due, and record one", Run: checkpointRun}

func init() { Register(checkpointCommand) }

// checkpointUsage is what the command prints.
const checkpointUsage = "usage: crw manage checkpoint [-h] [--project P] [--linear-export F] [--pair-eval F]\n" +
	"       crw manage checkpoint record --project P --summary-file F"

// checkpointRun is crw manage checkpoint. The store is read read-only, so a reading is always
// safe to run beside a live relay. Exit 1 means a project is due, 0 that none is, 3 that the
// store could not be read at all.
func checkpointRun(ctx context.Context, e *Env, args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		fmt.Fprintln(e.Stdout, checkpointUsage)
		return 0
	}
	if len(args) > 0 && args[0] == "record" {
		return checkpointRunRecord(e, args[1:])
	}
	opts, err := checkpointParseArgs(args)
	if err != nil {
		fmt.Fprintln(e.Stderr, checkpointUsage)
		fmt.Fprintf(e.Stderr, "crw manage checkpoint: error: %v\n", err)
		return usageExit
	}
	reports, err := Checkpoint(ctx, e, coreDefaults(e), opts)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage checkpoint: error: %v\n", err)
		return checkpointStoreExit
	}
	data, err := json.Marshal(reports)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage checkpoint: error: %v\n", err)
		return checkpointStoreExit
	}
	fmt.Fprintf(e.Stdout, "%s\n", data)
	for _, report := range reports {
		if report.Due {
			return checkpointDueExit
		}
	}
	return 0
}

// checkpointRunRecord is crw manage checkpoint record.
func checkpointRunRecord(e *Env, args []string) int {
	values, err := checkpointParseFlags(args, map[string]bool{"project": true, "summary-file": true})
	if err == nil && values["project"] == "" {
		err = errors.New("--project is required")
	}
	if err == nil && values["summary-file"] == "" {
		err = errors.New("--summary-file is required")
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, checkpointUsage)
		fmt.Fprintf(e.Stderr, "crw manage checkpoint record: error: %v\n", err)
		return usageExit
	}
	path, err := checkpointRecord(e, coreDefaults(e), values["project"], values["summary-file"])
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage checkpoint record: error: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.Stdout, "{\"project\":%q,\"record\":%q}\n", values["project"], path)
	return 0
}

// checkpointParseArgs reads the reading's flags.
func checkpointParseArgs(args []string) (CheckpointOptions, error) {
	values, err := checkpointParseFlags(args, map[string]bool{"project": true, "linear-export": true, "pair-eval": true})
	if err != nil {
		return CheckpointOptions{}, err
	}
	return CheckpointOptions{
		Project:      values["project"],
		LinearExport: values["linear-export"],
		PairEval:     values["pair-eval"],
	}, nil
}

// checkpointParseFlags reads "--name value" and "--name=value" pairs, refusing anything else. A
// following token that starts another option is a missing value, so the "--name=value" form
// stays the way to pass such a string.
func checkpointParseFlags(args []string, allowed map[string]bool) (map[string]string, error) {
	values := map[string]string{}
	for i := 0; i < len(args); i++ {
		name := args[i]
		if !strings.HasPrefix(name, "--") {
			return nil, fmt.Errorf("unexpected argument %q", name)
		}
		key, value := strings.TrimPrefix(name, "--"), ""
		if eq := strings.IndexByte(key, '='); eq >= 0 {
			key, value = key[:eq], key[eq+1:]
		} else {
			i++
			if i >= len(args) || strings.HasPrefix(args[i], "--") {
				return nil, fmt.Errorf("the option %s needs a value", name)
			}
			value = args[i]
		}
		if !allowed[key] {
			return nil, fmt.Errorf("unknown option %s", name)
		}
		values[key] = value
	}
	return values, nil
}
