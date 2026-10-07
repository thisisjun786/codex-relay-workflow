package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
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

// dagReviewSource reads one group of anomalies from the store. This issue registers the store
// source; a later issue adds the host-record source to the same list, so the review is called
// through one place rather than through a growing switch.
type dagReviewSource func(ctx context.Context, in *dagReviewInput) error

// dagReviewSources is every source a review runs, in order. A source appends to in.review.
var dagReviewSources = []dagReviewSource{
	dagReviewStoreSources,
	dagHostSources,
}

// dagReviewInput is what a source reads: the open store, the plan facts, the lanes, the clock
// and the review being built.
type dagReviewInput struct {
	store  *dagReviewStore
	now    time.Time
	stall  time.Duration
	facts  []dagReviewFacts
	lanes  []dagReviewLaneTurn
	review *Review
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
	store, err := dagReviewOpenStore(ctx, state)
	if err != nil {
		return review, err
	}
	defer store.Close()
	plans, err := store.dagReviewReadPlans(ctx, section.Plans)
	if err != nil {
		return review, fmt.Errorf("read the plans: %w", err)
	}
	if err := dagReviewCheckConfigured(section.Plans, plans); err != nil {
		return review, err
	}
	in := &dagReviewInput{store: store, now: e.Now(), stall: time.Duration(stall) * time.Minute, review: &review}
	for _, plan := range plans {
		facts, err := store.dagReviewReadFacts(ctx, plan, &review.Checks)
		if err != nil {
			return review, fmt.Errorf("read plan %s: %w", plan.planID, err)
		}
		in.facts = append(in.facts, facts)
	}
	if in.lanes, err = store.dagReviewReadLanes(ctx, section.Plans); err != nil {
		return review, fmt.Errorf("read the merge lanes: %w", err)
	}
	for _, source := range dagReviewSources {
		if err := source(ctx, in); err != nil {
			return review, err
		}
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
		dagReviewReleasedBeforePredecessor(in, facts)
		dagReviewReleasedRepeatedly(in, facts)
		dagReviewExclusiveOverlapRunning(in, facts)
		dagReviewLandedNotObserved(in, facts)
		if err := dagReviewChildWithoutRelease(ctx, in, facts); err != nil {
			return err
		}
	}
	return dagReviewLaneTurnStalled(in)
}

// dagReviewPlanShape records what the review counted for one plan.
func dagReviewPlanShape(in *dagReviewInput, facts dagReviewFacts) {
	integrated := 0
	for _, acceptance := range facts.acceptances {
		if dagReviewIntegratedAt(facts, acceptance.nodeID, "", "") != "" {
			integrated++
		}
	}
	in.review.Plans = append(in.review.Plans, DagReviewPlan{
		Plan: facts.plan.planID, Revision: facts.revision, Nodes: len(facts.nodes), Edges: len(facts.edges),
		Released: len(facts.releases), Accepted: len(facts.acceptances), Integrated: integrated,
	})
}

// dagReviewIssueByNode maps every live node id of the plan to its issue.
func dagReviewIssueByNode(facts dagReviewFacts) map[string]string {
	issue := map[string]string{}
	for _, node := range facts.nodes {
		issue[node.nodeID] = node.issueKey
	}
	return issue
}

// dagReviewFirstRelease is each node's first release instant, which is when the node was
// released for execution.
func dagReviewFirstRelease(facts dagReviewFacts) map[string]time.Time {
	first := map[string]time.Time{}
	for _, release := range facts.releases {
		instant, ok := dagReviewInstant(release.decidedAt)
		if !ok {
			continue
		}
		if existing, seen := first[release.nodeID]; !seen || instant.Before(existing) {
			first[release.nodeID] = instant
		}
	}
	return first
}

// dagReviewIntegratedAt is the node's earliest effective integration observation, or "" when it
// has none: an observation that is not an ancestor or that was reverted does not integrate. An
// empty repository means any target; a named one narrows the reading to that repository and base
// ref, which is how an integrated edge is judged, because the edge names the target it orders.
func dagReviewIntegratedAt(facts dagReviewFacts, nodeID, repository, baseRef string) string {
	byAcceptance := map[string]string{}
	for _, acceptance := range facts.acceptances {
		byAcceptance[acceptance.acceptanceID] = acceptance.nodeID
	}
	best := ""
	for _, observation := range facts.observations {
		if !observation.isAncestor || observation.revertedBy != "" {
			continue
		}
		if byAcceptance[observation.acceptanceID] != nodeID {
			continue
		}
		if repository != "" && (observation.repository != repository || observation.baseRef != baseRef) {
			continue
		}
		if best == "" || observation.observedAt < best {
			best = observation.observedAt
		}
	}
	return best
}

