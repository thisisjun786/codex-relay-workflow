package store

import "database/sql"

// Row types for the domain tables todo 19 ports. Fields follow contract/schema/relay-sqlite.sql
// column order exactly, NOT NULL columns as plain values and nullable ones as sql.Null*, so a
// row read back from a Python-written store keeps NULL apart from an empty value.

// AssignmentMarksRow is one assignment_marks row, every column in DDL order.
type AssignmentMarksRow struct {
	RelationshipID      string
	Mark                string
	EventID             string
	ExecutionGeneration int64
	RevisionHash        string
	Evidence            string
	Actor               string
	MarkedAt            string
}

const assignmentMarksColumns = "relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at"

func scanAssignmentMarks(row scanner) (AssignmentMarksRow, error) {
	var r AssignmentMarksRow
	err := row.Scan(&r.RelationshipID, &r.Mark, &r.EventID, &r.ExecutionGeneration, &r.RevisionHash, &r.Evidence, &r.Actor, &r.MarkedAt)
	return r, err
}

// AttemptSettingsViolationsRow is one attempt_settings_violations row, every column in DDL order.
type AttemptSettingsViolationsRow struct {
	RequestID  string
	EventID    string
	Findings   string
	ObservedAt string
}

const attemptSettingsViolationsColumns = "request_id, event_id, findings, observed_at"

func scanAttemptSettingsViolations(row scanner) (AttemptSettingsViolationsRow, error) {
	var r AttemptSettingsViolationsRow
	err := row.Scan(&r.RequestID, &r.EventID, &r.Findings, &r.ObservedAt)
	return r, err
}

// AuthorizedSettingsRow is one authorized_settings row, every column in DDL order.
type AuthorizedSettingsRow struct {
	TaskID     string
	Settings   string
	Source     string
	RecordedAt string
}

const authorizedSettingsColumns = "task_id, settings, source, recorded_at"

func scanAuthorizedSettings(row scanner) (AuthorizedSettingsRow, error) {
	var r AuthorizedSettingsRow
	err := row.Scan(&r.TaskID, &r.Settings, &r.Source, &r.RecordedAt)
	return r, err
}

// CanonicalCriteriaRow is one canonical_criteria row, every column in DDL order.
type CanonicalCriteriaRow struct {
	RelationshipID string
	CriterionID    string
	Title          string
	Required       int64
	SourceRef      sql.NullString
	SetDigest      string
	RecordedAt     string
}

const canonicalCriteriaColumns = "relationship_id, criterion_id, title, required, source_ref, set_digest, recorded_at"

func scanCanonicalCriteria(row scanner) (CanonicalCriteriaRow, error) {
	var r CanonicalCriteriaRow
	err := row.Scan(&r.RelationshipID, &r.CriterionID, &r.Title, &r.Required, &r.SourceRef, &r.SetDigest, &r.RecordedAt)
	return r, err
}

// ClaimContextRow is one claim_context row, every column in DDL order.
type ClaimContextRow struct {
	EventID   string
	SetDigest sql.NullString
	BoundAt   string
}

const claimContextColumns = "event_id, set_digest, bound_at"

func scanClaimContext(row scanner) (ClaimContextRow, error) {
	var r ClaimContextRow
	err := row.Scan(&r.EventID, &r.SetDigest, &r.BoundAt)
	return r, err
}

// CoordinationConflictsRow is one coordination_conflicts row, every column in DDL order.
type CoordinationConflictsRow struct {
	ID         int64
	At         string
	Domain     string
	Subject    string
	Reason     string
	Incumbent  string
	Challenger string
	Detail     sql.NullString
}

const coordinationConflictsColumns = "id, at, domain, subject, reason, incumbent, challenger, detail"

func scanCoordinationConflicts(row scanner) (CoordinationConflictsRow, error) {
	var r CoordinationConflictsRow
	err := row.Scan(&r.ID, &r.At, &r.Domain, &r.Subject, &r.Reason, &r.Incumbent, &r.Challenger, &r.Detail)
	return r, err
}

// DeliveryIntentRow is one delivery_intent row, every column in DDL order.
type DeliveryIntentRow struct {
	EventID         string
	RelationshipID  string
	Kind            string
	RecipientTaskID string
	Attempts        int64
	NextRetryAt     sql.NullFloat64
	LastError       sql.NullString
	NotedAt         string
}

const deliveryIntentColumns = "event_id, relationship_id, kind, recipient_task_id, attempts, next_retry_at, last_error, noted_at"

func scanDeliveryIntent(row scanner) (DeliveryIntentRow, error) {
	var r DeliveryIntentRow
	err := row.Scan(&r.EventID, &r.RelationshipID, &r.Kind, &r.RecipientTaskID, &r.Attempts, &r.NextRetryAt, &r.LastError, &r.NotedAt)
	return r, err
}

// EditAgreementsRow is one edit_agreements row, every column in DDL order.
type EditAgreementsRow struct {
	AgreementID     string
	RegionID        string
	Repository      string
	BaseRevision    string
	LeftProject     string
	RightProject    string
	PeerLinkID      string
	ProposerTaskID  string
	IssueKey        sql.NullString
	ConstraintText  string
	LeftCondition   sql.NullString
	RightCondition  sql.NullString
	LeftAcceptedAt  sql.NullString
	RightAcceptedAt sql.NullString
	NextOwner       sql.NullString
	State           string
	Tenure          int64
	Supersedes      sql.NullString
	SupersededBy    sql.NullString
	CloseReason     sql.NullString
	ProposedAt      string
	UpdatedAt       string
	ClosedAt        sql.NullString
}

const editAgreementsColumns = "agreement_id, region_id, repository, base_revision, left_project, right_project, peer_link_id, proposer_task_id, issue_key, constraint_text, left_condition, right_condition, left_accepted_at, right_accepted_at, next_owner, state, tenure, supersedes, superseded_by, close_reason, proposed_at, updated_at, closed_at"

func scanEditAgreements(row scanner) (EditAgreementsRow, error) {
	var r EditAgreementsRow
	err := row.Scan(&r.AgreementID, &r.RegionID, &r.Repository, &r.BaseRevision, &r.LeftProject, &r.RightProject, &r.PeerLinkID, &r.ProposerTaskID, &r.IssueKey, &r.ConstraintText, &r.LeftCondition, &r.RightCondition, &r.LeftAcceptedAt, &r.RightAcceptedAt, &r.NextOwner, &r.State, &r.Tenure, &r.Supersedes, &r.SupersededBy, &r.CloseReason, &r.ProposedAt, &r.UpdatedAt, &r.ClosedAt)
	return r, err
}

