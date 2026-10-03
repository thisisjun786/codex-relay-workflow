// Package goalplan is the Go form of the goalplan record of CXC v0.2.40 pabcd-state/src/goalplan.ts (commit 3c1459ac): the
// plan a loop keeps at .crw/goalplans/<slug>/goalplan.json beside its ledger. types.go holds the record types and constants
// (goalplan.ts:34-291); revive.go the strict revival of the sub-records of a stored plan (:333-467): review rounds, their
// lanes and plan-file lists, source identities and the final gate. The slug rules, the plan revival that calls these
// functions, reads, writes, locks and the rest of the file belong to later issues.
//
// Behaviour is ported as-is, oracle defects included. A stored value never becomes a record by decoding it into the
// structs: a revive function rebuilds each field from known keys and defaults or drops what is malformed, so a hand-written
// or foreign-version plan cannot widen what a review or the final gate says happened.
//
// JSON names are the oracle's. Fields are in the order the oracle's revival inserts its keys, which is the order a plan has
// once it was read and written back, not the order of the TypeScript interfaces. An optional field the oracle keeps even
// when it is empty is a pointer (nil is absent, a pointer to "" is the empty string); an optional list is a plain slice with
// omitzero (nil is absent, a non-nil empty slice is []), because "absent stays absent, [] stays []" is a stored shape the
// oracle preserves. A field the oracle refuses when empty is a plain value with omitempty.
package goalplan

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"

// The files of a plan under .crw/goalplans/<slug>/ (goalplan.ts:40-44; the directory is crwdir.DirName/GoalplansSubdir/<slug>).
const (
	GoalplansSubdir       = "goalplans"
	GoalplanFile          = "goalplan.json"
	GoalplanLedgerFile    = "ledger.jsonl"
	GoalplanLockDir       = ".goalplan.lock"
	GoalplanLockOwnerFile = "owner.json"
)

// GoalplanLockRetryDelaysMs is GOALPLAN_LOCK_RETRY_DELAYS_MS: the waits, in milliseconds, between attempts to take the write lock.
func GoalplanLockRetryDelaysMs() []int { return []int{5, 10, 20, 40} }

// SupportedMaxSchemaVersion is the highest schemaVersion this build understands. A plan that declares more is refused on
// read and on validate, never clamped: a build that accepted a newer plan would strip the fields it never learned and the
// next write would persist that loss.
const SupportedMaxSchemaVersion = 3

// DefaultNewSchemaVersion is the schema a new plan declares when the caller does not choose one. It is not the maximum: that
// is what a build can read, this is what a fresh plan should claim. A plan of schema 2 or more must clear a final gate that
// no shipped verb opens, so defaulting to the maximum made every new plan impossible to complete.
const DefaultNewSchemaVersion = 1

// PlanFileHash is one file a review round names and its sha256 (freeze.ts:23). The path is as the round recorded it; revival
// does not look at it beyond its being a non-empty string.
type PlanFileHash struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

// SourceIdentity is the source tree a review or the final gate looked at, as a plan stores it: the state package's type, which
// keeps an absent tree hash apart from an empty one and the oracle's key order.
type SourceIdentity = state.SourceIdentity

// CriterionStatus is open until evidence is captured for a criterion.
type CriterionStatus string

// The statuses of a criterion.
const (
	CriterionOpen CriterionStatus = "open"
	CriterionMet  CriterionStatus = "met"
)

// CriterionSurface is what a criterion exercises; it decides whether the final gate demands a QA receipt on top of a test receipt.
type CriterionSurface string

// The surfaces of a criterion.
const (
	SurfaceLogic   CriterionSurface = "logic"
	SurfaceWeb     CriterionSurface = "web"
	SurfaceTUI     CriterionSurface = "tui"
	SurfaceDesktop CriterionSurface = "desktop"
)

// PresentedSurface is how a criterion's surface is presented; only "native" exists.
type PresentedSurface string

// PresentedNative is the one presented surface.
const PresentedNative PresentedSurface = "native"

// TaskStatus is the state of a task.
type TaskStatus string

