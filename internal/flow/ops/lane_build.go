package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
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

// OpUnitCheck is the label of crw.unitCheck (unitCheckOp), the op that records a unit check (a reviewadapter.Check of a node's work).
// Its completed row carries in Data the role (unit-check), node, execution_id, criteria_digest, tree (the fingerprint the check saw),
// commit when the check ran on a commit (none before it), and the reviewadapter.Result as result. The relay reads the same row
// (dagsched agentReview) and crw.accept assembles a premerge record from it.
const OpUnitCheck = "crw." + NameUnitCheck

// ---- crw.unitCheck ----

// UnitChecker runs one unit check; the shipped lane's is reviewadapter.Adapter.
type UnitChecker interface {
	Check(context.Context, reviewadapter.Request) (reviewadapter.Result, error)
}

// UnitCheck is what crw.unitCheck reaches outside the journal: the checker, the profile of the tool for a mode ("uncommitted" or
// "committed"), the profile a check of that mode falls back to when the first tool's quota is spent (Fallback; nil: no fallback, a
// quota holds the node) and the time limit of one call.
type UnitCheck struct {
	Checker  UnitChecker
	Profile  func(mode string) (reviewadapter.Profile, error)
	Fallback func(mode string) (reviewadapter.Profile, error)
	Limit    time.Duration
}

// The modes of a unit check: the uncommitted changes an executor call left in the node's worktree (the pending tree crw.commit will
// commit), or the committed range base..head of a checked-out head (land-head).
const (
	UnitCheckUncommitted = "uncommitted"
	UnitCheckCommitted   = "committed"
)

// UnitCheckGuideRev is the revision of the review guide unitCheckPrompt builds the prompt from.
const UnitCheckGuideRev = "crw-unit-check/1"

// StateTreeMoved is the state of a unit check that was not run because the node's worktree is no longer the pending tree of the call
// it was to judge: crw.commit commits that pending tree, so a check of another tree would be on no head.
const StateTreeMoved = "tree_moved"

// StateHeadMoved is the state of a unit check that was not run because the executor call it was to judge committed its work (its
// completed row records executor_head_moved and HEAD is not the start it was given): the uncommitted changes are not the change
// crw.commit commits on that start.
const StateHeadMoved = "head_moved"

// StateReceiptHeld is the state of an uncommitted unit check that was not run because the executor call it was to judge has no
// accepted evidence receipt to hand it (CRW-1463, C12): the row and the answer name the hold (receipt_missing, receipt_unverified,
// receipt_not_copied) the lane writes through crw.hold with source unit_check.
const StateReceiptHeld = "receipt_held"

// The receipt states a unit-check row records besides the backend's own (accepted): the judged call had no assignment (no backend
// recorded one), or the check judged a committed range, which no executor wrote.
const (
	ReceiptNotAssigned = "not_assigned"
	ReceiptNoExecutor  = "no_executor"
)

