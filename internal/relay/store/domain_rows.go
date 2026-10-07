package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

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

// CRW-767: the merge train's rows, beside MergeTurnsRow as the decision in docs/port/decisions.md
// section 79 puts them. A train is one row and a member is one row, both written once by the train
// commands (a later issue), and every later step is an appended merge_train_events row, because the
// zone's triggers abort an UPDATE and a DELETE. The train's state is therefore the newest event's
// kind and never a column (MergeTrainState), the way a plan's head revision is MAX(revision_no) of
// its log. turn_id is a plain column with no key into merge_turns: the zone forbids a foreign key
// into a v1 table, so the writer is what keeps it pointing at a real turn.

// MergeTrainRow is one merge_trains row, every column in DDL order.
type MergeTrainRow struct {
	TrainID      string
	TargetKey    string
	Repository   string
	BaseRef      string
	BaseSHA      string
	LeaderTaskID string
	CreatedAt    string
}

const mergeTrainColumns = "train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at"

func scanMergeTrain(row scanner) (MergeTrainRow, error) {
	var r MergeTrainRow
	err := row.Scan(&r.TrainID, &r.TargetKey, &r.Repository, &r.BaseRef, &r.BaseSHA, &r.LeaderTaskID, &r.CreatedAt)
	return r, err
}

// MergeTrainMemberRow is one merge_train_members row, every column in DDL order. Seq is the
// member's FIFO place in the train and MemberHead is the head its pull request showed when the
// train formed.
type MergeTrainMemberRow struct {
	TrainID        string
	Seq            int64
	TurnID         string
	PRNumber       int64
	RelationshipID string
	MemberHead     string
}

const mergeTrainMemberColumns = "train_id, seq, turn_id, pr_number, relationship_id, member_head"

func scanMergeTrainMember(row scanner) (MergeTrainMemberRow, error) {
	var r MergeTrainMemberRow
	err := row.Scan(&r.TrainID, &r.Seq, &r.TurnID, &r.PRNumber, &r.RelationshipID, &r.MemberHead)
	return r, err
}

// MergeTrainEventKind is the closed set of merge_train_events.kind values: a train opened, a member
// prefix verified, a member landed, the train abandoned or the train done. The train's state is the
// kind of its newest event, so these five are also its five states.
const (
	MergeTrainOpened    = "opened"
	MergeTrainVerified  = "verified"
	MergeTrainLanded    = "landed"
	MergeTrainAbandoned = "abandoned"
	MergeTrainDone      = "done"
)

// MergeTrainEventRow is one merge_train_events row, every column in DDL order. DetailJSON is the
// event's body, the empty object when the kind carries none; a verified event carries the member
// seq, check_id, prefix_head, prefix_tree and run_id, and a landed event the member seq and
// landed_sha (the decision in docs/port/decisions.md section 79).
type MergeTrainEventRow struct {
	TrainID    string
	Seq        int64
	Kind       string
	Actor      string
	DetailJSON string
	RecordedAt string
}

const mergeTrainEventColumns = "train_id, seq, kind, actor, detail_json, recorded_at"

func scanMergeTrainEvent(row scanner) (MergeTrainEventRow, error) {
	var r MergeTrainEventRow
	err := row.Scan(&r.TrainID, &r.Seq, &r.Kind, &r.Actor, &r.DetailJSON, &r.RecordedAt)
	return r, err
}

// RecordMergeTrain appends one merge_trains row through the caller's transaction (s.exec runs where
// ctx points, so the command that opens a train commits this row with the rest of its work) and
// never opens one of its own. A train is written once: a second row of the same train_id is the
// table's PRIMARY KEY refusal, and a later change is an appended event, never an update.
func RecordMergeTrain(ctx context.Context, s *Store, r MergeTrainRow) error {
	_, err := s.exec(ctx, "INSERT INTO merge_trains ("+mergeTrainColumns+") VALUES (?,?,?,?,?,?,?)",
		r.TrainID, r.TargetKey, r.Repository, r.BaseRef, r.BaseSHA, r.LeaderTaskID, r.CreatedAt)
	return err
}