// The statuses of a task.
const (
	TaskPending TaskStatus = "pending"
	TaskDone    TaskStatus = "done"
)

// WorkPhaseStatus is the state of a work phase. Blocked and superseded are both "not done" and neither counts as success: a
// blocked phase still holds the goal open, a superseded one does not because another phase covers it.
type WorkPhaseStatus string

// The statuses of a work phase.
const (
	WorkPhasePending    WorkPhaseStatus = "pending"
	WorkPhaseInProgress WorkPhaseStatus = "in_progress"
	WorkPhaseDone       WorkPhaseStatus = "done"
	WorkPhaseBlocked    WorkPhaseStatus = "blocked"
	WorkPhaseSuperseded WorkPhaseStatus = "superseded"
)

// GoalplanCriterion is one completion criterion. Surface is absent on a schema 1 plan and stays absent when the stored value
// is not a known surface: a schema 2 plan rejects an absent one, so a typo cannot buy a QA exemption.
type GoalplanCriterion struct {
	ID               string           `json:"id"`
	Scenario         string           `json:"scenario"`
	ExpectedEvidence string           `json:"expectedEvidence"`
	CapturedEvidence *string          `json:"capturedEvidence"` // null until captured
	Status           CriterionStatus  `json:"status"`
	Surface          CriterionSurface `json:"surface,omitempty"`
	Presented        PresentedSurface `json:"presented,omitempty"`
}

// GoalplanTask is one task of a work phase. DependsOn names tasks of the same phase that must be done first (task ids are
// unique per phase, so a dependency across phases cannot be written); absent and [] both mean no dependency and each is stored
// as written. Outcome is the verifiable evidence of a done task, kept in the plan because the plan commit and the ledger append
// are not atomic.
type GoalplanTask struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Status    TaskStatus `json:"status"`
	DependsOn []string   `json:"dependsOn,omitzero"`
	Outcome   string     `json:"outcome,omitempty"`
}

// GoalplanWorkPhase is one work phase, one full PABCD cycle. Tasks and CriteriaIDs are never nil. DependsOn lists phases of the
// plan that must be done first; AwaitsDecision lists decision ids, and an open one pauses the phase without changing its
// status. BlockedReason says why a blocked phase cannot proceed and SupersededBy names the phase that took over; both are
// required of their status.
type GoalplanWorkPhase struct {
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Status         WorkPhaseStatus `json:"status"`
	Tasks          []GoalplanTask  `json:"tasks"`
	CriteriaIDs    []string        `json:"criteriaIds"`
	DependsOn      []string        `json:"dependsOn,omitzero"`
	AwaitsDecision []string        `json:"awaitsDecision,omitzero"`
	BlockedReason  *string         `json:"blockedReason,omitempty"`
	SupersededBy   *string         `json:"supersededBy,omitempty"`
}

// DecisionStatus is whether a question has been answered.
type DecisionStatus string

// The statuses of a decision.
const (
	DecisionOpen    DecisionStatus = "open"
	DecisionDecided DecisionStatus = "decided"
)

// GoalplanDecision is a question put to the user. When Options is present the recommendation is one of them. Answer and
// DecidedAt belong to a decided one only.
type GoalplanDecision struct {
	ID             string         `json:"id"`
	Question       string         `json:"question"`
	Status         DecisionStatus `json:"status"`
	Answer         string         `json:"answer,omitempty"`
	AskedAt        string         `json:"askedAt"`
	DecidedAt      string         `json:"decidedAt,omitempty"`
	Recommendation string         `json:"recommendation,omitempty"`
	Options        []string       `json:"options,omitzero"`
}

// HostSource says where a host link came from.
type HostSource string

// The sources of a host link.
const (
	HostSourceFreeze HostSource = "freeze"
	HostSourceNone   HostSource = "none"
)

// GoalplanHostLink is provenance only: Armed records that the main session armed a goal at the freeze boundary. Nothing here
// ever creates or writes a host goal.
type GoalplanHostLink struct {
	Armed   bool       `json:"armed"`
	ArmedAt *string    `json:"armedAt"`
	Source  HostSource `json:"source"`
}

