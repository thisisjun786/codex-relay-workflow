package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// This file is crw.crosscheck (CRW-1473): the review of a milestone's integrated range at its close, after the final gate passed, by
// two passes of a review engine of another model family than every author of the milestone. The op runs the engine itself, in Go,
// through the review adapter's seam (UnitChecker, the shipped reviewadapter.Adapter); no agent call is made. Pass A judges coverage
// (every criterion and decision, met or not, with evidence), pass B hunts bugs; each runs in a checkout of the gate head of its own
// under the run's directory, and their findings are merged into one verdict by one rule (CrosscheckRule, premerge.Blocks).
//
// This revision serves round 1 only: a prior verdict, or a round other than 1, is refused (crosscheck_prior_invalid).

// CrosscheckGuideRev is the revision of the review guide the crosscheck's packets are built from; V records it.
const CrosscheckGuideRev = "crw-crosscheck/1"

// CrosscheckPromptLimit is the most bytes a pass prompt may have: the engines take the prompt as one command-line word
// (reviewadapter.ExecRun), which Linux bounds at 128 KiB. A longer prompt is not sent (crosscheck_prompt_too_large); the prompt carries
// references to the range and the receipts, never their bytes.
const CrosscheckPromptLimit = 120 << 10

// CrosscheckRule is the one close rule of the crosscheck, word for word as the crosscheck packets say it.
const CrosscheckRule = "The impact of each finding is one of core_function, safety_or_data, permission_boundary, criterion_unmet, regression_introduced, minor_separable: " +
	"a finding of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block. " +
	"The milestone closes only on a verified crosscheck: verified means no finding blocks; a finding that does not block is carried as a follow-up."

// The states of a crosscheck outcome (X.state).
const (
	CrosscheckStateVerdict   = "verdict"
	CrosscheckStateEvidence  = "evidence"
	CrosscheckStateHeld      = "held"
	CrosscheckStateRefused   = "refused"
	CrosscheckStateFailed    = "failed"
	CrosscheckStateNoVerdict = "no_verdict"
)

// The reasons of a refused or failed crosscheck that the op itself decides.
const (
	ReasonCrosscheckAuthorUndecided     = "crosscheck_author_undecided"
	ReasonCrosscheckNoIndependentEngine = "crosscheck_no_independent_engine"
	ReasonCrosscheckPriorInvalid        = "crosscheck_prior_invalid"
	ReasonCrosscheckNoRunDir            = "crosscheck_no_run_dir"
	ReasonCrosscheckPromptTooLarge      = "crosscheck_prompt_too_large"
	ReasonCrosscheckWorktreeDirty       = "crosscheck_worktree_dirty"
	ReasonCrosscheckWorktreeMoved       = "crosscheck_worktree_moved"
)

// The two passes of a round.
const (
	CrosscheckPassA = "A"
	CrosscheckPassB = "B"
)

// The directories under the run's directory (Deps.LandRoot): the checkouts of the passes, and the receipts file of a round.
const (
	crosscheckWorktreeDirectoryName = "crosscheck-wt"
	crosscheckDirectoryName         = "crosscheck"
)

// Crosscheck is what crw.crosscheck reaches outside the journal: the checker that runs one pass (the review adapter), the engine profiles
// in the order a round tries them (Profiles is given the run's directory, where a profile may keep a file such as astra's strict
// schema), the time limit of one pass, the run's author pairs (role -> model for architect, executor and executor-t1; "" for a pair the
// run's role table leaves undecided), and the checkout of a pass (nil: Lane.crosscheckWorktree, a detached worktree of the head under
// the run's directory).
type Crosscheck struct {
	Checker  UnitChecker
	Profiles func(runDir string) ([]reviewadapter.Profile, error)
	Limit    time.Duration
	Authors  map[string]string
	Checkout func(ctx context.Context, ms string, round, evidenceRound int, pass string, attempt int, head string) (dir string, done func() error, err error)
}

// CrosscheckCheckoutError is a checkout of a pass the op does not run a pass in: the reason (crosscheck_worktree_dirty,
// crosscheck_worktree_moved), the directory, which is kept as it is for the 자식 to read, and what was found.
type CrosscheckCheckoutError struct {
	Reason, Dir, Detail string
}

func (e *CrosscheckCheckoutError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.Reason, e.Dir, e.Detail)
}

