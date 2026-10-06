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

// improveProposeCriteriaTitle is the title a criteria record's candidate carries: the
// record's own what is the string "registered" or "current_set", which names nothing.
const improveProposeCriteriaTitle = "criteria set"

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

// improveProposeTitleOf is the title a record's candidate carries. A criteria record's own
// what is the string "registered" or "current_set", which names nothing, so it reads as the
// criteria set; an audit record's what is a verdict, so it reads as the audit finding it is.
func improveProposeTitleOf(record improveRecord) string {
	switch record.Kind {
	case improveKindCriteria:
		return improveProposeCriteriaTitle
	case improveKindAudit:
		if status := strings.TrimSpace(record.What); status != "" {
			return "audit finding " + status
		}
		return "audit finding"
	}
	if what := strings.TrimSpace(record.What); what != "" {
		return what
	}
	return strings.TrimSpace(record.Key)
}

// improveProposeProjectOf is the project a record happened in, so the candidate names its
// owners. A record whose kind carries no project reads as the empty one.
func improveProposeProjectOf(record improveRecord) string {
	switch record.Kind {
	case improveKindSplit, improveKindCriteria, improveKindAudit:
		return strings.TrimSpace(record.Key)
	case improveKindFault, improveKindGeneration, improveKindDag, improveKindDraft:
		return strings.TrimSpace(record.Where)
	}
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
		if strings.TrimSpace(record.What) == "needs_changes_revision" {
			return improveProposeImpactHigh
		}
		return improveProposeImpactMedium
	case improveKindFault, improveKindIntervention:
		return improveProposeImpactMedium
	case improveKindAudit:
		switch strings.ToUpper(strings.TrimSpace(record.What)) {
		case "P0", "P1":
			return improveProposeImpactHigh
		}
		return improveProposeImpactMedium
	}
	return improveProposeImpactLow
}

// improveProposeSighting is one origin a record was seen at, in the audit draft seen shape the
// shared crw-issue-draft/1 format carries: the source, what the record is about, where it
// happened, and when it was last seen. Two records with one origin are one sighting, so a
// rerun over the same bundle never doubles a seen entry.
func improveProposeSighting(record improveRecord) auditDraftSeen {
	return auditDraftSeen{
		Mode:    improveProposeSource,
		Subject: strings.TrimSpace(record.Key),
		Head:    strings.TrimSpace(record.Where),
		At:      strings.TrimSpace(record.LastAt),
	}
}

// improveProposeCandidates groups the bundle's records into candidates. Records that share the
// audit draft fingerprint of their (kind, title) pair are one candidate, however their project
// or evidence differ, so the same kind of friction seen many times in several projects is one
// entry with a count rather than one entry per project. The kind stands in for the where part
// of the shared fingerprint: an improvement record carries no file path, and keying on the
// project would split one repeated friction into one candidate per project.
func improveProposeCandidates(bundle improveBundle) []improveProposeCandidate {
	byKey := map[string]int{}
	candidates := []improveProposeCandidate{}
	for _, record := range bundle.Records {
		// An issue record is the exported issue list, which suppresses candidates rather than
		// becoming one.
		if record.Kind == improveKindIssue {
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
		sighting := improveProposeSighting(record)
		project := improveProposeProjectOf(record)
		if at, ok := byKey[key]; ok {
			current := &candidates[at]
			current.Count += count
			if impact < current.Impact {
				current.Impact = impact
			}
			improveProposeAddProject(&current.Projects, project, count)
			current.Evidence = append(current.Evidence, record.Evidence...)
			if !auditDraftSeenHas(current.seen, sighting) {
				current.seen = append(current.seen, sighting)
			}
			continue
		}
		byKey[key] = len(candidates)
		candidate := improveProposeCandidate{
			Key: key, Kind: record.Kind, Title: title,
			Impact: impact, Count: count,
			Evidence: append([]string(nil), record.Evidence...),
			seen:     []auditDraftSeen{sighting},
		}
		improveProposeAddProject(&candidate.Projects, project, count)
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
	project = strings.TrimSpace(project)
	if project == "" {
		project = auditDraftOwnerUnknown
	}
	for i := range *projects {
		if (*projects)[i].Project == project {
			(*projects)[i].Count += count
			return
		}
	}
	*projects = append(*projects, improveProposeProject{Project: project, Count: count})
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

// improveProposeIssueIndex is the exported issue list as the two keys a candidate is matched
// against: the issue keys, and the normalized issue titles. A candidate that carries either
// one is already on Linear and is not proposed again.
func improveProposeIssueIndex(bundle improveBundle) (keys, titles map[string]bool) {
	keys = map[string]bool{}
	titles = map[string]bool{}
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
	}
	return keys, titles
}

// improveProposeSuppressed reports whether an exported issue already covers a candidate: the
// same fingerprint (an issue whose key is the candidate key) or the same title key.
func improveProposeSuppressed(candidate improveProposeCandidate, keys, titles map[string]bool) bool {
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

// improveProposeSeenSection replaces the seen section of a stored improve body with the
// rendered sighting list, so a draft a later run saw again names that run in the body the
// management session reads. The defect description, the projects and the evidence stay as
// first written; only this feature's own body is rewritten, never another feature's.
func improveProposeSeenSection(body string, seen []auditDraftSeen) string {
	const startMarker = "## Seen\n\n"
	const endMarker = "\n## Evidence\n\n"
	start := strings.Index(body, startMarker)
	if start < 0 {
		return body
	}
	rest := body[start+len(startMarker):]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return body
	}
	return body[:start+len(startMarker)] + improveProposeSeenLines(seen) + endMarker + rest[end+len(endMarker):]
}

// improveProposeOwner is the project a draft names when it is drafted from several: the first
// one, in sorted order, so the same bundle always names the same owner.
func improveProposeOwner(projects []improveProposeProject) string {
	if len(projects) == 0 || projects[0].Project == auditDraftOwnerUnknown {
		return ""
	}
	return projects[0].Project
}

// improveProposeSeverity is the draft severity an impact rank maps to: a blockage or a
// needs_changes is P1 and everything else is P2, which keeps the draft comparable with the
// audit drafts without claiming a grade the bundle did not carry.
func improveProposeSeverity(impact int) string {
	if impact <= improveProposeImpactMedium {
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
// nothing new.
func improveProposeRun(e *Env, bundlePath string, dryRun bool) (improveProposeReport, error) {
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
	candidates := improveProposeCandidates(bundle)
	keys, titles := improveProposeIssueIndex(bundle)
	kept := make([]improveProposeCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if improveProposeSuppressed(candidate, keys, titles) {
			report.Suppressed = append(report.Suppressed, candidate.Key)
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
	dir := auditDraftDir(e, cfg)
	if _, err := auditDraftIndexLoad(dir); err != nil {
		return report, err
	}
	fresh := 0
	for _, candidate := range kept {
		path := filepath.Join(dir, candidate.Key+".json")
		doc, err := auditDraftLoad(path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return report, err
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
		// list only grows, and only this feature's own body is re-rendered.
		changed := false
		for _, sighting := range candidate.seen {
			if !auditDraftSeenHas(doc.Seen, sighting) {
				doc.Seen = append(doc.Seen, sighting)
				changed = true
			}
		}
		if !changed {
			continue
		}
		if doc.Source == improveProposeSource {
			doc.Body = improveProposeSeenSection(doc.Body, doc.Seen)
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
func improveRunPropose(_ context.Context, e *Env, args []string) int {
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
	report, err := improveProposeRun(e, bundle, dryRun)
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
