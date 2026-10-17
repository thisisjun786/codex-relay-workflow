package ops

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// PlanNode is a node of the plan as the relay reads it for the lane: the issue it carries, the nodes it waits for (a dependency
// or an order edge compile put there; a group's members are chained by order edges), the edit regions it declares and the digest
// of its slice of the plan revision. Its Regions are the issue's planned regions as the handoff spells them (compile.Region.String,
// which may hold "#" or an unclean path); the release declares each by its normal form, read from the plan's fields (CRW-1411).
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
	// EndCondition are the commands of the done condition of the node's issue, read from the body the handoff carries and never run by
	// the relay (CRW-1382): the design-stage explorer runs them once. Absent when the handoff carries no body or the body names none.
	EndCondition []string `json:"end_condition,omitempty"`
	// PlannedRegions are the node's regions with what it plans of each (CRW-1461): the change (rename or delete of a symbol) and the
	// exclusive hold the lane admits it by. Set only when some region renames or deletes or is held exclusive, and then the whole
	// planned list, in the order of Regions and with each Text the same as its Regions entry; absent when every region is a plain edit.
	PlannedRegions []LaneRegion `json:"planned_regions,omitempty"`
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
	// Surfaces are the surfaces of the milestone's criteria (logic, web, tui, desktop); web, tui or desktop makes a QA receipt part
	// of the final gate. Grade is the work class of the milestone (C2, C3, ...): from C3 the QA evidence is judged by the two
	// oracle passes. RequiredCommands is the plan's required set of verification commands (empty: CILocalCommand).
	Surfaces         []string `json:"surfaces"`
	Grade            string   `json:"grade"`
	RequiredCommands []string `json:"required_commands"`
}

// ReleaseRequest releases an issue node to an executor of the run: the node is admitted by the scheduler (the regions it edits
// do not overlap a running node's, a slot is free, the host has the memory) and its slot reserved.
type ReleaseRequest struct {
	Plan, Node, RunID string
	// Revision and DesignDigest are the design revision the release is made under; the lane's packets are built from it.
	Revision, DesignDigest string
	// SliceDigest is the node's slice of the plan the flow compared (CRW-1288) and PlanDigest the digest of that plan: the relay
	// compares SliceDigest with the node's slice in the transaction that reserves the slot and records the release, and a slice that
	// is another releases nothing (Reason plan_changed). An empty SliceDigest compares nothing.
	SliceDigest, PlanDigest string
	// RequestID makes the release idempotent: the same request is the same release.
	RequestID string
	// AuthorModel and AuthorEffort are the pair that wrote the node's work when it exists as the node is released: a head made outside a
	// run, whose author threads the run read back (CRW-1354). The relay records it on the node's execution, and its acceptance judges the
	// reviewer against it. Empty for a release whose work the run's executors will write.
	AuthorModel, AuthorEffort string
	// Land marks the release of a head made outside a run (CRW-1354): a node whose current execution another run holds is answered
	// Released false with ReasonLandNodeHeld and that run and execution (HeldRun, HeldExecution) before the run claims the plan's
	// coordinator epoch, so the land takes nothing from the run that holds the node.
	Land bool
	// Regions are the node's admission regions (CRW-1391): the plan's regions together with the DESIGN's edit_regions (AdmissionRegions),
	// empty when both sides name none. The relay declares them for the node (dag_node_regions) before it releases it when the node has
	// no declaration in force, so two nodes the flow admits together are judged by the regions they edit. A node with no admission
	// regions and no declaration in force is undeclared: its edit overlap is unknown and does not block the release, which warns of it
	// (CRW-1576). A declaration that fails is a warning of the release (RegionsWarning), and the release goes on.
	Regions []string
	// Admitted are the admission regions of the milestone's other nodes, by node (CRW-1391): the relay's release judgment reads every
	// ready node ranked before this one as a holder of its declaration, so the relay declares those of them that have no declaration in
	// force too, before the release, and a real clash is judged on the declarations of both sides. A declaration of another node that
	// fails is that node's own release's to warn of.
	Admitted map[string][]string
}

