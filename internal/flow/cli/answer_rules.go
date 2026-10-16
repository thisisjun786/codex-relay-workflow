package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// The answer of a wait (`crw flow answer`). The manager records a question and its answer in the user-decision record
// (`crw manage decisions`, CRW-718); the run applies it: the record and the `decision_applied` row of the run journal are two
// facts, and the second is the only one a wait resumes on. The record binds the question it answers by fields the relay's
// record has no member of its own for yet, so the binding is spelled in the members it has:
//
//	blocking  one plan_node entry, ref "<run>/<node>"  (the node is empty for a question of the milestone)
//	source    kind "flow_question" (an escalation answer: the orchestrator or the manager answers) or "flow_jun_decision" (a
//	          decision only Jun takes; the manager records it), ref "<question id>@<revision>#<attempt>:<plan basis>"
//
// CRW-718 gives the record run, node, attempt and question revision members of their own; BindingOf is the one place that reads
// them.
//
// A record may instead name, as its source, the child's project_decision_request report that lifted the question (CRW-1574):
//
//	source    kind "report", ref "<report id>"  (a report row of the run, of kind project_decision_request)
//	blocking  none, or one plan_node entry "<run>/<node>" of the question's node
//
// The report row names its question when report-up was given --question (members question, revision, node); a report without them
// binds the question the claim names only when it is the one question the run waits on. The attempt and the plan basis are the
// journal's and the run's (reportBinding); every later check of Judge is the same as for a flow_question record.

// The kinds of question a record can bind.
const (
	SourceQuestion    = "flow_question"
	SourceJunDecision = "flow_jun_decision"
	// SourceReport is a record whose source is the child's project_decision_request report (CRW-1574): the report binds the question.
	SourceReport = "report"
)

// Binding is the question a record answers, as the record names it.
type Binding struct {
	Source    string
	Run, Node string
	Question  string
	Revision  int
	Attempt   int
	// PlanBasis is the digest of the plan revision the question was asked under (PlanBasis).
	PlanBasis string
}

