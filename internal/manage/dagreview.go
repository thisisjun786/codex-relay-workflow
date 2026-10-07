package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// dagReviewUnmeasured is the state of a reading the store cannot support: the table the check
// reads is absent, so the review says so rather than repairing the store.
const dagReviewUnmeasured = "unmeasured"

// dagReviewStallDefaultMinutes is how long a lane or a landing may stand still before it is an
// anomaly; the dag_review settings section overrides it.
const dagReviewStallDefaultMinutes = 15

// The six anomaly kinds this issue reads from the relay store.
const (
	dagReviewKindReleasedBeforePredecessor = "released_before_predecessor"
	dagReviewKindReleasedRepeatedly        = "released_repeatedly"
	dagReviewKindExclusiveOverlapRunning   = "exclusive_overlap_running"
	dagReviewKindLandedNotObserved         = "landed_not_observed"
	dagReviewKindChildWithoutRelease       = "child_without_release"
	dagReviewKindLaneTurnStalled           = "lane_turn_stalled"
)

// The execution kinds that are a re-execution of a node. A correction generation leaves a
// dag_node_executions row of kind correction and no dag_releases row, so the scheduler's own
// execution links are what tell a node that ran twice from one that ran once. A parent_handover
// (dag-adopt binding a replacement parent to the same generation) is not a re-execution.
const (
	dagReviewExecutionInitial    = "initial"
	dagReviewExecutionCorrection = "correction"
)

// dagReviewAnomalyExit is the status of a review that found something; a review that found
// nothing exits 0 and one that could not read the store exits dagReviewStoreExit.
const (
	dagReviewAnomalyExit = 1
	dagReviewStoreExit   = 3
)

// DagReviewAnomaly is one thing a review found. Node and Issue are empty when the anomaly is about a
// lane rather than a node, and Detail carries the numbers a reader needs to act.
type DagReviewAnomaly struct {
	Kind   string `json:"kind"`
	Plan   string `json:"plan"`
	Node   string `json:"node"`
	Issue  string `json:"issue"`
	Detail string `json:"detail"`
}

// Check is one reading the review could not take. State is measured or unmeasured; an
// unmeasured check means the store predates the table the reading needs.
type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// DagReviewPlan is one reviewed plan's shape, as the review counts it.
type DagReviewPlan struct {
	Plan       string `json:"plan"`
	Revision   int    `json:"revision"`
	Nodes      int    `json:"nodes"`
	Edges      int    `json:"edges"`
	Released   int    `json:"released"`
	Accepted   int    `json:"accepted"`
	Integrated int    `json:"integrated"`
}

// Review is what one dag-review run reports: the plans it read, the anomalies it found and the
// checks it could not take.
type Review struct {
	Plans     []DagReviewPlan    `json:"plans"`
	Anomalies []DagReviewAnomaly `json:"anomalies"`
	Checks    []Check            `json:"checks"`
}

// dagReviewSection is the dag_review settings section: which plans to review and how long a
// lane may stand still. An empty plan list means every plan the store carries.
type dagReviewSection struct {
	Plans        []string `json:"plans"`
	StallMinutes int      `json:"stall_minutes"`
}

// dagReviewSource reads one group of anomalies from one place. A source appends to in.review. The
// two sources a review runs are called from the two places their reads belong: the store source
// inside the store's snapshot, and the host-record source outside it.
type dagReviewSource func(ctx context.Context, in *dagReviewInput) error

// dagReviewInput is what a source reads: the open store, the plan facts, the lanes, the clock
// and the review being built.
type dagReviewInput struct {
	store  *dagReviewStore
	now    time.Time
	stall  time.Duration
	facts  []dagReviewFacts
	lanes  []dagReviewLaneTurn
	review *Review
	// edgeReadingReported is whether the one reading this review cannot take was already named.
	// It is a property of the scheduler's exported surface rather than of a plan, so it is
	// recorded once for the review rather than once per plan.
	edgeReadingReported bool
}

