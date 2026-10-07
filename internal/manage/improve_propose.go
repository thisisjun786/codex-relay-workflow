package manage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// crw manage improve propose turns one improvement evidence bundle into ranked repeated-
// friction candidates and one follow-up issue draft per candidate. It reuses the audit
// drafts' fingerprint, lock, directory and index, so a friction the audit already drafted is
// not drafted a second time. The product only proposes: it never releases, merges or writes
// to Linear.

// The source token every draft this command writes carries, the schema of the report it
// prints, and its usage line.
const (
	improveProposeSource       = "improve"
	improveProposeReportSchema = "crw-improve-propose/1"
	improveProposeUsage        = "usage: crw manage improve propose --bundle FILE [--dry-run]"
)

// The impact ranks a candidate carries. A smaller rank is more important, so the ranking
// puts a blockage above a plain audit note: a refused or blocked record is high, a fault or a
// generation is medium, and a DAG metric or a draft is low.
const (
	improveProposeImpactHigh   = 0
	improveProposeImpactMedium = 1
	improveProposeImpactLow    = 2
)

// improveProposeProject is one project a friction happened in, with how many times it was
// seen there, so a candidate that reaches several projects names each one and its count.
type improveProposeProject struct {
	Project string `json:"project"`
	Count   int    `json:"count"`
}

// improveProposeCandidate is one repeated-friction candidate: the shared fingerprint that
// names it, the kind of friction, the title the draft carries, every project it happened in
// with its count, its impact rank, how many times it was seen, its estimated cost and the
// origin locations behind it.
type improveProposeCandidate struct {
	Key      string                  `json:"key"`
	Kind     string                  `json:"kind"`
	Title    string                  `json:"title"`
	Projects []improveProposeProject `json:"projects"`
	Impact   int                     `json:"impact"`
	Count    int                     `json:"count"`
	Cost     int                     `json:"cost"`
	Evidence []string                `json:"evidence"`

	seen []auditDraftSeen

	// seenProjects is the project each sighting was seen in, so an update of a draft that already
	// exists adds only the sightings this run newly recorded to that project's count. It does not
	// reach the report: the report names the candidate, not how a merge counts it.
	seenProjects map[auditDraftSeen]string

	// seenCounts is how many occurrences each sighting stands for. A record that aggregates its
	// occurrences into one row reports its own count at that one origin; a record with an origin per
	// occurrence reports one each. It does not reach the report either.
	seenCounts map[auditDraftSeen]int
}

// improveProposeReport is what crw manage improve propose prints: the ranked candidates, the
// drafts this run created and grew, the candidates a listed issue suppressed, and how many
// new drafts the cap left for a later run.
type improveProposeReport struct {
	Schema     string                    `json:"schema"`
	BundlePath string                    `json:"bundle"`
	Candidates []improveProposeCandidate `json:"candidates"`
	Created    []auditDraftSummary       `json:"created"`
	Updated    []auditDraftSummary       `json:"updated"`
	Suppressed []string                  `json:"suppressed"`
	Remaining  int                       `json:"remaining"`
	DryRun     bool                      `json:"dry_run"`
}

// improveProposeParseArgs reads --bundle FILE and --dry-run.
func improveProposeParseArgs(args []string) (bundle string, dryRun, help bool, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-h" || args[i] == "--help":
			return "", false, true, nil
		case args[i] == "--dry-run":
			dryRun = true
		case args[i] == "--bundle":
			if i+1 >= len(args) {
				return "", false, false, errors.New("the option --bundle needs a value")
			}
			i++
			bundle = args[i]
			if bundle == "" {
				return "", false, false, errors.New("the option --bundle needs a value")
			}
		case strings.HasPrefix(args[i], "--bundle="):
			bundle = strings.TrimPrefix(args[i], "--bundle=")
			if bundle == "" {
				return "", false, false, errors.New("the option --bundle needs a value")
			}
		default:
			return "", false, false, fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if bundle == "" {
		return "", false, false, errors.New("the option --bundle is required")
	}
	return bundle, dryRun, false, nil
}

// improveProposeReadBundle reads a crw-improve-bundle/1 document. A missing file, malformed
// JSON or another schema is an error: a bundle this command cannot trust is never turned
// into drafts.
func improveProposeReadBundle(path string) (improveBundle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return improveBundle{}, err
	}
	var bundle improveBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return improveBundle{}, fmt.Errorf("%s: %w", path, err)
	}
	if bundle.Schema != improveBundleSchema {
		return improveBundle{}, fmt.Errorf("%s: schema %q is not %s", path, bundle.Schema, improveBundleSchema)
	}
	return bundle, nil
}