// PlanBasis is the digest of the plan a run pinned: the plan id and its revision. An answer given under another revision of the plan
// is not the answer to the question the run asked.
func PlanBasis(planID string, revision int64) string {
	sum := sha256.Sum256([]byte(planID + "@" + strconv.FormatInt(revision, 10)))
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

// BindingRef spells the ref of the source member of a record that binds a question.
func BindingRef(b Binding) string {
	return fmt.Sprintf("%s@%d#%d:%s", b.Question, b.Revision, b.Attempt, b.PlanBasis)
}

// BindingOf reads the question a record binds.
func BindingOf(rec decisions.Record) (Binding, error) {
	b := Binding{Source: rec.Source.Kind}
	if b.Source != SourceQuestion && b.Source != SourceJunDecision {
		return b, fmt.Errorf("the record's source is %q, not a question of a run (%s or %s)", rec.Source.Kind, SourceQuestion, SourceJunDecision)
	}
	var nodes []decisions.Blocking
	for _, x := range rec.Blocking {
		if x.Kind == decisions.BlockingPlanNode {
			nodes = append(nodes, x)
		}
	}
	if len(nodes) != 1 {
		return b, fmt.Errorf("the record blocks %d plan nodes, it binds exactly one run and node", len(nodes))
	}
	run, node, ok := strings.Cut(nodes[0].Ref, "/")
	if !ok || run == "" {
		return b, fmt.Errorf("the record's plan node %q is not <run>/<node>", nodes[0].Ref)
	}
	b.Run, b.Node = run, node
	ref := rec.Source.Ref
	head, basis, ok := strings.Cut(ref, ":")
	if !ok || basis == "" {
		return b, fmt.Errorf("the record's source ref %q is not <question>@<revision>#<attempt>:<plan basis>", ref)
	}
	qr, att, ok := strings.Cut(head, "#")
	if !ok {
		return b, fmt.Errorf("the record's source ref %q names no attempt", ref)
	}
	q, rev, ok := strings.Cut(qr, "@")
	if !ok || q == "" {
		return b, fmt.Errorf("the record's source ref %q names no question revision", ref)
	}
	var err1, err2 error
	b.Revision, err1 = strconv.Atoi(rev)
	b.Attempt, err2 = strconv.Atoi(att)
	if err1 != nil || err2 != nil || b.Revision < 1 || b.Attempt < 0 {
		return b, fmt.Errorf("the record's source ref %q has a revision or attempt that is not a number", ref)
	}
	b.Question, b.PlanBasis = q, basis
	return b, nil
}

// reportBinding reads the question a record whose source is a report binds (CRW-1574): the report is a project_decision_request row of
// the run among events; the question is the one the row names (report-up --question), else the claim's when it is the one question the
// run waits on (or the one this record answered already: a replay). The node, the attempt and the plan basis are the journal's and the
// run's; a plan_node blocking entry is not needed, and one the record has must be the question's.
func reportBinding(rec decisions.Record, events []engine.Event, run RunRecord, claim Claim) (Binding, error) {
	b := Binding{Source: SourceReport, Run: run.RunID, PlanBasis: PlanBasis(run.PlanID, run.PlanRevision)}
	id := rec.Source.Ref
	var row map[string]any
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == report.KindReport && str(e.Data, "report_id") == id && str(e.Data, "run") == run.RunID {
			row = e.Data
			break
		}
	}
	if id == "" || row == nil {
		return b, fmt.Errorf("the record's report %s is not a report of run %s", cell(id), run.RunID)
	}
	if kind := str(row, "report_kind"); kind != report.KindProjectDecisionRequest {
		return b, fmt.Errorf("the record's report %s is a %s, not a %s", id, kind, report.KindProjectDecisionRequest)
	}
	questions := Questions(events)
	var q *Question
	if named := str(row, "question"); named != "" {
		b.Question, b.Revision, b.Node = named, num(row, "revision"), str(row, "node")
		for i := range questions {
			if questions[i].ID == named {
				q = &questions[i]
			}
		}
		if q != nil {
			b.Node, b.Attempt = q.Node, q.Attempt
		}
	} else {
		var waiting []*Question
		for i := range questions {
			switch {
			case questions[i].ID == claim.Question && questions[i].Applied == rec.DecisionID:
				// this record answered the question already: the same answer again is a replay, not a new binding
				q = &questions[i]
			case questions[i].Waits():
				waiting = append(waiting, &questions[i])
			}
		}
		if q == nil {
			if len(waiting) != 1 {
				return b, fmt.Errorf("the record's report %s names no question and run %s waits on %d questions: report-up --question names it", id, run.RunID, len(waiting))
			}
			q = waiting[0]
		}
		b.Question, b.Revision, b.Node, b.Attempt = q.ID, q.Revision, q.Node, q.Attempt
	}
	for _, x := range rec.Blocking {
		if x.Kind == decisions.BlockingPlanNode && x.Ref != b.Run+"/"+b.Node {
			return b, fmt.Errorf("the record's plan node %q is not the node of question %s (%s/%s)", x.Ref, b.Question, b.Run, b.Node)
		}
	}
	return b, nil
}

// Claim is what the caller of `crw flow answer` says it is answering. It is compared with the record and with the journal, so an
// answer is applied only when all three name the same question.
type Claim struct {
	Decision string
	Run      string
	Node     string
	Question string
	Revision int
	// By is who applies the answer (the orchestrator thread, or the operator), written into the row.
	By string
	// Answers are the parts of the recorded answer for each question an ask joins, by question digest (CRW-1381): `crw flow answer
	// --answer-for DIGEST=TEXT`. Empty: the whole answer is every question's.
	Answers map[string]string
}

