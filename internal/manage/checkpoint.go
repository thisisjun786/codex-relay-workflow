package manage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay store a checkpoint reads and the directory a checkpoint record lives in. The store
// is the one the manage relay helper resolves; the record is the management session's own
// state, which is never the relay zone.
const (
	checkpointStoreFile    = "relay.sqlite3"
	checkpointSummaryLimit = 2000
	checkpointDirName      = "checkpoint"
)

// checkpointBusyTimeout is how long a read waits for a writer holding the store.
const (
	checkpointBusyTimeout = 5 * time.Second
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
// 0 and one that could not read the store exits checkpointStoreExit. An input file the reading
// cannot read is no longer an exit of its own: it leaves only the signals that depend on it
// unmeasured, so a broken export is reported rather than fatal.
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
	// ClosedStates are the issue states that mean an issue left the backlog. The backlog signal
	// reads closure from the state, never from the presence of a completion instant, because an
	// export may omit completedAt.
	ClosedStates []string
}

// checkpointDefaultThresholds is the issue's own set.
var checkpointDefaultThresholds = checkpointThresholds{
	Merges: 20, RunningHours: 8, NeedsChangesOrSplit: 3, PairEvalP0P1: 2, BacklogNet: 15,
	ClosedStates: checkpointClosedStates,
}

// checkpointSectionDoc is the checkpoint settings section: one optional override per threshold,
// so a section that names one key leaves the others at their default.
type checkpointSectionDoc struct {
	Merges              *int `json:"merges_since_checkpoint"`
	RunningHours        *int `json:"running_hours"`
	NeedsChangesOrSplit *int `json:"needs_changes_or_split_2h"`
	PairEvalP0P1        *int `json:"pair_eval_p0p1_since_checkpoint"`
	BacklogNet          *int `json:"backlog_net_4h"`
	// ClosedStates is a pointer so a section that names it as an empty list is honoured, while one
	// that omits it leaves the default list in place.
	ClosedStates *[]string `json:"closed_states"`
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
	// UnmeasuredReasons explains every signal this reading could not measure, and every named part
	// of a signal it measured only in part (a milestone whose completion instant is unknown). The
	// Unmeasured list stays the signal names whose count is absent; the reason map also carries the
	// partial misses, so a reader can tell a clean zero from a partly measured one.
	UnmeasuredReasons map[string]string `json:"unmeasured_reasons"`
	Since             string            `json:"since"`
}

// checkpointStore is the read-only handle on the relay store. It is opened by the store's own
// OpenInPlace, which applies the no-sidecar rule (mode=ro, immutable=1 when the write-ahead log
// holds no frame) AND checks after the open that SQLite's main database is the very file whose
// sidecars were examined, so a component replaced between the examination and the open cannot be
// read with the other file's parameters. A reading can neither create a table nor write a row,
// and a table a store predates is an unmeasured reading rather than a repair.
type checkpointStore struct {
	ro *store.ReadOnly
	// snapshot is the store the reading's queries run on inside checkpointInSnapshot: the same
	// read-only connection, with the open transaction selected by the context, so every query of
	// one reading sees one committed state. It is nil outside that call.
	snapshot *store.Store
}

// checkpointInSnapshot runs one reading inside a single deferred snapshot. Every query of the
// reading then sees the same committed state, and the scheduler's own integration judgement reads
// the same rows as the counts beside it. The read-only store takes no writer lock, so this holds
// nothing back from a live relay.
func (s *checkpointStore) checkpointInSnapshot(ctx context.Context, run func(context.Context) error) error {
	return s.ro.ReadSnapshot(ctx, func(ctx context.Context, st *store.Store) error {
		s.snapshot = st
		return run(ctx)
	})
}

// checkpointQuerier is where one reading's queries run: the snapshot's transaction when one is
// open, otherwise the read-only pool.
func (s *checkpointStore) checkpointQuerier(ctx context.Context) checkpointQueryer {
	if s.snapshot != nil {
		return s.snapshot.Q(ctx)
	}
	return s.ro
}

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
	ro, err := store.OpenInPlace(ctx, path, checkpointBusyTimeout)
	if err != nil {
		return nil, fmt.Errorf("relay store: %w", err)
	}
	return &checkpointStore{ro: ro}, nil
}