// improveProposeFriction reports whether a record is a repeated friction: a thing that
// happened and can happen again, so a fix can be proposed for it. A split (a blocked receipt
// or a split decision), a refusal, a fault and a management intervention are friction. A
// generation is friction only when its reason is needs_changes_revision: an initial_assignment
// or a decision_reply is normal operation. The other kinds a bundle carries are references
// rather than occurrences: a criteria record's count is the size of a criteria set, an audit
// record's what is a grading outcome, a dag record's count is a sample count, and a draft
// record is a draft this product already wrote. None of them becomes a candidate.
func improveProposeFriction(record improveRecord) bool {
	switch record.Kind {
	case improveKindSplit, improveKindRefusal, improveKindFault, improveKindIntervention:
		return true
	case improveKindGeneration:
		return strings.TrimSpace(record.What) == improveProposeNeedsChanges
	}
	return false
}

// improveProposeNeedsChanges is the one generation reason that is friction: a child sent back
// to correct its work.
const improveProposeNeedsChanges = "needs_changes_revision"

// improveProposeTitleOf is the title a record's candidate carries: the reason or signature the
// record holds, or its key when it carries no what.
func improveProposeTitleOf(record improveRecord) string {
	if what := strings.TrimSpace(record.What); what != "" {
		return what
	}
	return strings.TrimSpace(record.Key)
}

// improveProposeProjectOf is the project a record happened in, so the candidate names its
// owners. A record whose kind carries no project reads as the empty one.
func improveProposeProjectOf(record improveRecord) string {
	switch record.Kind {
	// A split record's key is the project key the issue belongs to (721's improveSplitKey), which
	// is empty when the relationship carries no scope: an issue key is not a project, and such a
	// record stays owner-unknown.
	case improveKindSplit:
		return strings.TrimSpace(record.Key)
	}
	// No other friction kind names a project honestly: a fault's where is the composite fault
	// scope key (a product name, product:project, or a workspace|product|workspace|project
	// tuple), a refusal's key is its reason, a generation's where is its relationship id, and
	// an intervention carries neither. Such a candidate stays owner-unknown rather than
	// claiming a field that is not a project.
	return ""
}

// improveProposeImpactOf is the impact rank of one record: a blockage or a refused operation
// is high, a needs_changes generation and a P0 or P1 audit finding are high, a fault or a
// plain generation is medium, and a DAG metric, a criteria refresh or a draft is low.
func improveProposeImpactOf(record improveRecord) int {
	switch record.Kind {
	case improveKindSplit, improveKindRefusal:
		return improveProposeImpactHigh
	case improveKindGeneration:
		if strings.TrimSpace(record.What) == improveProposeNeedsChanges {
			return improveProposeImpactHigh
		}
		return improveProposeImpactMedium
	case improveKindFault, improveKindIntervention:
		return improveProposeImpactMedium
	}
	return improveProposeImpactLow
}

// improveProposeRecordSightings is the sightings one record makes, in the audit draft seen shape
// the shared crw-issue-draft/1 format carries: the source, what the record is about, where it was
// seen, and when it was last seen. A record that merges two or more origin locations was seen
// once at each of them, so it makes one sighting per location. Head names that location, because
// it is what tells the sightings apart, and At is the record's last sighting. A record that names
// no location makes one sighting of the record's where, so the shape does not change when a
// record gains its first location.
func improveProposeRecordSightings(record improveRecord) []auditDraftSeen {
	subject, at := strings.TrimSpace(record.Key), strings.TrimSpace(record.LastAt)
	locations := improveProposeOrigins(record)
	if len(locations) == 0 {
		return []auditDraftSeen{{Mode: improveProposeSource, Subject: subject, Head: strings.TrimSpace(record.Where), At: at}}
	}
	out := make([]auditDraftSeen, 0, len(locations))
	for _, location := range locations {
		out = append(out, auditDraftSeen{Mode: improveProposeSource, Subject: subject, Head: location, At: at})
	}
	return out
}

// improveProposeOrigins is the origin locations of one record: the evidence entries that name
// where the friction was seen. Only a split record attaches evidence that is context rather than an
// occurrence — the issue a split without a scope belongs to, or the decision reply that answered a
// blockage — so the exclusion is asked of the record kind. For every other kind each non-empty
// evidence entry is a place the friction happened, however its text begins.
func improveProposeOrigins(record improveRecord) []string {
	locations := make([]string, 0, len(record.Evidence))
	for _, evidence := range record.Evidence {
		trimmed := strings.TrimSpace(evidence)
		if trimmed == "" || improveProposeContextEvidence(record.Kind, trimmed) {
			continue
		}
		locations = append(locations, trimmed)
	}
	return locations
}