// unitCheckOp is crw.unitCheck(node, call, opts): one unit check of a node's work by the review tool, run in Go through Deps.UnitCheck
// (no agent call), recorded as one completed row of role unit-check that crw.accept and the relay take as the judgment of the head
// (CRW-1463). With no opts.commit it judges the uncommitted changes the executor call `call` left in the node's worktree: the
// worktree's snapshot must be that call's pending tree and its HEAD the start the call was given, else the tool is not run and the row
// says tree_moved or head_moved (no role: no reader takes it). With opts.commit and opts.base it judges the committed range base..commit checked out in the node's worktree.
//
// An uncommitted check reads the evidence receipt the judged call's completed row records (evidence_receipt, CRW-1463 C12): an accepted
// receipt's step-bus copy whose bytes hash to its digest goes into the prompt and the row records it (state, sha256, copy, ignored);
// a call with no evidence_receipt had no assignment and records not_assigned; any other receipt (missing, unverified, accepted but
// not copied, a copy that cannot be read or does not hash to the digest) is not handed over: the tool is not run, and the row carries
// the node, the call, the receipt as read and hold receipt_*, with no role, tree or commit, so no reader takes it and a resume reads
// the receipt again. A committed check judges a range no executor wrote and records no_executor.
//
// A committed check gives the request the range (Request.Base and Head), which a profile's Args may pass to the tool (the Codex
// committed profile's --base).
//
// The op holds nothing itself and never calls the relay. An invalid answer is asked once more with the same request; the second record
// is kept (retried, first_state invalid). When the tool's quota is spent (the final state of the first profile's calls is quota) and
// UnitCheck.Fallback is wired, the same request is asked once of the fallback profile for the mode (CRW-1517: Bugbot after a Codex
// review), with its own retry of an invalid answer; no other state falls back, and there is at most one fallback. The row's result is
// the record of the profile that answered last, which every reader takes; the first profile's quota record is the answer's fallback,
// and the row names it (fallback_from, the first profile's name; fallback_state quota; fallback_model; fallback_retried when that quota
// came on the retry of an invalid answer); retried and first_state are about the last profile's calls. A check whose last tool gave no
// judgment for its quota or no answer is recorded with role unit-check and hold unit_check_<state> on the row and in the answer, and is
// not proven: the lane holds the node through crw.hold with source unit_check, and a resume asks the tools again. worktree_changed and
// tool_failed are recorded and proven, for the lane to escalate. A Go error of the checker (a bad request, a fingerprint, the end of the
// context) or a fallback profile that cannot be had is the op's error and writes no row.
type unitCheckOp struct{ l *Lane }

// UnitCheckOpts are the options of crw.unitCheck.
type UnitCheckOpts struct {
	Round  int      `json:"round"`
	Final  bool     `json:"final"`
	Commit *HeadRef `json:"commit"`
	Base   string   `json:"base"`
}

// UnitCheckOutcome is the answer of crw.unitCheck: the adapter's record (the row's result, which crw.accept and the relay decode as a
// reviewadapter.Result), the hold the lane is to write (unit_check_quota, unit_check_no_answer; "" for none), whether the record is
// that of the retry of an invalid answer, the executor call it judged ("" for a committed check), the call id of the check itself
// (the reviewer call an acceptance on it names) and, when the check fell back after a quota, the first profile's quota record
// (Fallback; nil when it did not). State is tree_moved for a check that was not run.
type UnitCheckOutcome struct {
	reviewadapter.Result
	Hold     string                `json:"hold"`
	Retried  bool                  `json:"retried"`
	Call     string                `json:"call"`
	CheckID  string                `json:"check_id"`
	Fallback *reviewadapter.Result `json:"fallback,omitempty"`
}