// RecordMergeTrainMember appends one merge_train_members row through the caller's transaction. A
// member is written once at its FIFO place: a second row of the same (train_id, seq) is the
// table's UNIQUE refusal, and a member of a train that does not exist is the foreign key's.
func RecordMergeTrainMember(ctx context.Context, s *Store, r MergeTrainMemberRow) error {
	_, err := s.exec(ctx, "INSERT INTO merge_train_members ("+mergeTrainMemberColumns+") VALUES (?,?,?,?,?,?)",
		r.TrainID, r.Seq, r.TurnID, r.PRNumber, r.RelationshipID, r.MemberHead)
	return err
}

// RecordMergeTrainEvent appends one merge_train_events row through the caller's transaction. The
// table is append-only: an event is never updated or deleted, the pair (train_id, seq) is unique,
// and the kind is one of the five MergeTrain* constants. The caller sets Seq to the next sequence
// number of the train, so a repeated step is the UNIQUE refusal rather than a second row.
func RecordMergeTrainEvent(ctx context.Context, s *Store, r MergeTrainEventRow) error {
	_, err := s.exec(ctx, "INSERT INTO merge_train_events ("+mergeTrainEventColumns+") VALUES (?,?,?,?,?,?)",
		r.TrainID, r.Seq, r.Kind, r.Actor, r.DetailJSON, r.RecordedAt)
	return err
}

// MergeTrain reads one train by its id. found is false when the train has no row, or when the
// store predates the zone: neither is an error, the way VerifiedHead answers.
func MergeTrain(ctx context.Context, s *Store, trainID string) (MergeTrainRow, bool, error) {
	present, err := dagZoneTable(ctx, s, "merge_trains")
	if err != nil || !present {
		return MergeTrainRow{}, false, err
	}
	row, err := queryRow(ctx, s, scanMergeTrain, "SELECT "+mergeTrainColumns+" FROM merge_trains WHERE train_id = ?", trainID)
	if errors.Is(err, sql.ErrNoRows) {
		return MergeTrainRow{}, false, nil
	}
	if err != nil {
		return MergeTrainRow{}, false, err
	}
	return row, true, nil
}

// MergeTrainMembers reads every member of a train, in FIFO order. A store without the table holds
// none.
func MergeTrainMembers(ctx context.Context, s *Store, trainID string) ([]MergeTrainMemberRow, error) {
	present, err := dagZoneTable(ctx, s, "merge_train_members")
	if err != nil || !present {
		return nil, err
	}
	return queryRows(ctx, s, scanMergeTrainMember, "SELECT "+mergeTrainMemberColumns+" FROM merge_train_members WHERE train_id = ? ORDER BY seq", trainID)
}

// MergeTrainMember reads one member of a train by its sequence number. found is false when the
// train holds no such member, or when the store predates the zone.
func MergeTrainMember(ctx context.Context, s *Store, trainID string, seq int64) (MergeTrainMemberRow, bool, error) {
	present, err := dagZoneTable(ctx, s, "merge_train_members")
	if err != nil || !present {
		return MergeTrainMemberRow{}, false, err
	}
	row, err := queryRow(ctx, s, scanMergeTrainMember, "SELECT "+mergeTrainMemberColumns+" FROM merge_train_members WHERE train_id = ? AND seq = ?", trainID, seq)
	if errors.Is(err, sql.ErrNoRows) {
		return MergeTrainMemberRow{}, false, nil
	}
	if err != nil {
		return MergeTrainMemberRow{}, false, err
	}
	return row, true, nil
}

