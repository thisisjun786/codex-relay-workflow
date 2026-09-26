package store

import (
	"context"
	"database/sql"
	"strings"
)

// Edit-region tables: edit_regions, edit_agreements, edit_followups, edit_revision_marks,
// edit_reaffirmations.

// RecordEditRegion is editregion.py:612: a place is classified once, a replay changes nothing.
func (s *Store) RecordEditRegion(ctx context.Context, r EditRegionsRow) error {
	_, err := s.exec(ctx, "INSERT INTO edit_regions (region_id, repository, base_revision, path,"+
		" region_kind, region_key, region_class, regenerate_from, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT (region_id) DO NOTHING",
		r.RegionID, r.Repository, r.BaseRevision, r.Path, r.RegionKind, r.RegionKey, r.RegionClass,
		r.RegenerateFrom, r.RecordedAt)
	return err
}

// EditRegion is editregion.py:536.
func (s *Store) EditRegion(ctx context.Context, regionID string) (EditRegionsRow, error) {
	return queryRow(ctx, s, scanEditRegions, "SELECT "+editRegionsColumns+" FROM edit_regions WHERE region_id = ?", regionID)
}

// InsertEditAgreement is editregion.py:642 EditRegions.propose.
func (s *Store) InsertEditAgreement(ctx context.Context, a EditAgreementsRow) error {
	_, err := s.exec(ctx, "INSERT INTO edit_agreements (agreement_id, region_id, repository,"+
		" base_revision, left_project, right_project, peer_link_id,"+
		" proposer_task_id, issue_key, constraint_text, left_condition,"+
		" right_condition, left_accepted_at, right_accepted_at, next_owner,"+
		" state, tenure, supersedes, proposed_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		a.AgreementID, a.RegionID, a.Repository, a.BaseRevision, a.LeftProject, a.RightProject,
		a.PeerLinkID, a.ProposerTaskID, a.IssueKey, a.ConstraintText, a.LeftCondition,
		a.RightCondition, a.LeftAcceptedAt, a.RightAcceptedAt, a.NextOwner, a.State, a.Tenure,
		a.Supersedes, a.ProposedAt, a.UpdatedAt)
	return err
}

// EditAgreement is editregion.py:190 EditRegions.agreement.
func (s *Store) EditAgreement(ctx context.Context, agreementID string) (EditAgreementsRow, error) {
	return queryRow(ctx, s, scanEditAgreements, "SELECT "+editAgreementsColumns+" FROM edit_agreements WHERE agreement_id = ?", agreementID)
}

// LiveEditAgreement is editregion.py:569: the pair's live agreement on one region (a replay).
func (s *Store) LiveEditAgreement(ctx context.Context, regionID, left, right string) (EditAgreementsRow, error) {
	return queryRow(ctx, s, scanEditAgreements, "SELECT "+editAgreementsColumns+" FROM edit_agreements"+
		"  WHERE region_id = ? AND left_project = ? AND right_project = ?"+
		"    AND state IN ('proposed','agreed','reopened') AND superseded_by IS NULL", regionID, left, right)
}

// HighestAgreementTenure is editregion.py:625; 0 for a pair that never agreed on the region.
func (s *Store) HighestAgreementTenure(ctx context.Context, regionID, left, right string) (int64, error) {
	var top sql.NullInt64
	err := s.q(ctx).QueryRowContext(ctx, "SELECT MAX(tenure) AS top FROM edit_agreements"+
		"  WHERE region_id = ? AND left_project = ? AND right_project = ?", regionID, left, right).Scan(&top)
	return top.Int64, err
}

// RegionAgreement is one row of editregion.py:208 EditRegions.show: an agreement with its place.
type RegionAgreement struct {
	EditAgreementsRow
	RegionPath     string
	RegionKind     string
	RegionKey      string
	RegionClass    string
	RegenerateFrom sql.NullString
}

// RegionAgreements is editregion.py:208, in proposal order.
func (s *Store) RegionAgreements(ctx context.Context, repository string) ([]RegionAgreement, error) {
	return queryRows(ctx, s, func(row scanner) (RegionAgreement, error) {
		var r RegionAgreement
		a := &r.EditAgreementsRow
		return r, row.Scan(&a.AgreementID, &a.RegionID, &a.Repository, &a.BaseRevision, &a.LeftProject,
			&a.RightProject, &a.PeerLinkID, &a.ProposerTaskID, &a.IssueKey, &a.ConstraintText,
			&a.LeftCondition, &a.RightCondition, &a.LeftAcceptedAt, &a.RightAcceptedAt, &a.NextOwner,
			&a.State, &a.Tenure, &a.Supersedes, &a.SupersededBy, &a.CloseReason, &a.ProposedAt,
			&a.UpdatedAt, &a.ClosedAt, &r.RegionPath, &r.RegionKind, &r.RegionKey, &r.RegionClass,
			&r.RegenerateFrom)
	}, "SELECT "+prefixed("a.", editAgreementsColumns)+", r.path AS region_path, r.region_kind AS region_kind,"+
		"       r.region_key AS region_key, r.region_class AS region_class,"+
		"       r.regenerate_from AS regenerate_from"+
		"  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"+
		" WHERE a.repository = ? ORDER BY a.proposed_at, a.agreement_id", repository)
}

// AcceptEditAgreementSide is editregion.py:993; side is "left" or "right".
func (s *Store) AcceptEditAgreementSide(ctx context.Context, agreementID, side, at string) error {
	column := "left_accepted_at"
	if side == "right" {
		column = "right_accepted_at"
	}
	_, err := s.exec(ctx, "UPDATE edit_agreements SET "+column+" = ?, updated_at = ?"+
		" WHERE agreement_id = ?", at, at, agreementID)
	return err
}