// Close releases the read-only handle.
func (s *checkpointStore) Close() error { return s.ro.Close() }

// checkpointQueryer is the read-only surface a query needs, which store.ReadOnly satisfies.
type checkpointQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// checkpointHasTable reports whether the store carries a table. The DAG zone is additive, so a
// store written before it lacks tables this reading would otherwise read.
func (s *checkpointStore) checkpointHasTable(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.checkpointQuerier(ctx).QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// checkpointZoneComplete reports whether the store carries every table the integration judgement
// reads. A store that holds only some of them (an install interrupted between the zone's
// statements) is not a measured zero: the scheduler answers not-integrated when
// dag_node_executions is absent, which would read as "no integration" rather than "not measured".
func (s *checkpointStore) checkpointZoneComplete(ctx context.Context) (bool, error) {
	for _, table := range checkpointIntegrationTables {
		present, err := s.checkpointHasTable(ctx, table)
		if err != nil || !present {
			return false, err
		}
	}
	return true, nil
}

// checkpointRows runs one query and scans every row through scan.
func checkpointRows[T any](ctx context.Context, db checkpointQueryer, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
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
	return checkpointRows(ctx, s.checkpointQuerier(ctx), query+" ORDER BY 1", nil, func(rows *sql.Rows) (string, error) {
		var project string
		return project, rows.Scan(&project)
	})
}

// checkpointProjectInstant is one project-scoped instant of the store.
type checkpointProjectInstant struct{ project, at string }

// checkpointMerges reads the landed merge turns, with the instant each landed.
func (s *checkpointStore) checkpointMerges(ctx context.Context) ([]checkpointProjectInstant, error) {
	return checkpointRows(ctx, s.checkpointQuerier(ctx),
		"SELECT project_key, COALESCE(NULLIF(closed_at, ''), updated_at) FROM merge_turns WHERE state = 'landed' ORDER BY turn_id",
		nil, func(rows *sql.Rows) (checkpointProjectInstant, error) {
			var row checkpointProjectInstant
			return row, rows.Scan(&row.project, &row.at)
		})
}

// checkpointVerdicts reads the needs_changes rulings, with the instant each was decided.
func (s *checkpointStore) checkpointVerdicts(ctx context.Context) ([]checkpointProjectInstant, error) {
	return checkpointRows(ctx, s.checkpointQuerier(ctx),
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
	rows, err := checkpointRows(ctx, s.checkpointQuerier(ctx),
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

// checkpointIntegrationTables are the tables the integration judgement and the instant it reads
// need: the plan that carries the project, the acceptances it judges, the executions that say
// which relationship ran a node, and the observations the instant comes from. The DAG zone is
// additive and a store written before it lacks all of them; a store that holds only some (an
// install interrupted between the ledger's statements) must not read as a measured zero, because
// dagsched.ExecutionIntegrated answers (false, false, nil) for a store without
// dag_node_executions, and the instant query would fail on a missing observations table.
var checkpointIntegrationTables = []string{"dag_plans", "dag_acceptances", "dag_node_executions", "dag_integration_observations"}

// checkpointAcceptance is one active acceptance of the DAG zone: what the integration judgement
// is asked about, and the project whose reading counts the answer.
type checkpointAcceptance struct {
	acceptanceID string
	project      string
	relationship string
	event        string
	generation   int64
	revision     string
	head         string
}

// checkpointTargetObservation is one satisfying observation of an acceptance in one target: the
// target, the sequence it was written in, and the instant it recorded.
type checkpointTargetObservation struct {
	repository string
	baseRef    string
	seq        int64
	at         string
}

// checkpointIntegrationReading is one reading of the integration signal: the instants it counted,
// and, per project, why that project's reading is unmeasured although the store carries the zone.
// A project absent from reasons is measured. Keeping the reason per project is what stops one
// project's unreadable observation instant from blanking every other project's count, and what
// lets --project read only its own acceptances.
type checkpointIntegrationReading struct {
	instants []checkpointProjectInstant
	reasons  map[string]string
}

// checkpointIntegrations counts the acceptances the relay scheduler itself calls integrated, and
// the instant each integrated at. The judgement is dagsched's own ExecutionIntegrated (every
// target of the node plus the parent's merged mark, targets.go nodeIntegrated), so this reading
// cannot drift from the scheduler's: an acceptance observed in only one of a node's two targets
// is not counted, and neither is one whose merged mark does not stand on its event, generation
// and revision.
//
// Each acceptance is judged on what it stands on now: acceptance.StandOf resolves the newest valid
// base refresh recorded for it, so an acceptance the parent refreshed onto a later generation is
// judged on that generation's merged mark rather than the one it was accepted with. The reading is
// returned per project: an integrated acceptance whose satisfying observation carries an instant
// this build cannot read leaves only its own project unmeasured, and the other projects keep their
// counts. Passing project restricts the acceptance read itself, so --project never reads another
// project's acceptance.
//
// The instant is read from the observation records alone: per target, the first satisfying
// observation of the current containment run, and the acceptance's instant is the latest of those
// per-target minima. The row selection mirrors dagsched's integratedAt (internal/relay/dagsched/
// edges.go:395), which is the reference implementation; a later change there does not propagate
// here, which this issue's scope accepts.
func (s *checkpointStore) checkpointIntegrations(ctx context.Context, project string) (checkpointIntegrationReading, error) {
	var out checkpointIntegrationReading
	// The scheduler's judgement runs on the snapshot's own transaction, so it reads the same
	// committed state as the counts beside it. A caller that reached here without one would have
	// the judgement read outside the reading, so it is refused rather than served.
	if s.snapshot == nil {
		return out, errors.New("the integration judgement needs the reading's snapshot")
	}
	// The stand is resolved on the same transaction as the judgement, and StandOf wants the store's
	// own querier, so it is taken from the snapshot directly.
	q := s.snapshot.Q(ctx)
	acceptances, err := checkpointRows(ctx, s.checkpointQuerier(ctx),
		"SELECT a.acceptance_id, p.project_key, a.relationship_id, a.event_id, a.execution_generation, a.revision_hash, a.head_sha"+
			" FROM dag_acceptances a JOIN dag_plans p ON p.plan_id = a.plan_id"+
			" WHERE a.state = 'active' AND a.head_sha IS NOT NULL AND a.head_sha <> ''"+
			" AND (? = '' OR p.project_key = ?)"+
			" ORDER BY p.project_key, a.acceptance_id",
		[]any{project, project}, func(rows *sql.Rows) (checkpointAcceptance, error) {
			var row checkpointAcceptance
			return row, rows.Scan(&row.acceptanceID, &row.project, &row.relationship, &row.event, &row.generation, &row.revision, &row.head)
		})
	if err != nil {
		return out, err
	}
	if len(acceptances) == 0 {
		return out, nil
	}
	scheduler := &dagsched.Scheduler{Store: s.snapshot}
	for _, acc := range acceptances {
		stand, err := acceptance.StandOf(ctx, q, acc.acceptanceID, acc.relationship, acc.generation, acc.event, acc.revision, acc.head)
		if err != nil {
			return out, err
		}
		applicable, integrated, err := scheduler.ExecutionIntegrated(ctx, stand.RelationshipID, stand.EventID, stand.Generation, stand.RevisionHash)
		if err != nil {
			return out, err
		}
		if !applicable || !integrated {
			continue
		}
		at, measured, err := s.checkpointIntegrationInstant(ctx, acc, stand)
		if err != nil {
			return out, err
		}
		if !measured {
			// The judgement said integrated, so a satisfying observation exists; one whose instant
			// this build cannot read leaves only that acceptance's project unmeasured rather than
			// dropping the acceptance or blanking every other project's reading.
			if out.reasons == nil {
				out.reasons = map[string]string{}
			}
			out.reasons[acc.project] = "an integrated acceptance of " + acc.project + " carries no readable observation instant"
			continue
		}
		out.instants = append(out.instants, checkpointProjectInstant{project: acc.project, at: at})
	}
	return out, nil
}

// checkpointIntegrationInstant is when one integrated acceptance landed everywhere it had to:
// per target the first satisfying observation of the current containment run, and the latest of
// those across targets (the decided answer's rule). The acceptance is already judged integrated,
// so this reads the instants of that judgement rather than deciding it again.
//
// The merged mark, the observed subject and the landed candidate head are the STAND's, not the
// acceptance's own: after a base refresh the acceptance stands on the later generation, and its own
// event, generation, revision and head would match no observation.
//
// "First" is by observed_seq, the order the relay writes observations in, exactly as the
// reference implementation picks its row (internal/relay/dagsched/edges.go:413). The second
// result is false when the chosen row's instant is not in a form this build reads, which is an
// unmeasured reading rather than a zero.
func (s *checkpointStore) checkpointIntegrationInstant(ctx context.Context, acc checkpointAcceptance, stand acceptance.Stand) (string, bool, error) {
	rows, err := checkpointRows(ctx, s.checkpointQuerier(ctx),
		"SELECT o.repository, o.base_ref, o.observed_seq, o.observed_at"+
			" FROM dag_integration_observations o"+
			" JOIN assignment_marks k ON k.relationship_id = ? AND k.mark = 'merged'"+
			"  AND k.event_id = ? AND k.execution_generation = ? AND k.revision_hash = ?"+
			" WHERE o.acceptance_id = ? AND o.subject_sha = ? AND o.is_ancestor = 1 AND o.reverted_by IS NULL"+
			"  AND (o.merge_turn_id IS NULL OR EXISTS (SELECT 1 FROM merge_turns m WHERE m.turn_id = o.merge_turn_id"+
			"   AND m.state = 'landed' AND m.candidate_head = ? AND m.repository = o.repository AND m.base_ref = o.base_ref))"+
			"  AND NOT EXISTS (SELECT 1 FROM dag_integration_observations o2"+
			"   WHERE o2.acceptance_id = o.acceptance_id AND o2.repository = o.repository"+
			"    AND o2.base_ref = o.base_ref AND o2.observed_seq > o.observed_seq AND o2.is_ancestor = 0)"+
			" ORDER BY o.repository, o.base_ref, o.observed_seq",
		[]any{stand.RelationshipID, stand.EventID, stand.Generation, stand.RevisionHash,
			acc.acceptanceID, stand.Head, stand.Head},
		func(rows *sql.Rows) (checkpointTargetObservation, error) {
			var row checkpointTargetObservation
			return row, rows.Scan(&row.repository, &row.baseRef, &row.seq, &row.at)
		})
	if err != nil {
		return "", false, err
	}
	// Rows arrive in observed_seq order, so the first row of each target is that target's first
	// satisfying observation.
	first := map[[2]string]checkpointTargetObservation{}
	for _, row := range rows {
		key := [2]string{row.repository, row.baseRef}
		if _, seen := first[key]; !seen {
			first[key] = row
		}
	}
	latest := ""
	var latestInstant time.Time
	for _, key := range checkpointTargetKeys(first) {
		row := first[key]
		instant, ok := checkpointInstant(row.at)
		if !ok {
			return "", false, nil
		}
		if latest == "" || instant.After(latestInstant) {
			latest, latestInstant = row.at, instant
		}
	}
	if latest == "" {
		return "", false, nil
	}
	return latest, true, nil
}

// checkpointTargetKeys is the keys of a per-target map in a stable order, so the latest instant
// does not depend on Go's map iteration order.
func checkpointTargetKeys(values map[[2]string]checkpointTargetObservation) [][2]string {
	keys := make([][2]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	return keys
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

// checkpointPairEvalRowLimit is how many unreadable row numbers a reason names, so a file broken
// on every line still produces a short reason.
const checkpointPairEvalRowLimit = 5

// checkpointReadPairEval reads the pair-eval JSONL. Any non-empty row this build cannot read — its
// JSON does not parse, or its instant is in no form this build knows — leaves the whole signal
// unmeasured: a reading that silently dropped such a row would report a partly read file as a
// complete measurement. The error names the file and the unreadable row numbers (up to
// checkpointPairEvalRowLimit of them). Blank lines carry no finding and are skipped.
//
// A file that was given and read with no unreadable row is a measured reading even when it holds no
// finding at all, so the result is an empty slice and never nil: the caller tells "the file was
// given and read" from "no file was given" by nil alone, and a whitespace-only file would otherwise
// be reported as no evaluation having been given.
func checkpointReadPairEval(path string) ([]checkpointPairEval, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := []checkpointPairEval{}
	var unreadable []int
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row checkpointPairEval
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			unreadable = append(unreadable, i+1)
			continue
		}
		if _, ok := checkpointInstant(row.At); !ok {
			unreadable = append(unreadable, i+1)
			continue
		}
		out = append(out, row)
	}
	if len(unreadable) > 0 {
		return nil, fmt.Errorf("%s: %s could not be read", path, checkpointPairEvalRows(unreadable))
	}
	return out, nil
}

// checkpointPairEvalRows renders the row numbers a reading could not read, at most
// checkpointPairEvalRowLimit of them, so the reason names where the file is broken without growing
// with the file.
func checkpointPairEvalRows(rows []int) string {
	limit := len(rows)
	if limit > checkpointPairEvalRowLimit {
		limit = checkpointPairEvalRowLimit
	}
	parts := make([]string, limit)
	for i := 0; i < limit; i++ {
		parts[i] = "row " + strconv.Itoa(rows[i])
	}
	if len(rows) > limit {
		parts = append(parts, "...")
	}
	return strings.Join(parts, ", ")
}

// checkpointClosedStates is the default set of states that mean an issue left the backlog, the
// issue's own list. A project may replace it with the checkpoint section's closed_states.
var checkpointClosedStates = []string{"Done", "Canceled", "Cancelled", "Duplicate"}

// checkpointClosedState reports whether an exported issue state means the issue left the backlog,
// given the configured list. The comparison is on the whole state name, case-folded and trimmed,
// so a custom name such as "Incomplete" or "Not Done" is not read as closed.
func checkpointClosedState(state string, closedStates []string) bool {
	name := strings.ToLower(strings.TrimSpace(state))
	if name == "" {
		return false
	}
	for _, closed := range closedStates {
		if strings.ToLower(strings.TrimSpace(closed)) == name {
			return true
		}
	}
	return false
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

// checkpointCountsInWindow counts the instants inside a rolling window ending at now. The window
// is closed at now; its start is exclusive when it is the checkpoint baseline (startAtBaseline),
// so an event the checkpoint already looked at is not counted again, and inclusive when it is the
// window's own edge.
func checkpointCountsInWindow(instants []checkpointProjectInstant, project string, from, now time.Time, startAtBaseline bool) int {
	count := 0
	for _, row := range instants {
		if row.project != project {
			continue
		}
		instant, ok := checkpointInstant(row.at)
		if !ok {
			continue
		}
		if instant.Before(from) || (startAtBaseline && !instant.After(from)) || instant.After(now) {
			continue
		}
		count++
	}
	return count
}

// checkpointWindowStart is where a rolling window begins: its own edge, or the baseline when the
// baseline reaches at least as far back. The second result is true when the start is the
// baseline, which makes that instant exclusive: the issue makes the checkpoint record the next
// calculation's baseline, so a window reaching behind it would keep reporting events the
// checkpoint already looked at and the signal could never clear. A baseline that coincides
// exactly with the window's own edge is still the baseline the checkpoint wrote, so it is
// exclusive too; the edge is inclusive only when no such baseline exists.
func checkpointWindowStart(now time.Time, window time.Duration, since time.Time, hasSince bool) (time.Time, bool) {
	start := now.Add(-window)
	if hasSince && !since.Before(start) {
		return since, true
	}
	return start, false
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
	if doc.ClosedStates != nil {
		thresholds.ClosedStates = *doc.ClosedStates
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

	// The two input files are read before the store, because a file the reading cannot use is a
	// reason for the signals that depend on it rather than a failure of the whole reading.
	var export *checkpointExport
	exportReason := ""
	if opts.LinearExport != "" {
		read, err := checkpointReadExport(opts.LinearExport)
		if err != nil {
			exportReason = fmt.Sprintf("the linear export could not be read: %v", err)
		} else {
			export = &read
		}
	}
	var pairEval []checkpointPairEval
	pairEvalReason := ""
	if opts.PairEval != "" {
		read, err := checkpointReadPairEval(opts.PairEval)
		if err != nil {
			pairEvalReason = fmt.Sprintf("the pair evaluation could not be read: %v", err)
		} else {
			pairEval = read
		}
	}

	now := e.Now()
	var reports []CheckpointReport
	// One snapshot for the whole reading: the counts and the scheduler's own integration judgement
	// then read the same committed state.
	err = st.checkpointInSnapshot(ctx, func(ctx context.Context) error {
		projects, err := st.checkpointProjects(ctx)
		if err != nil {
			return fmt.Errorf("read the projects: %w", err)
		}
		merges, err := st.checkpointMerges(ctx)
		if err != nil {
			return fmt.Errorf("read the merge turns: %w", err)
		}
		verdicts, err := st.checkpointVerdicts(ctx)
		if err != nil {
			return fmt.Errorf("read the verdicts: %w", err)
		}
		decisions, err := st.checkpointSplitDecisions(ctx)
		if err != nil {
			return fmt.Errorf("read the decisions: %w", err)
		}
		zoned, err := st.checkpointZoneComplete(ctx)
		if err != nil {
			return fmt.Errorf("read the DAG zone: %w", err)
		}
		// The integration read is narrowed to the asked project, so --project never reads another
		// project's acceptance and that project's unreadable instant cannot reach this report.
		var integrations checkpointIntegrationReading
		if zoned {
			if integrations, err = st.checkpointIntegrations(ctx, opts.Project); err != nil {
				return fmt.Errorf("read the integration observations: %w", err)
			}
		}

		records, err := checkpointRecordInstants(auditStateDir(e, cfg))
		if err != nil {
			return fmt.Errorf("read the checkpoint records: %w", err)
		}

		if opts.Project != "" {
			projects = []string{opts.Project}
		}
		reports = make([]CheckpointReport, 0, len(projects))
		for _, project := range projects {
			reports = append(reports, checkpointBuildReport(checkpointInput{
				project: project, now: now, thresholds: thresholds,
				closedStates: thresholds.ClosedStates,
				merges:       merges, verdicts: verdicts, decisions: decisions, integrations: integrations,
				zoned: zoned, records: records[project],
				export: export, pairEval: pairEval,
				exportReason: exportReason, pairEvalReason: pairEvalReason,
			}))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(reports, func(i, j int) bool { return reports[i].Project < reports[j].Project })
	return reports, nil
}

// checkpointInput is everything one project's report is built from.
type checkpointInput struct {
	project      string
	now          time.Time
	thresholds   checkpointThresholds
	closedStates []string
	merges       []checkpointProjectInstant
	verdicts     []checkpointProjectInstant
	decisions    []checkpointProjectInstant
	integrations checkpointIntegrationReading
	zoned        bool
	records      []time.Time
	export       *checkpointExport
	pairEval     []checkpointPairEval
	// exportReason is why the linear export could not be read or parsed, and pairEvalReason the
	// same for the pair evaluation. Either one leaves only the signals that depend on that input
	// unmeasured and the rest of the reading computed.
	exportReason   string
	pairEvalReason string
}

// checkpointBuildReport builds one project's reading.
func checkpointBuildReport(in checkpointInput) CheckpointReport {
	report := CheckpointReport{
		Project: in.project, Reasons: []string{}, Unmeasured: []string{},
		UnmeasuredReasons: map[string]string{},
	}
	// unmeasured records one signal as unmeasured with its reason, so the list and the reason map
	// cannot drift apart.
	unmeasured := func(signal, reason string) {
		report.Unmeasured = append(report.Unmeasured, signal)
		report.UnmeasuredReasons[signal] = reason
	}
	since, hasSince := time.Time{}, false
	if len(in.records) > 0 {
		latest := in.records[0]
		for _, instant := range in.records {
			if instant.After(latest) {
				latest = instant
			}
		}
		since, hasSince = latest, true
		report.Since = latest.UTC().Format(checkpointInstantLayout)
		// running_hours is measured from the baseline too, so recording a checkpoint clears it.
		// A signal anchored at the project's first record would stay true forever once eight hours
		// passed, because no later record could move it: it would report due on every reading and
		// the tool could never say the project was up to date.
		report.Counts.RunningHours = in.now.Sub(latest).Hours()
	} else {
		unmeasured(checkpointSignalRunningHours, "the project has no checkpoint record, so there is no baseline to measure from")
	}

	report.Counts.MergesSinceCheckpoint = checkpointCountsAfter(in.merges, in.project, since, hasSince)
	report.Counts.NeedsChangesSinceCheckpoint = checkpointCountsAfter(in.verdicts, in.project, since, hasSince)
	report.Counts.SplitDecisionsSinceCheckpoint = checkpointCountsAfter(in.decisions, in.project, since, hasSince)
	if in.zoned {
		if reason, bad := in.integrations.reasons[in.project]; bad {
			unmeasured(checkpointSignalIntegrations, reason)
		} else {
			count := checkpointCountsAfter(in.integrations.instants, in.project, since, hasSince)
			report.Counts.IntegrationsSinceCheckpoint = &count
		}
	} else {
		unmeasured(checkpointSignalIntegrations, "the store has no DAG zone, so no integration is recorded to read")
	}

	windowStart, startAtBaseline := checkpointWindowStart(in.now, checkpointNeedsChangesWindow, since, hasSince)
	report.Counts.NeedsChangesOrSplit2h = checkpointCountsInWindow(in.verdicts, in.project, windowStart, in.now, startAtBaseline) +
		checkpointCountsInWindow(in.decisions, in.project, windowStart, in.now, startAtBaseline)

	switch {
	case in.exportReason != "":
		unmeasured(checkpointSignalMilestone, in.exportReason)
		unmeasured(checkpointSignalBacklog, in.exportReason)
	case in.export != nil:
		milestone, skipped, milestoneReason := checkpointMilestone(in, since, hasSince)
		report.Counts.MilestoneIntegrated = milestone
		if milestone == nil {
			if milestoneReason == "" {
				milestoneReason = "every fully closed milestone of the project has an issue with an unknown completion instant, so when one integrated is unknown"
			}
			unmeasured(checkpointSignalMilestone, milestoneReason)
		}
		for _, name := range skipped {
			report.UnmeasuredReasons[checkpointMilestoneKey(name)] = "the milestone " + name + " is fully closed but at least one of its issues carries no completion instant, so when it integrated is unknown"
		}
		backlog, backlogReason, backlogMeasured := checkpointBacklogNet(in, since, hasSince)
		report.Counts.BacklogNet4h = backlog
		if !backlogMeasured {
			unmeasured(checkpointSignalBacklog, backlogReason)
		}
	default:
		unmeasured(checkpointSignalMilestone, "no linear export was given, so no milestone is read")
		unmeasured(checkpointSignalBacklog, "no linear export was given, so no backlog issue is read")
	}
	switch {
	case in.pairEvalReason != "":
		unmeasured(checkpointSignalPairEval, in.pairEvalReason)
	case in.pairEval != nil:
		count := checkpointPairEvalCount(in, since, hasSince)
		report.Counts.PairEvalP0P1SinceCheckpoint = &count
	default:
		unmeasured(checkpointSignalPairEval, "no pair evaluation was given, so no P0 or P1 finding is read")
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

// checkpointMilestoneKey namespaces a skipped milestone in the report's reason map, so a
// milestone whose name happens to equal a signal identifier cannot overwrite that signal's own
// explanation. Signal identifiers carry no colon, so the two namespaces cannot collide.
func checkpointMilestoneKey(name string) string { return "milestone:" + name }

// checkpointMilestone counts the project's integrated milestones. A milestone whose every issue
// is closed has integrated, and its integration instant is its LAST issue's completion, so the
// milestone's instant is knowable only when every closed issue of it carries a completion
// instant. A fully closed milestone with any unknown completion instant is skipped and named: the
// unknown one may have closed after the baseline, so counting the known maximum would report a
// false zero. A single such milestone no longer blanks the whole signal.
//
// The second result names the milestones that were skipped for unknown timing, which the report
// carries as reasons whether or not the signal itself ended up unmeasured. The third is the
// signal's own reason when the export names no milestone for the project at all, which is a
// missing input rather than an unknown time and must not be reported as one.
func checkpointMilestone(in checkpointInput, since time.Time, hasSince bool) (*int, []string, string) {
	type milestone struct {
		total     int
		completed int
		latest    time.Time
		// untimed counts the closed issues whose completion instant is missing or unreadable.
		// The milestone's instant is the last issue's, so any one of them makes it unknowable.
		untimed int
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
		if !checkpointClosedState(issue.State, in.closedStates) {
			continue
		}
		entry.completed++
		completed, ok := checkpointInstant(issue.CompletedAt)
		if !ok {
			entry.untimed++
			continue
		}
		if completed.After(entry.latest) {
			entry.latest = completed
		}
	}
	if len(seen) == 0 {
		return nil, nil, "the linear export names no milestone for the project, so no milestone is read"
	}
	count := 0
	var skipped []string
	closed := 0
	timedClosed := 0
	for _, name := range checkpointSortedKeys(seen) {
		entry := seen[name]
		if entry.total == 0 || entry.completed != entry.total {
			continue
		}
		closed++
		if entry.untimed > 0 {
			skipped = append(skipped, name)
			continue
		}
		timedClosed++
		if hasSince && !entry.latest.After(since) {
			continue
		}
		count++
	}
	// The decided answer: the signal is unmeasured only when every fully closed milestone has
	// unknown timing. A milestone whose every closed issue is timed but predates the baseline
	// keeps the signal measured (0), because its integration instant is knowable.
	if closed > 0 && timedClosed == 0 {
		return nil, skipped, ""
	}
	return &count, skipped, ""
}

// checkpointBacklogNet is the backlog's net change over the rolling window: the issues that
// entered it in the window (created then) minus the issues that left it in the window (closed
// then). An issue created and closed inside one window therefore counts zero, not minus one,
// which is the change the backlog actually saw.
//
// Whether an issue left the backlog is its state, matched against the configured closed list,
// never the presence of a completion instant: an export may omit completedAt, and an issue that
// is closed counts as having left even then. The two results beside the count are the reason and
// whether the signal was measured: a closed issue with no completion instant that was created
// outside the window leaves the net unknowable for this reading, because when it left is
// unknown, so the signal is unmeasured rather than a guess.
func checkpointBacklogNet(in checkpointInput, since time.Time, hasSince bool) (*int, string, bool) {
	from, startAtBaseline := checkpointWindowStart(in.now, checkpointBacklogWindow, since, hasSince)
	inWindow := func(instant time.Time) bool {
		if instant.Before(from) || instant.After(in.now) {
			return false
		}
		return !startAtBaseline || instant.After(from)
	}
	net := 0
	for _, issue := range in.export.Issues {
		if issue.Project != in.project {
			continue
		}
		created, createdOK := checkpointInstant(issue.CreatedAt)
		createdInside := createdOK && inWindow(created)
		if createdInside {
			net++
		}
		// An open issue is still in the backlog whatever completion instant it carries, so only
		// its creation moves the net; the leftover instant is ignored.
		if !checkpointClosedState(issue.State, in.closedStates) {
			continue
		}
		completed, completedOK := checkpointInstant(issue.CompletedAt)
		switch {
		case completedOK && inWindow(completed):
			net--
		case !completedOK && createdInside:
			// Closed with no completion instant but created inside the window: it entered and
			// left inside it, so it nets to zero. The subtraction completes the pair.
			net--
		case !completedOK:
			// Closed with no completion instant and not known to have been created inside the
			// window: when it left is unknown, so the net cannot be stated for this reading.
			return nil, "a closed issue of " + in.project + " carries no completion instant and was not created inside the window, so when it left the backlog is unknown", false
		}
	}
	return &net, "", true
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
	// O_NOFOLLOW refuses a record path that is a symbolic link, so an existing link cannot send
	// the append to a target outside the state directory; the file is then checked to be a regular
	// one, so a fifo or device at that path is refused too.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	if info, err := file.Stat(); err != nil {
		file.Close()
		return "", err
	} else if !info.Mode().IsRegular() {
		file.Close()
		return "", fmt.Errorf("the record path %s is not a regular file", path)
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
