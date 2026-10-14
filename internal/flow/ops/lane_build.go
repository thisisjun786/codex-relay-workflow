package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// HeadRef is a head as a script hands it to an operation: the commit id, or the object crw.commit answered.
type HeadRef struct {
	Commit string
}

// UnmarshalJSON accepts a commit id or an object with a commit member.
func (h *HeadRef) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		h.Commit = s
		return nil
	}
	var o struct {
		Commit string `json:"commit"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	h.Commit = o.Commit
	return nil
}

// ---- crw.commit ----

// commitOp is crw.commit(node, call, regions): the commit of exactly the working snapshot an executor call recorded
// (pending_tree), in the node's current attempt worktree, on the commit its HEAD is at. The run commits and reads the head from
// git; a model never does. The paths the commit changes outside the declared edit regions are recorded, not refused.
type commitOp struct{ l *Lane }

// CommitOutcome is the answer of crw.commit.
type CommitOutcome struct {
	Commit     string   `json:"commit"`
	Parent     string   `json:"parent"`
	Tree       string   `json:"tree"`
	Changed    []string `json:"changed"`
	OutOfScope []string `json:"out_of_scope"`
	// Empty is set when the commit changes nothing against its parent.
	Empty bool `json:"empty"`
}

func (o *commitOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var ex CallRef
	var regions []string
	if err := args(req.Args, &node, &ex, &regions); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.commit(node, call, regions): %w", err)
	}
	r := rows(req.Journal.Events())
	term, ok := r.terminal(ex.CallID)
	tree := ""
	if ok {
		tree = dataStr(term, "pending_tree")
	}
	wt := r.worktreeOf(node)
	if wt == "" || tree == "" {
		return engine.OpResult{}, fmt.Errorf("crw.commit: node %s: no worktree (%q) or no pending tree (%q) recorded for call %s", node, wt, tree, ex.CallID)
	}
	g := o.l.d.Git
	head, err := Head(ctx, g, wt)
	if err != nil {
		return engine.OpResult{}, err
	}
	out := CommitOutcome{Tree: tree, Changed: []string{}, OutOfScope: []string{}}
	strayHead := ""
	// An op that ran and did not write its row runs again under the same request id: the commit it made is the one this call
	// makes (its message names the request), and nothing is committed twice.
	if msg, err := g(ctx, wt, nil, "log", "-1", "--format=%B"); err == nil && strings.Contains(msg, req.RequestID) {
		out.Commit = head
		if parent, err := g(ctx, wt, nil, "rev-parse", "--verify", head+"^"); err == nil {
			out.Parent = parent
		}
	} else {
		// A session executor that committed left HEAD at a stray commit, which its completed row records (executor_head_moved,
		// kept on the attempt's stray ref): the pending tree is committed on the start it recorded, over the HEAD observed now.
		parent, expect := head, ""
		if moved, ok := term.Data["executor_head_moved"].(map[string]any); ok && str(moved, "start") != "" && str(moved, "start") != head {
			parent, expect = str(moved, "start"), head
		}
		made, err := Commit(ctx, g, wt, CommitRequest{Tree: tree, Parent: parent, Expect: expect, Message: fmt.Sprintf("crw %s %s\n\n%s", node, short(ex.CallID), req.RequestID)})
		if err != nil {
			return engine.OpResult{}, err
		}
		out.Commit, out.Parent = made.Commit, made.Parent
		if expect != "" {
			strayHead = expect
		}
	}
	scope, err := Scope(ctx, g, wt, out.Parent, out.Tree, regions)
	if err != nil {
		return engine.OpResult{}, err
	}
	out.Changed, out.OutOfScope = scope.Changed, scope.OutOfScope
	out.Empty = len(out.Changed) == 0
	data := map[string]any{"node": node, "commit": out.Commit, "parent": out.Parent, "tree": out.Tree, "out_of_scope": out.OutOfScope, "call": ex.CallID, "empty": out.Empty}
	if strayHead != "" {
		data["stray_head"] = strayHead
	}
	// The receipt is the executor's report, not the judgement of its worktree: the commit row keeps what it said beside what the
	// run committed - its outcome, and the plan mismatch the re-plan signal of the milestone counts (F09).
	if len(ex.Output) > 0 && string(ex.Output) != "null" {
		receipt, err := DecodeReceipt(ex.Output)
		if err != nil {
			return engine.OpResult{}, fmt.Errorf("crw.commit: the output of call %s is no RECEIPT: %w", ex.CallID, err)
		}
		data["outcome"] = receipt.Outcome
		if receipt.PlanMismatch != nil {
			data["plan_mismatch"] = *receipt.PlanMismatch
		}
	}
	return engine.OpResult{Result: result(out), Data: data}, nil
}

func (o *commitOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	wt := rows(ev).worktreeOf(dataStr(row, "node"))
	if wt == "" {
		return false, "the node has no worktree", nil
	}
	head, err := Head(ctx, o.l.d.Git, wt)
	if err != nil {
		return false, err.Error(), nil
	}
	anc, err := IsAncestor(ctx, o.l.d.Git, wt, dataStr(row, "commit"), head)
	if err != nil || !anc {
		return false, "the commit is not on the history of the node's worktree", nil
	}
	return true, "", nil
}

// ---- crw.verify ----

// verifyOp is crw.verify(node, head, opts): the verification of a committed head by the Verifier (crw-dev ci local, which leaves a
// verification-record/1). A verification that could not run is not a failed one: it is answered with ran false, and the lane holds
// the node instead of counting it as evidence against the executor.
//
// With no node it is the verification of the integrated head of a milestone, for the final gate (CRW-1206): opts names the
// milestone, the command asked for (a command of the plan's required set) and the base of the range, and the head is verified in
// a clean worktree of the repository of the run, since no node's worktree is the place of an integrated head. The row it leaves
// has the node "" and the milestone, which is how the gate finds it.
type verifyOp struct{ l *Lane }

// VerifyOpts are the options of a milestone-level crw.verify.
type VerifyOpts struct {
	Milestone string `json:"milestone"`
	Command   string `json:"command"`
	Base      string `json:"base"`
}

// VerifyResult is the answer of crw.verify.
type VerifyResult struct {
	Ran      bool     `json:"ran"`
	Pass     bool     `json:"pass"`
	Command  string   `json:"command"`
	Digest   string   `json:"digest"`
	Summary  string   `json:"summary"`
	Failures []string `json:"failures"`
	Error    string   `json:"error"`
	Head     string   `json:"head"`
	// Base is the commit the verification and the acceptance of the head stand on (stableBase).
	Base string `json:"base"`
}

func (o *verifyOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var head HeadRef
	var opts VerifyOpts
	if err := args(req.Args, &node, &head, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.verify(node, head, opts): %w", err)
	}
	if head.Commit == "" {
		return engine.OpResult{}, fmt.Errorf("crw.verify: node %s: no head to verify", node)
	}
	if node == "" && opts.Milestone == "" {
		return engine.OpResult{}, fmt.Errorf("crw.verify: a verification with no node is the one of a milestone's head: name the milestone")
	}
	r := rows(req.Journal.Events())
	wt := r.worktreeOf(node)
	if node == "" {
		wt = o.l.d.Repo
	}
	res := VerifyResult{Failures: []string{}, Head: head.Commit}
	data := map[string]any{"node": node, "head": head.Commit}
	if node == "" {
		data["milestone"] = opts.Milestone
	}
	var record json.RawMessage
	base := ""
	if o.l.d.Verifier == nil {
		res.Error = "no verifier is wired"
	} else {
		// the range is the one the acceptance judges: from the commit the node's work grew from (stableBase), not from the merge-base
		// with origin/dev that `crw-dev ci local` falls back to
		if node == "" {
			base = opts.Base
		} else if rel, ok := releaseRowOf(r, node); ok {
			b, berr := o.l.stableBase(ctx, dataStr(rel, "plan"), wt, head.Commit)
			if berr != nil {
				return engine.OpResult{}, berr
			}
			base = b
		}
		out, err := o.l.d.Verifier.Verify(ctx, VerifyRequest{Node: node, Head: head.Commit, Worktree: wt, Base: base, Command: opts.Command})
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Ran, res.Pass, res.Command, res.Digest, res.Summary = true, out.Pass, out.Command, out.Digest, out.Summary
			if out.Failures != nil {
				res.Failures = out.Failures
			}
			record = out.Record
			if out.Note != "" {
				data["verifier_note"] = out.Note
			}
		}
	}
	res.Base = base
	data["ok"], data["ran"], data["command"], data["digest"], data["error"], data["base"] = res.Pass, res.Ran, res.Command, res.Digest, res.Error, base
	if len(record) > 0 {
		data["record"] = string(record)
	}
	return engine.OpResult{Result: result(res), Data: data}, nil
}

// Proven: a verification that ran stays what it was (a pass, or the failure its record shows). One that did not run (the verifier
// is not installed, it left no record) is no result to replay: the node is held, and a resume that finds the verifier back asks it
// again.
func (o *verifyOp) Proven(_ context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	if !dataBool(row, "ran") {
		return false, "the verification did not run", nil
	}
	return true, "", nil
}

// stableBase is the base a head is verified and accepted over: the commit of the integration target that the node's work grew
// from, found as the merge-base of the head and the tip. It is an ancestor of the head by construction and it does not move when
// another node lands on the target, so the verification record, the premerge record and the acceptance of one head name the same
// commit; the tip itself is sampled at each step and is neither.
func (l *Lane) stableBase(ctx context.Context, plan, worktree, head string) (string, error) {
	tip, err := l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return "", err
	}
	base, err := l.d.Git(ctx, worktree, nil, "merge-base", head, tip)
	if err != nil {
		return "", fmt.Errorf("the head %s and the integration tip %s have no merge base: %w", head, tip, err)
	}
	return strings.TrimSpace(base), nil
}

// ---- crw.review ----

// reviewOp is crw.review(node, head, criteriaDigest, call): the ReviewResult of a reviewer call. State verdict is the only state
// that is a round, a fix or tier evidence, and it needs a completed call whose answer is a valid VERDICT by a reviewer whose model
// family differs from every author of the node. The other states - held, unknown, refused (independence_refused included),
// null_model, superseded - are the call's own status, and none of them is a verdict.
type reviewOp struct{ l *Lane }

// FindingRef names one defect of a verdict by the digest that says two rounds found the same thing.
type FindingRef struct {
	Ref    string `json:"ref"`
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
}

// ReviewResult is the answer of crw.review.
type ReviewResult struct {
	State          string          `json:"state"`
	Reason         string          `json:"reason"`
	Verdict        json.RawMessage `json:"verdict"`
	Findings       []FindingRef    `json:"findings"`
	Head           string          `json:"head"`
	CriteriaDigest string          `json:"criteria_digest"`
	Call           Brief           `json:"call"`
	// Retried is set when the result is that of a second reviewer thread, asked after the first answered null_model.
	Retried bool `json:"retried"`
}

var whitespace = regexp.MustCompile(`\s+`)

// FindingDigest says whether two defects are the same finding: the kind, the place and the words, lower case and one space apart.
func FindingDigest(d Defect) string {
	return textDigest(strings.Join([]string{d.Kind, d.File, fmt.Sprint(d.Line), strings.ToLower(strings.TrimSpace(whitespace.ReplaceAllString(d.Finding, " ")))}, "\x00"))[:16]
}

func (o *reviewOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node, criteria string
	var head HeadRef
	var call CallRef
	var opts struct {
		Retried bool `json:"retried"`
	}
	if err := args(req.Args, &node, &head, &criteria, &call, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.review(node, head, criteriaDigest, call, opts): %w", err)
	}
	res := ReviewResult{Verdict: json.RawMessage(`null`), Findings: []FindingRef{}, Head: head.Commit, CriteriaDigest: criteria, Call: call.brief(), Retried: opts.Retried}
	var v ReviewVerdict
	detail := ""
	switch call.Status {
	case "completed":
		var err error
		if v, err = DecodeVerdict(call.Output); err != nil {
			res.State, res.Reason = StateNullModel, "verdict_invalid"
			detail = err.Error()
			break
		}
		jr := rows(req.Journal.Events())
		if why := independent(call.Meta.ConfiguredPair.Model, jr.authorModels(node)); why != "" {
			res.State, res.Reason = StateRefused, why
			break
		}
		// a verified verdict evaluated every criterion of the node's plan once: the criteria it names are the ones the premerge
		// record will hold, and the digest the acceptance compares is that of the whole set
		if v.Verdict == "verified" {
			if err := CheckCoverage(v.Criteria, expectedCriteria(jr, node)); err != nil {
				res.State, res.Reason = StateNullModel, "verdict_invalid"
				detail = err.Error()
				break
			}
		}
		res.State, res.Verdict = StateVerdict, call.Output
		for _, d := range v.Defects {
			res.Findings = append(res.Findings, FindingRef{Ref: d.Ref, Kind: d.Kind, Digest: FindingDigest(d)})
		}
	case "null_model":
		res.State, res.Reason = StateNullModel, call.Reason
	case "held":
		res.State, res.Reason = StateHeld, call.Reason
	case "unknown":
		res.State, res.Reason = StateUnknown, call.Reason
	case "refused":
		res.State, res.Reason = StateRefused, call.Reason
	default:
		res.State, res.Reason = StateSuperseded, call.Reason
	}
	return engine.OpResult{Result: result(res), Data: map[string]any{
		"node": node, "head": head.Commit, "criteria_digest": criteria, "state": res.State, "reason": res.Reason, "verdict": v.Verdict,
		"call": call.CallID, "reviewer_model": call.Meta.ConfiguredPair.Model, "defects": len(v.Defects), "retried": opts.Retried, "detail": detail,
	}}, nil
}

// expectedCriteria are the ids of the criteria of a node's plan, as the milestone row of the journal recorded the node: c1 to cn in
// the plan's order (nil when the plan lists none).
func expectedCriteria(r rows, node string) []string {
	rel, ok := designRowOf(r, node)
	if !ok {
		return nil
	}
	n, ok := planNodeOf(r, dataStr(rel, "plan"), dataStr(rel, "milestone"), node)
	if !ok {
		return nil
	}
	return CriterionIDs(len(n.Criteria))
}

func (o *reviewOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}