// improveProposeContextEvidence reports whether one evidence entry of a record of this kind is
// context rather than an origin location. Only a split record attaches such evidence: the issue a
// split without a scope belongs to, or the decision reply whose note gave a blockage its reason.
// Another kind's real location may begin with either prefix, so the kind decides and the text alone
// never does.
func improveProposeContextEvidence(kind, entry string) bool {
	if kind != improveKindSplit {
		return false
	}
	return strings.HasPrefix(entry, improveEvidenceIssuePrefix) || strings.HasPrefix(entry, improveEvidenceAnswerPrefix)
}

// improveProposeSightingIdentity is what makes two sightings the same occurrence: the source and
// the origin location. Neither the time nor the subject is part of it. A record's last sighting
// moves forward every time the record is seen again, so a time that participated would make every
// earlier occurrence look new on the next run and count it a second time. A split's subject is the
// project its relationship currently carries, and a later reading may learn that project, so a
// subject that participated would count one origin twice the moment its owner changed.
func improveProposeSightingIdentity(sighting auditDraftSeen) auditDraftSeen {
	sighting.At = ""
	sighting.Subject = ""
	return sighting
}

// improveProposeSeenIndex is the position of the stored sighting that is the same occurrence as
// this one, or -1 when the draft does not carry the occurrence. A caller uses it to read or update
// the entry the draft holds for an origin.
func improveProposeSeenIndex(seen []auditDraftSeen, entry auditDraftSeen) int {
	identity := improveProposeSightingIdentity(entry)
	for i, have := range seen {
		if improveProposeSightingIdentity(have) == identity {
			return i
		}
	}
	return -1
}

// improveProposeSeenHas reports whether a seen list already carries this occurrence. It compares
// identities rather than whole entries, so a sighting whose record was seen again is recognised
// as the occurrence it already is.
func improveProposeSeenHas(seen []auditDraftSeen, entry auditDraftSeen) bool {
	identity := improveProposeSightingIdentity(entry)
	for _, have := range seen {
		if improveProposeSightingIdentity(have) == identity {
			return true
		}
	}
	return false
}

// improveProposeCandidates groups the bundle's records into candidates. Records that share the
// audit draft fingerprint of their (kind, title) pair are one candidate, however their project
// or evidence differ, so the same friction seen many times in several projects is one entry
// with a count rather than one entry per project. The kind stands in for the where part of the
// shared fingerprint, because an improvement record carries no file path.
func improveProposeCandidates(bundle improveBundle) []improveProposeCandidate {
	byKey := map[string]int{}
	candidates := []improveProposeCandidate{}
	for _, record := range bundle.Records {
		// Only a friction kind is a candidate: the issue records are the exported issue list,
		// which suppresses candidates rather than becoming one, and the reference kinds carry
		// no occurrence to count.
		if !improveProposeFriction(record) {
			continue
		}
		title := improveProposeTitleOf(record)
		if title == "" {
			continue
		}
		key := improveProposeFingerprint(record.Kind, title)
		count := record.Count
		if count <= 0 {
			count = 1
		}
		impact := improveProposeImpactOf(record)
		sightings := improveProposeRecordSightings(record)
		project := improveProposeProjectOf(record)
		if at, ok := byKey[key]; ok {
			current := &candidates[at]
			current.Count += count
			if impact < current.Impact {
				current.Impact = impact
			}
			improveProposeAddProject(&current.Projects, project, count)
			current.Evidence = append(current.Evidence, record.Evidence...)
			improveProposeAddSightings(current, project, sightings, improveProposeSightingWeights(record, len(sightings)))
			continue
		}
		byKey[key] = len(candidates)
		candidate := improveProposeCandidate{
			Key: key, Kind: record.Kind, Title: title,
			Impact: impact, Count: count,
			Evidence: append([]string(nil), record.Evidence...),
		}
		improveProposeAddProject(&candidate.Projects, project, count)
		improveProposeAddSightings(&candidate, project, sightings, improveProposeSightingWeights(record, len(sightings)))
		candidates = append(candidates, candidate)
	}
	for i := range candidates {
		sort.Slice(candidates[i].Projects, func(a, b int) bool {
			return candidates[i].Projects[a].Project < candidates[i].Projects[b].Project
		})
		candidates[i].Evidence = improveSortedEvidence(candidates[i].Evidence)
		// The estimated cost is the number of projects the friction reaches: each one needs the
		// fix verified on its own. The bundle carries no size estimate, so a value derived from
		// the input is the only one a rerun over the same bundle reproduces exactly.
		candidates[i].Cost = len(candidates[i].Projects)
		if candidates[i].Cost == 0 {
			candidates[i].Cost = 1
		}
	}
	return candidates
}