// MergeTrainEvents reads a train's whole event log, oldest sequence first: the log a reader folds
// into the train's state. A store without the table holds no event.
func MergeTrainEvents(ctx context.Context, s *Store, trainID string) ([]MergeTrainEventRow, error) {
	present, err := dagZoneTable(ctx, s, "merge_train_events")
	if err != nil || !present {
		return nil, err
	}
	return queryRows(ctx, s, scanMergeTrainEvent, "SELECT "+mergeTrainEventColumns+" FROM merge_train_events WHERE train_id = ? ORDER BY seq", trainID)
}

// MergeTrainEvent reads one event of a train by its sequence number. found is false when the train
// holds no such event, or when the store predates the zone.
func MergeTrainEvent(ctx context.Context, s *Store, trainID string, seq int64) (MergeTrainEventRow, bool, error) {
	present, err := dagZoneTable(ctx, s, "merge_train_events")
	if err != nil || !present {
		return MergeTrainEventRow{}, false, err
	}
	row, err := queryRow(ctx, s, scanMergeTrainEvent, "SELECT "+mergeTrainEventColumns+" FROM merge_train_events WHERE train_id = ? AND seq = ?", trainID, seq)
	if errors.Is(err, sql.ErrNoRows) {
		return MergeTrainEventRow{}, false, nil
	}
	if err != nil {
		return MergeTrainEventRow{}, false, err
	}
	return row, true, nil
}