// DagReview is crw manage dag-review: it reads the relay store read-only and reports the DAG
// anomalies it finds. It is exported because a later issue calls the same review with more
// sources. It never writes to the store; the one host state it writes is the rollout offset file
// below the configured state directory, and only when the caller did not ask for --no-state.
func DagReview(ctx context.Context, e *Env, cfg *Config) (Review, error) {
	// The host-record source reads the App Server and the parents' rollouts, which the source
	// signature does not carry; the scope travels on the context it runs with.
	ctx = dagHostBind(ctx, cfg)
	review := Review{Plans: []DagReviewPlan{}, Anomalies: []DagReviewAnomaly{}, Checks: []Check{}}
	section := dagReviewSection{}
	if err := cfg.Section("dag_review", &section); err != nil {
		return review, fmt.Errorf("the dag_review section: %w", err)
	}
	stall := dagReviewStallDefaultMinutes
	if section.StallMinutes > 0 {
		stall = section.StallMinutes
	}
	state, err := e.relayHelperState(ctx, cfg)
	if err != nil {
		return review, err
	}
	handle, err := dagReviewOpenStore(ctx, state)
	if err != nil {
		return review, err
	}
	defer handle.Close()
	in := &dagReviewInput{store: handle, now: e.Now(), stall: time.Duration(stall) * time.Minute, review: &review}
	// One open, one snapshot: the plans, the scheduler's own reading of each plan beside this
	// review's list queries, the merge lanes and the six checks all run inside the same deferred
	// snapshot, so no reading of the review sees a different store state from another. The open is
	// the relay-read helper's (CRW-836), so the review creates no write-ahead log or index either.
	if err := handle.dagReviewSnapshot(ctx, state, func(ctx context.Context, st *store.Store) error {
		plans, err := handle.dagReviewReadPlans(ctx, section.Plans)
		if err != nil {
			return fmt.Errorf("read the plans: %w", err)
		}
		if err := dagReviewCheckConfigured(section.Plans, plans); err != nil {
			return err
		}
		for _, plan := range plans {
			facts, err := handle.dagReviewReadFacts(ctx, st, plan, &review.Checks)
			if err != nil {
				return fmt.Errorf("read plan %s: %w", plan.planID, err)
			}
			in.facts = append(in.facts, facts)
		}
		if in.lanes, err = handle.dagReviewReadLanes(ctx, section.Plans); err != nil {
			return fmt.Errorf("read the merge lanes: %w", err)
		}
		// The store readings run inside the snapshot; the host readings do not, because they
		// touch no SQLite state and a network call should not hold the read snapshot open.
		return dagReviewStoreSources(ctx, in)
	}); err != nil {
		return review, err
	}
	if err := dagHostSources(ctx, in); err != nil {
		return review, err
	}
	sort.SliceStable(review.Anomalies, func(i, j int) bool {
		if review.Anomalies[i].Kind != review.Anomalies[j].Kind {
			return review.Anomalies[i].Kind < review.Anomalies[j].Kind
		}
		if review.Anomalies[i].Plan != review.Anomalies[j].Plan {
			return review.Anomalies[i].Plan < review.Anomalies[j].Plan
		}
		return review.Anomalies[i].Node < review.Anomalies[j].Node
	})
	return review, nil
}

// dagReviewCheckConfigured refuses a configured plan id the store does not carry. A scoped
// review that silently reviewed nothing would read as a clean review of a plan that does not
// exist, which is the opposite of what the setting asks for.
func dagReviewCheckConfigured(wanted []string, plans []dagReviewPlanRef) error {
	if len(wanted) == 0 {
		return nil
	}
	found := map[string]bool{}
	for _, plan := range plans {
		found[plan.planID] = true
	}
	for _, id := range wanted {
		if !found[id] {
			return fmt.Errorf("the configured plan %q is not in the store", id)
		}
	}
	return nil
}

