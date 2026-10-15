package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file and the lane_*.go files are the crw.* operations of the project lane (F08, CRW-1201): the engine.Op behind each
// recording or effect-bearing call of internal/flow/workflows/project-lane.js. An Op takes a position of its path like an agent
// call, writes a completed row of its own (crw.<name>) with the fields the proofs read (proofs.go), and is replayed on a resume
// only while Proven says its effect still holds. Everything an op knows about the world beyond the journal it asks of one of the
// seams below, so the same ops run against the real scheduler, git and CI and against the scenario harness
// (internal/flow/scenario) with a temporary store, a temporary repository and the fake host:
//
//   - Relay is the DAG scheduler of the relay (internal/relay/dagsched): the plan's nodes and their release, the agent
//     acceptance and the integration of R05, the closure of an execution and the return of its slot of R07;
//   - Verifier is the verification of a head (crw-dev ci local, which leaves a verification-record/1);
//   - Decisions is where flow_spawn_decisions and flow_step_decisions rows are appended;
//   - Threads is the host as the milestone close reads and archives the reviewer threads of the nodes (F13, CRW-1206): the audit
//     of each acceptance and the archive of the threads whose audit passed;
//   - Reporter is the seam of the issue that follows (F11: the delivery of the report): the lane records the call and asks the
//     seam, nothing more.
//
// The args of an op are the JSON array of the script's arguments; its result is the JSON the script gets. A CallResult passed
// as an argument arrives as CallRef.

// Op names (the crw.<name> member the engine installs).
const (
	NameMilestone      = "milestone"
	NameDesignAudit    = "designAudit"
	NameDesignLedger   = "designLedger"
	NameRelease        = "release"
	NameIssuePlan      = "issuePlan"
	NameSpawnDecision  = "spawnDecision"
	NameExploreRoute   = "exploreRoute"
	NameExploreRecord  = "exploreRecord"
	NameCommit         = "commit"
	NameVerify         = "verify"
	NameReview         = "review"
	NameStepRecord     = "stepRecord"
	NameAccept         = "accept"
	NameIntegrate      = "integrate"
	NameEscalate       = "escalate"
	NameHold           = "hold"
	NameDecide         = "decide"
	NameMilestoneClose = "milestoneClose"
	NameReportUp       = "reportUp"
	NameFinalGate      = "finalGate"
	NameQAReceipt      = "qaReceipt"
	NameQACapture      = "qaCapture"
	NameReviewerFinal  = "reviewerFinal"
	NameAudit          = "audit"
	NameArchive        = "archive"
)

// The Ops that have no constant in proofs.go.
const (
	OpMilestone      = "crw." + NameMilestone
	OpDesignAudit    = "crw." + NameDesignAudit
	OpDesignLedger   = "crw." + NameDesignLedger
	OpRelease        = "crw." + NameRelease
	OpIssuePlan      = "crw." + NameIssuePlan
	OpSpawnDecision  = "crw." + NameSpawnDecision
	OpExploreRoute   = "crw." + NameExploreRoute
	OpExploreRecord  = "crw." + NameExploreRecord
	OpStepRecord     = "crw." + NameStepRecord
	OpEscalate       = "crw." + NameEscalate
	OpHold           = "crw." + NameHold
	OpReleaseChild   = engine.OpLegacyReleaseChild // an old run's recording call, read only: the lane installs no crw.releaseChild (CRW-1415)
	OpDecide         = "crw." + NameDecide
	OpMilestoneClose = "crw." + NameMilestoneClose
	OpReportUp       = "crw." + NameReportUp
	OpAudit          = "crw." + NameAudit
	OpArchive        = "crw." + NameArchive
	// OpCommitName is OpCommit under the name the agent allocator and the engine use for the rows of crw.commit.
	OpCommitName = OpCommit
)

// The flow rows the lane writes besides its op rows.
const (
	// EvDecisionApplied is the row `crw flow answer` (F12) appends to the run journal when it has applied a decision: the only
	// signal crw.decide resumes on.
	EvDecisionApplied = "decision_applied"
)

// Deps is what the lane operations are built on.
type Deps struct {
	// RunID is the run the operations write for.
	RunID string
	// Repo is the git repository the nodes' worktrees and the integration ref live in.
	Repo string
	// Git runs git; nil runs the git binary.
	Git GitFunc
	// Journal is the run's journal: the operations read the rows of earlier operations and calls from it.
	Journal  engine.Journal
	Relay    Relay
	Verifier Verifier
	// Decisions receives the rows of flow_spawn_decisions and flow_step_decisions; nil keeps them in memory only.
	Decisions Decisions
	// Threads reads and archives the reviewer threads at the milestone close; nil reports every audit as unobserved and archives
	// nothing. Reporter is the seam of F11; nil records the call and answers that nothing is wired.
	Threads  Threads
	Reporter Reporter
	// Now is the clock of the rows' decided_at; nil is time.Now.
	Now func() time.Time
	// Actor is the task that runs the lane (the registered parent of the project).
	Actor string
	// LandRoot is the directory the worktree of a head made outside a run is checked out under (the run's directory, CRW-1354); "" refuses
	// a land release.
	LandRoot string
}

// Lane is the set of operations of the project lane.
type Lane struct {
	d Deps
}