// ReviewRoundStatus is where a review round stands.
type ReviewRoundStatus string

// The statuses of a review round.
const (
	ReviewPending          ReviewRoundStatus = "pending"
	ReviewLaunching        ReviewRoundStatus = "launching"
	ReviewInFlight         ReviewRoundStatus = "in_flight"
	ReviewApproved         ReviewRoundStatus = "approved"
	ReviewChangesRequested ReviewRoundStatus = "changes_requested"
	ReviewInconclusive     ReviewRoundStatus = "inconclusive"
)

// ReviewPurpose is what a round is for. A plan audit and a final code gate cannot stand in for each other, so each purpose
// carries its own cursor.
type ReviewPurpose string

// The purposes of a review round.
const (
	PurposePlanAudit ReviewPurpose = "plan_audit"
	PurposeFinalGate ReviewPurpose = "final_gate"
)

// Verdict is what a reviewer concluded.
type Verdict string

// The verdicts a reviewer can give.
const (
	VerdictPass     Verdict = "pass"
	VerdictNearPass Verdict = "near-pass"
	VerdictFail     Verdict = "fail"
)

// ReviewLane is the reviewer launch of a round. LaunchID is minted when the round opens and travels out with the spawn
// packet; the rest is what the reviewer reports back: its session, workspace, the sha256 of the verdict document it produced,
// its verdict and the source state it actually looked at.
type ReviewLane struct {
	LaunchID        string          `json:"launchId"`
	ReviewerSession *string         `json:"reviewerSession,omitempty"`
	WorkspaceRoot   *string         `json:"workspaceRoot,omitempty"`
	ArtifactSha256  *string         `json:"artifactSha256,omitempty"`
	Verdict         Verdict         `json:"verdict,omitempty"`
	SourceIdentity  *SourceIdentity `json:"sourceIdentity,omitempty"`
}

// ReviewRoundState is one review round: RoundID is r1, r2, ... increasing per plan, PlanPath the document under audit and
// PlanSha256 its hash when the round opened (the caller computes it). OwnerSessionID, WorkPhaseID, PlanUnit, PlanEpoch and
// PlanFiles bind the round to what it was opened against (REVIEW-BINDING-01); all are optional so older rounds still parse,
// and the A to B gate reads a missing one as a refusal, so nothing is grandfathered into approval. PlanFiles are the exact
// files the round names, hashed in path order into PlanSha256.
type ReviewRoundState struct {
	RoundID        string            `json:"roundId"`
	Purpose        ReviewPurpose     `json:"purpose"`
	PlanPath       string            `json:"planPath"`
	PlanSha256     string            `json:"planSha256"`
	Status         ReviewRoundStatus `json:"status"`
	Lane           ReviewLane        `json:"lane"`
	OpenedAt       string            `json:"openedAt"`
	ClosedAt       *string           `json:"closedAt,omitempty"`
	OwnerSessionID string            `json:"ownerSessionId,omitempty"`
	WorkPhaseID    string            `json:"workPhaseId,omitempty"`
	PlanUnit       string            `json:"planUnit,omitempty"`
	PlanEpoch      string            `json:"planEpoch,omitempty"`
	PlanFiles      []PlanFileHash    `json:"planFiles,omitzero"`
}

// FinalGateStatus is where the final gate stands.
type FinalGateStatus string

// The statuses of the final gate.
const (
	GatePending      FinalGateStatus = "pending"
	GateInFlight     FinalGateStatus = "in_flight"
	GateApproved     FinalGateStatus = "approved"
	GateInconclusive FinalGateStatus = "inconclusive"
)