// dagReviewStoreSources runs the six store readings of this issue.
func dagReviewStoreSources(ctx context.Context, in *dagReviewInput) error {
	for _, facts := range in.facts {
		dagReviewPlanShape(in, facts)
		dagReviewReleasedBeforePredecessor(in)
		dagReviewReleasedRepeatedly(in, facts)
		dagReviewExclusiveOverlapRunning(in, facts)
		dagReviewLandedNotObserved(in, facts)
		if err := dagReviewChildWithoutRelease(ctx, in, facts); err != nil {
			return err
		}
	}
	return dagReviewLaneTurnStalled(in)
}

// dagReviewPlanShape records what the review counted for one plan. Every count is the scheduler's
// own: the denominator, the accepted and integrated measures and the stages come from Progress.
// Only the edge count is this review's, and an edge is counted rather than judged.
func dagReviewPlanShape(in *dagReviewInput, facts dagReviewFacts) {
	progress := facts.progress
	released := 0
	for _, node := range progress.Nodes {
		if dagReviewNodeReleased(node) {
			released++
		}
	}
	in.review.Plans = append(in.review.Plans, DagReviewPlan{
		Plan: progress.Reading.PlanID, Revision: int(progress.Denominator.Revision),
		Nodes: progress.Denominator.Nodes, Edges: len(facts.edges),
		Released: released, Accepted: progress.Cumulative.Accepted.Nodes, Integrated: progress.Cumulative.Integrated.Nodes,
	})
}

// dagReviewNodeReleased is whether the plan has released the node. Two of Progress's stages are
// reached only by a node with a release or an execution behind it; the rest are the plan's
// lifecycle overlays, which a node the plan holds before any release carries too, so those are
// released only when the reading shows a release: a relationship, an execution, the managed start
// of a release whose child is not bound yet, or a held slot.
func dagReviewNodeReleased(node dagsched.NodeProgress) bool {
	switch node.Stage {
	case dagsched.StageWaitingPredecessor, dagsched.StageWaitingDecision, dagsched.StageWaitingResource, dagsched.StageReady:
		return false
	case dagsched.StageCancelled, dagsched.StageArchived, dagsched.StagePaused:
		return dagReviewNodeOwned(node)
	}
	return true
}

// dagReviewNodeOwned is whether the plan's own reading shows the node was ever released. A node
// the plan holds (paused, cancelled or archived by a revision) before any release has none of
// these links, and its stage alone cannot tell it from one whose relationship was paused after a
// release, so the release evidence is what decides.
func dagReviewNodeOwned(node dagsched.NodeProgress) bool {
	return len(node.Links.Executions) > 0 || node.Links.Relationship != nil || node.Links.Managed != nil || node.HoldsSlot
}

// dagReviewIssueByNode maps every live node id of the plan to its issue, as the scheduler's own
// reading of the plan spells it.
func dagReviewIssueByNode(facts dagReviewFacts) map[string]string {
	issue := map[string]string{}
	for _, node := range facts.progress.Nodes {
		issue[node.NodeID] = node.IssueKey
	}
	return issue
}

// dagReviewReleasedBeforePredecessor records that this review cannot take the reading. When an
// edge became satisfied is known only to the scheduler's unexported edgeStatus and integratedAt,
// and it exports no reading of that time yet, so the review names the gap as an unmeasured check
// and reports no anomaly rather than judging edge satisfaction with a second implementation of
// the scheduler's rule. internal/relay is not edited for it: the export is a follow-up elsewhere.
func dagReviewReleasedBeforePredecessor(in *dagReviewInput) {
	if in.edgeReadingReported {
		return
	}
	in.edgeReadingReported = true
	in.review.Checks = append(in.review.Checks, Check{
		Name: dagReviewKindReleasedBeforePredecessor, State: dagReviewUnmeasured,
		Detail: "the scheduler exports no edge reading with the time it became satisfied",
	})
}

