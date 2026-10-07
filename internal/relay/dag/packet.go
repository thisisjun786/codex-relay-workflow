package dag

import (
	"fmt"
	"sort"
	"strings"
)

// The packet identity of a feature issue (CRW-839). One issue used to be one node, one child and one
// pull request. A feature issue may now be delivered by several packets: each packet is an implementation
// node that carries its own packet_id, declares the feature criteria it takes (covers) and, where two
// packets take the same criterion, which of them owns it (owns). Nothing else identifies a packet: the
// plan is the registry, and the execution rows carry the same packet_id to the release and the child.
//
// A node without a packet_id keeps the meaning it always had (one node per issue). These rules are
// judged on the plan a revision would produce, like every other rule of the fold, and they are the
// writer's: a log written before packets existed holds neither a packet nor a declaration and is still
// the log, so a replay does not re-judge them (applyChanges's strict flag).

// applyFeatureCriteria is the criteria history after the declarations of one revision, in order: a
// declaration replaces the live one for its issue_key (retired at this revision) and is appended. A
// declaration with no criteria is refused by the reader, so a row always holds at least one criterion.
func applyFeatureCriteria(rows []FeatureCriteriaRow, rev int64, decls []FeatureCriteriaDecl) []FeatureCriteriaRow {
	out := append([]FeatureCriteriaRow(nil), rows...)
	for _, d := range decls {
		for i := range out {
			if out[i].IssueKey == d.IssueKey && out[i].RetiredRev == 0 {
				out[i].RetiredRev = rev
			}
		}
		out = append(out, FeatureCriteriaRow{IssueKey: d.IssueKey, Criteria: append([]Criterion(nil), d.Criteria...), IntroducedRev: rev})
	}
	return out
}

// liveCriteria is the declared criteria of each issue in a fold: the rows that are still current.
func liveCriteria(rows []FeatureCriteriaRow) map[string][]Criterion {
	out := map[string][]Criterion{}
	for _, r := range rows {
		if r.RetiredRev == 0 {
			out[r.IssueKey] = r.Criteria
		}
	}
	return out
}

// criteriaAt is the declared criteria of each issue as of revision rev, the way lifeAt reads the lifecycle.
func criteriaAt(rows []FeatureCriteriaRow, rev int64) []FeatureCriteriaRow {
	var out []FeatureCriteriaRow
	for _, r := range rows {
		if r.IntroducedRev > rev || (r.RetiredRev != 0 && r.RetiredRev <= rev) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IssueKey < out[j].IssueKey })
	return out
}

