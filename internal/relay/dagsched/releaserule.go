package dagsched

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// OverlapCounts are overlaps by grade: for one candidate, the holders it overlaps by the worst grade of their overlap; for a pass, the sum over the candidates it judged.
type OverlapCounts struct{ Mechanical, Local, Exclusive int }

// Counted is the overlaps that count: a mechanical overlap is settled by its rule at merge time and is left out.
func (c OverlapCounts) Counted() int { return c.Local + c.Exclusive }

func (c *OverlapCounts) add(o OverlapCounts) {
	c.Mechanical += o.Mechanical
	c.Local += o.Local
	c.Exclusive += o.Exclusive
}

func (c OverlapCounts) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "mechanical", Value: c.Mechanical}, {Key: "local", Value: c.Local}, {Key: "exclusive", Value: c.Exclusive}}
}

// BasisRow is one overlap a judgement rests on: the holder, the place both touch (the deeper of the two paths) in a repository, the grade of the overlap, the grade and rule each side was judged
// at, and the recent conflict observations of the plan on that place. An undeclared side has no place and no grades and says which side it is.
type BasisRow struct {
	Holder, Repository, Path, Undeclared, Grade            string
	CandidateGrade, CandidateRule, HolderGrade, HolderRule string
	Observations, Conflicts, Unattributed                  int
}

// ReleaseJudgement is how an implementation candidate is released and on what basis (CRW-409). The decision follows the grades alone; the observations in the basis are evidence about the
// places the overlaps are on.
type ReleaseJudgement struct {
	Rule     string
	Overlaps OverlapCounts
	Basis    []BasisRow // worst grade first, at most MaxBasisRows
	Omitted  int        // rows left out of Basis
	// HeldByPolicy is set when the release policy has switched local-optimistic release off and the candidate's worst overlap is local: the rule then reads defer (CRW-411, optimism.go). PolicyReason
	// is the policy's reason, for the detail.
	HeldByPolicy bool
	PolicyReason string
}

func optionalOrNil(s string) any { return optionalText(s) }

func (b BasisRow) object() contract.OrderedObject {
	o := contract.OrderedObject{{Key: "holder", Value: b.Holder}, {Key: "repository", Value: optionalOrNil(b.Repository)}, {Key: "path", Value: optionalOrNil(b.Path)}, {Key: "grade", Value: b.Grade},
		{Key: "candidate_grade", Value: optionalOrNil(b.CandidateGrade)}, {Key: "candidate_rule", Value: optionalOrNil(b.CandidateRule)},
		{Key: "holder_grade", Value: optionalOrNil(b.HolderGrade)}, {Key: "holder_rule", Value: optionalOrNil(b.HolderRule)},
		{Key: "recent_observations", Value: b.Observations}, {Key: "recent_conflicts", Value: b.Conflicts}, {Key: "unattributed_conflicts", Value: b.Unattributed}}
	if b.Undeclared != "" {
		o = append(o, contract.Field{Key: "undeclared", Value: b.Undeclared})
	}
	return o
}

func (b BasisRow) canonical() map[string]any {
	m := map[string]any{"holder": b.Holder, "grade": b.Grade, "recent_observations": b.Observations, "recent_conflicts": b.Conflicts, "unattributed_conflicts": b.Unattributed}
	for key, value := range map[string]string{"repository": b.Repository, "path": b.Path, "candidate_grade": b.CandidateGrade, "candidate_rule": b.CandidateRule,
		"holder_grade": b.HolderGrade, "holder_rule": b.HolderRule, "undeclared": b.Undeclared} {
		if value != "" {
			m[key] = value
		}
	}
	return m
}

func (j ReleaseJudgement) object() contract.OrderedObject {
	basis := make([]any, len(j.Basis))
	for i, b := range j.Basis {
		basis[i] = b.object()
	}
	o := contract.OrderedObject{{Key: "rule", Value: j.Rule}, {Key: "overlaps", Value: j.Overlaps.object()}, {Key: "basis", Value: basis}, {Key: "basis_omitted", Value: j.Omitted}}
	if j.HeldByPolicy {
		o = append(o, contract.Field{Key: "held_by_policy", Value: true})
	}
	return o
}

// canonical is the judgement as a pass record and the reading's digest keep it: a map of plain values, absent where a field is empty.
func (j ReleaseJudgement) canonical() map[string]any {
	basis := make([]any, len(j.Basis))
	for i, b := range j.Basis {
		basis[i] = b.canonical()
	}
	m := map[string]any{"rule": j.Rule, "basis": basis, "basis_omitted": j.Omitted,
		"overlaps": map[string]any{"mechanical": j.Overlaps.Mechanical, "local": j.Overlaps.Local, "exclusive": j.Overlaps.Exclusive}}
	if j.HeldByPolicy {
		m["held_by_policy"] = true
	}
	return m
}