// dagReviewReleasedRepeatedly reports a node the relay executed more than once. The executions are
// the scheduler's own (Progress node links): a correction generation leaves a row in
// dag_node_executions and no dag_releases row, so counting releases would read a node that ran
// twice as run once. The detail carries how many times, because a correction and a redefinition
// are not the same thing.
func dagReviewReleasedRepeatedly(in *dagReviewInput, facts dagReviewFacts) {
	issue := dagReviewIssueByNode(facts)
	for _, node := range facts.progress.Nodes {
		initial, correction := 0, 0
		for _, execution := range node.Links.Executions {
			switch execution.Kind {
			case dagReviewExecutionInitial:
				initial++
			case dagReviewExecutionCorrection:
				correction++
			}
		}
		if initial+correction < 2 {
			continue
		}
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagReviewKindReleasedRepeatedly, Plan: facts.plan.planID, Node: node.NodeID, Issue: issue[node.NodeID],
			Detail: fmt.Sprintf("executed %d times (initial %d, correction %d)", initial+correction, initial, correction),
		})
	}
}

// dagReviewExclusiveOverlapRunning reports two nodes in flight at once whose declared regions the
// scheduler grades exclusive. The in-flight set is the scheduler's own stage of each node: a node
// holds its edit regions from its release until its accepted head lands, which is exactly the span
// between the releasing and integrated stages, and a release ended by dag-release-close owns
// nothing (the scheduler reads such a node planned, then ready). A node whose stage is ambiguous
// cannot be placed, so it is named as an unmeasured check instead of being reported. Local and
// mechanical overlaps are released on purpose and settled at merge time, so they are not anomalies.
func dagReviewExclusiveOverlapRunning(in *dagReviewInput, facts dagReviewFacts) {
	issue := dagReviewIssueByNode(facts)
	var inflight []string
	for _, node := range facts.progress.Nodes {
		if node.Stage == dagsched.StageAmbiguous {
			in.review.Checks = append(in.review.Checks, Check{
				Name: dagReviewKindExclusiveOverlapRunning, State: dagReviewUnmeasured,
				Detail: fmt.Sprintf("node %s is in the ambiguous stage, so whether it holds its regions cannot be read", node.NodeID),
			})
			continue
		}
		if dagReviewNodeInFlight(node) {
			inflight = append(inflight, node.NodeID)
		}
	}
	sort.Strings(inflight)
	byNode := map[string][]dagReviewRegion{}
	for _, region := range facts.regions {
		byNode[region.nodeID] = append(byNode[region.nodeID], region)
	}
	for i, left := range inflight {
		for _, right := range inflight[i+1:] {
			if detail, found := dagReviewExclusivePair(byNode[left], byNode[right], issue[left], issue[right]); found {
				in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
					Kind: dagReviewKindExclusiveOverlapRunning, Plan: facts.plan.planID, Node: left,
					Issue: issue[left], Detail: detail,
				})
			}
		}
	}
}

// dagReviewNodeInFlight is whether the node holds its edit regions now: the plan released it and
// its accepted head has not landed. The stage decides for a node with an execution behind it; the
// paused stage is shared with a node the plan holds before any release, so there the node's own
// release evidence decides, and a node nobody released holds nothing.
func dagReviewNodeInFlight(node dagsched.NodeProgress) bool {
	switch node.Stage {
	case dagsched.StageReleasing, dagsched.StageCreationUnknown, dagsched.StageRunning, dagsched.StageReported,
		dagsched.StageVerifying, dagsched.StageCorrecting, dagsched.StageAccepted, dagsched.StageStale:
		return true
	case dagsched.StagePaused:
		return dagReviewNodeOwned(node)
	}
	return false
}

// dagReviewExclusivePair grades every pair of the two nodes' regions with the scheduler's own
// rule and returns the first exclusive one, so this review cannot disagree with the scheduler.
func dagReviewExclusivePair(left, right []dagReviewRegion, leftIssue, rightIssue string) (string, bool) {
	for _, a := range left {
		for _, b := range right {
			if dagsched.PairGrade(dagReviewRegionOf(a), dagReviewRegionOf(b)) == dagsched.GradeExclusive {
				return fmt.Sprintf("%s and %s overlap exclusively at %s", leftIssue, rightIssue, a.path), true
			}
		}
	}
	return "", false
}