// NewLane returns the operations over the dependencies. A nil Git is the git binary and a nil Decisions an in-memory sink.
func NewLane(d Deps) *Lane {
	if d.Git == nil {
		d.Git = ExecGit("git")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Decisions == nil {
		d.Decisions = &MemoryDecisions{}
	}
	return &Lane{d: d}
}

// Ops returns the operations by the name the engine installs them under (crw.<name>). Config.Ops takes the map as it is.
func (l *Lane) Ops() map[string]engine.Op {
	return map[string]engine.Op{
		NameMilestone:        &milestoneOp{l},
		NameDesignAudit:      &designAuditOp{l},
		NameDesignLedger:     &designLedgerOp{l},
		NameRelease:          &releaseOp{l},
		NameIssuePlan:        &issuePlanOp{l},
		NameSpawnDecision:    &spawnDecisionOp{l},
		NameExploreRoute:     &exploreRouteOp{l},
		NameExploreRecord:    &exploreRecordOp{l},
		NameCommit:           &commitOp{l},
		NameVerify:           &verifyOp{l},
		NameReview:           &reviewOp{l},
		NameStepRecord:       &stepRecordOp{l},
		NameAccept:           &acceptOp{l},
		NameIntegrate:        &integrateOp{l},
		NameEscalate:         &escalateOp{l},
		NameHold:             &holdOp{l},
		NameDecide:           &decideOp{l},
		NameMilestoneClose:   &milestoneCloseOp{l},
		NameReportUp:         &reportUpOp{l},
		NameFinalGate:        &finalGateOp{l},
		NameQAReceipt:        &qaReceiptOp{l},
		NameQACapture:        &qaCaptureOp{l},
		NameReviewerFinal:    &reviewerFinalOp{l},
		NameAudit:            &auditOp{l},
		NameArchive:          &archiveOp{l},
		NameReplanSignals:    &replanSignalsOp{l},
		NameCurrentDesign:    &currentDesignOp{l},
		NameReflection:       &reflectionOp{l},
		NamePlanRead:         &planReadOp{l},
		NameSignalBind:       &signalBindOp{l},
		NameSignalDone:       &signalDoneOp{l},
		NameSignalClose:      &signalCloseOp{l},
		NameDesignHead:       &designHeadOp{l},
		NameReleasedPlans:    &releasedPlansOp{l},
		NameRegionsOverlap:   &regionsOverlapOp{l},
		NameAdmissionRegions: &admissionRegionsOp{l},
	}
}

// CallRef is a CallResult as a script hands it to an operation: the fields the operations read.
type CallRef struct {
	CallID string          `json:"callId"`
	Handle string          `json:"handle"`
	Status string          `json:"status"`
	Reason string          `json:"reason"`
	Output json.RawMessage `json:"output"`
	Meta   struct {
		Role           string `json:"role"`
		ConfiguredPair struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"configuredPair"`
		ThreadID string `json:"threadId"`
	} `json:"meta"`
}

// Brief is the part of a call an operation passes back to the script: enough to name the call and why it ended as it did.
type Brief struct {
	CallID string `json:"callId"`
	Handle string `json:"handle"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Role   string `json:"role"`
}

func (c CallRef) brief() Brief {
	return Brief{CallID: c.CallID, Handle: c.Handle, Status: c.Status, Reason: c.Reason, Role: c.Meta.Role}
}

// args decodes the JSON array of an op's arguments into the values, in order; a missing trailing argument leaves its value
// as it was, and more arguments than values are an error (a script that passes one the op does not know is wrong).
func args(raw json.RawMessage, into ...any) error {
	var list []json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &list); err != nil {
			return fmt.Errorf("the arguments are not a JSON array: %w", err)
		}
	}
	if len(list) > len(into) {
		return fmt.Errorf("takes at most %d arguments, got %d", len(into), len(list))
	}
	for i, v := range list {
		if string(v) == "null" || len(v) == 0 {
			continue
		}
		if err := json.Unmarshal(v, into[i]); err != nil {
			return fmt.Errorf("argument %d: %w", i+1, err)
		}
	}
	return nil
}

// result marshals the value an op answers.
func result(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// digestOf is the sha256 of the canonical JSON of v (maps encode with sorted keys). A milestone's is the digest of its plan, which leaves
// the nodes' end conditions out (planned).
func digestOf(v any) string {
	switch m := v.(type) {
	case Milestone:
		v = m.planned()
	case *Milestone:
		if m != nil {
			v = m.planned()
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprint(v))
	}
	var canon any
	if json.Unmarshal(b, &canon) == nil {
		if c, err := json.Marshal(canon); err == nil {
			b = c
		}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func textDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// provenAlways is the Proven of an op that only reads or only records: its row stays true.
func provenAlways(context.Context, []engine.Event, engine.Event) (bool, string, error) {
	return true, "", nil
}

// short is a prefix of an id for a message.
func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// planned is the milestone as the digest of its plan reads it: the nodes without their end conditions (CRW-1382). The end condition is
// the handoff body's, which the run pins by its digest, not the relay's plan, so it is no change of the plan; and a run that pinned its
// plan before the milestone carried it compares the same digest after.
func (m Milestone) planned() Milestone {
	carries := false
	for _, n := range m.Nodes {
		carries = carries || len(n.EndCondition) > 0
	}
	if !carries {
		return m
	}
	nodes := make([]PlanNode, len(m.Nodes))
	for i, n := range m.Nodes {
		n.EndCondition = nil
		nodes[i] = n
	}
	m.Nodes = nodes
	return m
}