// EditFollowupsRow is one edit_followups row, every column in DDL order.
type EditFollowupsRow struct {
	FollowupID      string
	AgreementID     string
	TriggerText     string
	AcceptanceText  string
	IssueRef        sql.NullString
	AssigneeTaskID  sql.NullString
	AssigneeProject sql.NullString
	AcceptedAt      sql.NullString
	State           string
	CloseReason     sql.NullString
	RecordedBy      string
	RecordedAt      string
	UpdatedAt       string
}

const editFollowupsColumns = "followup_id, agreement_id, trigger_text, acceptance_text, issue_ref, assignee_task_id, assignee_project, accepted_at, state, close_reason, recorded_by, recorded_at, updated_at"

func scanEditFollowups(row scanner) (EditFollowupsRow, error) {
	var r EditFollowupsRow
	err := row.Scan(&r.FollowupID, &r.AgreementID, &r.TriggerText, &r.AcceptanceText, &r.IssueRef, &r.AssigneeTaskID, &r.AssigneeProject, &r.AcceptedAt, &r.State, &r.CloseReason, &r.RecordedBy, &r.RecordedAt, &r.UpdatedAt)
	return r, err
}

// EditReaffirmationsRow is one edit_reaffirmations row, every column in DDL order.
type EditReaffirmationsRow struct {
	AgreementID            string
	PredecessorID          string
	Actor                  string
	ActorProject           string
	FromRevision           string
	ToRevision             string
	ConstraintRevision     string
	LeftConditionRevision  sql.NullString
	RightConditionRevision sql.NullString
	RecordedAt             string
}

const editReaffirmationsColumns = "agreement_id, predecessor_id, actor, actor_project, from_revision, to_revision, constraint_revision, left_condition_revision, right_condition_revision, recorded_at"

func scanEditReaffirmations(row scanner) (EditReaffirmationsRow, error) {
	var r EditReaffirmationsRow
	err := row.Scan(&r.AgreementID, &r.PredecessorID, &r.Actor, &r.ActorProject, &r.FromRevision, &r.ToRevision, &r.ConstraintRevision, &r.LeftConditionRevision, &r.RightConditionRevision, &r.RecordedAt)
	return r, err
}

// EditRegionsRow is one edit_regions row, every column in DDL order.
type EditRegionsRow struct {
	RegionID       string
	Repository     string
	BaseRevision   string
	Path           string
	RegionKind     string
	RegionKey      string
	RegionClass    string
	RegenerateFrom sql.NullString
	RecordedAt     string
}

const editRegionsColumns = "region_id, repository, base_revision, path, region_kind, region_key, region_class, regenerate_from, recorded_at"

func scanEditRegions(row scanner) (EditRegionsRow, error) {
	var r EditRegionsRow
	err := row.Scan(&r.RegionID, &r.Repository, &r.BaseRevision, &r.Path, &r.RegionKind, &r.RegionKey, &r.RegionClass, &r.RegenerateFrom, &r.RecordedAt)
	return r, err
}

// EditRevisionMarksRow is one edit_revision_marks row, every column in DDL order.
type EditRevisionMarksRow struct {
	MarkID       string
	Repository   string
	FromRevision string
	ToRevision   string
	Actor        string
	RecordedAt   string
}

const editRevisionMarksColumns = "mark_id, repository, from_revision, to_revision, actor, recorded_at"

func scanEditRevisionMarks(row scanner) (EditRevisionMarksRow, error) {
	var r EditRevisionMarksRow
	err := row.Scan(&r.MarkID, &r.Repository, &r.FromRevision, &r.ToRevision, &r.Actor, &r.RecordedAt)
	return r, err
}

// ExecutionLimitsRow is one execution_limits row, every column in DDL order.
type ExecutionLimitsRow struct {
	LimitID    string
	ScopeKind  string
	ScopeKey   string
	Dimension  string
	Unit       string
	Ceiling    float64
	Enforce    int64
	DeclaredBy string
	Source     string
	Revision   int64
	DeclaredAt string
	UpdatedAt  string
}

const executionLimitsColumns = "limit_id, scope_kind, scope_key, dimension, unit, ceiling, enforce, declared_by, source, revision, declared_at, updated_at"

func scanExecutionLimits(row scanner) (ExecutionLimitsRow, error) {
	var r ExecutionLimitsRow
	err := row.Scan(&r.LimitID, &r.ScopeKind, &r.ScopeKey, &r.Dimension, &r.Unit, &r.Ceiling, &r.Enforce, &r.DeclaredBy, &r.Source, &r.Revision, &r.DeclaredAt, &r.UpdatedAt)
	return r, err
}

// ExecutionSlotsRow is one execution_slots row, every column in DDL order.
type ExecutionSlotsRow struct {
	SlotID        string
	SubjectKind   string
	SubjectKey    string
	ParentTaskID  string
	ProjectKey    string
	InitiativeKey sql.NullString
	Tenure        int64
	State         string
	ReservedBy    string
	ReservedAt    string
	ReleasedAt    sql.NullString
	ReleasedBy    sql.NullString
	ReleaseReason sql.NullString
	Detail        sql.NullString
}

const executionSlotsColumns = "slot_id, subject_kind, subject_key, parent_task_id, project_key, initiative_key, tenure, state, reserved_by, reserved_at, released_at, released_by, release_reason, detail"

func scanExecutionSlots(row scanner) (ExecutionSlotsRow, error) {
	var r ExecutionSlotsRow
	err := row.Scan(&r.SlotID, &r.SubjectKind, &r.SubjectKey, &r.ParentTaskID, &r.ProjectKey, &r.InitiativeKey, &r.Tenure, &r.State, &r.ReservedBy, &r.ReservedAt, &r.ReleasedAt, &r.ReleasedBy, &r.ReleaseReason, &r.Detail)
	return r, err
}

// ExecutionUsageRow is one execution_usage row, every column in DDL order.
type ExecutionUsageRow struct {
	ScopeKind  string
	ScopeKey   string
	Dimension  string
	Observed   float64
	ObservedBy string
	Method     string
	ObservedAt string
}

const executionUsageColumns = "scope_kind, scope_key, dimension, observed, observed_by, method, observed_at"