// dagReviewRegionOf is the scheduler's own region type, so the grade is decided by the same
// function the scheduler uses rather than by a second implementation of the rules.
func dagReviewRegionOf(region dagReviewRegion) dagsched.Region {
	return dagsched.Region{
		Repository: region.repository, Path: region.path, Kind: region.kind, Key: region.key,
		Change: region.change, Exclusive: region.exclusive, Grade: region.grade, Rule: region.rule,
	}
}

// dagReviewLandedNotObserved reports a lane that landed an acceptance whose node was never observed
// integrated in the target that lane landed on, once the landing has stood still past the
// threshold. The turn names the node through its relationship, which is the link the relay itself
// keeps, and the observation is looked for in the turn's own repository and base ref: an
// observation of another target does not resolve this landing, because a landing is observed in the
// branch it landed on. Only whether a landing was observed at all is read here; whether the node is
// integrated is the scheduler's reading (Progress), and this check does not judge it again.
func dagReviewLandedNotObserved(in *dagReviewInput, facts dagReviewFacts) {
	issue := dagReviewIssueByNode(facts)
	landed := map[string]string{}
	for _, turn := range in.lanes {
		if turn.state != "landed" || turn.relationshipID == "" {
			continue
		}
		instant, ok := dagReviewInstant(turn.closedAt)
		if !ok {
			instant, ok = dagReviewInstant(turn.updatedAt)
		}
		if !ok || in.now.Sub(instant) <= in.stall {
			continue
		}
		target := turn.repository + "#" + turn.baseRef
		for _, acceptance := range facts.acceptances {
			if acceptance.relationshipID != turn.relationshipID {
				continue
			}
			if dagReviewObservedIn(facts, acceptance.acceptanceID, turn.repository, turn.baseRef, instant) {
				continue
			}
			landed[acceptance.nodeID] = target
		}
	}
	for _, node := range dagReviewSortedKeys(landed) {
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagReviewKindLandedNotObserved, Plan: facts.plan.planID, Node: node, Issue: issue[node],
			Detail: fmt.Sprintf("the lane landed and no integration was observed in %s within %s", landed[node], in.stall),
		})
	}
}

// dagReviewObservedIn is whether an integration observation row exists for the acceptance in
// exactly the turn's target, recorded no earlier than the turn's close. It reads the row's
// existence alone: what the observation says about containment, and whether the node is
// integrated, are the scheduler's readings.
func dagReviewObservedIn(facts dagReviewFacts, acceptanceID, repository, baseRef string, closed time.Time) bool {
	for _, observation := range facts.observations {
		if observation.acceptanceID != acceptanceID || observation.repository != repository || observation.baseRef != baseRef {
			continue
		}
		instant, ok := dagReviewInstant(observation.observedAt)
		if !ok {
			continue
		}
		if !instant.Before(closed) {
			return true
		}
	}
	return false
}

// dagReviewChildWithoutRelease reports a relationship of the plan's project, opened after the
// plan, with no DAG execution record: the plan did not release that child.
func dagReviewChildWithoutRelease(ctx context.Context, in *dagReviewInput, facts dagReviewFacts) error {
	for _, child := range facts.children {
		executed, err := in.store.dagReviewExecutionExists(ctx, facts.plan.planID, child.relationshipID)
		if err != nil {
			return err
		}
		if executed {
			continue
		}
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagReviewKindChildWithoutRelease, Plan: facts.plan.planID, Issue: child.issueKey,
			Detail: "the relationship has no DAG execution record",
		})
	}
	return nil
}

// dagReviewLaneTurnStalled reports a lane turn that holds or is merging with no update past the
// threshold; the detail carries the PR, the holder and how many turns are waiting behind it.
func dagReviewLaneTurnStalled(in *dagReviewInput) error {
	for _, turn := range in.lanes {
		if turn.state != "holding" && turn.state != "merging" {
			continue
		}
		instant, ok := dagReviewInstant(turn.updatedAt)
		if !ok {
			instant, ok = dagReviewInstant(turn.heldAt)
		}
		if !ok || in.now.Sub(instant) <= in.stall {
			continue
		}
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagReviewKindLaneTurnStalled, Node: turn.turnID,
			Detail: fmt.Sprintf("PR #%d held by %s in state %s for %s, %d waiting",
				turn.prNumber, turn.holder, turn.state, in.now.Sub(instant).Round(time.Minute), turn.waiters),
		})
	}
	return nil
}

