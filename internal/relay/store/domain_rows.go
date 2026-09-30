package store

import "database/sql"

// Row types for the domain tables todo 19 ports. Fields follow contract/schema/relay-sqlite.sql
// column order exactly, NOT NULL columns as plain values and nullable ones as sql.Null*, so a
// row read back from a Python-written store keeps NULL apart from an empty value.

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

// EditRevisionMarksRow is one edit_revision_marks row, every column in DDL order.
type EditRevisionMarksRow struct {
	MarkID       string
	Repository   string
	FromRevision string
	ToRevision   string
	Actor        string
	RecordedAt   string
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