// improveProposeFingerprint is the shared audit draft fingerprint over a candidate's identity.
// It calls the audit drafts' function rather than hashing here, so an improvement draft and an
// audit draft of the same (where, what) pair carry one key.
func improveProposeFingerprint(where, what string) string {
	return auditDraftFingerprint(where, what)
}

// improveProposeAddProject adds one record's occurrence to a candidate's project list.
func improveProposeAddProject(projects *[]improveProposeProject, project string, count int) {
	project = improveProposeProjectKey(project)
	for i := range *projects {
		if (*projects)[i].Project == project {
			(*projects)[i].Count += count
			return
		}
	}
	*projects = append(*projects, improveProposeProject{Project: project, Count: count})
}

// improveProposeProjectKey is a record's project as a candidate names it: the trimmed key, or the
// marker a record that names no project carries. A split whose relationship has no scope reaches
// this as the empty string, so an issue key never takes a project's place.
func improveProposeProjectKey(project string) string {
	if trimmed := strings.TrimSpace(project); trimmed != "" {
		return trimmed
	}
	return auditDraftOwnerUnknown
}

// improveProposeAddSightings adds the sightings one record makes to a candidate, once each, and
// remembers the project each was seen in and how many occurrences it stands for. A merge counts a
// project by the occurrences this run newly added to it, so the maps are what keep a stored count
// and a grown seen list in step. A record the candidate already carries a sighting from keeps the
// count that sighting was first given, so two rows behind one origin are not counted twice.
func improveProposeAddSightings(candidate *improveProposeCandidate, project string, sightings []auditDraftSeen, weights []int) {
	if candidate.seenProjects == nil {
		candidate.seenProjects = map[auditDraftSeen]string{}
	}
	if candidate.seenCounts == nil {
		candidate.seenCounts = map[auditDraftSeen]int{}
	}
	owner := improveProposeProjectKey(project)
	for i, sighting := range sightings {
		if improveProposeSeenHas(candidate.seen, sighting) {
			continue
		}
		candidate.seen = append(candidate.seen, sighting)
		if _, ok := candidate.seenProjects[sighting]; !ok {
			candidate.seenProjects[sighting] = owner
		}
		if _, ok := candidate.seenCounts[sighting]; !ok {
			candidate.seenCounts[sighting] = weights[i]
		}
	}
}

// improveProposeRank orders the candidates the issue fixes: by occurrence count, then by
// impact, then by the estimated cost. The key is the last tiebreak, so the same bundle always
// ranks the same way whatever order its records arrived in.
func improveProposeRank(candidates []improveProposeCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Count != candidates[j].Count {
			return candidates[i].Count > candidates[j].Count
		}
		if candidates[i].Impact != candidates[j].Impact {
			return candidates[i].Impact < candidates[j].Impact
		}
		if candidates[i].Cost != candidates[j].Cost {
			return candidates[i].Cost < candidates[j].Cost
		}
		return candidates[i].Key < candidates[j].Key
	})
}

// improveProposeIssueIndex is the exported issue list as the three keys a candidate is matched
// against: the draft fingerprints the issues were registered under, the issue keys, and the
// normalized issue titles. A candidate that carries any one of them is already on Linear and is
// not proposed again.
func improveProposeIssueIndex(bundle improveBundle) (keys, titles, fingerprints map[string]bool) {
	keys = map[string]bool{}
	titles = map[string]bool{}
	fingerprints = map[string]bool{}
	for _, record := range bundle.Records {
		if record.Kind != improveKindIssue {
			continue
		}
		if key := strings.TrimSpace(record.Key); key != "" {
			keys[key] = true
		}
		if title := auditDraftNormalizeWhat(record.What); title != "" {
			titles[title] = true
		}
		// The issue list names the fingerprint the issue was registered under, which is the
		// candidate key itself, so a renamed issue still suppresses its candidate.
		if fingerprint := strings.TrimSpace(record.Fingerprint); fingerprint != "" {
			fingerprints[fingerprint] = true
		}
	}
	return keys, titles, fingerprints
}

// improveProposeBodySection is the text of one section of a stored improve body: from the last
// occurrence of its heading to the last occurrence of the next one. The renderer writes the reason
// into What first and the four sections in order after it, so a heading inside the reason is always
// earlier than the section this feature wrote, and the last occurrence is the real one. Reading the
// first occurrence would let a reason that quotes a heading be parsed as a project list or an
// evidence list of its own.
func improveProposeBodySection(body, heading, next string) (string, bool) {
	start := strings.LastIndex(body, heading)
	if start < 0 {
		return "", false
	}
	rest := body[start+len(heading):]
	if next != "" {
		if end := strings.LastIndex(rest, next); end >= 0 {
			rest = rest[:end]
		}
	}
	return rest, true
}