// MergeTrainState is the state of a train: the kind of its newest event, which is what the zone
// stores in place of a state column. found is false when the train has no event, or when the store
// predates the zone. An eventless train is the moment between merge-train-open and its first
// appended event.
func MergeTrainState(ctx context.Context, s *Store, trainID string) (string, bool, error) {
	present, err := dagZoneTable(ctx, s, "merge_train_events")
	if err != nil || !present {
		return "", false, err
	}
	var kind string
	err = s.q(ctx).QueryRowContext(ctx, "SELECT kind FROM merge_train_events WHERE train_id = ? ORDER BY seq DESC LIMIT 1", trainID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return kind, true, nil
}

// MergeTrainOfTurn is the train a turn is a member of, and its state, when that train is still live:
// CRW-768's member guard reads it so a member turn refuses merge-turn-check and merge-turn-land while
// the train is opened or verified, and is left alone once the train landed, was done or abandoned.
// found is false when the turn is in no live train, or when the store predates the zone. The state
// is the newest event's kind (MergeTrainState); a train with no event yet is opened by definition.
func MergeTrainOfTurn(ctx context.Context, s *Store, turnID string) (MergeTrainRow, string, bool, error) {
	present, err := dagZoneTable(ctx, s, "merge_train_members")
	if err != nil || !present {
		return MergeTrainRow{}, "", false, err
	}
	// every membership of the turn is read, newest train first, and the newest event's kind decides:
	// a turn that was in an abandoned train and now waits in a replacement must be answered by the
	// replacement, so the first historical membership is not enough (finding 4)
	members, err := queryRows(ctx, s, scanMergeTrainMember, "SELECT "+mergeTrainMemberColumns+" FROM merge_train_members WHERE turn_id = ? ORDER BY train_id DESC", turnID)
	if err != nil {
		return MergeTrainRow{}, "", false, err
	}
	for _, member := range members {
		state, found, err := MergeTrainState(ctx, s, member.TrainID)
		if err != nil {
			return MergeTrainRow{}, "", false, err
		}
		if !found {
			state = MergeTrainOpened
		}
		if state != MergeTrainOpened && state != MergeTrainVerified {
			continue
		}
		row, found, err := MergeTrain(ctx, s, member.TrainID)
		if err != nil {
			return MergeTrainRow{}, "", false, err
		}
		if !found {
			continue
		}
		return row, state, true, nil
	}
	return MergeTrainRow{}, "", false, nil
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

// AcceptanceRefreshRow is one dag_acceptance_refreshes row, every column in DDL order (CRW-728): the
// proof that an acceptance's head is the head the parent's ruling named plus merges of the base and
// nothing else. Its RefreshID is the digest of the rest of the row (RefreshDigest), so a reader can
// tell a row the relay wrote from one written by hand.
type AcceptanceRefreshRow struct {
	RefreshID           string
	AcceptanceID        string
	RefreshSeq          int64
	RelationshipID      string
	ExecutionGeneration int64
	EventID             string
	RevisionHash        string
	HeadSHA             string
	VerifiedHeadSHA     string
	BaseRepository      string
	BaseRef             string
	BaseTipSHA          string
	ProofJSON           string
	ResolvedPathsJSON   string
	RecordedByTaskID    string
	CoordinatorEpoch    int64
	RecordedAt          string
}

const acceptanceRefreshColumns = "refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, verified_head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at"

func scanAcceptanceRefresh(row scanner) (AcceptanceRefreshRow, error) {
	var r AcceptanceRefreshRow
	err := row.Scan(&r.RefreshID, &r.AcceptanceID, &r.RefreshSeq, &r.RelationshipID, &r.ExecutionGeneration, &r.EventID,
		&r.RevisionHash, &r.HeadSHA, &r.VerifiedHeadSHA, &r.BaseRepository, &r.BaseRef, &r.BaseTipSHA, &r.ProofJSON,
		&r.ResolvedPathsJSON, &r.RecordedByTaskID, &r.CoordinatorEpoch, &r.RecordedAt)
	return r, err
}

// VerifiedHeadRow is one dag_verified_heads row, every column in DDL order (CRW-728): the head the
// verdict turn of an event verified, fixed at ruling time so a later acceptance can prove against it.
type VerifiedHeadRow struct {
	EventID             string
	RelationshipID      string
	ExecutionGeneration int64
	VerdictTurnID       string
	HeadSHA             string
	RecordedByTaskID    string
	RecordedAt          string
}

const verifiedHeadColumns = "event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at"

func scanVerifiedHead(row scanner) (VerifiedHeadRow, error) {
	var r VerifiedHeadRow
	err := row.Scan(&r.EventID, &r.RelationshipID, &r.ExecutionGeneration, &r.VerdictTurnID, &r.HeadSHA, &r.RecordedByTaskID, &r.RecordedAt)
	return r, err
}

// AcceptanceRefreshSchema names the record RefreshDigest digests: the identity of one dag_acceptance_refreshes row.
const AcceptanceRefreshSchema = "dag-acceptance-refresh/1"

// ErrRefreshNotRecorded is the answer of a reader that found no row, or found one whose id is not the
// digest of its own content. It is not a refusal of the relay: a store that predates the table answers
// it too, so a caller reads it as "there is no proof here".
var ErrRefreshNotRecorded = errors.New("store: no acceptance refresh recorded")

// RefreshDigest is the identity of a refresh: its refresh_id is the digest of everything the row
// records but the time, the author and the coordinator epoch, so a row written by hand under another
// content is not read as one. It is built the way dagsched.refreshDigest builds the base refresh's id
// (dagsched/baserefresh.go), over the same canonical JSON every digest of the relay hashes, and lives
// here because the table's writer is this package and internal/relay/dag imports it (not the reverse).
func RefreshDigest(r AcceptanceRefreshRow) string {
	return "dar-" + digest(pyjson.Dumps(map[string]any{
		"schema":               AcceptanceRefreshSchema,
		"acceptance_id":        r.AcceptanceID,
		"refresh_seq":          r.RefreshSeq,
		"relationship_id":      r.RelationshipID,
		"execution_generation": r.ExecutionGeneration,
		"event_id":             r.EventID,
		"revision_hash":        r.RevisionHash,
		"head_sha":             r.HeadSHA,
		"verified_head_sha":    r.VerifiedHeadSHA,
		"base_repository":      r.BaseRepository,
		"base_ref":             r.BaseRef,
		"base_tip_sha":         r.BaseTipSHA,
		"proof":                r.ProofJSON,
		"resolved_paths":       r.ResolvedPathsJSON,
	}, pyjson.Options{Compact: true, SortKeys: true, Unicode: true}), 32)
}

// refreshRowValid is whether a stored row's id is the digest of its own content.
func refreshRowValid(r AcceptanceRefreshRow) bool { return r.RefreshID == RefreshDigest(r) }

// dagZoneTable reports whether the store holds the named zone table. The zone arrives with the first
// write open (D-01), so a read-only reader of a store that predates it finds no table; that is absence
// and not an error, the way dagsched.validStands reads dag_base_refreshes.
func dagZoneTable(ctx context.Context, s *Store, table string) (bool, error) {
	var name string
	err := s.q(ctx).QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name == table, nil
}

// RecordAcceptanceRefresh appends one dag_acceptance_refreshes row through the caller's transaction
// (s.exec runs where ctx points, so the acceptance that writes its proof and this row commit together)
// and never opens one of its own. It does not compute the id: the caller sets RefreshID with
// RefreshDigest, so the row a reader accepts is the row the caller proved.
func RecordAcceptanceRefresh(ctx context.Context, s *Store, r AcceptanceRefreshRow) error {
	_, err := s.exec(ctx, "INSERT INTO dag_acceptance_refreshes ("+acceptanceRefreshColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		r.RefreshID, r.AcceptanceID, r.RefreshSeq, r.RelationshipID, r.ExecutionGeneration, r.EventID, r.RevisionHash,
		r.HeadSHA, r.VerifiedHeadSHA, r.BaseRepository, r.BaseRef, r.BaseTipSHA, r.ProofJSON, r.ResolvedPathsJSON,
		r.RecordedByTaskID, r.CoordinatorEpoch, r.RecordedAt)
	return err
}

// AcceptanceRefresh reads one refresh of an acceptance by its sequence number. A row whose id is not
// the digest of its own content is not read, and a store without the table answers ErrRefreshNotRecorded.
func AcceptanceRefresh(ctx context.Context, s *Store, acceptanceID string, refreshSeq int64) (AcceptanceRefreshRow, error) {
	present, err := dagZoneTable(ctx, s, "dag_acceptance_refreshes")
	if err != nil {
		return AcceptanceRefreshRow{}, err
	}
	if !present {
		return AcceptanceRefreshRow{}, ErrRefreshNotRecorded
	}
	row, err := queryRow(ctx, s, scanAcceptanceRefresh, "SELECT "+acceptanceRefreshColumns+" FROM dag_acceptance_refreshes WHERE acceptance_id = ? AND refresh_seq = ?", acceptanceID, refreshSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return AcceptanceRefreshRow{}, ErrRefreshNotRecorded
	}
	if err != nil {
		return AcceptanceRefreshRow{}, err
	}
	if !refreshRowValid(row) {
		return AcceptanceRefreshRow{}, ErrRefreshNotRecorded
	}
	return row, nil
}

// AcceptanceRefreshes reads every valid refresh of an acceptance, oldest sequence first. The rows the
// relay wrote are the ones whose id digests their content; a store without the table holds none.
func AcceptanceRefreshes(ctx context.Context, s *Store, acceptanceID string) ([]AcceptanceRefreshRow, error) {
	present, err := dagZoneTable(ctx, s, "dag_acceptance_refreshes")
	if err != nil || !present {
		return nil, err
	}
	rows, err := queryRows(ctx, s, scanAcceptanceRefresh, "SELECT "+acceptanceRefreshColumns+" FROM dag_acceptance_refreshes WHERE acceptance_id = ? ORDER BY refresh_seq", acceptanceID)
	if err != nil {
		return nil, err
	}
	valid := make([]AcceptanceRefreshRow, 0, len(rows))
	for _, row := range rows {
		if refreshRowValid(row) {
			valid = append(valid, row)
		}
	}
	return valid, nil
}

// RecordVerifiedHead appends one dag_verified_heads row through the caller's transaction. One event
// holds one row: a second insert is the table's UNIQUE refusal, as a duplicate of any other identity
// in the zone is, and the caller reads it as such.
func RecordVerifiedHead(ctx context.Context, s *Store, r VerifiedHeadRow) error {
	_, err := s.exec(ctx, "INSERT INTO dag_verified_heads ("+verifiedHeadColumns+") VALUES (?,?,?,?,?,?,?)",
		r.EventID, r.RelationshipID, r.ExecutionGeneration, r.VerdictTurnID, r.HeadSHA, r.RecordedByTaskID, r.RecordedAt)
	return err
}

// VerifiedHead reads the head a verdict turn verified for an event. found is false when the event has
// none, or when the store predates the table: neither is an error.
func VerifiedHead(ctx context.Context, s *Store, eventID string) (VerifiedHeadRow, bool, error) {
	present, err := dagZoneTable(ctx, s, "dag_verified_heads")
	if err != nil || !present {
		return VerifiedHeadRow{}, false, err
	}
	row, err := queryRow(ctx, s, scanVerifiedHead, "SELECT "+verifiedHeadColumns+" FROM dag_verified_heads WHERE event_id = ?", eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return VerifiedHeadRow{}, false, nil
	}
	if err != nil {
		return VerifiedHeadRow{}, false, err
	}
	return row, true, nil
}

// DagExecutionPacketRow is one dag_execution_packets row, every column in DDL order (CRW-839): the
// packet a released node is executed under. Branch is NULL when the release request named no work
// branch, so it is a sql.NullString rather than an empty string.
type DagExecutionPacketRow struct {
	RelationshipID string
	PlanID         string
	NodeID         string
	IssueKey       string
	PacketID       string
	Branch         sql.NullString
	RecordedAt     string
}

const dagExecutionPacketColumns = "relationship_id, plan_id, node_id, issue_key, packet_id, branch, recorded_at"

func scanDagExecutionPacket(row scanner) (DagExecutionPacketRow, error) {
	var r DagExecutionPacketRow
	err := row.Scan(&r.RelationshipID, &r.PlanID, &r.NodeID, &r.IssueKey, &r.PacketID, &r.Branch, &r.RecordedAt)
	return r, err
}

// RecordDagExecutionPacket appends one dag_execution_packets row through the caller's transaction. The
// table is append-only and relationship_id is its primary key, so a second row for one relationship is
// the primary key's refusal rather than a re-recording of the same packet.
func RecordDagExecutionPacket(ctx context.Context, s *Store, r DagExecutionPacketRow) error {
	_, err := s.exec(ctx, "INSERT INTO dag_execution_packets ("+dagExecutionPacketColumns+") VALUES (?,?,?,?,?,?,?)",
		r.RelationshipID, r.PlanID, r.NodeID, r.IssueKey, r.PacketID, r.Branch, r.RecordedAt)
	return err
}

// DagExecutionPacket reads the packet of one relationship. found is false when the relationship has no
// row, or when the store predates the zone: neither is an error, the way VerifiedHead answers.
func DagExecutionPacket(ctx context.Context, s *Store, relationshipID string) (DagExecutionPacketRow, bool, error) {
	present, err := dagZoneTable(ctx, s, "dag_execution_packets")
	if err != nil || !present {
		return DagExecutionPacketRow{}, false, err
	}
	row, err := queryRow(ctx, s, scanDagExecutionPacket, "SELECT "+dagExecutionPacketColumns+" FROM dag_execution_packets WHERE relationship_id = ?", relationshipID)
	if errors.Is(err, sql.ErrNoRows) {
		return DagExecutionPacketRow{}, false, nil
	}
	if err != nil {
		return DagExecutionPacketRow{}, false, err
	}
	return row, true, nil
}
