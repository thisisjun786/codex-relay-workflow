package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The integration target record (CRW-1304). The relay records which remote branch an integration ref is pushed to, and the head it read
// on that remote branch, so a move of the remote outside the integration path is seen without a push (docs/relay/dag-scheduler.md).
// A row is one of two kinds. A target row says "this integration ref of this checkout is pushed to this remote branch"; the newest
// target row of an integration ref is its current target. An observed row repeats the target's identity and names the head the relay
// read on the remote branch; the newest observed row of a target is the last head read. Rows are never edited or removed.

// The kinds and sources of a row.
const (
	TargetKindTarget   = "target"
	TargetKindObserved = "observed"

	// TargetSourceRecord is dag-integration-target-record, TargetSourceIntegrate the --remote and --remote-ref flags of dag-integrate,
	// TargetSourcePush a dag-integrate-push that read the remote head, TargetSourceCheck dag-integration-target-check.
	TargetSourceRecord    = "record"
	TargetSourceIntegrate = "integrate"
	TargetSourcePush      = "push"
	TargetSourceCheck     = "check"
)

// integrationTargetZone is appended to the zone ledger (dagZone) after the agent acceptance tables.
var integrationTargetZone = []string{
	`CREATE TABLE IF NOT EXISTS dag_integration_targets (
    target_id       TEXT PRIMARY KEY CHECK (target_id <> ''),
    kind            TEXT NOT NULL CHECK (kind IN ('target', 'observed')),
    checkout        TEXT NOT NULL CHECK (checkout <> ''),
    integration_ref TEXT NOT NULL CHECK (integration_ref <> ''),
    remote          TEXT NOT NULL CHECK (remote <> ''),
    remote_ref      TEXT NOT NULL CHECK (remote_ref <> ''),
    plan_id         TEXT NOT NULL,
    observed_head   TEXT NOT NULL,
    source          TEXT NOT NULL CHECK (source IN ('record', 'integrate', 'push', 'check')),
    recorded_by     TEXT NOT NULL CHECK (recorded_by <> ''),
    recorded_at     TEXT NOT NULL,
    CHECK ((kind = 'target' AND observed_head = '') OR (kind = 'observed' AND observed_head <> ''))
)`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_no_update BEFORE UPDATE ON dag_integration_targets
BEGIN SELECT RAISE(ABORT, 'dag_integration_targets rows are append-only: never updated'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_no_delete BEFORE DELETE ON dag_integration_targets
BEGIN SELECT RAISE(ABORT, 'dag_integration_targets rows are append-only: never deleted'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_target_id_not_null BEFORE INSERT ON dag_integration_targets
WHEN NEW.target_id IS NULL
BEGIN SELECT RAISE(ABORT, 'dag_integration_targets.target_id is NULL: a row is addressed by a non-empty id'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_pk_guard BEFORE INSERT ON dag_integration_targets
WHEN EXISTS (SELECT 1 FROM dag_integration_targets WHERE target_id = NEW.target_id)
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: dag_integration_targets.target_id'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_rowid_guard BEFORE INSERT ON dag_integration_targets
WHEN NEW.rowid <> -1 AND EXISTS (SELECT 1 FROM dag_integration_targets WHERE rowid = NEW.rowid)
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: dag_integration_targets.rowid'); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_rowid_minus1_mark_guard BEFORE INSERT ON dag_integration_targets
WHEN NEW.rowid = -1
BEGIN DELETE FROM dag_zone_rowid_marks WHERE table_name = 'dag_integration_targets';
  INSERT INTO dag_zone_rowid_marks (table_name) SELECT 'dag_integration_targets' WHERE EXISTS (SELECT 1 FROM dag_integration_targets WHERE rowid = -1); END`,
	`CREATE TRIGGER IF NOT EXISTS dag_integration_targets_rowid_minus1_guard AFTER INSERT ON dag_integration_targets
WHEN NEW.rowid = -1 AND EXISTS (SELECT 1 FROM dag_zone_rowid_marks WHERE table_name = 'dag_integration_targets')
BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: dag_integration_targets.rowid'); END`,
}

// IntegrationTargetRow is one dag_integration_targets row. TargetID is computed by RecordIntegrationTarget and left empty by callers.
type IntegrationTargetRow struct {
	TargetID       string
	Kind           string
	Checkout       string
	IntegrationRef string
	Remote         string
	RemoteRef      string
	PlanID         string
	// ObservedHead is the head read on the remote branch: set on an observed row, empty on a target row.
	ObservedHead string
	Source       string
	RecordedBy   string
	RecordedAt   string
}

const integrationTargetColumns = "target_id, kind, checkout, integration_ref, remote, remote_ref, plan_id, observed_head, source, recorded_by, recorded_at"

func scanIntegrationTarget(row scanner) (IntegrationTargetRow, error) {
	var r IntegrationTargetRow
	err := row.Scan(&r.TargetID, &r.Kind, &r.Checkout, &r.IntegrationRef, &r.Remote, &r.RemoteRef, &r.PlanID, &r.ObservedHead, &r.Source, &r.RecordedBy, &r.RecordedAt)
	return r, err
}

// checkIntegrationTarget is the shape the table's CHECKs require, said before a write so the caller reads which part is wrong.
func checkIntegrationTarget(r IntegrationTargetRow) error {
	for name, value := range map[string]string{"checkout": r.Checkout, "integration ref": r.IntegrationRef, "remote": r.Remote, "remote ref": r.RemoteRef, "actor": r.RecordedBy} {
		if value == "" {
			return fmt.Errorf("an integration target row names its %s", name)
		}
	}
	switch r.Source {
	case TargetSourceRecord, TargetSourceIntegrate, TargetSourcePush, TargetSourceCheck:
	default:
		return fmt.Errorf("an integration target row has no source %q", r.Source)
	}
	switch {
	case r.Kind == TargetKindTarget && r.ObservedHead == "":
	case r.Kind == TargetKindObserved && r.ObservedHead != "":
	default:
		return fmt.Errorf("a %q integration target row holds the observed head %q: a target row holds none and an observed row holds one", r.Kind, r.ObservedHead)
	}
	return nil
}

// RecordIntegrationTarget appends one row and answers true, unless the newest row of its kind for the same integration ref (a target row)
// or the same remote branch (an observed row) already says it: a target row for the same remote and remote ref, an observed row for the
// same head. Then nothing is written and it answers false, so a repeat of the same fact is a no-op and a change, also a return to an
// earlier value, is a new row. The comparison and the write run in one transaction. A store that predates the table is extended by its next
// write open, so the table is there.
func RecordIntegrationTarget(ctx context.Context, s *Store, r IntegrationTargetRow) (appended bool, err error) {
	if err := checkIntegrationTarget(r); err != nil {
		return false, err
	}
	err = s.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		newest, found, err := newestIntegrationTarget(txCtx, s, r)
		if err != nil {
			return err
		}
		if found && newest.sameFact(r) {
			return nil
		}
		var count int64
		if err := s.q(txCtx).QueryRowContext(txCtx, "SELECT COUNT(*) FROM dag_integration_targets").Scan(&count); err != nil {
			return err
		}
		r.TargetID = integrationTargetID(r, count)
		_, err = s.exec(txCtx, "INSERT INTO dag_integration_targets ("+integrationTargetColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			r.TargetID, r.Kind, r.Checkout, r.IntegrationRef, r.Remote, r.RemoteRef, r.PlanID, r.ObservedHead, r.Source, r.RecordedBy, r.RecordedAt)
		appended = err == nil
		return err
	})
	return appended, err
}

// sameFact is whether a stored newest row already says what r says: the same remote branch for a target row, the same head for an observed row.
func (r IntegrationTargetRow) sameFact(o IntegrationTargetRow) bool {
	if r.Kind == TargetKindTarget {
		return r.Remote == o.Remote && r.RemoteRef == o.RemoteRef
	}
	return r.ObservedHead == o.ObservedHead
}

// newestIntegrationTarget reads the newest row r is compared with: the newest target row of its integration ref, or the newest observed row of its remote branch.
func newestIntegrationTarget(ctx context.Context, s *Store, r IntegrationTargetRow) (IntegrationTargetRow, bool, error) {
	query := "SELECT " + integrationTargetColumns + " FROM dag_integration_targets WHERE kind = ? AND checkout = ? AND integration_ref = ?"
	args := []any{r.Kind, r.Checkout, r.IntegrationRef}
	if r.Kind == TargetKindObserved {
		query += " AND remote = ? AND remote_ref = ?"
		args = append(args, r.Remote, r.RemoteRef)
	}
	row, err := queryRow(ctx, s, scanIntegrationTarget, query+" ORDER BY rowid DESC LIMIT 1", args...)
	if errors.Is(err, sql.ErrNoRows) {
		return IntegrationTargetRow{}, false, nil
	}
	return row, err == nil, err
}

// integrationTargetID names a row by its content and its place in the table, so a return to an earlier value is a row of its own.
func integrationTargetID(r IntegrationTargetRow, position int64) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{fmt.Sprint(position), r.Kind, r.Checkout, r.IntegrationRef, r.Remote, r.RemoteRef, r.PlanID, r.ObservedHead, r.Source, r.RecordedBy, r.RecordedAt}, "\x00")))
	return "dit-" + hex.EncodeToString(sum[:16])
}