func scanExecutionUsage(row scanner) (ExecutionUsageRow, error) {
	var r ExecutionUsageRow
	err := row.Scan(&r.ScopeKind, &r.ScopeKey, &r.Dimension, &r.Observed, &r.ObservedBy, &r.Method, &r.ObservedAt)
	return r, err
}

// FaultAdoptionsRow is one fault_adoptions row, every column in DDL order.
type FaultAdoptionsRow struct {
	FaultID     string
	ExternalRef string
	Scope       string
	State       string
	CreatedAt   string
	UpdatedAt   string
}

const faultAdoptionsColumns = "fault_id, external_ref, scope, state, created_at, updated_at"

func scanFaultAdoptions(row scanner) (FaultAdoptionsRow, error) {
	var r FaultAdoptionsRow
	err := row.Scan(&r.FaultID, &r.ExternalRef, &r.Scope, &r.State, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// FaultAliasesRow is one fault_aliases row, every column in DDL order.
type FaultAliasesRow struct {
	AliasID   string
	FaultID   string
	CreatedAt string
}

const faultAliasesColumns = "alias_id, fault_id, created_at"

func scanFaultAliases(row scanner) (FaultAliasesRow, error) {
	var r FaultAliasesRow
	err := row.Scan(&r.AliasID, &r.FaultID, &r.CreatedAt)
	return r, err
}

// FaultBudgetUsesRow is one fault_budget_uses row, every column in DDL order.
type FaultBudgetUsesRow struct {
	UseID   int64
	Product string
	Kind    string
	Ref     string
	UsedAt  string
	UsedTS  float64
}

const faultBudgetUsesColumns = "use_id, product, kind, ref, used_at, used_ts"

func scanFaultBudgetUses(row scanner) (FaultBudgetUsesRow, error) {
	var r FaultBudgetUsesRow
	err := row.Scan(&r.UseID, &r.Product, &r.Kind, &r.Ref, &r.UsedAt, &r.UsedTS)
	return r, err
}

// FaultCursorsRow is one fault_cursors row, every column in DDL order.
type FaultCursorsRow struct {
	Source    string
	Position  sql.NullString
	Pages     int64
	UpdatedAt string
}

const faultCursorsColumns = "source, position, pages, updated_at"

func scanFaultCursors(row scanner) (FaultCursorsRow, error) {
	var r FaultCursorsRow
	err := row.Scan(&r.Source, &r.Position, &r.Pages, &r.UpdatedAt)
	return r, err
}

// FaultLedgerRow is one fault_ledger row, every column in DDL order.
type FaultLedgerRow struct {
	FaultID         string
	Product         string
	FaultClass      string
	Component       string
	Severity        string
	Signature       string
	Scope           string
	ScopeKey        string
	State           string
	Cycle           int64
	Episode         int64
	OccurrenceCount int64
	ReopenCount     int64
	Detail          sql.NullString
	Suppression     sql.NullString
	ExternalRef     sql.NullString
	FirstSeenAt     string
	LastSeenAt      string
	ClearedAt       sql.NullString
	PublishedAt     sql.NullString
	ResolvedAt      sql.NullString
	UpdatedAt       string
}

const faultLedgerColumns = "fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, cycle, episode, occurrence_count, reopen_count, detail, suppression, external_ref, first_seen_at, last_seen_at, cleared_at, published_at, resolved_at, updated_at"

func scanFaultLedger(row scanner) (FaultLedgerRow, error) {
	var r FaultLedgerRow
	err := row.Scan(&r.FaultID, &r.Product, &r.FaultClass, &r.Component, &r.Severity, &r.Signature, &r.Scope, &r.ScopeKey, &r.State, &r.Cycle, &r.Episode, &r.OccurrenceCount, &r.ReopenCount, &r.Detail, &r.Suppression, &r.ExternalRef, &r.FirstSeenAt, &r.LastSeenAt, &r.ClearedAt, &r.PublishedAt, &r.ResolvedAt, &r.UpdatedAt)
	return r, err
}

// FaultLimitsRow is one fault_limits row, every column in DDL order.
type FaultLimitsRow struct {
	Product       string
	Kind          string
	MaxCount      int64
	WindowSeconds float64
	UpdatedAt     string
}

const faultLimitsColumns = "product, kind, max_count, window_seconds, updated_at"

func scanFaultLimits(row scanner) (FaultLimitsRow, error) {
	var r FaultLimitsRow
	err := row.Scan(&r.Product, &r.Kind, &r.MaxCount, &r.WindowSeconds, &r.UpdatedAt)
	return r, err
}

// FaultLinksRow is one fault_links row, every column in DDL order.
type FaultLinksRow struct {
	FaultID            string
	ExternalRef        string
	ProjectRef         sql.NullString
	ObservedProjectRef sql.NullString
	State              string
	Revision           int64
	UpdatedAt          string
}

const faultLinksColumns = "fault_id, external_ref, project_ref, observed_project_ref, state, revision, updated_at"

func scanFaultLinks(row scanner) (FaultLinksRow, error) {
	var r FaultLinksRow
	err := row.Scan(&r.FaultID, &r.ExternalRef, &r.ProjectRef, &r.ObservedProjectRef, &r.State, &r.Revision, &r.UpdatedAt)
	return r, err
}

// FaultNotificationsRow is one fault_notifications row, every column in DDL order.
type FaultNotificationsRow struct {
	NotificationID string
	FaultID        string
	Product        string
	Kind           string
	Reason         sql.NullString
	Cycle          int64
	Ref            sql.NullString
	State          string
	Token          sql.NullString
	Owner          sql.NullString
	LeaseUntil     sql.NullFloat64
	Attempts       int64
	LastError      sql.NullString
	CreatedAt      string
	UpdatedAt      string
	DeliveredAt    sql.NullString
	AckRef         sql.NullString
	ExaminedSeq    sql.NullInt64
}

const faultNotificationsColumns = "notification_id, fault_id, product, kind, reason, cycle, ref, state, token, owner, lease_until, attempts, last_error, created_at, updated_at, delivered_at, ack_ref, examined_seq"

func scanFaultNotifications(row scanner) (FaultNotificationsRow, error) {
	var r FaultNotificationsRow
	err := row.Scan(&r.NotificationID, &r.FaultID, &r.Product, &r.Kind, &r.Reason, &r.Cycle, &r.Ref, &r.State, &r.Token, &r.Owner, &r.LeaseUntil, &r.Attempts, &r.LastError, &r.CreatedAt, &r.UpdatedAt, &r.DeliveredAt, &r.AckRef, &r.ExaminedSeq)
	return r, err
}

// FaultOccurrencesRow is one fault_occurrences row, every column in DDL order.
type FaultOccurrencesRow struct {
	OccurrenceID   string
	FaultID        string
	Episode        int64
	OccurrenceKey  string
	Severity       string
	Cleared        int64
	Detail         sql.NullString
	Evidence       string
	EvidenceDigest string
	Truncated      int64
	ObservedAt     sql.NullString
	RecordedAt     string
	RecordedTS     float64
}

const faultOccurrencesColumns = "occurrence_id, fault_id, episode, occurrence_key, severity, cleared, detail, evidence, evidence_digest, truncated, observed_at, recorded_at, recorded_ts"

func scanFaultOccurrences(row scanner) (FaultOccurrencesRow, error) {
	var r FaultOccurrencesRow
	err := row.Scan(&r.OccurrenceID, &r.FaultID, &r.Episode, &r.OccurrenceKey, &r.Severity, &r.Cleared, &r.Detail, &r.Evidence, &r.EvidenceDigest, &r.Truncated, &r.ObservedAt, &r.RecordedAt, &r.RecordedTS)
	return r, err
}

// FaultOvertakenDeliveriesRow is one fault_overtaken_deliveries row, every column in DDL order.
type FaultOvertakenDeliveriesRow struct {
	EventID string
	Reason  string
	NotedAt string
}

const faultOvertakenDeliveriesColumns = "event_id, reason, noted_at"

func scanFaultOvertakenDeliveries(row scanner) (FaultOvertakenDeliveriesRow, error) {
	var r FaultOvertakenDeliveriesRow
	err := row.Scan(&r.EventID, &r.Reason, &r.NotedAt)
	return r, err
}

// FaultPoliciesRow is one fault_policies row, every column in DDL order.
type FaultPoliciesRow struct {
	Product       string
	FaultClass    string
	Severity      string
	Threshold     sql.NullInt64
	WindowSeconds sql.NullFloat64
	Reason        string
	UpdatedAt     string
}

const faultPoliciesColumns = "product, fault_class, severity, threshold, window_seconds, reason, updated_at"

func scanFaultPolicies(row scanner) (FaultPoliciesRow, error) {
	var r FaultPoliciesRow
	err := row.Scan(&r.Product, &r.FaultClass, &r.Severity, &r.Threshold, &r.WindowSeconds, &r.Reason, &r.UpdatedAt)
	return r, err
}

// FaultPublicationAttemptsRow is one fault_publication_attempts row, every column in DDL order.
type FaultPublicationAttemptsRow struct {
	AttemptID     int64
	PublicationID string
	Attempt       int64
	Owner         string
	Takeover      int64
	ClaimedAt     string
	ClaimedTS     float64
	IssuedAt      sql.NullString
	IssuedTS      sql.NullFloat64
	Outcome       sql.NullString
	Error         sql.NullString
	Ended         int64
	EndedAt       sql.NullString
}

const faultPublicationAttemptsColumns = "attempt_id, publication_id, attempt, owner, takeover, claimed_at, claimed_ts, issued_at, issued_ts, outcome, error, ended, ended_at"

func scanFaultPublicationAttempts(row scanner) (FaultPublicationAttemptsRow, error) {
	var r FaultPublicationAttemptsRow
	err := row.Scan(&r.AttemptID, &r.PublicationID, &r.Attempt, &r.Owner, &r.Takeover, &r.ClaimedAt, &r.ClaimedTS, &r.IssuedAt, &r.IssuedTS, &r.Outcome, &r.Error, &r.Ended, &r.EndedAt)
	return r, err
}

// FaultPublicationPayloadsRow is one fault_publication_payloads row, every column in DDL order.
type FaultPublicationPayloadsRow struct {
	PublicationID string
	ProjectRef    sql.NullString
	Payload       sql.NullString
	HoldReason    sql.NullString
	UpdatedAt     string
	TargetMode    sql.NullString
}

const faultPublicationPayloadsColumns = "publication_id, project_ref, payload, hold_reason, updated_at, target_mode"

func scanFaultPublicationPayloads(row scanner) (FaultPublicationPayloadsRow, error) {
	var r FaultPublicationPayloadsRow
	err := row.Scan(&r.PublicationID, &r.ProjectRef, &r.Payload, &r.HoldReason, &r.UpdatedAt, &r.TargetMode)
	return r, err
}

// FaultPublicationsRow is one fault_publications row, every column in DDL order.
type FaultPublicationsRow struct {
	PublicationID  string
	FaultID        string
	Kind           string
	TriggerKey     string
	Cycle          int64
	TrackerRef     sql.NullString
	ExternalRef    sql.NullString
	Summary        string
	IdentityDigest string
	State          string
	Attempts       int64
	NextAttemptAt  sql.NullFloat64
	ClaimToken     sql.NullString
	LeaseOwner     sql.NullString
	LeaseUntil     sql.NullFloat64
	IssuedAt       sql.NullString
	LastError      sql.NullString
	ExternalResult sql.NullString
	CreatedAt      string
	UpdatedAt      string
	ConfirmedAt    sql.NullString
}

const faultPublicationsColumns = "publication_id, fault_id, kind, trigger_key, cycle, tracker_ref, external_ref, summary, identity_digest, state, attempts, next_attempt_at, claim_token, lease_owner, lease_until, issued_at, last_error, external_result, created_at, updated_at, confirmed_at"

func scanFaultPublications(row scanner) (FaultPublicationsRow, error) {
	var r FaultPublicationsRow
	err := row.Scan(&r.PublicationID, &r.FaultID, &r.Kind, &r.TriggerKey, &r.Cycle, &r.TrackerRef, &r.ExternalRef, &r.Summary, &r.IdentityDigest, &r.State, &r.Attempts, &r.NextAttemptAt, &r.ClaimToken, &r.LeaseOwner, &r.LeaseUntil, &r.IssuedAt, &r.LastError, &r.ExternalResult, &r.CreatedAt, &r.UpdatedAt, &r.ConfirmedAt)
	return r, err
}

// FaultRemediationsRow is one fault_remediations row, every column in DDL order.
type FaultRemediationsRow struct {
	RemediationID string
	FaultID       string
	Cycle         int64
	Kind          string
	Ref           string
	Method        sql.NullString
	Outcome       sql.NullString
	Detail        sql.NullString
	RecordedAt    string
}

const faultRemediationsColumns = "remediation_id, fault_id, cycle, kind, ref, method, outcome, detail, recorded_at"

func scanFaultRemediations(row scanner) (FaultRemediationsRow, error) {
	var r FaultRemediationsRow
	err := row.Scan(&r.RemediationID, &r.FaultID, &r.Cycle, &r.Kind, &r.Ref, &r.Method, &r.Outcome, &r.Detail, &r.RecordedAt)
	return r, err
}

// FaultTargetProjectsRow is one fault_target_projects row, every column in DDL order.
type FaultTargetProjectsRow struct {
	ScopeKey   string
	Product    string
	ProjectRef sql.NullString
	RecordedAt string
}

const faultTargetProjectsColumns = "scope_key, product, project_ref, recorded_at"

func scanFaultTargetProjects(row scanner) (FaultTargetProjectsRow, error) {
	var r FaultTargetProjectsRow
	err := row.Scan(&r.ScopeKey, &r.Product, &r.ProjectRef, &r.RecordedAt)
	return r, err
}

// FaultTargetsRow is one fault_targets row, every column in DDL order.
type FaultTargetsRow struct {
	ScopeKey   string
	TrackerRef string
	RecordedAt string
}

const faultTargetsColumns = "scope_key, tracker_ref, recorded_at"

func scanFaultTargets(row scanner) (FaultTargetsRow, error) {
	var r FaultTargetsRow
	err := row.Scan(&r.ScopeKey, &r.TrackerRef, &r.RecordedAt)
	return r, err
}

// FaultTimelineRow is one fault_timeline row, every column in DDL order.
type FaultTimelineRow struct {
	Seq        int64
	FaultID    string
	Cycle      int64
	Kind       string
	RefID      sql.NullString
	Detail     sql.NullString
	RecordedAt string
	RecordedTS float64
}

const faultTimelineColumns = "seq, fault_id, cycle, kind, ref_id, detail, recorded_at, recorded_ts"

func scanFaultTimeline(row scanner) (FaultTimelineRow, error) {
	var r FaultTimelineRow
	err := row.Scan(&r.Seq, &r.FaultID, &r.Cycle, &r.Kind, &r.RefID, &r.Detail, &r.RecordedAt, &r.RecordedTS)
	return r, err
}

// IncidentRoutesRow is one incident_routes row, every column in DDL order.
type IncidentRoutesRow struct {
	FaultID         string
	ProductKey      string
	Workspace       string
	Disposition     string
	Stage           string
	Target          string
	Origin          string
	ClaimedSeverity string
	Goal            sql.NullString
	Classification  sql.NullString
	SupersededBy    sql.NullString
	Reported        sql.NullString
	Detail          sql.NullString
	CheckedSeq      int64
	CreatedAt       string
	UpdatedAt       string
}

const incidentRoutesColumns = "fault_id, product_key, workspace, disposition, stage, target, origin, claimed_severity, goal, classification, superseded_by, reported, detail, checked_seq, created_at, updated_at"

func scanIncidentRoutes(row scanner) (IncidentRoutesRow, error) {
	var r IncidentRoutesRow
	err := row.Scan(&r.FaultID, &r.ProductKey, &r.Workspace, &r.Disposition, &r.Stage, &r.Target, &r.Origin, &r.ClaimedSeverity, &r.Goal, &r.Classification, &r.SupersededBy, &r.Reported, &r.Detail, &r.CheckedSeq, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// LinkageConflictsRow is one linkage_conflicts row, every column in DDL order.
type LinkageConflictsRow struct {
	ID         int64
	At         string
	ScopeKind  string
	ScopeKey   string
	Reason     string
	Incumbent  string
	Challenger string
	Detail     sql.NullString
}

const linkageConflictsColumns = "id, at, scope_kind, scope_key, reason, incumbent, challenger, detail"

func scanLinkageConflicts(row scanner) (LinkageConflictsRow, error) {
	var r LinkageConflictsRow
	err := row.Scan(&r.ID, &r.At, &r.ScopeKind, &r.ScopeKey, &r.Reason, &r.Incumbent, &r.Challenger, &r.Detail)
	return r, err
}

// ManagedStartRequestsRow is one managed_start_requests row, every column in DDL order.
type ManagedStartRequestsRow struct {
	RequestID           string
	IssueKey            string
	RequestFingerprint  string
	FingerprintVersion  string
	Workspace           string
	MarkerRoot          string
	SocketIdentity      string
	CreateRequestID     string
	DispatchRequestID   string
	State               string
	Revision            int64
	ChildTaskID         sql.NullString
	StandbyTurnID       sql.NullString
	RelationshipID      sql.NullString
	ExecutionGeneration sql.NullInt64
	ReceiptStatus       sql.NullString
	ReleaseReason       sql.NullString
	CreatedAt           string
	UpdatedAt           string
}

const managedStartRequestsColumns = "request_id, issue_key, request_fingerprint, fingerprint_version, workspace, marker_root, socket_identity, create_request_id, dispatch_request_id, state, revision, child_task_id, standby_turn_id, relationship_id, execution_generation, receipt_status, release_reason, created_at, updated_at"

func scanManagedStartRequests(row scanner) (ManagedStartRequestsRow, error) {
	var r ManagedStartRequestsRow
	err := row.Scan(&r.RequestID, &r.IssueKey, &r.RequestFingerprint, &r.FingerprintVersion, &r.Workspace, &r.MarkerRoot, &r.SocketIdentity, &r.CreateRequestID, &r.DispatchRequestID, &r.State, &r.Revision, &r.ChildTaskID, &r.StandbyTurnID, &r.RelationshipID, &r.ExecutionGeneration, &r.ReceiptStatus, &r.ReleaseReason, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// MergeTurnChecksRow is one merge_turn_checks row, every column in DDL order.
type MergeTurnChecksRow struct {
	CheckID       string
	TurnID        string
	HeadSHA       string
	BaseSHA       string
	Required      string
	ChecksDigest  string
	Checks        string
	ReviewDigest  string
	Review        string
	Result        string
	RefusalReason sql.NullString
	RecordedAt    string
}

const mergeTurnChecksColumns = "check_id, turn_id, head_sha, base_sha, required, checks_digest, checks, review_digest, review, result, refusal_reason, recorded_at"

func scanMergeTurnChecks(row scanner) (MergeTurnChecksRow, error) {
	var r MergeTurnChecksRow
	err := row.Scan(&r.CheckID, &r.TurnID, &r.HeadSHA, &r.BaseSHA, &r.Required, &r.ChecksDigest, &r.Checks, &r.ReviewDigest, &r.Review, &r.Result, &r.RefusalReason, &r.RecordedAt)
	return r, err
}

// MergeTurnLedgerRow is one merge_turn_ledger row, every column in DDL order.
type MergeTurnLedgerRow struct {
	EntryID        string
	TurnID         string
	Kind           string
	FromState      sql.NullString
	ToState        sql.NullString
	EvidenceKind   string
	ActorTaskID    string
	Evidence       string
	IdempotencyKey string
	RecordedAt     string
}

const mergeTurnLedgerColumns = "entry_id, turn_id, kind, from_state, to_state, evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at"

func scanMergeTurnLedger(row scanner) (MergeTurnLedgerRow, error) {
	var r MergeTurnLedgerRow
	err := row.Scan(&r.EntryID, &r.TurnID, &r.Kind, &r.FromState, &r.ToState, &r.EvidenceKind, &r.ActorTaskID, &r.Evidence, &r.IdempotencyKey, &r.RecordedAt)
	return r, err
}

// MergeTurnsRow is one merge_turns row, every column in DDL order.
type MergeTurnsRow struct {
	TurnID          string
	TargetKey       string
	Repository      string
	BaseRef         string
	ProjectKey      string
	HolderTaskID    string
	HolderHostID    string
	RelationshipID  sql.NullString
	PRNumber        sql.NullInt64
	CandidateHead   string
	DeclaredReady   int64
	State           string
	Tenure          int64
	CheckedBaseSHA  sql.NullString
	LandedSHA       sql.NullString
	ObservedBaseSHA sql.NullString
	CloseReason     sql.NullString
	RequestedAt     string
	HeldAt          sql.NullString
	MergingAt       sql.NullString
	ClosedAt        sql.NullString
	UpdatedAt       string
}

const mergeTurnsColumns = "turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, declared_ready, state, tenure, checked_base_sha, landed_sha, observed_base_sha, close_reason, requested_at, held_at, merging_at, closed_at, updated_at"

func scanMergeTurns(row scanner) (MergeTurnsRow, error) {
	var r MergeTurnsRow
	err := row.Scan(&r.TurnID, &r.TargetKey, &r.Repository, &r.BaseRef, &r.ProjectKey, &r.HolderTaskID, &r.HolderHostID, &r.RelationshipID, &r.PRNumber, &r.CandidateHead, &r.DeclaredReady, &r.State, &r.Tenure, &r.CheckedBaseSHA, &r.LandedSHA, &r.ObservedBaseSHA, &r.CloseReason, &r.RequestedAt, &r.HeldAt, &r.MergingAt, &r.ClosedAt, &r.UpdatedAt)
	return r, err
}

// PollObservationsRow is one poll_observations row, every column in DDL order.
type PollObservationsRow struct {
	RelationshipID      string
	ExecutionGeneration int64
	TurnID              string
	LastStatus          sql.NullString
	LastPolledAt        sql.NullString
	LastAttemptAt       string
	LastError           sql.NullString
}

const pollObservationsColumns = "relationship_id, execution_generation, turn_id, last_status, last_polled_at, last_attempt_at, last_error"

func scanPollObservations(row scanner) (PollObservationsRow, error) {
	var r PollObservationsRow
	err := row.Scan(&r.RelationshipID, &r.ExecutionGeneration, &r.TurnID, &r.LastStatus, &r.LastPolledAt, &r.LastAttemptAt, &r.LastError)
	return r, err
}

// ProductBindingsRow is one product_bindings row, every column in DDL order.
type ProductBindingsRow struct {
	ProductKey string
	Kind       string
	Ref        string
	Record     string
	ObservedAt sql.NullString
	RecordedAt string
}

const productBindingsColumns = "product_key, kind, ref, record, observed_at, recorded_at"

func scanProductBindings(row scanner) (ProductBindingsRow, error) {
	var r ProductBindingsRow
	err := row.Scan(&r.ProductKey, &r.Kind, &r.Ref, &r.Record, &r.ObservedAt, &r.RecordedAt)
	return r, err
}

// ProductRegistryRow is one product_registry row, every column in DDL order.
type ProductRegistryRow struct {
	ProductKey string
	Record     string
	RecordedAt string
}

const productRegistryColumns = "product_key, record, recorded_at"

func scanProductRegistry(row scanner) (ProductRegistryRow, error) {
	var r ProductRegistryRow
	err := row.Scan(&r.ProductKey, &r.Record, &r.RecordedAt)
	return r, err
}

// RecipientLifecycleRow is one recipient_lifecycle row, every column in DDL order.
type RecipientLifecycleRow struct {
	TaskID         string
	RuntimeStatus  sql.NullString
	Archived       sql.NullInt64
	GoalStatus     sql.NullString
	CanAcceptInput sql.NullInt64
	Deliverable    string
	WithholdReason sql.NullString
	Detail         sql.NullString
	ObservedAt     string
}

const recipientLifecycleColumns = "task_id, runtime_status, archived, goal_status, can_accept_input, deliverable, withhold_reason, detail, observed_at"

func scanRecipientLifecycle(row scanner) (RecipientLifecycleRow, error) {
	var r RecipientLifecycleRow
	err := row.Scan(&r.TaskID, &r.RuntimeStatus, &r.Archived, &r.GoalStatus, &r.CanAcceptInput, &r.Deliverable, &r.WithholdReason, &r.Detail, &r.ObservedAt)
	return r, err
}

// RecipientRateRow is one recipient_rate row, every column in DDL order.
type RecipientRateRow struct {
	RecipientTaskID string
	WindowStart     float64
	Sends           int64
	LastSendAt      sql.NullFloat64
}

const recipientRateColumns = "recipient_task_id, window_start, sends, last_send_at"

func scanRecipientRate(row scanner) (RecipientRateRow, error) {
	var r RecipientRateRow
	err := row.Scan(&r.RecipientTaskID, &r.WindowStart, &r.Sends, &r.LastSendAt)
	return r, err
}

// RelationshipScopeRow is one relationship_scope row, every column in DDL order.
type RelationshipScopeRow struct {
	RelationshipID string
	ProjectKey     string
	RecordedAt     string
}

const relationshipScopeColumns = "relationship_id, project_key, recorded_at"

func scanRelationshipScope(row scanner) (RelationshipScopeRow, error) {
	var r RelationshipScopeRow
	err := row.Scan(&r.RelationshipID, &r.ProjectKey, &r.RecordedAt)
	return r, err
}

// ReportingSessionsRow is one reporting_sessions row, every column in DDL order.
type ReportingSessionsRow struct {
	AssignmentID      string
	SessionID         string
	DispatchRequestID string
	MarkerRoot        string
	Workspace         string
	IssueKey          sql.NullString
	Capability        string
	RecordedAt        string
}

const reportingSessionsColumns = "assignment_id, session_id, dispatch_request_id, marker_root, workspace, issue_key, capability, recorded_at"

func scanReportingSessions(row scanner) (ReportingSessionsRow, error) {
	var r ReportingSessionsRow
	err := row.Scan(&r.AssignmentID, &r.SessionID, &r.DispatchRequestID, &r.MarkerRoot, &r.Workspace, &r.IssueKey, &r.Capability, &r.RecordedAt)
	return r, err
}

// RouteIncidentsRow is one route_incidents row, every column in DDL order.
type RouteIncidentsRow struct {
	IncidentID  string
	FaultID     string
	Record      string
	RecordedAt  string
	RecordedSeq int64
}

const routeIncidentsColumns = "incident_id, fault_id, record, recorded_at, recorded_seq"

func scanRouteIncidents(row scanner) (RouteIncidentsRow, error) {
	var r RouteIncidentsRow
	err := row.Scan(&r.IncidentID, &r.FaultID, &r.Record, &r.RecordedAt, &r.RecordedSeq)
	return r, err
}

// RoutingPolicyRow is one routing_policy row, every column in DDL order.
type RoutingPolicyRow struct {
	PolicyKey  string
	Record     string
	Basis      string
	RecordedAt string
}

const routingPolicyColumns = "policy_key, record, basis, recorded_at"

func scanRoutingPolicy(row scanner) (RoutingPolicyRow, error) {
	var r RoutingPolicyRow
	err := row.Scan(&r.PolicyKey, &r.Record, &r.Basis, &r.RecordedAt)
	return r, err
}

// ScopeBindingsRow is one scope_bindings row, every column in DDL order.
type ScopeBindingsRow struct {
	BindingID    string
	Role         string
	ScopeKind    string
	ScopeKey     string
	TaskID       string
	HostID       string
	CWD          sql.NullString
	CXCSession   sql.NullString
	Status       string
	Revision     int64
	Supersedes   sql.NullString
	SupersededBy sql.NullString
	HandoverNote sql.NullString
	CreatedAt    string
	UpdatedAt    string
}

const scopeBindingsColumns = "binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session, status, revision, supersedes, superseded_by, handover_note, created_at, updated_at"

func scanScopeBindings(row scanner) (ScopeBindingsRow, error) {
	var r ScopeBindingsRow
	err := row.Scan(&r.BindingID, &r.Role, &r.ScopeKind, &r.ScopeKey, &r.TaskID, &r.HostID, &r.CWD, &r.CXCSession, &r.Status, &r.Revision, &r.Supersedes, &r.SupersededBy, &r.HandoverNote, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// ScopeDirectivesRow is one scope_directives row, every column in DDL order.
type ScopeDirectivesRow struct {
	DirectiveID  string
	ScopeKind    string
	ScopeKey     string
	FromTaskID   string
	FromScopeKey string
	LinkID       string
	LinkKind     string
	Digest       string
	Reference    sql.NullString
	Revision     int64
	Disposition  sql.NullString
	DecidedBy    sql.NullString
	DecidedAt    sql.NullString
	RecordedAt   string
}

const scopeDirectivesColumns = "directive_id, scope_kind, scope_key, from_task_id, from_scope_key, link_id, link_kind, digest, reference, revision, disposition, decided_by, decided_at, recorded_at"

func scanScopeDirectives(row scanner) (ScopeDirectivesRow, error) {
	var r ScopeDirectivesRow
	err := row.Scan(&r.DirectiveID, &r.ScopeKind, &r.ScopeKey, &r.FromTaskID, &r.FromScopeKey, &r.LinkID, &r.LinkKind, &r.Digest, &r.Reference, &r.Revision, &r.Disposition, &r.DecidedBy, &r.DecidedAt, &r.RecordedAt)
	return r, err
}

// ScopeLinksRow is one scope_links row, every column in DDL order.
type ScopeLinksRow struct {
	LinkID       string
	LinkKind     string
	UpperKind    string
	UpperKey     string
	UpperTaskID  string
	LowerKind    string
	LowerKey     string
	LowerTaskID  string
	Status       string
	Revision     int64
	SupersededBy sql.NullString
	CreatedAt    string
	UpdatedAt    string
}

const scopeLinksColumns = "link_id, link_kind, upper_kind, upper_key, upper_task_id, lower_kind, lower_key, lower_task_id, status, revision, superseded_by, created_at, updated_at"

func scanScopeLinks(row scanner) (ScopeLinksRow, error) {
	var r ScopeLinksRow
	err := row.Scan(&r.LinkID, &r.LinkKind, &r.UpperKind, &r.UpperKey, &r.UpperTaskID, &r.LowerKind, &r.LowerKey, &r.LowerTaskID, &r.Status, &r.Revision, &r.SupersededBy, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// SupervisorAttemptsRow is one supervisor_attempts row, every column in DDL order.
type SupervisorAttemptsRow struct {
	RequestID          string
	MessageID          string
	AttemptNo          int64
	Message            string
	State              string
	SendAttempted      string
	RetrySafe          int64
	TurnID             sql.NullString
	Record             string
	SentAt             string
	TransportStartedAt sql.NullString
	ObservedAt         string
	DeliveryToken      sql.NullString
}

const supervisorAttemptsColumns = "request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, turn_id, record, sent_at, transport_started_at, observed_at, delivery_token"

func scanSupervisorAttempts(row scanner) (SupervisorAttemptsRow, error) {
	var r SupervisorAttemptsRow
	err := row.Scan(&r.RequestID, &r.MessageID, &r.AttemptNo, &r.Message, &r.State, &r.SendAttempted, &r.RetrySafe, &r.TurnID, &r.Record, &r.SentAt, &r.TransportStartedAt, &r.ObservedAt, &r.DeliveryToken)
	return r, err
}

// SupervisorMessagesRow is one supervisor_messages row, every column in DDL order.
type SupervisorMessagesRow struct {
	MessageID       string
	ObligationID    string
	ObligationKind  string
	RelationshipID  string
	ProjectKey      sql.NullString
	Purpose         string
	Kind            string
	SenderTaskID    string
	RecipientTaskID string
	Subject         string
	Packet          string
	State           string
	AttemptCount    int64
	NextEligibleAt  sql.NullFloat64
	HoldReason      sql.NullString
	LeaseOwner      sql.NullString
	LeaseUntil      sql.NullFloat64
	StagedAt        string
	UpdatedAt       string
	EventID         sql.NullString
	SubmissionNo    sql.NullInt64
	Reading         sql.NullString
}

const supervisorMessagesColumns = "message_id, obligation_id, obligation_kind, relationship_id, project_key, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, attempt_count, next_eligible_at, hold_reason, lease_owner, lease_until, staged_at, updated_at, event_id, submission_no, reading"

func scanSupervisorMessages(row scanner) (SupervisorMessagesRow, error) {
	var r SupervisorMessagesRow
	err := row.Scan(&r.MessageID, &r.ObligationID, &r.ObligationKind, &r.RelationshipID, &r.ProjectKey, &r.Purpose, &r.Kind, &r.SenderTaskID, &r.RecipientTaskID, &r.Subject, &r.Packet, &r.State, &r.AttemptCount, &r.NextEligibleAt, &r.HoldReason, &r.LeaseOwner, &r.LeaseUntil, &r.StagedAt, &r.UpdatedAt, &r.EventID, &r.SubmissionNo, &r.Reading)
	return r, err
}

// SupervisorReadbacksRow is one supervisor_readbacks row, every column in DDL order.
type SupervisorReadbacksRow struct {
	MessageID  string
	ReadTurnID string
	Proof      string
	Verified   string
	RequestID  sql.NullString
	Detail     sql.NullString
	ReadAt     string
}

const supervisorReadbacksColumns = "message_id, read_turn_id, proof, verified, request_id, detail, read_at"

func scanSupervisorReadbacks(row scanner) (SupervisorReadbacksRow, error) {
	var r SupervisorReadbacksRow
	err := row.Scan(&r.MessageID, &r.ReadTurnID, &r.Proof, &r.Verified, &r.RequestID, &r.Detail, &r.ReadAt)
	return r, err
}

// SyncOutboxRow is one sync_outbox row, every column in DDL order.
type SyncOutboxRow struct {
	SyncID              string
	RelationshipID      string
	IssueKey            string
	Target              string
	TargetRef           string
	SubjectKind         string
	EventID             sql.NullString
	ExecutionGeneration sql.NullInt64
	RevisionHash        sql.NullString
	Verdict             sql.NullString
	IdentityDigest      string
	Summary             string
	State               string
	Attempts            int64
	NextAttemptAt       sql.NullFloat64
	LastError           sql.NullString
	LeaseOwner          sql.NullString
	LeaseUntil          sql.NullFloat64
	ClaimToken          sql.NullString
	ExternalRef         sql.NullString
	Readback            sql.NullString
	WrittenAt           sql.NullString
	ConfirmedAt         sql.NullString
	CreatedAt           string
	UpdatedAt           string
}

const syncOutboxColumns = "sync_id, relationship_id, issue_key, target, target_ref, subject_kind, event_id, execution_generation, revision_hash, verdict, identity_digest, summary, state, attempts, next_attempt_at, last_error, lease_owner, lease_until, claim_token, external_ref, readback, written_at, confirmed_at, created_at, updated_at"

func scanSyncOutbox(row scanner) (SyncOutboxRow, error) {
	var r SyncOutboxRow
	err := row.Scan(&r.SyncID, &r.RelationshipID, &r.IssueKey, &r.Target, &r.TargetRef, &r.SubjectKind, &r.EventID, &r.ExecutionGeneration, &r.RevisionHash, &r.Verdict, &r.IdentityDigest, &r.Summary, &r.State, &r.Attempts, &r.NextAttemptAt, &r.LastError, &r.LeaseOwner, &r.LeaseUntil, &r.ClaimToken, &r.ExternalRef, &r.Readback, &r.WrittenAt, &r.ConfirmedAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// SyncTargetsRow is one sync_targets row, every column in DDL order.
type SyncTargetsRow struct {
	RelationshipID string
	Target         string
	TargetRef      string
	RecordedAt     string
}

const syncTargetsColumns = "relationship_id, target, target_ref, recorded_at"

func scanSyncTargets(row scanner) (SyncTargetsRow, error) {
	var r SyncTargetsRow
	err := row.Scan(&r.RelationshipID, &r.Target, &r.TargetRef, &r.RecordedAt)
	return r, err
}

// TurnDeclarationsRow is one turn_declarations row, every column in DDL order.
type TurnDeclarationsRow struct {
	AssignmentID string
	SessionID    string
	TurnID       string
	Outcome      string
	DeclaredAt   string
	RecordedAt   string
}

const turnDeclarationsColumns = "assignment_id, session_id, turn_id, outcome, declared_at, recorded_at"

func scanTurnDeclarations(row scanner) (TurnDeclarationsRow, error) {
	var r TurnDeclarationsRow
	err := row.Scan(&r.AssignmentID, &r.SessionID, &r.TurnID, &r.Outcome, &r.DeclaredAt, &r.RecordedAt)
	return r, err
}

// VerdictContextRow is one verdict_context row, every column in DDL order.
type VerdictContextRow struct {
	EventID      string
	SetDigest    sql.NullString
	Coverage     string
	Findings     sql.NullString
	Reason       sql.NullString
	Currency     string
	HeadEventID  sql.NullString
	HeadRevision sql.NullString
	AckEvidence  string
	RecordedAt   string
}

const verdictContextColumns = "event_id, set_digest, coverage, findings, reason, currency, head_event_id, head_revision, ack_evidence, recorded_at"

func scanVerdictContext(row scanner) (VerdictContextRow, error) {
	var r VerdictContextRow
	err := row.Scan(&r.EventID, &r.SetDigest, &r.Coverage, &r.Findings, &r.Reason, &r.Currency, &r.HeadEventID, &r.HeadRevision, &r.AckEvidence, &r.RecordedAt)
	return r, err
}

// VerificationModeRow is one verification_mode row, every column in DDL order.
type VerificationModeRow struct {
	RelationshipID string
	Mode           string
	RecordedAt     string
}

const verificationModeColumns = "relationship_id, mode, recorded_at"

func scanVerificationMode(row scanner) (VerificationModeRow, error) {
	var r VerificationModeRow
	err := row.Scan(&r.RelationshipID, &r.Mode, &r.RecordedAt)
	return r, err
}
