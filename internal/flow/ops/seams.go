package ops

import (
	"context"
	"encoding/json"
	"time"
)

// PlanNode is a node of the plan as the relay reads it for the lane: the issue it carries, the nodes it waits for (a dependency
// or an order edge compile put there; a group's members are chained by order edges), the edit regions it declares and the digest
// of its slice of the plan revision.
type PlanNode struct {
	ID             string   `json:"id"`
	Issue          string   `json:"issue"`
	Title          string   `json:"title"`
	After          []string `json:"after"`
	Regions        []string `json:"regions"`
	Group          string   `json:"group"`
	Criteria       []string `json:"criteria"`
	CriteriaDigest string   `json:"criteria_digest"`
	SliceDigest    string   `json:"slice_digest"`
}

// Milestone is a milestone of a plan: its nodes in the order the scheduler offers them (dagsched.Ready: critical path, then
// descendants, then the node id), and the criteria digest of the milestone.
type Milestone struct {
	ID             string     `json:"id"`
	Plan           string     `json:"plan"`
	Title          string     `json:"title"`
	Nodes          []PlanNode `json:"nodes"`
	Criteria       []string   `json:"criteria"`
	CriteriaDigest string     `json:"criteria_digest"`
}

// ReleaseRequest releases an issue node to an executor of the run: the node is admitted by the scheduler (the regions it edits
// do not overlap a running node's, a slot is free, the host has the memory) and its slot reserved.
type ReleaseRequest struct {
	Plan, Node, RunID string
	// Revision and DesignDigest are the design revision the release is made under; the lane's packets are built from it.
	Revision, DesignDigest string
	// RequestID makes the release idempotent: the same request is the same release.
	RequestID string
}

// ReleaseResult is the scheduler's answer to a release. A node it holds back (Released false) names why in Reason
// (capacity_exhausted, a region overlap, a host short of memory) and is not started.
type ReleaseResult struct {
	Released    bool   `json:"released"`
	Reason      string `json:"reason"`
	ExecutionID string `json:"execution_id"`
	SlotID      string `json:"slot_id"`
	Replayed    bool   `json:"replayed"`
}

// AcceptRequest asks the relay to accept an agent node's head (dag-accept on the agent path, R05): the premerge record the run
// assembled from the verdict, the verification record, and the reviewer call the acceptance stands on.
type AcceptRequest struct {
	Plan, Node, Actor, RunID string
	Head, Base               string
	// Premerge is the premerge-record/1 document and Verification the verification-record/1 document.
	Premerge, Verification json.RawMessage
	CriteriaDigest         string
	// ReviewerCall is the call whose verdict the acceptance rests on.
	ReviewerCall string
	RequestID    string
}

// AcceptResult is the relay's answer to an acceptance. A refusal is Accepted false with the closed reason.
type AcceptResult struct {
	Accepted     bool   `json:"accepted"`
	Reason       string `json:"reason"`
	Detail       string `json:"detail"`
	AcceptanceID string `json:"acceptance_id"`
	Replayed     bool   `json:"replayed"`
}

// AcceptState is what the relay says about an acceptance now.
type AcceptState struct {
	Active, Stale bool
	Head          string
}

// IntegrateRequest asks the relay to land the node's accepted head on the integration target (dag-integrate, R05).
type IntegrateRequest struct {
	Plan, Node, Actor, RunID string
	AcceptanceID             string
	Head                     string
	RequestID                string
}

// IntegrateResult is the batch that moved the target.
type IntegrateResult struct {
	Landed         bool   `json:"landed"`
	Reason         string `json:"reason"`
	Detail         string `json:"detail"`
	BatchCommit    string `json:"batch_commit"`
	IntegrationRef string `json:"integration_ref"`
	Tip            string `json:"tip"`
	Replayed       bool   `json:"replayed"`
}

// CloseRequest ends or holds the current execution of an agent node (AgentClose, R07): a node given up (tiers_exhausted,
// blocked_scope, size_split) or cancelled (flow_cancel) has its slot returned in the transaction that closes it; a recovery hold
// (dispatch_unknown, host_failure) closes nothing.
type CloseRequest struct {
	Plan, Node, Actor, Reason string
	RequestID                 string
}