// dagReviewReleasedBeforePredecessor reports an integrated edge whose successor was released
// before the predecessor was observed integrated. An edge introduced after the release orders
// the merge rather than the release, so the release could not have waited for it.
func dagReviewReleasedBeforePredecessor(in *dagReviewInput, facts dagReviewFacts) {
	issue := dagReviewIssueByNode(facts)
	first := dagReviewFirstRelease(facts)
	for _, edge := range facts.edges {
		if edge.kind != "integrated" {
			continue
		}
		releasedAt, released := first[edge.to]
		if !released {
			continue
		}
		if introduced, ok := dagReviewInstant(edge.introducedAt); ok && introduced.After(releasedAt) {
			continue
		}
		observed := dagReviewIntegratedAt(facts, edge.from, edge.repository, edge.baseRef)
		instant, ok := dagReviewInstant(observed)
		if ok && !instant.After(releasedAt) {
			continue
		}
		detail := "the predecessor was never observed integrated"
		if ok {
			detail = "the predecessor was observed integrated at " + observed
		}
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagReviewKindReleasedBeforePredecessor, Plan: facts.plan.planID, Node: edge.to,
			Issue: issue[edge.to], Detail: detail,
		})
	}
}

// dagReviewReleasedRepeatedly reports a node released more than once; the detail carries how
// many times, because a correction and a redefinition are not the same thing.
func dagReviewReleasedRepeatedly(in *dagReviewInput, facts dagReviewFacts) {
	issue := dagReviewIssueByNode(facts)
	count := map[string]int{}
	for _, release := range facts.releases {
		count[release.nodeID]++
	}
	for _, node := range dagReviewSortedKeys(count) {
		if count[node] > 1 {
			in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
				Kind: dagReviewKindReleasedRepeatedly, Plan: facts.plan.planID, Node: node, Issue: issue[node],
				Detail: fmt.Sprintf("released %d times", count[node]),
			})
		}
	}
}

// dagReviewExclusiveOverlapRunning reports two nodes in flight at once whose declared regions
// the scheduler grades exclusive. Local and mechanical overlaps are released on purpose and
// settled at merge time, so they are not anomalies.
func dagReviewExclusiveOverlapRunning(in *dagReviewInput, facts dagReviewFacts) {
	issue := dagReviewIssueByNode(facts)
	first := dagReviewFirstRelease(facts)
	live := map[string]bool{}
	for _, node := range facts.nodes {
		live[node.nodeID] = true
	}
	landed := map[string]bool{}
	for _, turn := range in.lanes {
		if turn.state == "landed" {
			landed[turn.relationshipID] = true
		}
	}
	relationship := map[string]string{}
	for _, execution := range facts.executions {
		relationship[execution.nodeID] = execution.relationshipID
	}
	var inflight []string
	for node := range first {
		// A node retired by a later revision is no longer in the plan, so it holds nothing.
		if !live[node] {
			continue
		}
		// A node whose relationship is closed or cancelled no longer holds its regions, and one
		// whose lane already landed released them at the landing (the scheduler's bound state).
		if id := relationship[node]; id != "" && (!facts.liveRelationships[id] || landed[id]) {
			continue
		}
		if dagReviewIntegratedAt(facts, node, "", "") == "" {
			inflight = append(inflight, node)
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

// dagReviewLandedNotObserved reports a lane that landed an acceptance whose node was never
// observed integrated, once the landing has stood still past the threshold. The turn names the
// node through its relationship, which is the link the relay itself keeps.
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
		for _, acceptance := range facts.acceptances {
			if acceptance.relationshipID != turn.relationshipID {
				continue
			}
			if dagReviewIntegratedAt(facts, acceptance.nodeID, "", "") != "" {
				continue
			}
			landed[acceptance.nodeID] = turn.turnID
		}
	}
	for _, node := range dagReviewSortedKeys(landed) {
		in.review.Anomalies = append(in.review.Anomalies, DagReviewAnomaly{
			Kind: dagReviewKindLandedNotObserved, Plan: facts.plan.planID, Node: node, Issue: issue[node],
			Detail: fmt.Sprintf("the lane landed and no integration was observed within %s", in.stall),
		})
	}
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