// ReleaseResult is the scheduler's answer to a release. A node it holds back (Released false) names why in Reason
// (capacity_exhausted, a region overlap, a host short of memory) and is not started.
type ReleaseResult struct {
	Released    bool   `json:"released"`
	Reason      string `json:"reason"`
	ExecutionID string `json:"execution_id"`
	SlotID      string `json:"slot_id"`
	Replayed    bool   `json:"replayed"`
	// SliceDigest is the node's slice of the plan the relay committed the release under (the frozen manifest's); a replayed
	// release names the slice of the release it replays, which the relay answers only to a request that expects that slice.
	SliceDigest string `json:"slice_digest"`
	// PinnedRevision and HeadRevision are, for a release refused plan_changed, the plan revision the run pinned and the plan's head
	// revision at the relay; HeldExecution and HeldRun name the node's current execution the relay still holds and the run it was
	// released to ("" when there is none), which the run-level hold returns or records (CRW-1291).
	PinnedRevision int64  `json:"pinned_revision,omitempty"`
	HeadRevision   int64  `json:"head_revision,omitempty"`
	HeldExecution  string `json:"held_execution,omitempty"`
	HeldRun        string `json:"held_run,omitempty"`
	// HeldRunState is, for a land answered ReasonLandNodeHeld, the state of the run that holds the node as the run's records read
	// (running, dead, held, completed, failed, ...; "" when it is not read): a run that has ended is not resumed, and its execution is
	// given up with crw flow cancel of that run (CRW-1397). A replay of the release of another run (Run) carries that run's state too
	// (CRW-1404).
	HeldRunState string `json:"held_run_state,omitempty"`
	// Run is the run the answered execution was released to: the run asking for a new release, or, for a replay, the run whose release
	// it replays ("" when the relay does not say). A land of a node another run holds reads it (CRW-1354).
	Run string `json:"run,omitempty"`
	// RegionsDeclared says what became of the request's admission regions (CRW-1391): declared (this release declared them), kept (the
	// node had a declaration in force, which stands), unknown (the request named none and the node has none in force: RegionsWarning
	// says the edit overlap is unknown and does not block release, CRW-1576), failed (the declaration was refused or could not be made:
	// RegionsWarning says why, and the release went on) or "" (the relay did not say).
	RegionsDeclared string `json:"regions_declared,omitempty"`
	RegionsWarning  string `json:"regions_warning,omitempty"`
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
// blocked_scope, size_split), cancelled (flow_cancel) or left by a release the run never recorded when its plan was superseded
// (plan_superseded, CRW-1291) has its slot returned in the transaction that closes it; a recovery hold (dispatch_unknown,
// host_failure) closes nothing.
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

// ReasonEpochLost is the closed reason of a fenced write the run made after another session claimed the coordinator epoch of its plan
// (CRW-1285): the run ends at that write with the reason, and writes nothing more at the relay.
const ReasonEpochLost = "epoch_lost"

// ErrEpochLost is the error kind of that write, as a Relay answers it. Its message opens with refused:epoch_lost:, so the script reads
// the reason from the error of the op as it reads a refusal (crw:lib/lane), and errors.Is finds the kind through every wrap.
var ErrEpochLost = errors.New("refused:" + ReasonEpochLost + ": the run lost the coordinator epoch of its plan")

// ErrCloseConflict is the error kind of a close the relay refuses because the node's execution has to stay (CRW-1397): its accepted head
// landed or may be in the target (a disposition_conflict of AgentClose). A Relay wraps that refusal in it; every other close error is
// the relay's own, which a gate-only land asks again (a transient error).
var ErrCloseConflict = errors.New("the relay keeps the node's execution")

// MilestoneRepairRequest is the publication of a close's in-run repair (CRW-1490): the candidate Head of milestone Milestone of plan
// Plan, which must descend from and replace the failed gate's head Base. VerificationDigest is the digest of a fresh passing test
// verification of Head that the recorded gate carries - the relay records the move with it so the official push path reads the
// repaired close as a verified move.
type MilestoneRepairRequest struct {
	Plan, Milestone, Base, Head string
	VerificationDigest          string
	Verification                json.RawMessage `json:"verification,omitempty"`
}

// MilestoneRepairResult is the answer of IntegrateMilestoneRepair: Tip the integration target's tip after the call, and Moved true
// only when the target changed to a different head than Head (the repair was not published). Tip==Head with Moved false means the
// candidate stands, whether this call published it or it already was the tip.
type MilestoneRepairResult struct {
	Tip   string
	Moved bool
}

// MilestoneRepairIntegrator is the optional relay seam that publishes a close's repair: a fenced compare-and-swap of the
// integration ref from Base to Head, which the relay's IntegrationWrite serializes. A Relay that does not implement it makes a
// close with a repair refuse close_repair_unavailable.
type MilestoneRepairIntegrator interface {
	IntegrateMilestoneRepair(context.Context, MilestoneRepairRequest) (MilestoneRepairResult, error)
}

// Relay is the DAG scheduler of the relay as the lane uses it.
type Relay interface {
	// Milestone reads a milestone of a plan: its nodes in the order the scheduler offers them.
	Milestone(ctx context.Context, plan, milestone string) (Milestone, error)
	// Release admits a node to an executor of the run (dag-release --surface agent) and reserves its slot.
	Release(ctx context.Context, req ReleaseRequest) (ReleaseResult, error)
	// ReleaseStands reports whether a release is still the node's current one: true while its execution is open, and true for a node
	// that landed (integration ends the execution without taking the release back); false for an execution that was closed
	// (AgentClose) or released again. The lane does not rest on it for an execution its own journal shows it ended - a node it gave
	// up or landed - so a relay may answer false for a closed execution without the lane building the node again on a resume.
	ReleaseStands(ctx context.Context, plan, node, execution string) (bool, error)
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
	// Command is the verification command asked for (the final gate asks for each command of the plan's required set); empty is
	// the verifier's own command.
	Command string
}

// VerifyOutcome is the verification of a head: the command that ran, whether it passed, the digest of the
// verification-record/1 it left and the record itself, and the steps that failed with the tests they name.
type VerifyOutcome struct {
	Pass     bool
	Command  string
	Digest   string
	Record   json.RawMessage
	Summary  string
	Failures []string
	// Note is what the verifier says about how it ran, such as tools taken from PATH because nothing configured them (CRW-1319);
	// the crw.verify row records it as verifier_note.
	Note string
	// TestPackagesRun is the count of go test packages the verification ran tests in, as its record says (testPackagesRun of
	// verification-record/1, CRW-1328); nil is a record that does not say, which is unknown, never zero. Zero is the warning
	// no_tests_run of the crw.verify row and the final gate; Pass is the record's either way.
	TestPackagesRun *int
	// FailedTests are the failed steps of the record that give a reason, each with the tests and the packages its reason names and
	// the last lines of it (failedSteps, CRW-1469); none for a verification that passed. The crw.verify row and result carry them as
	// failed_tests.
	FailedTests []FailedStep
}

// WarnNoTestsRun is the warning of a verification whose record says it ran no test (CRW-1328): recorded, never a failure.
const WarnNoTestsRun = "no_tests_run"

// Verifier verifies a head. The real one runs `crw-dev ci local` (CILocalVerifier).
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (VerifyOutcome, error)
}