// FinalGateState is the final code gate of a plan of schema 2 or more. ReviewRoundID names the round that produced the verdict
// (its purpose is final_gate). QaRequired is not optional: it is frozen when the gate opens by scanning every criterion, so
// "QA is not needed" and "nobody decided yet" cannot look the same.
type FinalGateState struct {
	Status          FinalGateStatus `json:"status"`
	QaRequired      bool            `json:"qaRequired"`
	UpdatedAt       string          `json:"updatedAt"`
	ReviewRoundID   *string         `json:"reviewRoundId,omitempty"`
	TestReceiptPath *string         `json:"testReceiptPath,omitempty"`
	QaReceiptPath   *string         `json:"qaReceiptPath,omitempty"`
	Verdict         Verdict         `json:"verdict,omitempty"`
	SourceIdentity  *SourceIdentity `json:"sourceIdentity,omitempty"`
}

// SteeringEntry is one applied steering batch, the source of truth for idempotency; Summary says what the batch changed, never
// a copy of the plan.
type SteeringEntry struct {
	IdempotencyKey string `json:"idempotencyKey"`
	Rationale      string `json:"rationale"`
	Evidence       string `json:"evidence"`
	AppliedAt      string `json:"appliedAt"`
	Summary        string `json:"summary"`
}

// Goalplan is the durable plan of a loop. ActiveWorkPhaseID is the work-phase cursor the FSM does not hold across a D-close.
// ReviewRounds (oldest first) is absent on a plan created before review rounds existed; the two cursors are one per purpose.
// SchemaVersion is a JavaScript number, absent meaning 1; FinalGate is required of a plan of schema 2 or more.
type Goalplan struct {
	Objective              string              `json:"objective"`
	Slug                   string              `json:"slug"`
	CreatedAt              string              `json:"createdAt"`
	UpdatedAt              string              `json:"updatedAt"`
	ActiveWorkPhaseID      *string             `json:"activeWorkPhaseId"`
	WorkPhases             []GoalplanWorkPhase `json:"workPhases"`
	Criteria               []GoalplanCriterion `json:"criteria"`
	Host                   GoalplanHostLink    `json:"host"`
	ReviewRounds           []ReviewRoundState  `json:"reviewRounds,omitzero"`
	Decisions              []GoalplanDecision  `json:"decisions,omitzero"`
	ActivePlanAuditRoundID *string             `json:"activePlanAuditRoundId,omitempty"`
	ActiveFinalGateRoundID *string             `json:"activeFinalGateRoundId,omitempty"`
	SchemaVersion          *float64            `json:"schemaVersion,omitempty"`
	FinalGate              *FinalGateState     `json:"finalGate,omitempty"`
	SteeringLog            []SteeringEntry     `json:"steeringLog,omitzero"`
}

// GoalplanLedgerEvent is the kind of a ledger row. A dependency_registered row says when an edge appeared on a phase or task,
// which the final graph in the plan cannot; review_signoff_ignored and review_round_superseded say why a reviewer's verdict was
// not recorded and when a round was rolled past, which would otherwise leave no trace.
type GoalplanLedgerEvent string

// The events of a ledger row.
const (
	EventCreated               GoalplanLedgerEvent = "created"
	EventWorkphaseStarted      GoalplanLedgerEvent = "workphase_started"
	EventWorkphaseDone         GoalplanLedgerEvent = "workphase_done"
	EventTaskDone              GoalplanLedgerEvent = "task_done"
	EventCriterionMet          GoalplanLedgerEvent = "criterion_met"
	EventHostArmed             GoalplanLedgerEvent = "host_armed"
	EventSteered               GoalplanLedgerEvent = "steered"
	EventDependencyRegistered  GoalplanLedgerEvent = "dependency_registered"
	EventReviewSignoffIgnored  GoalplanLedgerEvent = "review_signoff_ignored"
	EventReviewRoundSuperseded GoalplanLedgerEvent = "review_round_superseded"
)

// GoalplanLedgerEntry is one row of the append-only ledger. RoundID and LaunchID say which round a row is about, so a reader
// filters by round rather than parsing Detail; they are absent on a row that is not about a round.
type GoalplanLedgerEntry struct {
	Ts       string              `json:"ts"`
	Slug     string              `json:"slug"`
	Event    GoalplanLedgerEvent `json:"event"`
	Detail   string              `json:"detail"`
	RoundID  string              `json:"roundId,omitempty"`
	LaunchID string              `json:"launchId,omitempty"`
}