// SetEditAgreementState is editregion.py:1000 (agreed once both sides accepted).
func (s *Store) SetEditAgreementState(ctx context.Context, agreementID, state, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_agreements SET state = ?, updated_at = ?"+
		" WHERE agreement_id = ?", state, at, agreementID)
	return err
}

// CloseEditAgreement is editregion.py:1022 (withdrawn or released).
func (s *Store) CloseEditAgreement(ctx context.Context, agreementID, state, reason, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"+
		" updated_at = ? WHERE agreement_id = ?", state, reason, at, at, agreementID)
	return err
}

// SupersedeEditAgreement is editregion.py:898: a carried agreement is released onto its successor.
func (s *Store) SupersedeEditAgreement(ctx context.Context, agreementID, released, reason, successor, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"+
		" superseded_by = ?, updated_at = ? WHERE agreement_id = ?", released, reason, at, successor, at, agreementID)
	return err
}

// ReopenEditAgreements is editregion.py:1205: agreements on a superseded revision reopen.
func (s *Store) ReopenEditAgreements(ctx context.Context, repository, fromRevision, reopened, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_agreements SET state = ?, updated_at = ?"+
		" WHERE repository = ? AND base_revision = ?"+
		"   AND state IN ('proposed','agreed') AND superseded_by IS NULL", reopened, at, repository, fromRevision)
	return err
}

// RecordEditFollowup is editregion.py:1374: a restated follow-up converges.
func (s *Store) RecordEditFollowup(ctx context.Context, f EditFollowupsRow) error {
	_, err := s.exec(ctx, "INSERT INTO edit_followups (followup_id, agreement_id, trigger_text,"+
		" acceptance_text, issue_ref, assignee_task_id, assignee_project,"+
		" accepted_at, state, recorded_by, recorded_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT (followup_id) DO NOTHING",
		f.FollowupID, f.AgreementID, f.TriggerText, f.AcceptanceText, f.IssueRef, f.AssigneeTaskID,
		f.AssigneeProject, f.AcceptedAt, f.State, f.RecordedBy, f.RecordedAt, f.UpdatedAt)
	return err
}

// EditFollowup is editregion.py:1385.
func (s *Store) EditFollowup(ctx context.Context, followupID string) (EditFollowupsRow, error) {
	return queryRow(ctx, s, scanEditFollowups, "SELECT "+editFollowupsColumns+" FROM edit_followups WHERE followup_id = ?", followupID)
}

// EditFollowups is editregion.py:251.
func (s *Store) EditFollowups(ctx context.Context, agreementID string) ([]EditFollowupsRow, error) {
	return queryRows(ctx, s, scanEditFollowups, "SELECT "+editFollowupsColumns+" FROM edit_followups WHERE agreement_id = ?"+
		" ORDER BY recorded_at, followup_id", agreementID)
}

// AcceptEditFollowup is editregion.py:1428.
func (s *Store) AcceptEditFollowup(ctx context.Context, followupID, actor, project, accepted, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_followups SET assignee_task_id = ?, assignee_project = ?,"+
		" accepted_at = ?, state = ? , updated_at = ? WHERE followup_id = ?", actor, project, at, accepted, at, followupID)
	return err
}

// SettleEditFollowup is editregion.py:1492.
func (s *Store) SettleEditFollowup(ctx context.Context, followupID, disposition string, reason sql.NullString, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_followups SET state = ?, close_reason = ?, updated_at = ?"+
		" WHERE followup_id = ?", disposition, reason, at, followupID)
	return err
}

// RecordEditRevisionMark is editregion.py:1189; UNIQUE(repository, from_revision) keeps one
// successor per revision.
func (s *Store) RecordEditRevisionMark(ctx context.Context, m EditRevisionMarksRow) error {
	_, err := s.exec(ctx, "INSERT INTO edit_revision_marks (mark_id, repository, from_revision,"+
		" to_revision, actor, recorded_at) VALUES (?,?,?,?,?,?)",
		m.MarkID, m.Repository, m.FromRevision, m.ToRevision, m.Actor, m.RecordedAt)
	return err
}

// EditRevisionMark is editregion.py:201: the mark leaving a revision, if it was superseded.
func (s *Store) EditRevisionMark(ctx context.Context, repository, fromRevision string) (EditRevisionMarksRow, error) {
	return queryRow(ctx, s, scanEditRevisionMarks, "SELECT "+editRevisionMarksColumns+" FROM edit_revision_marks"+
		"  WHERE repository = ? AND from_revision = ?", repository, fromRevision)
}

// RecordEditReaffirmation is editregion.py:904.
func (s *Store) RecordEditReaffirmation(ctx context.Context, r EditReaffirmationsRow) error {
	_, err := s.exec(ctx, "INSERT INTO edit_reaffirmations (agreement_id, predecessor_id, actor, actor_project,"+
		" from_revision, to_revision, constraint_revision, left_condition_revision,"+
		" right_condition_revision, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?)",
		r.AgreementID, r.PredecessorID, r.Actor, r.ActorProject, r.FromRevision, r.ToRevision,
		r.ConstraintRevision, r.LeftConditionRevision, r.RightConditionRevision, r.RecordedAt)
	return err
}

// EditReaffirmation is editregion.py:310.
func (s *Store) EditReaffirmation(ctx context.Context, agreementID string) (EditReaffirmationsRow, error) {
	return queryRow(ctx, s, scanEditReaffirmations, "SELECT "+editReaffirmationsColumns+" FROM edit_reaffirmations WHERE agreement_id = ?", agreementID)
}

// prefixed qualifies every column of a column list with a table alias.
func prefixed(alias, columns string) string {
	return alias + strings.ReplaceAll(columns, ", ", ", "+alias)
}