// The refusals of an answer. The code is stable; the detail says what differed.
const (
	RefusedNotAnswered  = "not_answered"
	RefusedBadBinding   = "bad_binding"
	RefusedOtherRun     = "other_run"
	RefusedOtherNode    = "other_node"
	RefusedNoQuestion   = "unknown_question"
	RefusedStaleRevison = "stale_revision"
	RefusedStaleAttempt = "stale_attempt"
	RefusedStalePlan    = "stale_plan_basis"
	RefusedAuthority    = "out_of_scope_authority"
	RefusedApplied      = "already_applied"
	// RefusedPartQuestion is a part of the answer for a question digest the ask does not join, and RefusedPartText a part that is not
	// text of the recorded answer (CRW-1381): input errors.
	RefusedPartQuestion = "unknown_question_digest"
	RefusedPartText     = "answer_part_not_recorded"
	// RefusedLegacyRun is an answer to a legacy `child` session run (CRW-1415): its records are read only, and an answer is not appended; every
	// other command that would write to it (cancel, pause, recover, dispatched, report-up) refuses it with the same code (legacyRefused).
	RefusedLegacyRun = "legacy_issue_child_run"
)

// WarnSingleAnswer is the warning of an answer that gives an ask of several questions no part for some of them: each of those questions
// takes the whole answer (CRW-1381).
const WarnSingleAnswer = "single_answer_for_joined_ask"

// Refusal is an answer the run does not apply.
type Refusal struct {
	Code, Detail string
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Detail }

// recordedReason is the reason a record holds for its end, as a refusal names it.
func recordedReason(reason string) string {
	if reason = strings.TrimSpace(reason); reason == "" {
		return "no reason recorded"
	}
	return "reason: " + reason
}

func refuse(code, format string, a ...any) *Refusal {
	return &Refusal{Code: code, Detail: fmt.Sprintf(format, a...)}
}

// Application is what applying an answer comes to: the row to append, or the row that already answers (a replay).
type Application struct {
	// Replayed is true when the journal already holds the row for this decision and question: nothing is appended.
	Replayed bool           `json:"replayed"`
	Seq      int            `json:"seq,omitempty"`
	Data     map[string]any `json:"row"`
}

// userVias are the ways the user's answer reaches the record; the parent's report read back is not one of them.
var userVias = []string{decisions.ViaDots, decisions.ViaDirectAsk, decisions.ViaFileImport, decisions.ViaManagementMessage}

