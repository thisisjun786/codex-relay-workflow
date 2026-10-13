package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// ---- crw.spawnDecision ----

// spawnDecisionOp is crw.spawnDecision(rule, decision, info): the record of one decision to spawn, to do the work inline, to reuse
// an answer, to skip, or to refuse, with the rule that made it and the inputs it rested on. The row of flow_spawn_decisions is
// the record; the row of the journal is its position. A rule is named by its key (RuleKeys), never by its id.
type spawnDecisionOp struct{ l *Lane }

// DecisionInfo is what a script says about a decision.
type DecisionInfo struct {
	Node   string         `json:"node"`
	Role   string         `json:"role"`
	Tier   string         `json:"tier"`
	Call   string         `json:"call"`
	Reason string         `json:"reason"`
	Kind   string         `json:"kind"`
	Inputs map[string]any `json:"inputs"`
}

// structural is whether a rule is one of the structural spawns (the others are signals or inline work).
func structural(id string) bool {
	switch id {
	case RuleExec1, RuleRev1, RuleRev2, RuleArch1, RuleRevF1, RuleChild1, RuleQA1, RuleQA2, RuleAudit:
		return true
	}
	return false
}

func (o *spawnDecisionOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var key, decision string
	var info DecisionInfo
	if err := args(req.Args, &key, &decision, &info); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.spawnDecision(rule, decision, info): %w", err)
	}
	id, ok := RuleID(key)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.spawnDecision: %q is no rule key", key)
	}
	switch decision {
	case DecisionSpawn, DecisionInline, DecisionReuse, DecisionSkip, DecisionRefused, DecisionOrchestrator:
	default:
		return engine.OpResult{}, fmt.Errorf("crw.spawnDecision: %q is no decision", decision)
	}
	row := o.l.spawnRow(req, id, decision, info, "")
	if err := o.l.d.Decisions.AppendSpawn(ctx, row); err != nil {
		return engine.OpResult{}, err
	}
	return engine.OpResult{Result: result(map[string]any{"decision_id": row.DecisionID, "rule": id, "decision": decision}),
		Data: map[string]any{"rule": id, "decision": decision, "node": info.Node, "decision_id": row.DecisionID, "role": info.Role, "tier": info.Tier}}, nil
}