func (o *unitCheckOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var ex CallRef
	var opts UnitCheckOpts
	if err := args(req.Args, &node, &ex, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.unitCheck(node, call, opts): %w", err)
	}
	uc := o.l.d.UnitCheck
	if uc == nil || uc.Checker == nil || uc.Profile == nil {
		return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: no unit checker is wired", node)
	}
	if node == "" {
		return engine.OpResult{}, fmt.Errorf("crw.unitCheck: a node is required")
	}
	mode, head := UnitCheckUncommitted, ""
	if opts.Commit != nil && opts.Commit.Commit != "" {
		mode, head = UnitCheckCommitted, opts.Commit.Commit
		if opts.Base == "" {
			return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: a committed check of %s needs the base of its range", node, head)
		}
		ex = CallRef{}
	} else if ex.CallID == "" {
		return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: an uncommitted check needs the executor call it judges", node)
	}
	profile, err := uc.Profile(mode)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: the %s profile: %w", node, mode, err)
	}
	r := rows(req.Journal.Events())
	wt := r.worktreeOf(node)
	if wt == "" {
		return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: no worktree is recorded", node)
	}
	g := o.l.d.Git
	data := map[string]any{"node": node, "mode": mode, "round": opts.Round, "final": opts.Final}
	snap := ""
	var receipt checkReceipt
	if mode == UnitCheckUncommitted {
		data["call"] = ex.CallID
		term, ok := r.terminal(ex.CallID)
		pending := ""
		if ok {
			pending = dataStr(term, "pending_tree")
		}
		if pending == "" {
			return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: no pending tree is recorded for call %s", node, ex.CallID)
		}
		if snap, err = Snapshot(ctx, g, wt); err != nil {
			return engine.OpResult{}, err
		}
		if snap != pending {
			data["state"], data["tree_moved"], data["pending_tree"], data["tree"] = StateTreeMoved, true, pending, snap
			out := UnitCheckOutcome{Result: reviewadapter.Result{State: StateTreeMoved, Detail: "the worktree is not the pending tree of call " + ex.CallID + "; the tool was not run",
				Findings: []reviewadapter.Finding{}}, Call: ex.CallID, CheckID: req.CallID}
			return engine.OpResult{Result: result(out), Data: data}, nil
		}
		// an executor that committed its work moved HEAD off the commit the node's attempt is at: the uncommitted changes are then not
		// the change crw.commit commits on that commit, so the tool would judge an empty or partial diff. The commit is the start the
		// session backend records for the call when it saw HEAD move (executor_head_moved), else the attempt's own (attemptHead), so a
		// backend that records no move (the App Server's) is read the same way.
		start := r.attemptHead(node)
		if moved, ok := term.Data["executor_head_moved"].(map[string]any); ok && str(moved, "start") != "" {
			start = str(moved, "start")
		}
		if start != "" {
			at, err := Head(ctx, g, wt)
			if err != nil {
				return engine.OpResult{}, err
			}
			if at != start {
				data["state"], data["pending_tree"], data["head_moved"] = StateHeadMoved, pending, map[string]any{"start": start, "head": at}
				out := UnitCheckOutcome{Result: reviewadapter.Result{State: StateHeadMoved, Detail: "the worktree's HEAD " + at + " is not the commit " + start +
					" the attempt of call " + ex.CallID + " is at (the executor committed); the uncommitted changes are not the change to commit, and the tool was not run", Findings: []reviewadapter.Finding{}},
					Call: ex.CallID, CheckID: req.CallID}
				return engine.OpResult{Result: result(out), Data: data}, nil
			}
		}
		var hold, detail string
		if receipt, hold, detail = executorReceipt(term); hold != "" {
			data["state"], data["hold"], data["receipt"], data["detail"] = StateReceiptHeld, hold, term.Data["evidence_receipt"], detail
			out := UnitCheckOutcome{Result: reviewadapter.Result{State: StateReceiptHeld, Detail: detail + "; the tool was not run", Findings: []reviewadapter.Finding{}},
				Hold: hold, Call: ex.CallID, CheckID: req.CallID}
			return engine.OpResult{Result: result(out), Data: data}, nil
		}
		data["receipt"] = receipt.row
	} else {
		data["receipt"] = map[string]any{"state": ReceiptNoExecutor}
		// the range is judged in the checkout of its head: a worktree at another commit would be a check of another tree
		at, err := Head(ctx, g, wt)
		if err != nil {
			return engine.OpResult{}, err
		}
		if at != head {
			return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: the worktree %s is at %s, not at the head %s to check", node, wt, at, head)
		}
		data["commit"], data["base"] = head, opts.Base
		// and in a clean checkout of it: a check of edited files would be recorded as the judgment of the commit
		commitTree, err := TreeOf(ctx, g, wt, head)
		if err != nil {
			return engine.OpResult{}, err
		}
		if snap, err = Snapshot(ctx, g, wt); err != nil {
			return engine.OpResult{}, err
		}
		if snap != commitTree {
			data["state"], data["tree_moved"], data["commit_tree"], data["tree"] = StateTreeMoved, true, commitTree, snap
			out := UnitCheckOutcome{Result: reviewadapter.Result{State: StateTreeMoved, Detail: "the worktree is not the tree of " + head + "; the tool was not run",
				Findings: []reviewadapter.Finding{}}, CheckID: req.CallID}
			return engine.OpResult{Result: result(out), Data: data}, nil
		}
	}
	if rel, ok := releaseRowOf(r, node); ok {
		data["execution_id"], data["criteria_digest"] = dataStr(rel, "execution_id"), dataStr(rel, "criteria_digest")
	}
	creq := reviewadapter.Request{Profile: profile, Worktree: wt, Prompt: unitCheckPrompt(r, node, mode, opts.Base, head, receipt), Schema: reviewadapter.AnswerSchema, Limit: uc.Limit,
		GuideRev: UnitCheckGuideRev}
	if mode == UnitCheckCommitted {
		// the range the reader of a committed profile takes an answer about (reviewadapter.Profile.Ranged), and the base a profile's Args
		// give the tool (the Codex committed profile's --base); the fallback's request carries the same range
		creq.Base, creq.Head = opts.Base, head
	}
	// check asks the tool of profile p, and once more with the same request on an invalid answer
	check := func(p reviewadapter.Profile) (reviewadapter.Result, bool, error) {
		r := creq
		r.Profile = p
		res, err := uc.Checker.Check(ctx, r)
		if err != nil {
			return res, false, fmt.Errorf("crw.unitCheck: node %s: %w", node, err)
		}
		if res.State != reviewadapter.StateInvalid {
			return res, false, nil
		}
		if res, err = uc.Checker.Check(ctx, r); err != nil {
			return res, true, fmt.Errorf("crw.unitCheck: node %s: the retry of an invalid answer: %w", node, err)
		}
		return res, true, nil
	}
	res, retried, err := check(profile)
	if err != nil {
		return engine.OpResult{}, err
	}
	out := UnitCheckOutcome{Call: ex.CallID, CheckID: req.CallID}
	if res.State == reviewadapter.StateQuota && uc.Fallback != nil {
		fallback, err := uc.Fallback(mode)
		if err != nil {
			return engine.OpResult{}, fmt.Errorf("crw.unitCheck: node %s: the %s fallback profile: %w", node, mode, err)
		}
		first := res
		if first.Findings == nil {
			first.Findings = []reviewadapter.Finding{}
		}
		out.Fallback = &first
		data["fallback_from"], data["fallback_state"], data["fallback_model"] = profile.Name, string(first.State), first.Model
		if retried {
			data["fallback_retried"] = true
		}
		if res, retried, err = check(fallback); err != nil {
			return engine.OpResult{}, fmt.Errorf("%w (the fallback after a %s quota)", err, profile.Name)
		}
	}
	if retried {
		data["first_state"] = string(reviewadapter.StateInvalid)
		out.Retried = true
	}
	if res.Findings == nil {
		res.Findings = []reviewadapter.Finding{}
	}
	out.Result = res
	switch res.State {
	case reviewadapter.StateQuota, reviewadapter.StateNoAnswer:
		out.Hold = "unit_check_" + string(res.State)
		data["hold"] = out.Hold
	}
	data["role"], data["state"], data["verdict"], data["retried"], data["findings"] = execution.AgentUnitCheck, string(res.State), res.Verdict, out.Retried, len(res.Findings)
	// the tree the check saw: the fingerprint the checker took before the call, else the snapshot the op compared with the pending tree
	data["tree"] = res.FingerprintBefore
	if res.FingerprintBefore == "" {
		data["tree"] = snap
	}
	return engine.OpResult{Result: result(out), Data: data}, nil
}