// CloseRange is the range of the integration target a milestone close judges: the tip the plan's first node started from and the
// tip now.
type CloseRange struct {
	Base, Tip string
}

// CloseOutcome is what the milestone close judged: the final gate over the integrated head, the review of reviewer-final that took
// the gate as its input (a close made before the crosscheck), the audit of the nodes' reviews that ran before the reviewer threads
// were archived, the crosscheck's verdict V (CRW-1473) and the alignment the reflection of the close answered.
type CloseOutcome struct {
	Gate       json.RawMessage `json:"gate"`
	Final      json.RawMessage `json:"final"`
	Audit      json.RawMessage `json:"audit"`
	Crosscheck json.RawMessage `json:"crosscheck"`
	Reflection string          `json:"reflection"`
}

// Threads is the host as the close reads and archives the reviewer threads of the nodes (CRW-1206): thread/turns/list and
// thread/items/list for the audit, thread/archive once an acceptance has its audit pass. A lane without one reports every audit
// as unobserved and archives nothing.
type Threads interface {
	// ReadTurns reads the turns of a thread by their ids, in full.
	ReadTurns(ctx context.Context, threadID string, turnIDs []string) ([]appserver.TurnReading, error)
	// ArchiveThread archives a thread.
	ArchiveThread(ctx context.Context, threadID string) error
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

// PullRequests is the part of the tracking repository's pull requests crw.mergeRequest reads: a pull request and the checks of its head
// (the integrator.PullRequestSource pattern: a local subset interface, which *tracking.GitHub satisfies).
type PullRequests interface {
	PullRequest(ctx context.Context, ref tracking.Ref) (tracking.PullRequest, error)
	Checks(ctx context.Context, ref tracking.Ref) (tracking.Checks, error)
}

// MergeCrosscheck is the crosscheck verdict at the pull request's head that a merge request states.
type MergeCrosscheck struct {
	Verdict, Head, EngineFamily, Model string
	Round                              int
}

// MergeRequest is the request to the parent to merge the pull request of a milestone: what the lane judged and may state, and nothing
// beyond the pull request ref and the SHAs (no path, no host, no id).
type MergeRequest struct {
	Plan, Milestone string
	// PR is the pull request as "OWNER/NAME#N"; Head and Base the commit it carries and the branch it goes to.
	PR, Head, Base string
	// Checks is the state of the head's checks (success: the only state that is requested).
	Checks     string
	Crosscheck MergeCrosscheck
	// Unresolved is the count from the PR head's persisted review dispositions; BotsNotConnected says the review bots are not connected,
	// so there was nothing to resolve.
	Unresolved       int
	BotsNotConnected bool
	// RequestID makes the delivery idempotent per head.
	RequestID string
}

// MergeRequester delivers a merge request. The Reporter that implements it is the one crw.mergeRequest asks (the Reporter interface
// itself does not change).
type MergeRequester interface {
	MergeRequest(ctx context.Context, r MergeRequest) (string, error)
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

// ---- the bundle pull request seams (CRW-1423) ----

// BundlePRTarget is where the pull request of a PR-unit milestone goes: the tracking repository, the checkout the head
// branch lives in, the remote and the persisted review record file. BaseRef "" says the plan is a direct landing: no pull
// request is opened.
type BundlePRTarget struct {
	Repo, Checkout, HeadRef, BaseRef, Remote, ReviewRecord string
}

// BundlePROpenRequest is what crw.prOpen hands the parent's pull request opener: the target the plan's handoff pinned, the
// milestone's own journal row and the close the journal proved (no caller-supplied acceptance).
type BundlePROpenRequest struct {
	Target    BundlePRTarget
	Milestone Milestone
	Close     CloseResultOutcome
}

// BundlePRResult is the parent's answer: not_required for a direct landing, opened when the pull request (fresh or already
// there) may go to the merge request, refused with a reason otherwise.
type BundlePRResult struct {
	State string `json:"state"` // not_required, opened, refused
	PR    string `json:"pr,omitempty"`
	Head  string `json:"head,omitempty"`
	Base  string `json:"base,omitempty"`
	// ReviewRecord is the crw-pr-review/1 record of the pull request mergeRequest must read.
	ReviewRecord string `json:"review_record,omitempty"`
	Reason       string `json:"reason,omitempty"`
}