func (o *spawnDecisionOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// spawnRow is the flow_spawn_decisions row of a decision made at an op call. Its id derives from the run, the call and the
// suffix (one op call can make several decisions), so the same call again - a resume that runs the op again - is the same row.
func (l *Lane) spawnRow(req engine.OpRequest, rule, decision string, info DecisionInfo, suffix string) SpawnRow {
	kind := info.Kind
	if kind == "" {
		switch {
		case decision == DecisionInline:
			kind = KindInline
		case structural(rule):
			kind = KindStructural
		default:
			kind = KindSignal
		}
	}
	evidence := map[string]any{"op_call": req.CallID, "role": info.Role, "tier": info.Tier, "inputs": info.Inputs}
	if info.Call != "" {
		evidence["call"] = info.Call
	}
	// The id derives from the run, the position of the op call, the suffix and the content of the decision. A position is not a
	// decision: an invalidated suffix that runs live again makes its decision at the same position with other inputs, and that is
	// another row (the table is append-only), while the same decision again - a resume that runs the op again - is the same row.
	content := digestOf(map[string]any{"rule": rule, "decision": decision, "kind": kind, "node": info.Node, "reason": info.Reason, "evidence": evidence})
	return SpawnRow{
		DecisionID: "sd_" + hashParts(req.RunID, req.CallID, suffix, content)[:32], RunID: req.RunID, Kind: kind, CallID: req.CallID, NodeID: info.Node,
		RuleID: rule, Decision: decision, Reason: info.Reason, Evidence: evidence, DecidedAt: l.d.Now(),
	}
}

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- crw.stepRecord ----

// stepRecordOp is crw.stepRecord(node, step, info): the flow_step_decisions row of a tier step - the step id crw.tierStep returned,
// the rule and the evidence it rested on, and the checkpoint the executor-t1 of the node starts from (the head of the node's
// last commit and its tree). The executor-t1 call carries the id; a step that is not in this table is a step nobody decided.
type stepRecordOp struct{ l *Lane }

// StepInfo is the evidence of a tier step as a script states it.
type StepInfo struct {
	From     string         `json:"from"`
	To       string         `json:"to"`
	Evidence map[string]any `json:"evidence"`
	Letter   string         `json:"letter"`
}

// Checkpoint is the state a fresh executor of the next tier starts from.
type Checkpoint struct {
	Head string `json:"head"`
	Tree string `json:"tree"`
}

func (o *stepRecordOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var step struct {
		ID string `json:"id"`
	}
	var info StepInfo
	if err := args(req.Args, &node, &step, &info); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.stepRecord(node, step, info): %w", err)
	}
	if node == "" || step.ID == "" {
		return engine.OpResult{}, fmt.Errorf("crw.stepRecord: a node and the step id crw.tierStep returned are required")
	}
	if info.From == "" {
		info.From = "executor"
	}
	if info.To == "" {
		info.To = "executor-t1"
	}
	r := rows(req.Journal.Events())
	cp := Checkpoint{}
	if last, ok := r.opRows(OpCommit).last(func(ev engine.Event) bool { return dataStr(ev, "node") == node }); ok {
		cp = Checkpoint{Head: dataStr(last, "commit"), Tree: dataStr(last, "tree")}
	}
	evidence := map[string]any{"letter": info.Letter}
	for k, v := range info.Evidence {
		evidence[k] = v
	}
	row := StepRow{StepID: step.ID, RunID: req.RunID, NodeID: node, FromTier: info.From, ToTier: info.To, RuleID: RuleTier1, Evidence: evidence,
		CheckpointHead: cp.Head, CheckpointTree: cp.Tree, DecidedAt: o.l.d.Now()}
	// The step id the engine mints belongs to a position. A suffix that runs live again after an earlier effect was invalidated
	// reaches the same position with another checkpoint, and the stored decision must describe the checkpoint the executor-t1 of
	// this run starts from: the id of that decision is the engine's id with the digest of what it decided.
	err := o.l.d.Decisions.AppendStep(ctx, row)
	if errors.Is(err, ErrStepConflict) {
		row.StepID = step.ID + "_" + digestOf(map[string]any{"head": cp.Head, "tree": cp.Tree, "evidence": evidence, "from": info.From, "to": info.To})[:12]
		err = o.l.d.Decisions.AppendStep(ctx, row)
	}
	if err != nil {
		return engine.OpResult{}, err
	}
	step.ID = row.StepID
	return engine.OpResult{Result: result(map[string]any{"id": step.ID, "rule": RuleTier1, "letter": info.Letter, "checkpoint": cp}),
		Data: map[string]any{"node": node, "step_id": step.ID, "rule": RuleTier1, "letter": info.Letter, "from": info.From, "to": info.To, "checkpoint_head": cp.Head, "checkpoint_tree": cp.Tree}}, nil
}

func (o *stepRecordOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.exploreRoute and crw.exploreRecord ----

// The kinds of question the explorer routing decides.
const (
	ExploreOpen     = "open_questions" // SP-EXPL-1: the open questions of the architect
	ExploreResearch = "research"       // SP-EXPL-2: the research question of an executor receipt
	ExploreEvidence = "evidence"       // SP-EXPL-3: the evidence_question of a reviewer defect
)

// The routes a question takes.
const (
	RouteSpawn        = "spawn"
	RouteInline       = "inline"
	RouteReuse        = "reuse"
	RouteOrchestrator = "orchestrator"
	RouteSkip         = "skip"
	// RouteEscalate: the node has used its research continuations of the tier; the question is a scope question.
	RouteEscalate = "escalate"
)

// Question is a question put to an explorer: an open question of the architect (id, scope, files_needed), the research
// question of a receipt (the text only), or the evidence_question of a defect.
type Question struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Scope       string   `json:"scope"`
	FilesNeeded []string `json:"files_needed"`
}