// CrosscheckOpts are the options of crw.crosscheck. Round 0 is round 1.
type CrosscheckOpts struct {
	Round   int             `json:"round"`
	Prior   json.RawMessage `json:"prior"`
	Authors json.RawMessage `json:"authors"`
	Prompts struct {
		A string `json:"A"`
		B string `json:"B"`
	} `json:"prompts"`
	Evidence      json.RawMessage `json:"evidence"`
	EvidenceRound int             `json:"evidence_round"`
}

// CrosscheckAuthor is one author of the milestone the engine must be independent of: where it was read (role: the run's role table;
// call: an author call row of the journal; thread: a correction author's thread), its name there, its model and the model's family.
type CrosscheckAuthor struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	Model  string `json:"model"`
	Family string `json:"family"`
}

// CrosscheckReceipt is the executor receipt of a node as the crosscheck handed it to the engine.
type CrosscheckReceipt struct {
	Node   string `json:"node"`
	State  string `json:"state"`
	SHA256 string `json:"sha256"`
}

// CrosscheckPass is the record of one attempt of one pass.
type CrosscheckPass struct {
	Pass         string `json:"pass"`
	Attempt      int    `json:"attempt"`
	State        string `json:"state"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	Model        string `json:"model"`
	PromptDigest string `json:"prompt_digest"`
	Voided       bool   `json:"voided"`
	// Retried is set when the pass answered invalid and this is the record of its one retry.
	Retried bool   `json:"retried"`
	Detail  string `json:"detail"`
}

// CrosscheckVerdictRecord is V, the verdict of a crosscheck: the word (verified | needs_changes), the merged findings, the ids and lines
// of the findings that do not block (carried as follow-ups), and everything the verdict stands on.
type CrosscheckVerdictRecord struct {
	Verdict        string                  `json:"verdict"`
	Findings       []reviewadapter.Finding `json:"findings"`
	Carried        []string                `json:"carried"`
	FollowUps      []string                `json:"follow_ups"`
	GuideRev       string                  `json:"guide_rev"`
	EngineFamily   string                  `json:"engine_family"`
	EngineFamilies []string                `json:"engine_families"`
	Model          string                  `json:"model"`
	Round          int                     `json:"round"`
	EvidenceRound  int                     `json:"evidence_round"`
	Milestone      string                  `json:"milestone"`
	Plan           string                  `json:"plan"`
	Head           string                  `json:"head"`
	Base           string                  `json:"base"`
	Run            string                  `json:"run"`
	AuthorFamilies []string                `json:"author_families"`
	Authors        []CrosscheckAuthor      `json:"authors"`
	PriorHead      string                  `json:"prior_head,omitempty"`
	PriorRun       string                  `json:"prior_run,omitempty"`
	PriorDigest    string                  `json:"prior_digest,omitempty"`
	Receipts       []CrosscheckReceipt     `json:"receipts"`
	ReceiptsFile   string                  `json:"receipts_file"`
	ReceiptsSHA256 string                  `json:"receipts_sha256"`
	Passes         []CrosscheckPass        `json:"passes"`
}

// CrosscheckQuestion is an evidence question of a blocking finding (state evidence).
type CrosscheckQuestion struct {
	ID          string   `json:"id"`
	Question    string   `json:"question"`
	Scope       string   `json:"scope"`
	FilesNeeded []string `json:"files_needed"`
}

// CrosscheckOutcome is X, the answer of crw.crosscheck: the state, the reason of a refused, failed or no_verdict crosscheck, the hold of a
// held one, what was found, V for state verdict (and evidence), and the evidence questions.
type CrosscheckOutcome struct {
	State     string                   `json:"state"`
	Reason    string                   `json:"reason"`
	Hold      string                   `json:"hold"`
	Detail    string                   `json:"detail"`
	Verdict   *CrosscheckVerdictRecord `json:"verdict"`
	Questions []CrosscheckQuestion     `json:"questions"`
}

// CrosscheckVerdictOf is the close rule (CrosscheckRule) over merged findings: needs_changes when a finding premerge.Blocks, else verified;
// carried are the ids of the findings that do not block.
func CrosscheckVerdictOf(findings []reviewadapter.Finding) (verdict string, carried []string) {
	verdict, carried = reviewadapter.VerdictVerified, []string{}
	for _, f := range findings {
		if premerge.Blocks(f.Severity, f.Impact) {
			verdict = reviewadapter.VerdictNeedsChanges
		} else {
			carried = append(carried, f.ID)
		}
	}
	return verdict, carried
}

// followUp is the line of a carried finding: `id [severity/impact] file:line text`.
func followUp(f reviewadapter.Finding) string {
	impact := f.Impact
	if impact == "" {
		impact = "none"
	}
	return fmt.Sprintf("%s [%s/%s] %s:%d %s", f.ID, f.Severity, impact, f.File, f.Line, f.Text)
}

// crosscheckOp is crw.crosscheck(milestone, gate, opts): one round of the crosscheck of a milestone over the gate the close passed. In
// order: the round (round 1 only; a prior is refused), the authors (an undecided pair refuses), the run's directory (it must be
// absolute), the engine (the first profile whose family is no author's), the executor receipts of the milestone's nodes (one that
// cannot be handed over holds), the prompts (over the limit fails), both checkouts (one that is not the head's tree fails), then the two
// passes, together. The row carries milestone, head, round, evidence_round, state, reason, hold and V as verdict.
type crosscheckOp struct{ l *Lane }

// crosscheckGate is the part of the gate (GateOutcome) the crosscheck reads.
type crosscheckGate struct {
	Milestone string `json:"milestone"`
	Plan      string `json:"plan"`
	Head      string `json:"head"`
	Base      string `json:"base"`
	Range     string `json:"range"`
}

func (o *crosscheckOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var gate crosscheckGate
	var opts CrosscheckOpts
	if err := args(req.Args, &ms, &gate, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck(milestone, gate, opts): %w", err)
	}
	cc := o.l.d.Crosscheck
	if cc == nil || cc.Checker == nil || cc.Profiles == nil {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: no crosschecker is wired", ms)
	}
	if ms == "" || gate.Head == "" {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: a milestone (%q) and the head of its gate (%q) are required", ms, gate.Head)
	}
	if cc.Limit <= 0 {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: the crosschecker has no time limit", ms)
	}
	r := rows(req.Journal.Events())
	m, plan, err := milestoneOf(r, ms)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: %w", err)
	}
	if m.Plan != "" {
		plan = m.Plan
	}
	round := opts.Round
	if round == 0 {
		round = 1
	}
	data := map[string]any{"milestone": ms, "plan": plan, "head": gate.Head, "round": round, "evidence_round": opts.EvidenceRound}
	finish := func(out CrosscheckOutcome) (engine.OpResult, error) {
		if out.Questions == nil {
			out.Questions = []CrosscheckQuestion{}
		}
		data["state"], data["reason"], data["detail"] = out.State, out.Reason, out.Detail
		if out.Hold != "" {
			data["hold"] = out.Hold
		}
		if out.Verdict != nil {
			data["verdict"] = out.Verdict
		} else {
			data["verdict"] = nil
		}
		return engine.OpResult{Result: result(out), Data: data}, nil
	}

	// the round: this revision serves round 1, which has no prior
	if present(opts.Prior) || round != 1 {
		return finish(CrosscheckOutcome{State: CrosscheckStateRefused, Reason: ReasonCrosscheckPriorInvalid,
			Detail: fmt.Sprintf("round %d with a prior %v: this crosscheck serves round 1 only, which has no prior", round, present(opts.Prior))})
	}

	// the authors: every pair the run's role table gives and every author call of the journal, none undecided
	authors, undecided := o.authors(cc, r)
	if len(undecided) > 0 {
		return finish(CrosscheckOutcome{State: CrosscheckStateRefused, Reason: ReasonCrosscheckAuthorUndecided,
			Detail: "the model of an author is not known, so no engine can be shown to be of another family: " + strings.Join(undecided, "; ")})
	}
	families := authorFamilies(authors)

	// the run's directory: the receipts file and the checkouts lie under it, never under a relative path
	root := o.l.d.LandRoot
	if root == "" || !filepath.IsAbs(root) {
		return finish(CrosscheckOutcome{State: CrosscheckStateFailed, Reason: ReasonCrosscheckNoRunDir,
			Detail: fmt.Sprintf("the run's directory %q is not an absolute path (Deps.LandRoot); nothing was written and no pass ran", root)})
	}

	// the engine: the first profile whose family is no author's
	profiles, err := cc.Profiles(root)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: the engine profiles: %w", ms, err)
	}
	profile, family, found := pickEngine(profiles, families)
	if !found {
		tried := make([]string, 0, len(profiles))
		for _, p := range profiles {
			tried = append(tried, p.Name+" ("+profileFamily(p)+")")
		}
		return finish(CrosscheckOutcome{State: CrosscheckStateRefused, Reason: ReasonCrosscheckNoIndependentEngine,
			Detail: fmt.Sprintf("no engine of %s is of another family than every author (%s)", strings.Join(tried, ", "), strings.Join(families, ", "))})
	}

	v := &CrosscheckVerdictRecord{Findings: []reviewadapter.Finding{}, Carried: []string{}, FollowUps: []string{}, GuideRev: CrosscheckGuideRev, EngineFamily: family,
		EngineFamilies: []string{family}, Model: profile.Model, Round: round, EvidenceRound: opts.EvidenceRound, Milestone: ms, Plan: plan, Head: gate.Head, Base: gate.Base,
		Run: req.RunID, AuthorFamilies: families, Authors: authors, Passes: []CrosscheckPass{}}
	data["engine_family"], data["model"] = family, profile.Model

	// the executor receipts of the milestone's nodes (C12)
	receipts, body, hold, holdNode, detail := milestoneReceipts(r, m)
	if hold != "" {
		data["node"] = holdNode
		return finish(CrosscheckOutcome{State: CrosscheckStateHeld, Hold: hold, Detail: detail + "; no pass ran"})
	}
	v.Receipts = receipts
	sum := sha256.Sum256(body)
	v.ReceiptsSHA256 = hex.EncodeToString(sum[:])
	if v.ReceiptsFile, err = writeReceiptsFile(filepath.Join(root, crosscheckDirectoryName), fmt.Sprintf("%s-r%d-receipts", pathName(ms), round), body, v.ReceiptsSHA256); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: the receipts file: %w", ms, err)
	}

	// the prompts: the script's pass texts and the receipts section
	if strings.TrimSpace(opts.Prompts.A) == "" || strings.TrimSpace(opts.Prompts.B) == "" {
		return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: the texts of both passes are required (opts.prompts.A and .B)", ms)
	}
	section := receiptsSection(v)
	prompts := map[string]string{CrosscheckPassA: withSection(opts.Prompts.A, section), CrosscheckPassB: withSection(opts.Prompts.B, section)}
	for _, pass := range []string{CrosscheckPassA, CrosscheckPassB} {
		if n := len(prompts[pass]); n > CrosscheckPromptLimit {
			return finish(CrosscheckOutcome{State: CrosscheckStateFailed, Reason: ReasonCrosscheckPromptTooLarge,
				Detail: fmt.Sprintf("the prompt of pass %s is %d bytes, over the limit of %d; it was not sent", pass, n, CrosscheckPromptLimit)})
		}
	}

	// both checkouts, each the head's tree, before either pass starts
	checkout := cc.Checkout
	if checkout == nil {
		checkout = o.l.crosscheckWorktree
	}
	type slot struct {
		pass string
		dir  string
		done func() error
		res  reviewadapter.Result
		rec  CrosscheckPass
		err  error
	}
	slots := []*slot{{pass: CrosscheckPassA}, {pass: CrosscheckPassB}}
	for _, s := range slots {
		dir, done, err := checkout(ctx, ms, round, opts.EvidenceRound, s.pass, 1, gate.Head)
		if err != nil {
			var ce *CrosscheckCheckoutError
			if errors.As(err, &ce) {
				return finish(CrosscheckOutcome{State: CrosscheckStateFailed, Reason: ce.Reason, Detail: fmt.Sprintf("pass %s: the checkout %s: %s; it is kept as it is and no pass ran", s.pass, ce.Dir, ce.Detail)})
			}
			return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: the checkout of pass %s: %w", ms, s.pass, err)
		}
		s.dir, s.done = dir, done
	}

	// the passes, together
	var wg sync.WaitGroup
	for _, s := range slots {
		wg.Add(1)
		go func(s *slot) {
			defer wg.Done()
			creq := reviewadapter.Request{Profile: profile, Worktree: s.dir, Prompt: prompts[s.pass], Schema: reviewadapter.AnswerSchema, Limit: cc.Limit, GuideRev: CrosscheckGuideRev}
			res, err := cc.Checker.Check(ctx, creq)
			retried := false
			if err == nil && res.State == reviewadapter.StateInvalid {
				res, err = cc.Checker.Check(ctx, creq)
				retried = true
			}
			if err != nil {
				s.err = fmt.Errorf("pass %s: %w", s.pass, err)
				return
			}
			s.res = res
			s.rec = CrosscheckPass{Pass: s.pass, Attempt: 1, State: string(res.State), ElapsedMS: res.ElapsedMS, Model: res.Model, PromptDigest: res.PromptDigest, Retried: retried, Detail: res.Detail}
		}(s)
	}
	wg.Wait()
	var cleanup []string
	for _, s := range slots {
		if s.err != nil || s.done == nil {
			continue
		}
		if err := s.done(); err != nil {
			cleanup = append(cleanup, fmt.Sprintf("pass %s: the checkout %s was not removed: %v", s.pass, s.dir, err))
		}
	}
	if len(cleanup) > 0 {
		data["cleanup"] = cleanup
	}
	for _, s := range slots {
		if s.err != nil {
			return engine.OpResult{}, fmt.Errorf("crw.crosscheck: milestone %s: %w", ms, s.err)
		}
	}
	for _, s := range slots {
		v.Passes = append(v.Passes, s.rec)
	}
	data["passes"] = v.Passes

	// a pass that failed fails the round, one that gave no judgment holds it; both answered: the verdict
	for _, s := range slots {
		switch s.res.State {
		case reviewadapter.StateOK, reviewadapter.StateQuota, reviewadapter.StateNoAnswer:
		case reviewadapter.StateWorktreeChanged, reviewadapter.StateToolFailed, reviewadapter.StateInvalid:
			return finish(CrosscheckOutcome{State: CrosscheckStateFailed, Reason: "crosscheck_" + string(s.res.State), Detail: fmt.Sprintf("pass %s: %s", s.pass, s.res.Detail)})
		default:
			// a state this op has no rule for is never a verdict
			return finish(CrosscheckOutcome{State: CrosscheckStateFailed, Reason: "crosscheck_pass_" + string(s.res.State),
				Detail: fmt.Sprintf("pass %s answered the state %q, which the crosscheck has no rule for: %s", s.pass, s.res.State, s.res.Detail)})
		}
	}
	for _, s := range slots {
		if s.res.State != reviewadapter.StateOK {
			return finish(CrosscheckOutcome{State: CrosscheckStateHeld, Hold: "crosscheck_" + string(s.res.State), Detail: fmt.Sprintf("pass %s gave no judgment (%s): %s", s.pass, s.res.State, s.res.Detail)})
		}
	}
	v.Findings = reviewadapter.MergeFindings(slots[0].res.Findings, slots[1].res.Findings)
	v.Verdict, v.Carried = CrosscheckVerdictOf(v.Findings)
	byID := map[string]reviewadapter.Finding{}
	for _, f := range v.Findings {
		byID[f.ID] = f
	}
	for _, id := range v.Carried {
		v.FollowUps = append(v.FollowUps, followUp(byID[id]))
	}
	return finish(CrosscheckOutcome{State: CrosscheckStateVerdict, Verdict: v})
}

// Proven: a recorded crosscheck stands whatever it found (its row is of the head, round and evidence round its call names); a held one
// is no result to replay, so a resume runs the round again.
func (o *crosscheckOp) Proven(_ context.Context, events []engine.Event, row engine.Event) (bool, string, error) {
	if h := dataStr(row, "hold"); h != "" {
		return false, "the crosscheck held (" + h + ")", nil
	}
	if v := CrosscheckProof(events, dataStr(row, "milestone"), dataStr(row, "head"), intOf(row.Data["round"]), intOf(row.Data["evidence_round"])); !v.Proven {
		return false, v.Why, nil
	}
	return true, "", nil
}

// present says whether a JSON member was given a value other than null.
func present(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null"
}

// authorCall is an author call row of the journal: the call, its role and node, and the model it ran (or was configured with).
type authorCall struct {
	CallID, Role, Node, Model string
}

// milestoneAuthorCalls are the author calls of the run's journal (the prepared rows milestoneAuthorModels reads: architect, executor and
// executor-t1), each once, in journal order, with the ones that name no model kept: such a call's model is undecided.
func (r rows) milestoneAuthorCalls() []authorCall {
	var out []authorCall
	seen := map[string]bool{}
	for _, ev := range r.prepared() {
		role := str(ev.Data, "role")
		switch role {
		case "architect", "executor", "executor-t1":
		default:
			continue
		}
		if seen[ev.CallID] {
			continue
		}
		seen[ev.CallID] = true
		model := str(ev.Data, "configured_model")
		if model == "" {
			model = str(ev.Data, "model")
		}
		out = append(out, authorCall{CallID: ev.CallID, Role: role, Node: str(ev.Data, "node"), Model: model})
	}
	return out
}

// authors are the authors the engine must be independent of: the run's role pairs (Crosscheck.Authors, by role name) and the author calls
// of the journal, and what of them is undecided (a role with no model, a call that names none). With neither, the authors are unknown.
func (o *crosscheckOp) authors(cc *Crosscheck, r rows) ([]CrosscheckAuthor, []string) {
	var out []CrosscheckAuthor
	var undecided []string
	roles := make([]string, 0, len(cc.Authors))
	for role := range cc.Authors {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		model := strings.TrimSpace(cc.Authors[role])
		if model == "" {
			undecided = append(undecided, "role "+role+" has no pair in the run's role table")
			continue
		}
		out = append(out, CrosscheckAuthor{Source: "role", Name: role, Model: model, Family: execution.ModelFamily(model)})
	}
	for _, c := range r.milestoneAuthorCalls() {
		if strings.TrimSpace(c.Model) == "" {
			undecided = append(undecided, fmt.Sprintf("call %s (%s of node %q) names no model", c.CallID, c.Role, c.Node))
			continue
		}
		out = append(out, CrosscheckAuthor{Source: "call", Name: c.CallID, Model: c.Model, Family: execution.ModelFamily(c.Model)})
	}
	if len(out) == 0 && len(undecided) == 0 {
		undecided = append(undecided, "the run names no author pair and the journal no author call")
	}
	for _, a := range out {
		if a.Family == "" {
			undecided = append(undecided, fmt.Sprintf("%s %s: the model %q names no family", a.Source, a.Name, a.Model))
		}
	}
	return out, undecided
}

// authorFamilies are the families of the authors, sorted, each once.
func authorFamilies(authors []CrosscheckAuthor) []string {
	out := []string{}
	for _, a := range authors {
		if !slices.Contains(out, a.Family) {
			out = append(out, a.Family)
		}
	}
	sort.Strings(out)
	return out
}

// profileFamily is the engine family of a profile in canonical form: its Family, else the family of its model ("" when neither names one).
func profileFamily(p reviewadapter.Profile) string {
	if p.Family != "" {
		f, _ := execution.CanonicalFamily(p.Family)
		return f
	}
	f, _ := execution.CanonicalFamily(execution.ModelFamily(p.Model))
	return f
}

// pickEngine is the first profile whose family is known and is none of the authors' families.
func pickEngine(profiles []reviewadapter.Profile, families []string) (reviewadapter.Profile, string, bool) {
	for _, p := range profiles {
		f := profileFamily(p)
		if f != "" && !slices.Contains(families, f) {
			return p, f, true
		}
	}
	return reviewadapter.Profile{}, "", false
}

// milestoneReceipts reads the executor receipt of each node of the milestone: the receipt of the node's newest completed unit-check row
// (R2 D-6). An accepted receipt's copy is read and must hash to its digest; not_assigned and no_executor are listed as they are, and a
// node with no unit-check row is no_executor. Any other receipt, or one that cannot be read or does not hash to its digest, is a hold
// (crosscheck_receipt_missing, _unverified, _not_copied, _mismatch) naming the node. It answers the receipts, the receipts file's bytes
// (a `## node <id> (sha256 <digest>)` block for each accepted receipt) and the hold.
func milestoneReceipts(r rows, m Milestone) ([]CrosscheckReceipt, []byte, string, string, string) {
	out := []CrosscheckReceipt{}
	var b strings.Builder
	fmt.Fprintf(&b, "# Executor receipts of milestone %s\n", m.ID)
	checks := r.opRows(OpUnitCheck)
	for _, n := range m.Nodes {
		// the node's newest check of a receipt: a recorded unit check (role unit-check), or a check that was not run because the receipt
		// could not be handed over (receipt_held, which records no role); a check not run for a moved tree or head judged no receipt
		row, ok := checks.last(func(ev engine.Event) bool {
			return dataStr(ev, "node") == n.ID && (dataStr(ev, "role") == execution.AgentUnitCheck || dataStr(ev, "state") == StateReceiptHeld)
		})
		if !ok {
			out = append(out, CrosscheckReceipt{Node: n.ID, State: ReceiptNoExecutor})
			continue
		}
		if dataStr(row, "state") == StateReceiptHeld {
			hold := strings.TrimPrefix(dataStr(row, "hold"), "receipt_")
			switch hold {
			case "missing", "unverified", "not_copied":
			default:
				hold = "unverified"
			}
			return nil, nil, "crosscheck_receipt_" + hold, n.ID, fmt.Sprintf("node %s: its newest unit check %s was not run: the executor receipt is held (%s): %s", n.ID, row.CallID,
				dataStr(row, "hold"), dataStr(row, "detail"))
		}
		rc, _ := row.Data["receipt"].(map[string]any)
		state := str(rc, "state")
		switch state {
		case ReceiptNotAssigned, ReceiptNoExecutor:
			out = append(out, CrosscheckReceipt{Node: n.ID, State: state})
			continue
		case "accepted":
		case "missing", "":
			return nil, nil, "crosscheck_receipt_missing", n.ID, fmt.Sprintf("node %s: the executor receipt of its unit check %s is missing", n.ID, row.CallID)
		default:
			return nil, nil, "crosscheck_receipt_unverified", n.ID, fmt.Sprintf("node %s: the executor receipt of its unit check %s is %s", n.ID, row.CallID, state)
		}
		digest := str(rc, "sha256")
		if digest == "" {
			return nil, nil, "crosscheck_receipt_unverified", n.ID, fmt.Sprintf("node %s: the accepted receipt of its unit check %s names no sha256", n.ID, row.CallID)
		}
		file := str(rc, "copy")
		if file == "" {
			file = str(rc, "path")
		}
		if file == "" || !filepath.IsAbs(file) {
			return nil, nil, "crosscheck_receipt_not_copied", n.ID, fmt.Sprintf("node %s: the accepted receipt of its unit check %s names no absolute file (%q)", n.ID, row.CallID, file)
		}
		body, err := readReceiptFile(file)
		if err != nil {
			return nil, nil, "crosscheck_receipt_not_copied", n.ID, fmt.Sprintf("node %s: the receipt %s of its unit check %s cannot be read: %v", n.ID, file, row.CallID, err)
		}
		if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != digest {
			return nil, nil, "crosscheck_receipt_mismatch", n.ID, fmt.Sprintf("node %s: the receipt %s does not hash to the sha256 %s its unit check %s recorded", n.ID, file, digest, row.CallID)
		}
		fmt.Fprintf(&b, "\n## node %s (sha256 %s)\n\n", n.ID, digest)
		b.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			b.WriteByte('\n')
		}
		out = append(out, CrosscheckReceipt{Node: n.ID, State: "accepted", SHA256: digest})
	}
	return out, []byte(b.String()), "", "", ""
}