// applyPolicy turns a local-optimistic judgement into a deferral while the release policy has local-optimistic release off (CRW-411): a local overlap then defers like an exclusive one. Every
// other rule is left as judged (independent and mechanical releases overlap nothing that counts, and an exclusive overlap defers already), and a policy that is on, or no policy, changes nothing.
func (j *ReleaseJudgement) applyPolicy(policy *OptimismState) {
	if policy == nil || policy.On || j.Rule != RuleLocalOptimistic {
		return
	}
	j.Rule, j.HeldByPolicy, j.PolicyReason = RuleDefer, true, policy.Reason
}

// deferDetail is the detail of a candidate the judgement cut: the sentence the reading always gave, then the rule, the row that decided it (the first exclusive one) and the counts.
func (j ReleaseJudgement) deferDetail() string {
	detail := "its edit regions overlap a node that is running or accepted and not yet landed, or a region is undeclared; release rule " + RuleDefer + ":"
	if j.HeldByPolicy {
		detail = "its edit regions overlap a node that is running or accepted and not yet landed; release rule " + RuleDefer + ": local-optimistic release is switched off by the release policy (" + j.PolicyReason + ");"
		for _, b := range j.Basis {
			if b.Grade == GradeLocal {
				detail += " local overlap with " + b.Holder + " on " + b.Path + " (candidate " + describeGrade(b.CandidateGrade, b.CandidateRule) + ", holder " + describeGrade(b.HolderGrade, b.HolderRule) + ")"
				break
			}
		}
		return detail + "; overlaps: mechanical " + itoa64(int64(j.Overlaps.Mechanical)) + ", local " + itoa64(int64(j.Overlaps.Local)) + ", exclusive " + itoa64(int64(j.Overlaps.Exclusive))
	}
	for _, b := range j.Basis {
		if b.Grade != GradeExclusive {
			continue
		}
		if b.Undeclared != "" {
			detail += " exclusive overlap with " + b.Holder + " (the " + b.Undeclared + " is undeclared)"
		} else {
			detail += " exclusive overlap with " + b.Holder + " on " + b.Path + " (candidate " + describeGrade(b.CandidateGrade, b.CandidateRule) + ", holder " + describeGrade(b.HolderGrade, b.HolderRule) + ")"
		}
		break
	}
	return detail + "; overlaps: mechanical " + itoa64(int64(j.Overlaps.Mechanical)) + ", local " + itoa64(int64(j.Overlaps.Local)) + ", exclusive " + itoa64(int64(j.Overlaps.Exclusive))
}

func describeGrade(grade, rule string) string {
	if rule == "" {
		return grade
	}
	return grade + " (" + rule + ")"
}

// A conflict observation of the plan, with the files it could not merge by the repository name they were recorded under.
type observedConflict struct {
	conflicts int
	files     map[string][]string
}

// observations are the plan's latest RecentObservations conflict observations, newest first. They are read once per reading and only when a candidate has something to overlap.
type observations struct {
	ctx   context.Context
	q     store.Querier
	plan  string
	read  bool
	items []observedConflict
	err   error
}