// improveProposeParseProjects reads the projects a stored improve body names, so a later run
// can carry them into the rewritten body. Only this feature's own body is parsed.
func improveProposeParseProjects(body string) []improveProposeProject {
	rest, ok := improveProposeBodySection(body, "## Where\n\n", "## Seen\n\n")
	if !ok {
		return nil
	}
	var out []improveProposeProject
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		line = strings.TrimPrefix(line, "- ")
		open := strings.LastIndex(line, " (")
		if open < 0 || !strings.HasSuffix(line, ")") {
			continue
		}
		count := 0
		if _, err := fmt.Sscanf(line[open+2:len(line)-1], "%d", &count); err != nil {
			continue
		}
		out = append(out, improveProposeProject{Project: strings.TrimSpace(line[:open]), Count: count})
	}
	return out
}

// improveProposeParseEvidence reads the evidence locations a stored improve body names.
func improveProposeParseEvidence(body string) []string {
	rest, ok := improveProposeBodySection(body, "## Evidence\n\n", "")
	if !ok {
		return nil
	}
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(line, "- ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	return out
}

// improveProposeIssueKeys is the issue keys a record's evidence names, the ones an earlier build
// could have taken for a project. A split whose relationship carried no scope keeps its issue key
// as issue:<KEY>, so a stored draft that took that key for its project can lose it on the next run.
func improveProposeIssueKeys(evidence []string) map[string]bool {
	keys := map[string]bool{}
	for _, entry := range evidence {
		trimmed := strings.TrimSpace(entry)
		if key := strings.TrimSpace(strings.TrimPrefix(trimmed, improveEvidenceIssuePrefix)); strings.HasPrefix(trimmed, improveEvidenceIssuePrefix) && key != "" {
			keys[key] = true
		}
	}
	return keys
}

// improveProposeMergeProjects merges the projects a stored body names with the ones the new
// candidate reached: a project's count is the count the draft already held plus the occurrences
// this run newly recorded there, and a project the new run no longer carries keeps its count, so a
// rewrite never drops a project and rerunning one bundle never grows a count.
//
// One stored project is dropped: a key this run's own evidence names as an issue key. An earlier
// build took the issue key of a split whose relationship carried no scope for the project, and an
// issue key is never a project, so a draft written then must lose it on the next run rather than
// keep a false owner. The key stays in the body's evidence as issue:<KEY>.
func improveProposeMergeProjects(stored, current []improveProposeProject, added, moved map[string]int, issueKeys map[string]bool) []improveProposeProject {
	out := make([]improveProposeProject, 0, len(stored))
	carried := 0
	for _, project := range stored {
		if project.Project != "" && issueKeys[project.Project] {
			// The occurrences are real, only the key was never a project: they move to the
			// unknown owner rather than being dropped with the false name.
			carried += project.Count
			continue
		}
		out = append(out, project)
	}
	if carried > 0 {
		found := false
		for i := range out {
			if out[i].Project == auditDraftOwnerUnknown {
				out[i].Count += carried
				found = true
			}
		}
		if !found {
			out = append(out, improveProposeProject{Project: auditDraftOwnerUnknown, Count: carried})
		}
	}
	// Each project's count is what the draft held plus the occurrences this run added there, plus
	// the occurrences that moved to or from it because their owner changed. A project whose
	// occurrences all moved away carries no count and is not kept.
	kept := out[:0]
	for _, project := range out {
		count := improveProposeMergedCount(project.Project, project.Count+added[project.Project]+moved[project.Project], current)
		if count <= 0 {
			continue
		}
		kept = append(kept, improveProposeProject{Project: project.Project, Count: count})
	}
	out = kept
	for _, project := range current {
		found := false
		for i := range out {
			if out[i].Project == project.Project {
				found = true
			}
		}
		if !found {
			out = append(out, improveProposeProject{Project: project.Project,
				Count: improveProposeMergedCount(project.Project, added[project.Project]+moved[project.Project], current)})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Project < out[b].Project })
	return out
}

// improveProposeMergedCount is the count one project carries after a merge: what the draft held
// plus the occurrences this run newly recorded there, or what the run own records report for that
// project when that is more. A source that aggregates its occurrences (a fault ledger row, whose
// count moves with no new origin location) reports a total the incremental sum does not reach, and
// the body must not understate it. A project the new run no longer carries is not passed here at
// all, so it keeps the count the draft already held.
func improveProposeMergedCount(project string, incremental int, current []improveProposeProject) int {
	for _, entry := range current {
		if entry.Project == project && entry.Count > incremental {
			incremental = entry.Count
		}
	}
	return incremental
}

// improveProposeSightingWeights is how many occurrences each of a record's sightings stands for.
// The record's own count is what the bundle reports for the friction it names, so the weights add
// up to it and the count is never lost: a record that aggregates its occurrences into one row (a
// fault ledger row, whose count moves while its origin stays the same) reports its whole count at
// that one origin, and a record that merged several origins shares its count among them as evenly
// as whole occurrences allow. A record whose count is not a whole multiple of its origins still
// reports its own total, with the remainder on the earliest origins, so the sum is exactly the
// count the source gave.
func improveProposeSightingWeights(record improveRecord, sightings int) []int {
	count := record.Count
	if count <= 0 {
		count = 1
	}
	if sightings <= 1 {
		return []int{count}
	}
	base, remainder := count/sightings, count%sightings
	weights := make([]int, sightings)
	for i := range weights {
		weights[i] = base
		if i < remainder {
			weights[i]++
		}
	}
	return weights
}

// improveProposeReconcileSightings brings a stored seen list and the counts its draft holds up to
// date with this run's candidate, and reports what changed. One origin is one sighting: an origin
// the draft already carries is not added again, and the owner its occurrence is counted under
// follows the latest reading, so learning the project of an existing split moves its count from
// the unknown owner to the project instead of counting the origin twice. An origin the draft does
// not carry yet adds one to the project this run saw it in. The owner moves are returned in a
// second map, because a stored project's count is otherwise carried forward unchanged.
func improveProposeReconcileSightings(candidate improveProposeCandidate, stored []auditDraftSeen, issueKeys map[string]bool) (seen []auditDraftSeen, added, moved map[string]int) {
	seen = append([]auditDraftSeen(nil), stored...)
	added = map[string]int{}
	moved = map[string]int{}
	for _, sighting := range candidate.seen {
		owner := improveProposeProjectKey(candidate.seenProjects[sighting])
		at := improveProposeSeenIndex(seen, sighting)
		if at < 0 {
			seen = append(seen, sighting)
			added[owner] += improveProposeSightingCount(candidate, sighting)
			continue
		}
		// The occurrence is already recorded. Its owner in this run may differ from the stored one:
		// a relationship that had no scope may have gained its project, so the count follows the
		// latest reading rather than staying under the owner it was first seen under.
		//
		// Only a split's subject is its project. Every other kind's subject is its own identity
		// text (a refusal's reason, a fault's class), and such a record never names a project, so
		// reading its subject as one would invent a project and count the occurrence twice.
		was := auditDraftOwnerUnknown
		if candidate.Kind == improveKindSplit {
			was = improveProposeProjectKey(seen[at].Subject)
		}
		seen[at].Subject = sighting.Subject
		seen[at].At = sighting.At
		// A stored owner this run's own evidence names as an issue key is not moved here: the merge
		// already drops that false project and carries its occurrences to the unknown owner, so
		// moving them again would count the same occurrence twice.
		if was != owner && !issueKeys[was] {
			count := improveProposeSightingCount(candidate, sighting)
			moved[was] -= count
			moved[owner] += count
		}
	}
	return seen, added, moved
}

// improveProposeSightingCount is how many occurrences one of a candidate's sightings stands for.
func improveProposeSightingCount(candidate improveProposeCandidate, sighting auditDraftSeen) int {
	if count := candidate.seenCounts[sighting]; count > 0 {
		return count
	}
	return 1
}

// improveProposeSuppressed reports whether an exported issue already covers a candidate: the issue
// list registered it under the candidate's own fingerprint, its key is the candidate key, or its
// title normalizes to the candidate's title.
func improveProposeSuppressed(candidate improveProposeCandidate, keys, titles, fingerprints map[string]bool) bool {
	if candidate.Key != "" && fingerprints[candidate.Key] {
		return true
	}
	if keys[candidate.Key] {
		return true
	}
	return titles[auditDraftNormalizeWhat(candidate.Title)]
}

// improveProposeDraftBody is a candidate's draft body: what the friction is, which projects
// it reaches, every origin it was seen at, and the locations a reader can check.
func improveProposeDraftBody(candidate improveProposeCandidate) string {
	var b strings.Builder
	b.WriteString("## What\n\n")
	b.WriteString(candidate.Title)
	b.WriteString("\n\n## Where\n\n")
	if len(candidate.Projects) == 0 {
		b.WriteString("(the friction names no project)\n")
	} else {
		for _, project := range candidate.Projects {
			fmt.Fprintf(&b, "- %s (%d)\n", project.Project, project.Count)
		}
	}
	b.WriteString("\n## Seen\n\n")
	b.WriteString(improveProposeSeenLines(candidate.seen))
	b.WriteString("\n## Evidence\n\n")
	b.WriteString(improveProposeEvidenceLines(candidate.Evidence))
	return b.String()
}

// improveProposeSeenLines is a sighting list as the body's seen section writes it.
func improveProposeSeenLines(seen []auditDraftSeen) string {
	var b strings.Builder
	for _, sighting := range seen {
		fmt.Fprintf(&b, "- source=%s subject=%s where=%s at=%s\n", sighting.Mode, sighting.Subject, sighting.Head, sighting.At)
	}
	return b.String()
}

// improveProposeEvidenceLines is an origin list as the body's evidence section writes it.
func improveProposeEvidenceLines(evidence []string) string {
	if len(evidence) == 0 {
		return "(the bundle recorded no origin location)\n"
	}
	var b strings.Builder
	for _, location := range evidence {
		b.WriteString("- " + location + "\n")
	}
	return b.String()
}

// improveProposeOwner is the project a draft names when it is drafted from several: the first
// known one, in sorted order, so the same bundle always names the same owner and an unknown
// project never takes the owner away from a known one.
func improveProposeOwner(projects []improveProposeProject) string {
	for _, project := range projects {
		if project.Project != "" && project.Project != auditDraftOwnerUnknown {
			return project.Project
		}
	}
	return ""
}

// improveProposeSeverity is the draft severity an impact rank maps to: a blockage or a
// needs_changes is P1 and everything else is P2, which keeps the draft comparable with the
// audit drafts without claiming a grade the bundle did not carry.
func improveProposeSeverity(impact int) string {
	if impact <= improveProposeImpactHigh {
		return "P1"
	}
	return "P2"
}

// improveProposeDraft is the draft one candidate becomes: the shared crw-issue-draft/1 shape,
// with this feature's source token and the candidate's key.
func improveProposeDraft(candidate improveProposeCandidate) *auditDraft {
	severity := improveProposeSeverity(candidate.Impact)
	return &auditDraft{
		Schema:      auditDraftSchema,
		Fingerprint: candidate.Key,
		Source:      improveProposeSource,
		Project:     improveProposeOwner(candidate.Projects),
		Title:       auditDraftTruncateRunes(candidate.Title, auditDraftTitleLimit),
		Severity:    severity,
		Body:        improveProposeDraftBody(candidate),
		Labels:      []string{improveProposeSource, severity},
		Seen:        candidate.seen,
		State:       auditDraftStateDraft,
	}
}

// improveProposeRun turns one bundle into drafts. It holds the drafts lock for the whole
// read-modify-write, so two proposes cannot create or grow one draft at once, and it only
// grows the seen list of a draft that already exists, so a rerun over the same bundle creates
// nothing new. A context that ended before the lock or during the write loop produces no new
// draft: an interrupted run never looks like a completed one.
func improveProposeRun(ctx context.Context, e *Env, bundlePath string, dryRun bool) (improveProposeReport, error) {
	return improveProposeRunCapped(ctx, e, bundlePath, dryRun, -1)
}

// improveProposeRunCapped is improveProposeRun with an override for the new-draft cap. A negative
// override keeps the configured cap; zero creates no new draft at all, which is how a run whose
// boundary and ref already have a roadmap leaves the candidates it could not draft for a later
// run instead of drafting more of them.
func improveProposeRunCapped(ctx context.Context, e *Env, bundlePath string, dryRun bool, capOverride int) (improveProposeReport, error) {
	report := improveProposeReport{
		Schema: improveProposeReportSchema, BundlePath: bundlePath,
		Candidates: []improveProposeCandidate{},
		Created:    []auditDraftSummary{},
		Updated:    []auditDraftSummary{},
		Suppressed: []string{},
		DryRun:     dryRun,
	}
	bundle, err := improveProposeReadBundle(bundlePath)
	if err != nil {
		return report, err
	}
	section, err := improveLoadSection(e)
	if err != nil {
		return report, err
	}
	maxNew := section.MaxNewDrafts
	if maxNew <= 0 {
		maxNew = auditDraftDefaultMaxNewDrafts
	}
	if capOverride >= 0 {
		maxNew = capOverride
	}
	candidates := improveProposeCandidates(bundle)
	keys, titles, fingerprints := improveProposeIssueIndex(bundle)
	kept := make([]improveProposeCandidate, 0, len(candidates))
	// A candidate the exported issue list already covers creates no new draft, but a draft
	// that already exists for its fingerprint still grows its seen list: the issue list says
	// the friction is registered, not that this run did not see it again.
	suppressedExisting := make([]improveProposeCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if improveProposeSuppressed(candidate, keys, titles, fingerprints) {
			report.Suppressed = append(report.Suppressed, candidate.Key)
			suppressedExisting = append(suppressedExisting, candidate)
			continue
		}
		kept = append(kept, candidate)
	}
	sort.Strings(report.Suppressed)
	improveProposeRank(kept)
	report.Candidates = kept
	if dryRun {
		return report, nil
	}
	cfg := coreDefaults(e)
	release, err := auditDraftLock(e, cfg)
	if err != nil {
		return report, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	dir := auditDraftDir(e, cfg)
	if _, err := auditDraftIndexLoad(dir); err != nil {
		return report, err
	}
	fresh := 0
	for _, candidate := range append(append([]improveProposeCandidate{}, kept...), suppressedExisting...) {
		// The context is checked before each durable effect, so a cancellation during the
		// write loop stops before the next draft rather than after it.
		if err := ctx.Err(); err != nil {
			return report, err
		}
		path := filepath.Join(dir, candidate.Key+".json")
		doc, err := auditDraftLoad(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return report, err
			}
			// A suppressed candidate never creates a draft; only an existing one grows.
			if improveProposeSuppressed(candidate, keys, titles, fingerprints) {
				continue
			}
			if fresh >= maxNew {
				report.Remaining++
				continue
			}
			if err := auditDraftSave(path, improveProposeDraft(candidate)); err != nil {
				return report, err
			}
			fresh++
			saved, err := auditDraftLoad(path)
			if err != nil {
				return report, err
			}
			report.Created = append(report.Created, auditDraftSummaryOf(saved))
			continue
		}
		// A draft with this fingerprint already exists, from this feature or from the audit. The
		// whole record is rewritten, so only fields this command understands may change: the seen
		// list gains the origins this run reached, and an occurrence whose owner changed is counted
		// under its latest owner. The evidence is the union of what the draft holds and what this
		// run reached, and the issue keys it names are what an earlier build could have taken for a
		// project. The added and moved counts are read before the seen list is updated, so they
		// name only what this run newly recorded or re-owned.
		evidence := improveSortedEvidence(append(improveProposeParseEvidence(doc.Body), candidate.Evidence...))
		issueKeys := improveProposeIssueKeys(evidence)
		seen, added, moved := improveProposeReconcileSightings(candidate, doc.Seen, issueKeys)
		changed := len(seen) != len(doc.Seen)
		doc.Seen = seen
		if doc.Source == improveProposeSource {
			// The whole record is rewritten, so the projects and the evidence the new run reached
			// are folded in rather than dropped: a project's count is what the draft already held
			// plus the occurrences this run newly recorded there, and the evidence is the union.
			merged := improveProposeCandidate{
				// The reason is this run's own text, which is the whole reason the fingerprint was
				// taken over. Reading it back out of the rendered body could not tell a Markdown
				// heading inside the reason from the end of its section, so a multiline reason
				// would be cut short on the next run.
				Title:    candidate.Title,
				Projects: improveProposeMergeProjects(improveProposeParseProjects(doc.Body), candidate.Projects, added, moved, issueKeys),
				Evidence: evidence,
				seen:     doc.Seen,
			}
			// A run whose records report a larger total for a project than the sightings alone
			// imply grows the body without adding a sighting, so the rewrite is judged by the
			// body it would write and not by the seen list alone.
			body := improveProposeDraftBody(merged)
			if !changed && body == doc.Body {
				continue
			}
			doc.Project = improveProposeOwner(merged.Projects)
			doc.Body = body
		} else {
			if !changed {
				continue
			}
			// A draft the audit wrote keeps its own body, and only its sighting section is
			// advanced, so the management session reading the body sees the sighting this run
			// recorded rather than a body that contradicts the file.
			doc.Body = auditDraftSeenSection(doc.Body, doc.Seen)
		}
		if err := auditDraftSave(path, doc); err != nil {
			return report, err
		}
		report.Updated = append(report.Updated, auditDraftSummaryOf(doc))
	}
	if err := auditDraftIndexSave(dir); err != nil {
		return report, err
	}
	return report, nil
}

// improveRunPropose is crw manage improve propose. It prints the report one run produced.
func improveRunPropose(ctx context.Context, e *Env, args []string) int {
	bundle, dryRun, help, err := improveProposeParseArgs(args)
	if help {
		fmt.Fprintln(e.Stdout, improveProposeUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(e.Stderr, improveProposeUsage)
		fmt.Fprintf(e.Stderr, "crw manage improve propose: error: %v\n", err)
		return usageExit
	}
	report, err := improveProposeRun(ctx, e, bundle, dryRun)
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve propose: error: %v\n", err)
		return 1
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve propose: error: %v\n", err)
		return 1
	}
	if _, err := e.Stdout.Write(append(data, '\n')); err != nil {
		fmt.Fprintf(e.Stderr, "crw manage improve propose: error: %v\n", err)
		return 1
	}
	return 0
}