// Proven: a recorded check stands, whatever it found; one that holds the node (no judgment for the tool's quota or a blank answer) is
// no result to replay, so a resume asks the tool again.
func (o *unitCheckOp) Proven(_ context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	if h := dataStr(row, "hold"); h != "" {
		return false, "the unit check held the node (" + h + ")", nil
	}
	return true, "", nil
}

// unitCheckPrompt is the prompt of a unit check (guide UnitCheckGuideRev): the scope sentence first, in the words the tool's reader
// expects of its scope (the uncommitted changes of the working tree, or the range base..head), then the node and the criteria of its
// plan the change is judged against, and in uncommitted mode the executor's accepted evidence receipt (EXECUTOR RECEIPT) when it has one.
func unitCheckPrompt(r rows, node, mode, base, head string, receipt checkReceipt) string {
	var b strings.Builder
	if mode == UnitCheckCommitted {
		fmt.Fprintf(&b, "Review the changes between %s and %s (git diff %s..%s).\n", base, head, base, head)
	} else {
		b.WriteString("Review the uncommitted changes in this working tree.\n")
	}
	issue := node
	var criteria []string
	rel, ok := designRowOf(r, node)
	if !ok {
		rel, ok = releaseRowOf(r, node)
	}
	if ok {
		if n, found := planNodeOf(r, dataStr(rel, "plan"), dataStr(rel, "milestone"), node); found {
			criteria = n.Criteria
			if n.Issue != "" {
				issue = n.Issue
			}
		}
	}
	fmt.Fprintf(&b, "\nThis is the unit check of node %s (issue %s) of a crw run. Report every bug, regression or unmet criterion you find in the change.\n", node, issue)
	if len(criteria) > 0 {
		b.WriteString("\nCRITERIA (what the change must do)\n")
		for i, id := range CriterionIDs(len(criteria)) {
			fmt.Fprintf(&b, "%s: %s\n", id, criteria[i])
		}
	}
	if receipt.body != nil {
		fmt.Fprintf(&b, "\nEXECUTOR RECEIPT (the evidence receipt the executor wrote, sha256 %s: what it says it ran, such as its red-first and mutation-red lines; "+
			"it is the executor's report, not a judgment of the change)\n", receipt.digest)
		body := receipt.body
		if len(body) > UnitCheckReceiptPromptLimit {
			body = body[:UnitCheckReceiptPromptLimit]
			if i := strings.LastIndexByte(string(body), '\n'); i >= 0 {
				body = body[:i+1]
			}
		}
		b.Write(body)
		if len(body) == 0 || body[len(body)-1] != '\n' {
			b.WriteByte('\n')
		}
		if len(body) < len(receipt.body) {
			fmt.Fprintf(&b, "(the receipt is %d bytes; the first %d are above; the whole receipt is the file %s)\n", len(receipt.body), len(body), receipt.file)
		}
	}
	return b.String()
}