func tableExists(ctx context.Context, q store.Querier, name string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (o *observations) load() ([]observedConflict, error) {
	if o.read {
		return o.items, o.err
	}
	o.read = true
	// a zone that predates the file rows is read as observations whose files were not recorded; a read-only open creates nothing, so it can meet one
	withFiles, err := tableExists(o.ctx, o.q, "dag_conflict_observation_files")
	if err != nil {
		o.err = err
		return nil, err
	}
	query := "SELECT o.observation_id, o.conflict_count, '', '' FROM (SELECT observation_id, conflict_count, observed_at FROM dag_conflict_observations WHERE plan_id = ? ORDER BY observed_at DESC, observation_id DESC LIMIT ?) o ORDER BY o.observed_at DESC, o.observation_id DESC"
	if withFiles {
		query = "SELECT o.observation_id, o.conflict_count, COALESCE(f.repository, ''), COALESCE(f.path, '') FROM (SELECT observation_id, conflict_count, observed_at FROM dag_conflict_observations WHERE plan_id = ? ORDER BY observed_at DESC, observation_id DESC LIMIT ?) o" +
			" LEFT JOIN dag_conflict_observation_files f ON f.observation_id = o.observation_id ORDER BY o.observed_at DESC, o.observation_id DESC, f.repository, f.path"
	}
	rows, err := o.q.QueryContext(o.ctx, query, o.plan, RecentObservations)
	if err != nil {
		o.err = err
		return nil, err
	}
	defer rows.Close()
	last := ""
	for rows.Next() {
		var id, repository, file string
		var conflicts int
		if err := rows.Scan(&id, &conflicts, &repository, &file); err != nil {
			o.err = err
			return nil, err
		}
		if id != last {
			o.items = append(o.items, observedConflict{conflicts: conflicts, files: map[string][]string{}})
			last = id
		}
		if file != "" {
			item := &o.items[len(o.items)-1]
			item.files[repository] = append(item.files[repository], file)
		}
	}
	o.err = rows.Err()
	return o.items, o.err
}

// about is what the latest observations say of a place in a repository: how many there are, how many conflicted in a file of the place, and how many conflicted without naming any file of this
// repository (observed before the files were recorded, or in another repository, or under a name that could not be tied to this one).
func (o *observations) about(repository, place string) (seen, conflicted, unattributed int, err error) {
	items, err := o.load()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, item := range items {
		names := item.files[repository]
		switch {
		case len(names) > 0:
			for _, name := range names {
				if within(name, place) {
					conflicted++
					break
				}
			}
		case item.conflicts > 0:
			unattributed++
		}
	}
	return len(items), conflicted, unattributed, nil
}

// placeOf is the place an overlap is on: the deeper of the two paths, or the path of the region that holds the whole repository.
func placeOf(a, b Region) string {
	switch {
	case a.Exclusive:
		return path.Clean(a.Path)
	case b.Exclusive:
		return path.Clean(b.Path)
	}
	if place, _, ok := commonPlace(a, b); ok {
		return place
	}
	return path.Clean(a.Path)
}

// judgeRelease judges a candidate against everything that holds regions. Per holder the worst grade over every pair of regions counts once; the candidate is released under the worst of them:
// no overlap is independent, mechanical overlaps are mechanical, a local one is local-optimistic and an exclusive one defers it. An undeclared node (the candidate or the holder) overlaps every other and
// is exclusive. The basis rows are every overlapping pair of regions with what the plan's latest conflict observations say of the place, worst grade first.
func judgeRelease(candidate holder, holders []holder, seen *observations) (ReleaseJudgement, error) {
	j := ReleaseJudgement{Rule: RuleIndependent}
	var rows []BasisRow
	for _, h := range holders {
		worst := ""
		switch {
		case candidate.Unknown:
			worst = GradeExclusive
			rows = append(rows, BasisRow{Holder: h.NodeID, Grade: GradeExclusive, Undeclared: "candidate"})
		case h.Unknown:
			worst = GradeExclusive
			rows = append(rows, BasisRow{Holder: h.NodeID, Grade: GradeExclusive, Undeclared: "holder"})
		default:
			for _, a := range candidate.Regions {
				for _, b := range h.Regions {
					grade := PairGrade(a, b)
					if grade == "" {
						continue
					}
					worst = worse(worst, grade)
					place := placeOf(a, b)
					seenN, conflicted, unattributed, err := seen.about(a.Repository, place)
					if err != nil {
						return ReleaseJudgement{}, err
					}
					ga, ra := foldedGrade(a)
					gb, rb := foldedGrade(b)
					rows = append(rows, BasisRow{Holder: h.NodeID, Repository: a.Repository, Path: place, Grade: grade, CandidateGrade: ga, CandidateRule: ra, HolderGrade: gb, HolderRule: rb,
						Observations: seenN, Conflicts: conflicted, Unattributed: unattributed})
				}
			}
		}
		switch worst {
		case GradeMechanical:
			j.Overlaps.Mechanical++
		case GradeLocal:
			j.Overlaps.Local++
		case GradeExclusive:
			j.Overlaps.Exclusive++
		}
	}
	switch {
	case j.Overlaps.Exclusive > 0:
		j.Rule = RuleDefer
	case j.Overlaps.Local > 0:
		j.Rule = RuleLocalOptimistic
	case j.Overlaps.Mechanical > 0:
		j.Rule = RuleMechanical
	}
	// a pair of regions can name the same row twice (a tree and a file under it, both on the file's place)
	unique := rows[:0:0]
	have := map[BasisRow]bool{}
	for _, r := range rows {
		if !have[r] {
			have[r] = true
			unique = append(unique, r)
		}
	}
	sort.SliceStable(unique, func(i, j int) bool {
		a, b := unique[i], unique[j]
		switch {
		case gradeWeight(a.Grade) != gradeWeight(b.Grade):
			return gradeWeight(a.Grade) > gradeWeight(b.Grade)
		case a.Holder != b.Holder:
			return a.Holder < b.Holder
		case a.Repository != b.Repository:
			return a.Repository < b.Repository
		case a.Path != b.Path:
			return a.Path < b.Path
		}
		return strings.Compare(a.CandidateGrade+a.CandidateRule+a.HolderGrade+a.HolderRule, b.CandidateGrade+b.CandidateRule+b.HolderGrade+b.HolderRule) < 0
	})
	if len(unique) > MaxBasisRows {
		j.Omitted = len(unique) - MaxBasisRows
		unique = unique[:MaxBasisRows]
	}
	j.Basis = unique
	return j, nil
}