// Judge compares an answered record with the journal of the run it answers and decides. It reads only its arguments and writes
// nothing: the order of its checks is the order of the refusals, so the first difference is the one it names. events are the rows of
// the run's journal (any rows; the questions are read from the escalation rows among them).
func Judge(rec decisions.Record, events []engine.Event, run RunRecord, claim Claim) (Application, *Refusal) {
	var none Application
	switch {
	case rec.State == decisions.StateWithdrawn:
		// a record withdrawn after its answer (decision-withdraw, also while an answer waited for the run) is not applied (CRW-1492)
		return none, refuse(RefusedNotAnswered, "the record %s is withdrawn (%s): a withdrawn decision is not applied", rec.DecisionID, recordedReason(rec.WithdrawnReason))
	case rec.State == decisions.StateExpired:
		return none, refuse(RefusedNotAnswered, "the record %s is expired (%s): an expired decision is not applied", rec.DecisionID, recordedReason(rec.ExpiredReason))
	case rec.State != decisions.StateAnswered && rec.State != decisions.StateApplied:
		return none, refuse(RefusedNotAnswered, "the record %s is %s; the manager records the answer first", rec.DecisionID, rec.State)
	}
	if claim.Decision != rec.DecisionID {
		return none, refuse(RefusedBadBinding, "the claim names decision %q, the record is %q", claim.Decision, rec.DecisionID)
	}
	var b Binding
	var err error
	if rec.Source.Kind == SourceReport {
		b, err = reportBinding(rec, events, run, claim)
	} else {
		b, err = BindingOf(rec)
	}
	if err != nil {
		return none, refuse(RefusedBadBinding, "%v", err)
	}
	switch {
	case b.Run != run.RunID || claim.Run != run.RunID:
		return none, refuse(RefusedOtherRun, "the record binds run %s, the claim names run %s, the journal is run %s", b.Run, claim.Run, run.RunID)
	case b.Node != claim.Node:
		return none, refuse(RefusedOtherNode, "the record binds node %q, the claim names node %q", b.Node, claim.Node)
	case b.Question != claim.Question || b.Revision != claim.Revision:
		return none, refuse(RefusedStaleRevison, "the record answers question %s revision %d, the claim names %s revision %d", b.Question, b.Revision, claim.Question, claim.Revision)
	}
	var q *Question
	questions := Questions(events)
	for i := range questions {
		if questions[i].ID == b.Question {
			q = &questions[i]
		}
	}
	if q == nil {
		return none, refuse(RefusedNoQuestion, "the run %s asked no question %s", run.RunID, b.Question)
	}
	if q.Node != b.Node {
		return none, refuse(RefusedOtherNode, "question %s is a question of node %q, the record binds node %q", q.ID, q.Node, b.Node)
	}
	if q.Revision != b.Revision || !q.Current {
		return none, refuse(RefusedStaleRevison, "question %s is revision %d and %s; the record answers revision %d", q.ID, q.Revision, map[bool]string{true: "current", false: "replaced by a later question of the node"}[q.Current], b.Revision)
	}
	if q.Attempt != b.Attempt {
		return none, refuse(RefusedStaleAttempt, "question %s was asked in attempt %d of the node, the record answers attempt %d", q.ID, q.Attempt, b.Attempt)
	}
	if want := PlanBasis(run.PlanID, run.PlanRevision); b.PlanBasis != want {
		return none, refuse(RefusedStalePlan, "the record answers under plan basis %s, the run is pinned to %s", b.PlanBasis, want)
	}
	if err := authorityOK(rec, b, run); err != nil {
		return none, err
	}
	// the parts of the answer for the questions the ask joins: each a question of the ask, each text of the recorded answer (CRW-1381)
	var parts map[string]string
	for digest, part := range claim.Answers {
		if !contains(q.Questions, digest) {
			return none, refuse(RefusedPartQuestion, "question %s joins the questions %s; the answer gives a part for %s", q.ID, strings.Join(q.Questions, ", "), digest)
		}
		if strings.TrimSpace(part) == "" || !strings.Contains(rec.AnswerText, part) {
			return none, refuse(RefusedPartText, "the part %q for question %s is not text of the answer the record %s holds", part, digest, rec.DecisionID)
		}
		if parts == nil {
			parts = map[string]string{}
		}
		parts[digest] = part
	}
	var warnings []string
	if len(q.Questions) > 1 {
		var whole []string
		for _, d := range q.Questions {
			if _, ok := parts[d]; !ok {
				whole = append(whole, d)
			}
		}
		if len(whole) > 0 {
			warnings = append(warnings, fmt.Sprintf("%s: question %s joins %d questions and the answer gives no part for %s, which take the whole answer (give each its part with --answer-for DIGEST=TEXT)",
				WarnSingleAnswer, q.ID, len(q.Questions), strings.Join(whole, ", ")))
		}
	}
	// the row of this decision, when there is one, is the application: the same answer again appends nothing
	for _, e := range events {
		if e.Kind == engine.KindFlow && e.Event == ops.EvDecisionApplied && str(e.Data, "ask") == q.ID {
			if d, ok := e.Data["decision"].(map[string]any); ok && str(d, "id") == rec.DecisionID {
				// the same decision is a replay only with the parts it was applied with (CRW-1397): other parts, or parts where it was
				// applied whole (or none where it had parts), would read as applied while the row holds another answer
				if stored := storedParts(d); !sameParts(stored, parts) {
					return none, refuse(RefusedApplied, "question %s was answered by decision %s already with the parts %s; this application gives the parts %s: apply a new decision for another answer",
						q.ID, rec.DecisionID, partsText(stored), partsText(parts))
				}
				return Application{Replayed: true, Seq: e.Seq, Data: e.Data}, nil
			}
			return none, refuse(RefusedApplied, "question %s was answered by decision %v already", q.ID, e.Data["decision"])
		}
	}
	row := map[string]any{
		"ask": q.ID, "node": q.Node, "run": run.RunID, "revision": q.Revision, "attempt": q.Attempt, "plan_basis": b.PlanBasis,
		"scope": "run:" + run.RunID, "target": q.Node, "action": actionOf(rec), "class": classOf(b),
		"applied_by": claim.By,
		"decision": map[string]any{
			"id": rec.DecisionID, "option": optionOf(rec), "text": rec.AnswerText, "by": rec.AnsweredBy, "via": rec.AnsweredVia,
			"answered_at": rec.AnsweredAt, "authority": rec.Authority.Kind, "authority_ref": rec.Authority.Ref,
		},
	}
	if parts != nil {
		row["decision"].(map[string]any)["answers"] = parts
	}
	if len(warnings) > 0 {
		row["warnings"] = warnings
	}
	return Application{Data: row}, nil
}