// ExploreContext says who asks.
type ExploreContext struct {
	Node      string `json:"node"`
	Milestone string `json:"milestone"`
	Tier      string `json:"tier"`
	// Regions are the edit regions the asker declared.
	Regions []string `json:"regions"`
	// Head is the committed head an evidence question is about (SP-EXPL-3): the explorer reads its worktree at that commit, and an
	// answer about one head is not an answer about another.
	Head string `json:"head"`
}

// Route is the decision for one question.
type Route struct {
	Index    int      `json:"index"`
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Digest   string   `json:"digest"`
	Route    string   `json:"route"`
	Rule     string   `json:"rule"`
	Reason   string   `json:"reason"`
	Answer   string   `json:"answer"`
	Files    []string `json:"files_needed"`
	// SameAs is the index of the first question of the round with this digest when this one repeats it (-1 for none).
	SameAs int `json:"same_as"`
	// Scope is the scope the asker gave the question, and Authority the notation of an out-of-authority kind it starts with ("" for
	// none, AuthorityScope).
	Scope     string `json:"scope"`
	Authority string `json:"authority"`
}

// AuthorityScope is the notation of an out-of-authority kind (AuthorityScopes) a scope starts with, compared without regard to
// case and only as a whole token, or "" when the scope starts with none.
func AuthorityScope(scope string) string {
	s := strings.ToLower(strings.TrimSpace(scope))
	for _, n := range AuthorityScopes {
		rest, ok := strings.CutPrefix(s, n)
		if !ok {
			continue
		}
		if rest == "" {
			return n
		}
		if c := rest[0]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return n
		}
	}
	return ""
}

// normalizeQuestion is the text a question's digest is made of: the words, lower case, one space apart.
func normalizeQuestion(q string) string { return strings.ToLower(strings.Join(strings.Fields(q), " ")) }

// QuestionDigest is the digest that says two questions are the same question.
func QuestionDigest(q string) string { return textDigest(normalizeQuestion(q)) }

// exploreRouteOp is crw.exploreRoute(kind, context, questions): it decides what happens to each question of an asker, applies the
// thresholds of the signals and records every decision:
//
//   - a question whose digest was answered in the run reuses the answer (SP-INL-2);
//   - a research question inside the asker's declared edit regions, or that needs at most InlineFiles files, is answered by the
//     asker (SP-INL-1). An open question of a DESIGN never is: the architect's turn that asked it has ended;
//   - an open question whose scope starts with the notation of an out-of-authority kind (AuthorityScope) goes to the orchestrator
//     thread as a design question and to no explorer (reason out_of_authority); the milestone goes on;
//   - otherwise an explorer is spawned (SP-EXPL-1/2/3) - for the open questions at most ExplorersPerMilestone in the milestone, the
//     rest go to the orchestrator thread as design questions (reason explorer_cap); for a research question at most
//     ResearchPerTier continuations a tier, then the node escalates as a scope question; for an evidence question at most
//     EvidenceExplorersPerNode a node.
//
// A call without a question that is due (an evidence round with no evidence_missing defect) records the skip.
type exploreRouteOp struct{ l *Lane }

// RoutePlan is the answer of crw.exploreRoute.
type RoutePlan struct {
	Kind   string  `json:"kind"`
	Routes []Route `json:"routes"`
}