// IntegrationTargets reads the current target of every integration ref: its newest target row, in the order the refs were first recorded.
// A store that predates the table holds none.
func IntegrationTargets(ctx context.Context, s *Store) ([]IntegrationTargetRow, error) {
	present, err := dagZoneTable(ctx, s, "dag_integration_targets")
	if err != nil || !present {
		return nil, err
	}
	return queryRows(ctx, s, scanIntegrationTarget, "SELECT "+integrationTargetColumns+" FROM dag_integration_targets t WHERE kind = 'target'"+
		" AND rowid = (SELECT MAX(rowid) FROM dag_integration_targets n WHERE n.kind = 'target' AND n.checkout = t.checkout AND n.integration_ref = t.integration_ref)"+
		" ORDER BY (SELECT MIN(rowid) FROM dag_integration_targets f WHERE f.kind = 'target' AND f.checkout = t.checkout AND f.integration_ref = t.integration_ref)")
}

// IntegrationTargetRecorded is whether any target row names this integration ref of this checkout on this remote branch, now or earlier.
func IntegrationTargetRecorded(ctx context.Context, s *Store, checkout, integrationRef, remote, remoteRef string) (bool, error) {
	present, err := dagZoneTable(ctx, s, "dag_integration_targets")
	if err != nil || !present {
		return false, err
	}
	var one int
	err = s.q(ctx).QueryRowContext(ctx, "SELECT 1 FROM dag_integration_targets WHERE kind = 'target' AND checkout = ? AND integration_ref = ? AND remote = ? AND remote_ref = ? LIMIT 1",
		checkout, integrationRef, remote, remoteRef).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// LatestIntegrationObservation reads the newest observed row of the remote branch a target names.
func LatestIntegrationObservation(ctx context.Context, s *Store, target IntegrationTargetRow) (IntegrationTargetRow, bool, error) {
	present, err := dagZoneTable(ctx, s, "dag_integration_targets")
	if err != nil || !present {
		return IntegrationTargetRow{}, false, err
	}
	target.Kind = TargetKindObserved
	return newestIntegrationTarget(ctx, s, target)
}

// IntegrationTargetsOfPlan reads the current targets whose record names the plan.
func IntegrationTargetsOfPlan(ctx context.Context, s *Store, plan string) ([]IntegrationTargetRow, error) {
	all, err := IntegrationTargets(ctx, s)
	if err != nil {
		return nil, err
	}
	var out []IntegrationTargetRow
	for _, t := range all {
		if t.PlanID == plan {
			out = append(out, t)
		}
	}
	return out, nil
}