// sameCriteria compares two criteria histories row by row, whatever order they were built in.
func sameCriteria(a, b []FeatureCriteriaRow) bool {
	if len(a) != len(b) {
		return false
	}
	// The key is canonical JSON and not a joined string: an id may hold the delimiter characters the
	// identifier grammar allows, and two different rows must never read as one.
	key := func(r FeatureCriteriaRow) string {
		return canonical(map[string]any{"issue_key": r.IssueKey, "introduced_rev": r.IntroducedRev, "retired_rev": r.RetiredRev, "criteria": criteriaList(r.Criteria)})
	}
	as, bs := make([]string, len(a)), make([]string, len(b))
	for i, r := range a {
		as[i] = key(r)
	}
	for i, r := range b {
		bs[i] = key(r)
	}
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// criteriaList is a criterion list as canonical JSON reads it, in id order so two equal sets serialize equally.
func criteriaList(list []Criterion) []any {
	sorted := append([]Criterion(nil), list...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make([]any, len(sorted))
	for i, c := range sorted {
		m := map[string]any{"id": c.ID, "required": c.Required}
		if c.Title != "" {
			// an unset title is absent, as it is on a node, so a criterion written without one reads back as it was written
			m["title"] = c.Title
		}
		out[i] = m
	}
	return out
}

// featureCriteriaObject is one issue's declaration as the plan digests it.
func featureCriteriaObject(r FeatureCriteriaRow) map[string]any {
	return map[string]any{"issue_key": r.IssueKey, "criteria": criteriaList(r.Criteria)}
}

// checkPackets judges the packet rules of the plan the changes produce:
//
//   - a packet identity belongs to an implementation node: a non_pr node carrying packet_id, covers or
//     owns is refused;
//   - several live nodes of one issue_key may share it only with a distinct, non-empty packet_id, so
//     (issue_key, packet_id) is unique among the live nodes;
//   - covers and owns are part of the packet identity: a node that declares either declares a packet_id;
//   - every cover names a criterion the feature declared, and every own is one of that node's covers;
//   - every required declared criterion is taken by at least one live packet of its issue;
//   - a criterion two or more live packets of one issue take has exactly one owner among them.
//
// nodePath names a node the way the other rules of the fold do (the change that introduced it, else its
// place in the plan), so a refusal points at the document the caller can fix.
func checkPackets(live map[string]liveNode, decls map[string][]Criterion, nodePath func(string) string) []Violation {
	var vs []Violation
	add := func(rule, path, format string, args ...any) {
		vs = append(vs, Violation{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
	}
	ids := make([]string, 0, len(live))
	for id := range live {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	byIssue := map[string][]string{}
	for _, id := range ids {
		n := live[id]
		if n.Kind != NodeImplementation {
			var stray []string
			if n.PacketID != "" {
				stray = append(stray, "packet_id")
			}
			if len(n.Covers) > 0 {
				stray = append(stray, "covers")
			}
			if len(n.Owns) > 0 {
				stray = append(stray, "owns")
			}
			if len(stray) > 0 {
				add(RulePacketFieldNotApplicable, nodePath(id), "%s node %s carries %s; a packet is an implementation node (one issue, one child, one pull request)", n.Kind, id, strings.Join(stray, ", "))
			}
			continue
		}
		byIssue[n.IssueKey] = append(byIssue[n.IssueKey], id)
	}
	issues := make([]string, 0, len(byIssue))
	for issue := range byIssue {
		issues = append(issues, issue)
	}
	sort.Strings(issues)
	for _, issue := range issues {
		nodes := byIssue[issue]
		sort.Strings(nodes)
		declared := decls[issue]
		declaredIDs := map[string]bool{}
		for _, c := range declared {
			declaredIDs[c.ID] = true
		}
		if len(nodes) > 1 {
			for _, id := range nodes {
				if live[id].PacketID == "" {
					add(RulePacketRequired, nodePath(id), "node %s shares issue %s with %d other live node(s) and carries no packet_id; several nodes may share an issue_key only with a distinct packet_id each", id, issue, len(nodes)-1)
				}
			}
		}
		seen := map[string]string{}
		for _, id := range nodes {
			p := live[id].PacketID
			if p == "" {
				continue
			}
			if other, dup := seen[p]; dup {
				add(RuleDuplicatePacket, nodePath(id), "nodes %s and %s share issue %s and packet_id %s; (issue_key, packet_id) is unique among a plan's live nodes", other, id, issue, p)
				continue
			}
			seen[p] = id
		}
		coverCount := map[string][]string{}
		ownerCount := map[string][]string{}
		for _, id := range nodes {
			n := live[id]
			if n.PacketID == "" && (len(n.Covers) > 0 || len(n.Owns) > 0) {
				add(RulePacketRequired, nodePath(id), "node %s declares covers or owns without a packet_id; a packet identity is a packet_id, its covers and its owns together", id)
			}
			covers := map[string]bool{}
			for _, c := range n.Covers {
				if !declaredIDs[c] {
					add(RuleCoversUnknownCriterion, nodePath(id), "node %s covers criterion %s, which issue %s does not declare", id, c, issue)
					continue
				}
				covers[c] = true
				coverCount[c] = append(coverCount[c], id)
			}
			for _, o := range n.Owns {
				if !covers[o] {
					add(RuleOwnsNotCovered, nodePath(id), "node %s owns criterion %s, which it does not cover", id, o)
					continue
				}
				ownerCount[o] = append(ownerCount[o], id)
			}
		}
		for _, c := range declared {
			if c.Required && len(coverCount[c.ID]) == 0 {
				add(RuleCriterionUncovered, "plan", "no live node of issue %s covers the required criterion %s; a required criterion is taken by at least one packet", issue, c.ID)
			}
		}
		shared := make([]string, 0, len(coverCount))
		for c, takers := range coverCount {
			if len(takers) > 1 {
				shared = append(shared, c)
			}
		}
		sort.Strings(shared)
		for _, c := range shared {
			takers := append([]string(nil), coverCount[c]...)
			sort.Strings(takers)
			owners := ownerCount[c]
			switch {
			case len(owners) == 0:
				add(RuleCriterionOwnerMissing, "plan", "criterion %s of issue %s is taken by %s and none of them names it in owns; a shared criterion has exactly one owner", c, issue, strings.Join(takers, ", "))
			case len(owners) > 1:
				sort.Strings(owners)
				add(RuleCriterionOwnerConflict, "plan", "criterion %s of issue %s is taken by %s and %s claim to own it; a shared criterion has exactly one owner", c, issue, strings.Join(takers, ", "), strings.Join(owners, ", "))
			}
		}
	}
	return vs
}