func (o *exploreRouteOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var kind string
	var ec ExploreContext
	var qs []Question
	if err := args(req.Args, &kind, &ec, &qs); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.exploreRoute(kind, context, questions): %w", err)
	}
	rule := ""
	switch kind {
	case ExploreOpen:
		rule = RuleExpl1
	case ExploreResearch:
		rule = RuleExpl2
	case ExploreEvidence:
		rule = RuleExpl3
	default:
		return engine.OpResult{}, fmt.Errorf("crw.exploreRoute: %q is no kind of question", kind)
	}
	r := rows(req.Journal.Events())
	answered := answeredDigests(r)
	spawnedMilestone, spawnedNode, spawnedTier := 0, 0, 0
	for _, prior := range r.opRows(OpExploreRoute) {
		pctx := routeContext(prior)
		list, _ := prior.Data["routes"].([]any)
		for _, item := range list {
			if rt, _ := item.(map[string]any); str(rt, "route") != RouteSpawn {
				continue
			}
			pkind := dataStr(prior, "kind")
			if pkind == ExploreOpen && kind == ExploreOpen && pctx.Milestone == ec.Milestone {
				spawnedMilestone++
			}
			if pctx.Node != "" && pctx.Node == ec.Node {
				if pkind == ExploreEvidence && kind == ExploreEvidence {
					spawnedNode++
				}
				if pkind == ExploreResearch && kind == ExploreResearch && pctx.Tier == ec.Tier {
					spawnedTier++
				}
			}
		}
	}
	plan := RoutePlan{Kind: kind, Routes: []Route{}}
	first := map[string]int{} // the first question of this call by digest: a repeat of it is the same question
	for i, q := range qs {
		rt := Route{Index: i, ID: q.ID, Question: q.Question, Digest: QuestionDigest(q.Question), Rule: rule, Files: q.FilesNeeded, SameAs: -1, Scope: q.Scope}
		if kind == ExploreOpen {
			rt.Authority = AuthorityScope(q.Scope)
		}
		if kind == ExploreEvidence && ec.Head != "" {
			rt.Digest = textDigest(normalizeQuestion(q.Question) + "\x00" + ec.Head)
		}
		if rt.Files == nil {
			rt.Files = []string{}
		}
		inlineOK := kind == ExploreResearch && q.FilesNeeded != nil && (len(q.FilesNeeded) <= InlineFiles || inRegions(q.FilesNeeded, ec.Regions))
		switch {
		case answered[rt.Digest] != nil:
			rt.Route, rt.Rule, rt.Answer, rt.Reason = RouteReuse, RuleInl2, *answered[rt.Digest], "answered_in_run"
		case first[rt.Digest] != 0:
			rt.Route, rt.Rule, rt.Reason, rt.SameAs = RouteReuse, RuleInl2, "same_question_in_round", first[rt.Digest]-1
		case inlineOK:
			rt.Route, rt.Rule, rt.Reason = RouteInline, RuleInl1, "asker_answers"
		case rt.Authority != "":
			rt.Route, rt.Reason = RouteOrchestrator, "out_of_authority"
		case kind == ExploreOpen && spawnedMilestone >= ExplorersPerMilestone:
			rt.Route, rt.Reason = RouteOrchestrator, "explorer_cap"
		case kind == ExploreResearch && spawnedTier >= ResearchPerTier:
			rt.Route, rt.Reason = RouteEscalate, "research_cap"
		case kind == ExploreEvidence && spawnedNode >= EvidenceExplorersPerNode:
			rt.Route, rt.Reason = RouteSkip, "evidence_cap"
		default:
			rt.Route = RouteSpawn
			switch kind {
			case ExploreOpen:
				spawnedMilestone++
			case ExploreResearch:
				spawnedTier++
			case ExploreEvidence:
				spawnedNode++
			}
		}
		if first[rt.Digest] == 0 {
			first[rt.Digest] = i + 1
		}
		plan.Routes = append(plan.Routes, rt)
		decision := rt.Route
		if decision == RouteEscalate {
			decision = DecisionRefused
		}
		info := DecisionInfo{Node: ec.Node, Role: "explorer", Tier: ec.Tier, Reason: rt.Reason, Inputs: map[string]any{
			"kind": kind, "digest": rt.Digest, "files_needed": len(q.FilesNeeded), "milestone": ec.Milestone, "spawned_in_milestone": spawnedMilestone, "spawned_for_node": spawnedNode,
			"scope_notation": rt.Authority}}
		if err := o.l.d.Decisions.AppendSpawn(ctx, o.l.spawnRow(req, rt.Rule, decision, info, fmt.Sprintf("q%d", i))); err != nil {
			return engine.OpResult{}, err
		}
	}
	if len(qs) == 0 {
		info := DecisionInfo{Node: ec.Node, Role: "explorer", Tier: ec.Tier, Reason: "no_question", Inputs: map[string]any{"kind": kind, "milestone": ec.Milestone}}
		if err := o.l.d.Decisions.AppendSpawn(ctx, o.l.spawnRow(req, rule, DecisionSkip, info, "none")); err != nil {
			return engine.OpResult{}, err
		}
	}
	routes := make([]map[string]any, 0, len(plan.Routes))
	for _, rt := range plan.Routes {
		routes = append(routes, map[string]any{"digest": rt.Digest, "route": rt.Route, "reason": rt.Reason, "scope_notation": rt.Authority})
	}
	return engine.OpResult{Result: result(plan), Data: map[string]any{"kind": kind, "node": ec.Node, "milestone": ec.Milestone, "tier": ec.Tier, "routes": routes}}, nil
}