// writeReceiptsFile writes the receipts of a round to <dir>/<name>.md and answers its path. A file that is there already with the same
// bytes (a resume of the same round) is used as it is; one with other bytes is the evidence of an earlier crosscheck's V and is never
// overwritten: the receipts go to <dir>/<name>-<sha12>.md instead, so every V's receipts_file keeps hashing to its receipts_sha256.
func writeReceiptsFile(dir, name string, body []byte, digest string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	for _, file := range []string{filepath.Join(dir, name+".md"), filepath.Join(dir, name+"-"+short(digest)+".md")} {
		old, err := os.ReadFile(file)
		switch {
		case err == nil && string(old) == string(body):
			return file, nil
		case err == nil:
			continue
		case !os.IsNotExist(err):
			return "", err
		}
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		if _, err := f.Write(body); err != nil {
			f.Close()
			return "", err
		}
		return file, f.Close()
	}
	return "", fmt.Errorf("the receipts files of %s in %s hold other bytes than the receipts of sha256 %s", name, dir, digest)
}

// receiptsSection is the EXECUTOR RECEIPTS section of a round-1 pass prompt: the file, its digest, each node's receipt and what to check
// against it.
func receiptsSection(v *CrosscheckVerdictRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nEXECUTOR RECEIPTS (the evidence receipts the executors wrote: what each says it ran, such as its red-first and mutation-red lines; "+
		"they are the executors' reports, not a judgment of the change)\nfile %s (sha256 %s)\n", v.ReceiptsFile, v.ReceiptsSHA256)
	for _, rc := range v.Receipts {
		if rc.State == "accepted" {
			fmt.Fprintf(&b, "node %s: accepted, sha256 %s\n", rc.Node, rc.SHA256)
		} else {
			fmt.Fprintf(&b, "node %s: %s\n", rc.Node, rc.State)
		}
	}
	b.WriteString("Read the file. For each criterion, check its red-first and mutation-red lines against the receipt of the node that claims them; " +
		"a criterion whose red record is missing or does not match the change is a finding.\n")
	return b.String()
}