// UnitCheckReceiptPromptLimit is the most bytes of an executor receipt a unit-check prompt carries: the tool takes the prompt as one
// command-line argument (reviewadapter.ExecRun), which Linux bounds at 128 KiB, while a receipt may be up to 16 MiB. A longer receipt is
// cut at a line end within the limit and the prompt names the file of its whole copy.
const UnitCheckReceiptPromptLimit = 32 << 10

// checkReceipt is the executor receipt an uncommitted unit check hands the tool: the row it records and, for an accepted receipt,
// the bytes of its step-bus copy and their digest (body nil for none).
type checkReceipt struct {
	row    map[string]any
	body   []byte
	digest string
	// file is the file the bytes were read from: the step-bus copy, or the receipt where the hook accepted it
	file string
}

// executorReceipt reads the evidence receipt the completed row of an executor call records (engine evidenceReceipt): the receipt to
// hand the check, or the hold of a receipt that cannot be handed over and why. The copy is the absolute step-bus path the backend
// wrote; the op reads that path only and never builds one. When the backend could not copy the receipt (copy_error) or its copy cannot
// be read, the receipt is read where the hook accepted it (the absolute path the row records, in the executor's worktree) and handed
// over when it still hashes to the receipt's digest; the row then records that path and the copy's error. So receipt_not_copied, a
// failure of the copy, clears when the check runs again (a resume) and the receipt can be read; missing and unverified receipts stand on
// the call's own record, which no resume changes.
func executorReceipt(term engine.Event) (checkReceipt, string, string) {
	raw, present := term.Data["evidence_receipt"]
	if !present || raw == nil {
		return checkReceipt{row: map[string]any{"state": ReceiptNotAssigned}}, "", ""
	}
	er, ok := raw.(map[string]any)
	if !ok {
		return checkReceipt{}, "receipt_unverified", fmt.Sprintf("the evidence receipt of the call is not an object (%T)", raw)
	}
	switch state := str(er, "state"); state {
	case "accepted":
	case "missing", "":
		return checkReceipt{}, "receipt_missing", "the executor's evidence receipt is missing: " + str(er, "detail")
	default:
		return checkReceipt{}, "receipt_unverified", "the executor's evidence receipt is " + state + ": " + str(er, "detail")
	}
	cp, digest := str(er, "copy"), str(er, "sha256")
	var body []byte
	why := ""
	switch {
	case cp == "":
		why = "the accepted evidence receipt was not copied to the step bus: " + str(er, "copy_error")
	case !filepath.IsAbs(cp):
		why = "the copy " + cp + " of the accepted evidence receipt is not an absolute step-bus path"
	case digest == "":
		return checkReceipt{}, "receipt_unverified", "the accepted evidence receipt names no sha256"
	default:
		b, err := os.ReadFile(cp)
		if err != nil {
			why = "the copy of the accepted evidence receipt cannot be read: " + err.Error()
		} else {
			body = b
		}
	}
	row := map[string]any{"state": "accepted", "sha256": digest}
	file := cp
	if body == nil {
		// no copy to read: the receipt where the hook accepted it, when the row names it by an absolute path
		orig := str(er, "path")
		if digest == "" {
			return checkReceipt{}, "receipt_not_copied", why
		}
		if orig == "" || !filepath.IsAbs(orig) {
			return checkReceipt{}, "receipt_not_copied", why + "; the row names no absolute path of the receipt the hook accepted"
		}
		b, err := readReceiptFile(orig)
		if err != nil {
			return checkReceipt{}, "receipt_not_copied", why + "; the receipt the hook accepted cannot be read either: " + err.Error()
		}
		body, file = b, orig
		row["path"] = orig
		if ce := str(er, "copy_error"); ce != "" {
			row["copy_error"] = ce
		} else {
			row["copy_error"] = why
		}
	} else {
		row["copy"] = cp
	}
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != digest {
		return checkReceipt{}, "receipt_unverified", "the receipt " + file + " of the accepted evidence receipt does not hash to its sha256 " + digest
	}
	if ig, ok := er["ignored"].(bool); ok {
		row["ignored"] = ig
	}
	return checkReceipt{row: row, body: body, digest: digest, file: file}, "", ""
}

