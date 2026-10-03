package dagsched

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// lanePos is where a live node stands in the merge order: its lane, when it entered it, and the tie-breakers. The merge lane serves its turns by requested_at and then turn id (decision D-16); an accepted result
// nothing holds back is next in line by the time it was accepted; every other node that holds regions is working, and is ordered by when its work began.
type lanePos struct {
	rank  int
	lane  string
	since string
	tie   string
	node  string
}

func (p lanePos) before(o lanePos) bool {
	switch {
	case p.rank != o.rank:
		return p.rank < o.rank
	case (p.since == "") != (o.since == ""):
		return p.since != "" // a node whose beginning is not stored goes last in its lane
	case p.since != o.since:
		return p.since < o.since
	case p.tie != o.tie:
		return p.tie < o.tie
	}
	return p.node < o.node
}

// lanePosition reads a holder's place from stored rows only. A node can be in the lane only while its accepted result is the one it would merge (current: it stands on the node's current relationship,
// generation and head event) and the plan does not hold it. Turn: an open merge turn (waiting, holding, merging or unknown) of its relationship for its accepted head, by requested_at and turn id. Accepted:
// an active acceptance the reading calls accepted (not blocked, stale, paused or cancelled), by accepted_at. Working: the rest, by the creation time of the earliest relationship of its executions, which a
// correction or a handover does not move, else by the decided_at of its release (a node that holds regions before it has a child).
func (s *Scheduler) lanePosition(ctx context.Context, q store.Querier, plan string, h orderHolder, current bool) (lanePos, error) {
	inLane := current && !h.Held && h.HasAcc && h.Acc.HeadSHA != ""
	if inLane {
		var requested, turn string
		found, err := queryOne(ctx, q, "SELECT requested_at, turn_id FROM merge_turns WHERE relationship_id = ? AND candidate_head = ? AND state IN ('waiting','holding','merging','unknown') ORDER BY requested_at, turn_id LIMIT 1",
			[]any{h.Acc.RelationshipID, h.Acc.HeadSHA}, &requested, &turn)
		if err != nil {
			return lanePos{}, err
		}
		if found {
			return lanePos{rank: 0, lane: LaneTurn, since: requested, tie: turn, node: h.NodeID}, nil
		}
	}
	if inLane && h.State == StateAccepted && h.Disp == DispDone {
		return lanePos{rank: 1, lane: LaneAccepted, since: h.Acc.AcceptedAt, node: h.NodeID}, nil
	}
	var since string
	var begun *string
	if _, err := queryOne(ctx, q, "SELECT MIN(r.created_at) FROM dag_node_executions e JOIN relationships r ON r.relationship_id = e.relationship_id WHERE e.plan_id = ? AND e.node_id = ?", []any{plan, h.NodeID}, &begun); err != nil {
		return lanePos{}, err
	}
	if begun != nil {
		since = *begun
	} else {
		var decided *string
		if _, err := queryOne(ctx, q, "SELECT MIN(decided_at) FROM dag_releases WHERE plan_id = ? AND node_id = ?", []any{plan, h.NodeID}, &decided); err != nil {
			return lanePos{}, err
		}
		if decided != nil {
			since = *decided
		}
	}
	return lanePos{rank: 2, lane: LaneWorking, since: since, node: h.NodeID}, nil
}

// measuredPair is the latest measurement of two nodes' heads: the member of the highest sweep that observed or replayed the pair (a replay is a measurement of the current heads even when its observation row is
// older than another pair of heads' observation).
type measuredPair struct {
	left, right, leftHead, rightHead, observation string
	conflicts                                     int
}