// dagReviewSortedKeys is a map's keys in a stable order, so a report does not move between runs.
func dagReviewSortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// dagReviewCommand is crw manage dag-review.
var dagReviewCommand = Command{Name: "dag-review", Summary: "report DAG anomalies read from the relay store", Run: dagReviewRun}

func init() { Register(dagReviewCommand) }

// dagReviewUsage is the one line the command prints.
const dagReviewUsage = "usage: crw manage dag-review [-h] [--anomalies-only] [--text] [--no-state]"

// dagReviewRun is crw manage dag-review. The store is read read-only, so a review is always
// safe to run beside a live relay. Exit 1 means the review found something, 0 that it did not,
// 3 that the store could not be read at all.
func dagReviewRun(ctx context.Context, e *Env, args []string) int {
	anomaliesOnly, text, noState := false, false, false
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			fmt.Fprintln(e.Stdout, dagReviewUsage)
			return 0
		case "--anomalies-only":
			anomaliesOnly = true
		case "--text":
			text = true
		case "--no-state":
			noState = true
		default:
			fmt.Fprintln(e.Stderr, dagReviewUsage)
			fmt.Fprintf(e.Stderr, "crw manage dag-review: error: unexpected argument %q\n", arg)
			return usageExit
		}
	}
	// --no-state reaches the host-record source on the context, so the review's own call keeps the
	// signature a later issue calls it through.
	ctx = dagHostNoState(ctx, noState)
	review, err := DagReview(ctx, e, coreDefaults(e))
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage dag-review: error: %v\n", err)
		return dagReviewStoreExit
	}
	if text {
		for _, line := range dagReviewText(review, anomaliesOnly) {
			fmt.Fprintln(e.Stdout, line)
		}
	} else {
		// --anomalies-only carries the anomalies and nothing else, so a caller can read the
		// findings without the plan shape and the checks around them.
		document := any(review)
		if anomaliesOnly {
			document = struct {
				Anomalies []DagReviewAnomaly `json:"anomalies"`
			}{Anomalies: review.Anomalies}
		}
		data, err := json.Marshal(document)
		if err != nil {
			fmt.Fprintf(e.Stderr, "crw manage dag-review: error: %v\n", err)
			return dagReviewStoreExit
		}
		fmt.Fprintf(e.Stdout, "%s\n", data)
	}
	if len(review.Anomalies) > 0 {
		return dagReviewAnomalyExit
	}
	return 0
}

// dagReviewText renders the review as one English line per record.
func dagReviewText(review Review, anomaliesOnly bool) []string {
	var lines []string
	if !anomaliesOnly {
		for _, plan := range review.Plans {
			lines = append(lines, fmt.Sprintf("plan %s revision %d: %d nodes, %d edges, %d released, %d accepted, %d integrated",
				plan.Plan, plan.Revision, plan.Nodes, plan.Edges, plan.Released, plan.Accepted, plan.Integrated))
		}
	}
	if len(review.Anomalies) == 0 {
		lines = append(lines, "anomalies: none")
	} else {
		lines = append(lines, fmt.Sprintf("anomalies: %d", len(review.Anomalies)))
		for _, anomaly := range review.Anomalies {
			lines = append(lines, " - "+strings.TrimSpace(strings.Join([]string{anomaly.Kind, anomaly.Plan, anomaly.Node, anomaly.Issue}, " "))+": "+anomaly.Detail)
		}
	}
	if !anomaliesOnly {
		for _, check := range review.Checks {
			lines = append(lines, fmt.Sprintf("check %s: %s (%s)", check.Name, check.State, check.Detail))
		}
	}
	return lines
}