func withSection(text, section string) string {
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + section
}

// pathName is a name safe as one path element.
func pathName(s string) string {
	return strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c == 0 {
			return '_'
		}
		return c
	}, s)
}

// crosscheckWorktree is the checkout of one pass: a detached worktree of the head at
// <LandRoot>/crosscheck-wt/<ms>-r<round>-e<evidence round>-<pass><attempt>-<head12>, made with `git worktree add --detach` in the
// repository. A directory that is there already (a resume) is used again only when its HEAD is the head and its working files are the
// head's tree (Snapshot == TreeOf): one at another commit is crosscheck_worktree_moved, one with a modified, deleted or untracked file
// crosscheck_worktree_dirty, and either is kept as it is. A fresh checkout is held to the same equality. done removes the checkout
// (`git worktree remove --force`, then `git worktree prune`) and refuses a path outside <LandRoot>/crosscheck-wt/.
func (l *Lane) crosscheckWorktree(ctx context.Context, ms string, round, evidenceRound int, pass string, attempt int, head string) (string, func() error, error) {
	if l.d.LandRoot == "" || !filepath.IsAbs(l.d.LandRoot) {
		return "", nil, fmt.Errorf("the run's directory %q is not an absolute path", l.d.LandRoot)
	}
	root := filepath.Join(l.d.LandRoot, crosscheckWorktreeDirectoryName)
	dir := filepath.Join(root, fmt.Sprintf("%s-r%d-e%d-%s%d-%s", pathName(ms), round, evidenceRound, pass, attempt, short(head)))
	g := l.d.Git
	clean := func() error {
		want, err := TreeOf(ctx, g, dir, head)
		if err != nil {
			return err
		}
		snap, err := Snapshot(ctx, g, dir)
		if err != nil {
			return err
		}
		if snap != want {
			return &CrosscheckCheckoutError{Reason: ReasonCrosscheckWorktreeDirty, Dir: dir,
				Detail: fmt.Sprintf("its working files (tree %s) are not the tree %s of %s: a file is modified, deleted or untracked", snap, want, head)}
		}
		return nil
	}
	if _, err := os.Lstat(dir); err == nil {
		at, err := Head(ctx, g, dir)
		if err != nil {
			return "", nil, &CrosscheckCheckoutError{Reason: ReasonCrosscheckWorktreeMoved, Dir: dir, Detail: fmt.Sprintf("its HEAD cannot be read: %v", err)}
		}
		if at != head {
			return "", nil, &CrosscheckCheckoutError{Reason: ReasonCrosscheckWorktreeMoved, Dir: dir, Detail: fmt.Sprintf("it is at %s, not at the head %s", at, head)}
		}
	} else if !os.IsNotExist(err) {
		return "", nil, err
	} else {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", nil, err
		}
		if _, err := g(ctx, l.d.Repo, nil, "worktree", "add", "--detach", dir, head); err != nil {
			return "", nil, fmt.Errorf("the checkout of %s at %s: %w", dir, head, err)
		}
	}
	if err := clean(); err != nil {
		return "", nil, err
	}
	done := func() error {
		rel, err := filepath.Rel(root, dir)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
			return fmt.Errorf("refusing to remove %s: it is not a checkout under %s", dir, root)
		}
		if _, err := g(context.WithoutCancel(ctx), l.d.Repo, nil, "worktree", "remove", "--force", dir); err != nil {
			return err
		}
		_, err = g(context.WithoutCancel(ctx), l.d.Repo, nil, "worktree", "prune")
		return err
	}
	return dir, done, nil
}