// latestPairs are the latest measurements of every pair of nodes of a plan that has one, whose observation is stored.
func latestPairs(ctx context.Context, q store.Querier, plan string) ([]measuredPair, error) {
	rows, err := q.QueryContext(ctx, "SELECT m.left_node_id, m.right_node_id, m.left_head, m.right_head, m.observation_id, o.conflict_count FROM dag_conflict_sweep_members m"+
		" JOIN dag_conflict_observations o ON o.observation_id = m.observation_id"+
		" WHERE m.plan_id = ? AND m.kind = 'pair' AND m.status <> 'unmeasured' AND m.sweep_seq = (SELECT MAX(x.sweep_seq) FROM dag_conflict_sweep_members x"+
		" WHERE x.plan_id = m.plan_id AND x.kind = 'pair' AND x.left_node_id = m.left_node_id AND x.right_node_id = m.right_node_id AND x.status <> 'unmeasured')"+
		" ORDER BY m.left_node_id, m.right_node_id", plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []measuredPair
	for rows.Next() {
		var p measuredPair
		if err := rows.Scan(&p.left, &p.right, &p.leftHead, &p.rightHead, &p.observation, &p.conflicts); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// observedFiles are the files an observation could not merge, as the repository names they were recorded under, and the distinct paths.
func observedFiles(ctx context.Context, q store.Querier, tip bool, observation string) (names, files []string, err error) {
	query := "SELECT repository, path FROM dag_conflict_observation_files WHERE observation_id = ? ORDER BY repository, path"
	if tip {
		query = "SELECT repository, path FROM dag_tip_conflict_observation_files WHERE observation_id = ? ORDER BY repository, path"
	}
	rows, err := q.QueryContext(ctx, query, observation)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	seenName, seenFile := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var repository, file string
		if err := rows.Scan(&repository, &file); err != nil {
			return nil, nil, err
		}
		if !seenName[repository] {
			seenName[repository] = true
			names = append(names, repository)
		}
		if !seenFile[file] {
			seenFile[file] = true
			files = append(files, file)
		}
	}
	sort.Strings(files)
	return names, files, rows.Err()
}

// coveringRegions are the regions of a declaration over a path, under the names the observed checkout was known by.
func coveringRegions(regions []Region, names []string, p string) []Region {
	var out []Region
	for _, r := range regions {
		if covers([]Region{r}, names, p) {
			out = append(out, r)
		}
	}
	return out
}

// pairGradeAcross is the grade of the overlap of two regions the nodes declared over one file, whichever spelling of the repository each used: a pair that the place rules do not overlap (two symbols of one file)
// still conflicted in that file, and is at least local.
func pairGradeAcross(a, b Region) string {
	a.Repository, b.Repository = "x", "x"
	if g := PairGrade(a, b); g != "" {
		return g
	}
	return GradeLocal
}

// classify is the worst grade of a conflict between two nodes' heads, read from their declarations: every conflicting file is graded by the worst overlap of every pair of regions that both cover it (a
// mechanical file settles only when all of them are mechanical), and a file one of the nodes did not declare is local at least and drift for that node. "" is a conflict every file of which a rule settles.
func classify(left, right string, declarations map[string][]Region, names, files []string) (grade string, drift []string) {
	grade = ""
	driftSet := map[string]bool{}
	for _, file := range files {
		a, b := coveringRegions(declarations[left], names, file), coveringRegions(declarations[right], names, file)
		fileGrade := GradeLocal
		if len(a) == 0 || len(b) == 0 {
			// the declaration did not name the path: drift for the node that did not, whatever else it holds
			if len(a) == 0 {
				driftSet[left] = true
			}
			if len(b) == 0 {
				driftSet[right] = true
			}
			// a path a delete, a rename or a hotspot names is exclusive at that place for the node that declared it, whether or not the other node declared it (CRW-431: that hold is on the place and no longer on the repository)
			for _, r := range append(append([]Region(nil), a...), b...) {
				if placeHold(r) {
					fileGrade = GradeExclusive
				}
			}
		} else {
			fileGrade = ""
			for _, x := range a {
				for _, y := range b {
					fileGrade = worse(fileGrade, pairGradeAcross(x, y))
				}
			}
		}
		// a region the declarer stated as holding the whole repository (Region.Exclusive; a rename, a delete and a hotspot hold their own place only, which the pair grades above judge): a conflict anywhere in it is exclusive for that node's hold, whether or not the node declared the path (which is what drift says)
		if holdsRepository(declarations[left], names) || holdsRepository(declarations[right], names) {
			fileGrade = GradeExclusive
		}
		grade = worse(grade, fileGrade)
	}
	for node := range driftSet {
		drift = append(drift, node)
	}
	sort.Strings(drift)
	if grade == GradeMechanical {
		return "", nil
	}
	return grade, drift
}

// holdsRepository is whether a declaration holds the whole of a repository the observed checkout is known as: a region with the exclusive flag, which is the declarer's word (CRW-431: a rename, a delete and a hotspot are exclusive at their own place and no longer set it).
func holdsRepository(regions []Region, names []string) bool {
	for _, r := range regions {
		if r.Exclusive && hasName(names, r.Repository) {
			return true
		}
	}
	return false
}

func capFiles(files []string) []string {
	if len(files) > MaxOrderFiles {
		return append([]string(nil), files[:MaxOrderFiles]...)
	}
	return append([]string{}, files...)
}

// headsCurrent compares the heads a measurement was made at with the heads the store holds now (the current accepted head of each node, which is all the store can know): yes when every node's head is stored
// and the same, no when a stored head differs, unknown when a node's head is not stored.
func headsCurrent(measuredLeft, measuredRight, storedLeft, storedRight string) string {
	switch {
	case storedLeft != "" && storedLeft != measuredLeft, storedRight != "" && storedRight != measuredRight:
		return HeadsCurrentNo
	case storedLeft != "" && storedRight != "":
		return HeadsCurrentYes
	}
	return HeadsCurrentUnknown
}

// orderConstraints derives the constraints of a plan's live holders from the stored measurements: for each pair of holders, the latest measurement of the pair, when it conflicts in a way no declared rule settles,
// orders the node that lands first before the other (the lane and the order of its position, lanePos). It also gives each holder the latest measurement of its head against the tip when that conflicts. It writes
// nothing, runs no git and reads no clock, so two readings of one store state agree; a store whose zone predates the tables has no constraint. The second result is the number of constrained pairs.
func (s *Scheduler) orderConstraints(ctx context.Context, q store.Querier, plan string, holders []orderHolder, declarations map[string][]Region) (map[string]*MergeOrder, int, error) {
	if len(holders) == 0 {
		return nil, 0, nil
	}
	for _, table := range []string{"dag_conflict_sweep_members", "dag_conflict_observations", "dag_conflict_observation_files", "dag_tip_conflict_observations", "dag_tip_conflict_observation_files", "dag_conflict_drift"} {
		ok, err := tableExists(ctx, q, table)
		if err != nil || !ok {
			return nil, 0, err
		}
	}
	byNode := map[string]orderHolder{}
	positions := map[string]lanePos{}
	stored := map[string]string{} // the head of each node's current accepted result, "" when it has none
	for _, h := range holders {
		rel, found, err := currentRelationshipOf(ctx, q, plan, h.NodeID)
		if err != nil {
			return nil, 0, err
		}
		head, err := s.currentAcceptanceHead(ctx, q, h.Acc, h.HasAcc, rel, found)
		if err != nil {
			return nil, 0, err
		}
		stored[h.NodeID] = head
		pos, err := s.lanePosition(ctx, q, plan, h, head != "")
		if err != nil {
			return nil, 0, err
		}
		byNode[h.NodeID], positions[h.NodeID] = h, pos
	}
	storedHead := func(node string) (string, error) { return stored[node], nil }
	pairs, err := latestPairs(ctx, q, plan)
	if err != nil {
		return nil, 0, err
	}
	out := map[string]*MergeOrder{}
	get := func(node string) *MergeOrder {
		if out[node] == nil {
			out[node] = &MergeOrder{}
		}
		return out[node]
	}
	constrained := 0
	for _, p := range pairs {
		_, leftHolds := byNode[p.left]
		_, rightHolds := byNode[p.right]
		if !leftHolds || !rightHolds || p.conflicts == 0 {
			continue
		}
		names, files, err := observedFiles(ctx, q, false, p.observation)
		if err != nil {
			return nil, 0, err
		}
		row := OrderRow{ObservationID: p.observation, Conflicts: p.conflicts, Files: capFiles(files), Grade: GradeLocal, DriftNodes: []string{}}
		if len(files) == 0 {
			// observed before the files were recorded: a conflict that nothing says a rule settles
			row.Unattributed = true
		} else if row.Grade, row.DriftNodes = classify(p.left, p.right, declarations, names, files); row.Grade == "" {
			continue
		}
		leftStored, err := storedHead(p.left)
		if err != nil {
			return nil, 0, err
		}
		rightStored, err := storedHead(p.right)
		if err != nil {
			return nil, 0, err
		}
		row.HeadsCurrent = headsCurrent(p.leftHead, p.rightHead, leftStored, rightStored)
		first, second := p.left, p.right
		if positions[second].before(positions[first]) {
			first, second = second, first
		}
		// seen from the later node the row names the earlier one, and the other way round
		later := row
		later.NodeID, later.Lane, later.Since = first, positions[first].lane, positions[first].since
		earlier := row
		earlier.NodeID, earlier.Lane, earlier.Since = second, positions[second].lane, positions[second].since
		get(second).After = append(get(second).After, later)
		get(first).Before = append(get(first).Before, earlier)
		constrained++
	}
	tips, err := latestTips(ctx, q, plan, byNode, stored)
	if err != nil {
		return nil, 0, err
	}
	for node, tip := range tips {
		get(node).Tip = tip
	}
	for _, mo := range out {
		sort.SliceStable(mo.After, func(i, j int) bool { return positions[mo.After[i].NodeID].before(positions[mo.After[j].NodeID]) })
		sort.SliceStable(mo.Before, func(i, j int) bool { return positions[mo.Before[i].NodeID].before(positions[mo.Before[j].NodeID]) })
		mo.Reason = orderReason(*mo)
	}
	return out, constrained, nil
}

// latestTips are, for the holders, the latest measurement of the node's head against a tip (the member of the highest sweep that measured it, and of that sweep's tips the one with most conflicts) when it
// conflicts.
func latestTips(ctx context.Context, q store.Querier, plan string, holders map[string]orderHolder, stored map[string]string) (map[string]*TipRow, error) {
	rows, err := q.QueryContext(ctx, "SELECT m.left_node_id, m.observation_id, m.conflicts, m.left_head_source, m.right_head, m.left_head FROM dag_conflict_sweep_members m"+
		" WHERE m.plan_id = ? AND m.kind = 'tip' AND m.status <> 'unmeasured' AND m.sweep_seq = (SELECT MAX(x.sweep_seq) FROM dag_conflict_sweep_members x"+
		" WHERE x.plan_id = m.plan_id AND x.kind = 'tip' AND x.left_node_id = m.left_node_id AND x.status <> 'unmeasured')"+
		" ORDER BY m.left_node_id, m.conflicts DESC, m.observation_id", plan)
	if err != nil {
		return nil, err
	}
	type tipMember struct {
		node, observation, source, tip, head string
		conflicts                            int
	}
	var members []tipMember
	for rows.Next() {
		var m tipMember
		if err := rows.Scan(&m.node, &m.observation, &m.conflicts, &m.source, &m.tip, &m.head); err != nil {
			_ = rows.Close()
			return nil, err
		}
		members = append(members, m)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := map[string]*TipRow{}
	for _, m := range members {
		if _, held := holders[m.node]; !held || m.conflicts == 0 || out[m.node] != nil {
			continue
		}
		_, files, err := observedFiles(ctx, q, true, m.observation)
		if err != nil {
			return nil, err
		}
		drift, err := loadDrift(ctx, q, m.observation)
		if err != nil {
			return nil, err
		}
		tip := &TipRow{ObservationID: m.observation, TipSHA: m.tip, Head: m.head, HeadSource: m.source, HeadsCurrent: HeadsCurrentUnknown, Conflicts: m.conflicts, Files: capFiles(files), DriftNodes: []string{}}
		switch current := stored[m.node]; {
		case current == "":
		case current == m.head:
			tip.HeadsCurrent = HeadsCurrentYes
		default:
			tip.HeadsCurrent = HeadsCurrentNo
		}
		seen := map[string]bool{}
		for _, d := range drift {
			if !seen[d.Node] {
				seen[d.Node] = true
				tip.DriftNodes = append(tip.DriftNodes, d.Node)
			}
		}
		sort.Strings(tip.DriftNodes)
		out[m.node] = tip
	}
	return out, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func describeRow(r OrderRow) string {
	text := fmt.Sprintf("%s (observation %s: %d conflicting %s", r.NodeID, r.ObservationID, r.Conflicts, plural(r.Conflicts, "file", "files"))
	if len(r.Files) > 0 {
		text += " " + strings.Join(r.Files, ", ")
	}
	if len(r.DriftNodes) > 0 {
		text += "; undeclared by " + strings.Join(r.DriftNodes, ", ")
	}
	if r.HeadsCurrent == HeadsCurrentNo {
		text += "; measured at other heads than the store holds now: measure again"
	}
	return text + ")"
}

// orderReason is the sentence a constraint reads as: which nodes land before and after this one, on what measurement, and what the later one does.
func orderReason(m MergeOrder) string {
	var parts []string
	if len(m.After) > 0 {
		rows := make([]string, len(m.After))
		for i, r := range m.After {
			rows[i] = describeRow(r)
		}
		parts = append(parts, "lands after "+strings.Join(rows, ", ")+", then refreshes its base and resolves the conflict there")
	}
	if len(m.Before) > 0 {
		rows := make([]string, len(m.Before))
		for i, r := range m.Before {
			rows[i] = describeRow(r)
		}
		parts = append(parts, "lands before "+strings.Join(rows, ", ")+", which "+plural(len(m.Before), "refreshes", "refresh")+" the base after it")
	}
	if m.Tip != nil {
		if m.Tip.HeadsCurrent == HeadsCurrentNo {
			parts = append(parts, fmt.Sprintf("a head of this node (%s) conflicted with the tip %s (observation %s: %d conflicting %s) and is no longer the head the store holds: measure again", m.Tip.Head, m.Tip.TipSHA, m.Tip.ObservationID, m.Tip.Conflicts, plural(m.Tip.Conflicts, "file", "files")))
		} else {
			parts = append(parts, fmt.Sprintf("its head conflicts with the tip %s (observation %s: %d conflicting %s): refresh the base", m.Tip.TipSHA, m.Tip.ObservationID, m.Tip.Conflicts, plural(m.Tip.Conflicts, "file", "files")))
		}
	}
	return strings.Join(parts, "; ")
}