func (o *exploreRouteOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

func routeContext(row engine.Event) ExploreContext {
	return ExploreContext{Node: dataStr(row, "node"), Milestone: dataStr(row, "milestone"), Tier: dataStr(row, "tier")}
}

// inRegions is whether every file a question needs lies inside the asker's declared edit regions (and it needs some).
func inRegions(files, regions []string) bool {
	if len(files) == 0 || len(regions) == 0 {
		return false
	}
	for _, f := range files {
		in := false
		for _, rg := range regions {
			if inRegion(rg, f) {
				in = true
				break
			}
		}
		if !in {
			return false
		}
	}
	return true
}

// Answer is the answer to a question as the run recorded it.
type Answer struct {
	Digest   string `json:"digest"`
	ID       string `json:"id"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
	Route    string `json:"route"`
	// Reason is the reason of the route (Route.Reason), and Authority the scope notation of an out-of-authority question.
	Reason    string   `json:"reason"`
	Authority string   `json:"authority"`
	Call      string   `json:"call"`
	Evidence  []string `json:"evidence"`
	// Findings is the explorer's FINDINGS as it answered, when an explorer did: exploreRecord reads it as a consumer of the schema.
	Findings json.RawMessage `json:"findings"`
}

// exploreRecordOp is crw.exploreRecord(kind, context, answers): the answers of a round of questions, kept so that a question of
// the same digest asked again in the run reuses the answer (SP-INL-2).
type exploreRecordOp struct{ l *Lane }

func (o *exploreRecordOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var kind string
	var ec ExploreContext
	var answers []Answer
	if err := args(req.Args, &kind, &ec, &answers); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.exploreRecord(kind, context, answers): %w", err)
	}
	list := make([]map[string]any, 0, len(answers))
	for _, a := range answers {
		if len(a.Findings) > 0 && string(a.Findings) != "null" {
			if _, err := DecodeFindings(a.Findings); err != nil {
				return engine.OpResult{}, fmt.Errorf("crw.exploreRecord: the answer to %q is no FINDINGS: %w", a.Question, err)
			}
		}
		list = append(list, map[string]any{"digest": a.Digest, "id": a.ID, "question": a.Question, "answer": a.Answer, "route": a.Route, "reason": a.Reason, "scope_notation": a.Authority, "call": a.Call})
	}
	return engine.OpResult{Result: result(map[string]any{"recorded": len(answers)}), Data: map[string]any{"kind": kind, "node": ec.Node, "milestone": ec.Milestone, "answers": list}}, nil
}

func (o *exploreRecordOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// answeredDigests are the answers the run recorded, by question digest. An inline route carries no answer and is not one, and
// neither is an empty answer (a repeat of a question that went to the orchestrator in its round).
func answeredDigests(r rows) map[string]*string {
	out := map[string]*string{}
	for _, row := range r.opRows(OpExploreRecord) {
		list, _ := row.Data["answers"].([]any)
		for _, item := range list {
			m, _ := item.(map[string]any)
			route := str(m, "route")
			if route != RouteSpawn && route != RouteReuse {
				continue
			}
			d := str(m, "digest")
			if d == "" {
				continue
			}
			a := str(m, "answer")
			if a == "" {
				continue
			}
			out[d] = &a
		}
	}
	return out
}