// CloseResult is what AgentClose answered.
type CloseResult struct {
	Closed       bool   `json:"closed"`
	Held         bool   `json:"held"`
	SlotReleased bool   `json:"slot_released"`
	Replayed     bool   `json:"replayed"`
	ExecutionID  string `json:"execution_id"`
	State        string `json:"state"`
}

// ChildRequest releases a node as an issue child (managed start of an issue-lane run, SP-CHILD-1) instead of an executor.
type ChildRequest struct {
	Plan, Node, RunID string
	Revision          string
	DesignDigest      string
	Reason            string
	RequestID         string
}

// ChildResult is the child that was started.
type ChildResult struct {
	Started bool   `json:"started"`
	Reason  string `json:"reason"`
	Child   string `json:"child"`
}

// Relay is the DAG scheduler of the relay as the lane uses it.
type Relay interface {
	// Milestone reads a milestone of a plan: its nodes in the order the scheduler offers them.
	Milestone(ctx context.Context, plan, milestone string) (Milestone, error)
	// Release admits a node to an executor of the run (dag-release --surface agent) and reserves its slot.
	Release(ctx context.Context, req ReleaseRequest) (ReleaseResult, error)
	// ReleaseStands reports whether a release is still the node's current one (its execution is not closed).
	ReleaseStands(ctx context.Context, plan, node, execution string) (bool, error)
	// ReleaseChild releases a node as an issue child.
	ReleaseChild(ctx context.Context, req ChildRequest) (ChildResult, error)
	// Accept is dag-accept on the agent path.
	Accept(ctx context.Context, req AcceptRequest) (AcceptResult, error)
	// AcceptState reads an acceptance now.
	AcceptState(ctx context.Context, acceptanceID string) (AcceptState, bool, error)
	// Integrate is dag-integrate on the agent path.
	Integrate(ctx context.Context, req IntegrateRequest) (IntegrateResult, error)
	// IntegrationTip is the tip of the integration target: the base of a node's worktree and the head the final gate judges.
	IntegrationTip(ctx context.Context, plan string) (string, error)
	// Close is AgentClose.
	Close(ctx context.Context, req CloseRequest) (CloseResult, error)
}

// VerifyRequest asks for the verification of a committed head in the node's worktree.
type VerifyRequest struct {
	Node, Head, Worktree, Base string
}

// VerifyOutcome is the verification of a head: the command that ran, whether it passed, the digest of the
// verification-record/1 it left and the record itself, and the steps that failed.
type VerifyOutcome struct {
	Pass     bool
	Command  string
	Digest   string
	Record   json.RawMessage
	Summary  string
	Failures []string
}

// Verifier verifies a head. The real one runs `crw-dev ci local` (CILocalVerifier).
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (VerifyOutcome, error)
}

// CloseRange is the range of the integration target a milestone close judges: the tip the plan's first node started from and the
// tip now.
type CloseRange struct {
	Base, Tip string
}

// CloseOutcome is what the milestone close answered: the final gate and the review of reviewer-final over the integrated range.
// This issue records the call; the gate and the final review are F13's.
type CloseOutcome struct {
	Wired bool            `json:"wired"`
	Gate  json.RawMessage `json:"gate"`
	Final json.RawMessage `json:"final"`
}

// Closer is the milestone close of F13: the final gate over the integrated range and reviewer-final.
type Closer interface {
	Close(ctx context.Context, ms Milestone, rng CloseRange) (CloseOutcome, error)
}

// Report is the milestone report that goes up to the initiative lead.
type Report struct {
	Plan, Milestone string
	Range           CloseRange
	Close           CloseOutcome
	RequestID       string
}

// Reporter delivers a milestone report (F11): the report row is the run's, the delivery is the reporter's.
type Reporter interface {
	ReportUp(ctx context.Context, r Report) (string, error)
}

// StepRow is a row of flow_step_decisions: the tier decision of a node, the rule that made it with the evidence it rested on, and
// the checkpoint a fresh executor of the next tier starts from.
type StepRow struct {
	StepID, RunID, NodeID, FromTier, ToTier, RuleID string
	Evidence                                        map[string]any
	CheckpointHead, CheckpointTree                  string
	DecidedAt                                       time.Time
}

// Decisions receives the decision rows (SpawnRow is a row of flow_spawn_decisions, native.go). A row is appended once: the same
// id again is the same decision.
type Decisions interface {
	AppendSpawn(ctx context.Context, row SpawnRow) error
	AppendStep(ctx context.Context, row StepRow) error
}