// storedParts are the parts of the answer a decision_applied row's decision holds (its answers member), nil when it was applied whole.
func storedParts(d map[string]any) map[string]string {
	var out map[string]string
	switch m := d["answers"].(type) {
	case map[string]string:
		for k, v := range m {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	case map[string]any:
		for k, v := range m {
			if out == nil {
				out = map[string]string{}
			}
			out[k], _ = v.(string)
		}
	}
	return out
}

// sameParts says whether two sets of parts are one: the same digests with the same texts (no parts on both sides is the same).
func sameParts(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// partsText spells parts for a refusal: digest=text pairs in the order of their digests, or "none (the whole answer)".
func partsText(parts map[string]string) string {
	if len(parts) == 0 {
		return "none (the whole answer)"
	}
	keys := make([]string, 0, len(parts))
	for k := range parts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = fmt.Sprintf("%s=%q", k, parts[k])
	}
	return strings.Join(out, ", ")
}

func classOf(b Binding) string {
	if b.Source == SourceJunDecision {
		return "jun_decision"
	}
	return "escalation"
}

// optionOf is the option the answer chose, "" for an answer in prose.
func optionOf(rec decisions.Record) string {
	for _, o := range rec.Options {
		if o.ID == rec.AnswerText {
			return o.ID
		}
	}
	return ""
}

func actionOf(rec decisions.Record) string {
	if o := optionOf(rec); o != "" {
		return o
	}
	return "text"
}

// authorityOK judges who may answer. An escalation of a run (a flow_question record, or one whose source is the child's
// project_decision_request report: CRW-1574) is answered by the orchestrator that holds the run (the parent class, with the run owner
// as its ref), by the manager (delegated-management) or by Jun (user); a decision only Jun takes is answered
// at the user class, recorded by the manager, and arrives by one of the ways the user's answer arrives, never as the parent's
// report read back.
func authorityOK(rec decisions.Record, b Binding, run RunRecord) *Refusal {
	kind := rec.Authority.Kind
	if _, known := decisions.AnswerAuthorityRank(kind); !known {
		return refuse(RefusedAuthority, "the record's authority %q is not a class of the answer vocabulary", kind)
	}
	if rec.AnsweredBy == "" || rec.AnsweredVia == "" {
		return refuse(RefusedAuthority, "the record names no one who answered it and no way the answer arrived")
	}
	if b.Source == SourceJunDecision {
		if kind != decisions.AuthorityUser {
			return refuse(RefusedAuthority, "a decision only Jun takes needs a %s-class answer, the record's authority is %s", decisions.AuthorityUser, kind)
		}
		if !contains(userVias, rec.AnsweredVia) {
			return refuse(RefusedAuthority, "a decision only Jun takes arrives as the user's answer (%s); %q is not one of them", strings.Join(userVias, ", "), rec.AnsweredVia)
		}
		return nil
	}
	if kind == decisions.AuthorityParent && rec.Authority.Ref != run.Owner {
		return refuse(RefusedAuthority, "a parent-class answer is the orchestrator that holds the run (%s); the record's authority ref is %q", run.Owner, rec.Authority.Ref)
	}
	return nil
}