// receiptFileLimit is the most bytes of an evidence receipt read in place of its copy, the stop hook's own limit (16 MiB).
const receiptFileLimit = 16 << 20

// readReceiptFile reads an evidence receipt where the hook accepted it: a regular file, not through a symbolic link, of at most
// receiptFileLimit bytes (the step-bus copy, which the backend wrote, is read as it is).
func readReceiptFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	body, err := io.ReadAll(io.LimitReader(f, receiptFileLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > receiptFileLimit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, receiptFileLimit)
	}
	return body, nil
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
	// Warnings are what the verification warns of without failing (no_tests_run, CRW-1328); additive.
	Warnings []string `json:"warnings,omitempty"`
	// FailedTests are the failed steps of the record with the tests and the packages they name and the last lines of their output
	// (CRW-1469); additive, absent for a verification that names none.
	FailedTests []FailedStep `json:"failed_tests,omitempty"`
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
			if len(out.FailedTests) > 0 {
				res.FailedTests = out.FailedTests
				data["failed_tests"] = out.FailedTests
			}
			if out.Note != "" {
				data["verifier_note"] = out.Note
			}
			// a record that says how many packages ran tests is recorded with the count, and one that says none ran warns: the pass is
			// the record's, and the warning is the row's and the result's (CRW-1328)
			if out.TestPackagesRun != nil {
				data["test_packages_run"] = *out.TestPackagesRun
				if *out.TestPackagesRun == 0 {
					data["warning"] = WarnNoTestsRun
					res.Warnings = append(res.Warnings, WarnNoTestsRun+": the verification record of "+head.Commit+" says no test package ran tests")
				}
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
		// a head made outside a run is released with no DESIGN (CRW-1354): its release row names the plan and the milestone
		if rel, ok = releaseRowOf(r, node); !ok {
			return nil
		}
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
