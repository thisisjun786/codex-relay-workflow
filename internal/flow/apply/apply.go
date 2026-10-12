// Package apply is `crw flow apply` (CRW-1204, F11): the orchestrator's application of the receipt of an oversized issue.
//
// An issue the architect judged size_fit=child is released as a relay child that only coordinates: it runs an issue-lane whose executor
// calls make every commit, and its reviewer (another model family) rules on the head. The child's receipt reaches the orchestrator thread
// as a relay delivery, a model turn. In that turn `crw flow apply --event E --turn T --wait 10m` waits for the reviewer verdict the child's
// run journaled for the head of the event, checks what the relay cannot check itself - that the criteria digest the reviewer saw is the
// one the relationship is judged under, that every commit of base..head was made by an executor call the run recorded, and that the
// reviewer's model family differs from every author pair the run observed - and then does claim, acknowledgement and verdict in one process
// (relay receive, R01) from the orchestrator's own turn. A wait that ends first writes nothing and answers that the receipt is applied in
// a later turn.
package apply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The states of an application.
const (
	// StateApplied: the verdict is recorded on the relay (claim, acknowledgement and verdict, or their replay).
	StateApplied = "applied"
	// StateNextTurn: the wait ended before the child's run journaled the verdict; nothing was written, apply again in a later turn.
	StateNextTurn = "next_turn"
	// StateRefused: a check failed or the relay refused a step; Reason names it.
	StateRefused = "refused"
)

// The closed reasons of a refusal.
const (
	ReasonUnknownEvent           = "unknown_event"
	ReasonRunNotFound            = "run_not_found"
	ReasonEmptyRange             = "empty_range"
	ReasonAuthorUnattributed     = "author_unattributed"
	ReasonCriteriaDigestMismatch = "criteria_digest_mismatch"
	ReasonIndependenceRefused    = "independence_refused"
	ReasonIndependenceUnproven   = "independence_unproven"
	ReasonCorrectionUnavailable  = "correction_unavailable"
	ReasonReceiveRefused         = "receive_refused"
	ReasonVerdictInvalid         = "verdict_invalid"
)

// The rows apply writes into the child run's flow_events.
const (
	KindApplied  = "apply_applied"
	KindRefused  = "apply_refused"
	KindOverride = "apply_override"
)

// DefaultPoll is how often the journal is read while apply waits.
const DefaultPoll = 2 * time.Second

// IssueLane is the workflow name of the run apply reads.
const IssueLane = "issue-lane"

// Request is one application.
type Request struct {
	// Event is the relay event id of the child's receipt and Turn the orchestrator's own turn id (the turn the claim is made from, the
	// acknowledgement is proved from and the verdict is ruled from).
	Event, Turn string
	// Run is the child's issue-lane run; empty finds the newest issue-lane run of the child task the event came from.
	Run string
	// Base is the commit the child's work started from; empty takes the event's, then the base of the run's first executor worktree.
	Base string
	// Wait is how long to wait for the verdict; 0 reads the journal once.
	Wait time.Duration
	// Override, when it carries a reason, lets the application go on over a reviewer whose model family equals an author's (the reason
	// is recorded). It never lets an unattributed commit or a criteria digest that differs through.
	Override string
	// ManifestRequest is the --manifest-request document dag-correct --prepare takes; a needs_changes verdict needs it.
	ManifestRequest string
}

// EventInfo is what the relay holds about the event.
type EventInfo struct {
	ID, Relationship, ChildTask, ParentTask string
	// Head is the revision the event reports (the head of the child's work).
	Head string
	// Base is the commit the relationship's work is judged from, "" when the relay names none.
	Base string
	// CriteriaDigest is the digest of the criteria set the relationship is judged under.
	CriteriaDigest string
	// Plan and Node are the plan node the relationship executes, "" when it executes none.
	Plan, Node string
}

// Deps are the things apply works through. Store holds the child's journal; the rest is the relay and git, which a test replaces.
type Deps struct {
	Store *store.Store
	// Events reads the event from the relay.
	Events EventReader
	// Commits lists the commits of base..head (git rev-list), oldest first.
	Commits func(ctx context.Context, repo, base, head string) ([]string, error)
	// Receive runs claim, ack and verdict (relay receive) over the verdict file and answers what the relay answered.
	Receive func(ctx context.Context, in ReceiveInput) (json.RawMessage, error)
	// Correct prepares the correction of a needs_changes verdict (relay dag-correct --prepare) and answers the instruction line.
	Correct func(ctx context.Context, in CorrectionInput) (string, error)
	// Reconcile brings the summary of the plan up to date. Apply calls it every time it runs on an event the relay holds, with the child's
	// run ("" when none was found), whatever the outcome; its failure is reported in the result, not fatal.
	Reconcile func(ctx context.Context, ev EventInfo, run string) error
	// Repo is the git repository the commits are listed in.
	Repo string
	// Dir is where the verdict file is written (default: the temporary directory).
	Dir   string
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	Poll  time.Duration
}

// EventReader reads a relay event.
type EventReader interface {
	Event(ctx context.Context, id string) (EventInfo, error)
}

// ErrUnknownEvent is the reader's answer for an id the relay does not hold.
var ErrUnknownEvent = errors.New("apply: the relay holds no such event")

// ReceiveInput is one relay receive.
type ReceiveInput struct {
	Event, Turn, VerdictFile string
}

// CorrectionInput asks for the correction instruction of a node.
type CorrectionInput struct {
	Plan, Node, Actor, ManifestRequest string
}

// Attribution says which executor call made a commit and on which pair.
type Attribution struct {
	Commit string `json:"commit"`
	Call   string `json:"call"`
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

// Result is what apply came to.
type Result struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	Event  string `json:"event"`
	Run    string `json:"run,omitempty"`
	// Verdict is the reviewer's verdict (verified or needs_changes) the application carried.
	Verdict        string        `json:"verdict,omitempty"`
	Head           string        `json:"head,omitempty"`
	Base           string        `json:"base,omitempty"`
	ReviewerCall   string        `json:"reviewer_call,omitempty"`
	ReviewerModel  string        `json:"reviewer_model,omitempty"`
	CriteriaDigest string        `json:"criteria_digest,omitempty"`
	Attributions   []Attribution `json:"attributions,omitempty"`
	Unattributed   []string      `json:"unattributed,omitempty"`
	Override       string        `json:"override,omitempty"`
	// Correction is the dag-correct --prepare instruction line a needs_changes verdict carries.
	Correction string `json:"correction,omitempty"`
	// Escalated is set when the refusal is escalated (an escalation row is written into the run).
	Escalated bool `json:"escalated,omitempty"`
	// Relay is the answer of relay receive.
	Relay json.RawMessage `json:"relay,omitempty"`
	// SummaryError is why the summary could not be brought up to date after the verdict (the verdict stands).
	SummaryError string `json:"summary_error,omitempty"`
}

var (
	eventPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	headPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) sleep(ctx context.Context, dur time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Apply applies the receipt of an oversized child. An error is an infrastructure failure (the store, the relay command); a check that
// fails is a Result that is refused.
//
// Whatever the application comes to once the relay holds the event (applied, refused, or the wait ended first), the plan summary is
// reconciled before Apply returns: a run that died after its last batch is recovered by the next apply, not only by one that records a
// verdict.
func Apply(ctx context.Context, d Deps, req Request) (res Result, err error) {
	res = Result{Event: req.Event, Override: strings.TrimSpace(req.Override)}
	if !eventPattern.MatchString(req.Event) {
		return res, errors.New("apply: --event is a 32 character lowercase hex event id")
	}
	if strings.TrimSpace(req.Turn) == "" {
		return res, errors.New("apply: --turn is the orchestrator's own turn id")
	}
	if d.Store == nil || d.Events == nil || d.Receive == nil || d.Commits == nil {
		return res, errors.New("apply: the store, the event reader, the commit reader and the relay are required")
	}
	ev, err := d.Events.Event(ctx, req.Event)
	if errors.Is(err, ErrUnknownEvent) {
		return refuse(res, ReasonUnknownEvent, "the relay holds no event "+req.Event), nil
	}
	if err != nil {
		return res, err
	}
	defer func() {
		if err != nil || d.Reconcile == nil {
			return
		}
		if rerr := d.Reconcile(ctx, ev, res.Run); rerr != nil {
			res.SummaryError = rerr.Error()
		}
	}()
	res.Head = ev.Head
	if !headPattern.MatchString(ev.Head) {
		return refuse(res, ReasonVerdictInvalid, fmt.Sprintf("the event reports %q as its head, not a commit id", ev.Head)), nil
	}
	poll := d.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	deadline := d.now().Add(req.Wait)
	var view runView
	for {
		view, err = d.findVerdict(ctx, req, ev)
		if err != nil {
			return res, err
		}
		if view.found || view.refusal != "" {
			break
		}
		remaining := deadline.Sub(d.now())
		if remaining <= 0 {
			res.State = StateNextTurn
			res.Run = view.run
			res.Detail = "the run of the child has not journaled a reviewer verdict for " + ev.Head + " yet; apply again in a later turn"
			return res, nil
		}
		if err := d.sleep(ctx, min(poll, remaining)); err != nil {
			return res, err
		}
	}
	res.Run = view.run
	if view.refusal != "" {
		return refuse(res, view.refusal, view.detail), nil
	}
	return d.judge(ctx, req, ev, view, res)
}

func refuse(res Result, reason, detail string) Result {
	res.State, res.Reason, res.Detail = StateRefused, reason, detail
	return res
}

// runView is what the child's journal says about the receipt.
type runView struct {
	run     string
	found   bool
	refusal string
	detail  string
	events  []engine.Event
	review  engine.Event
}

// findVerdict reads the journal of the child's run for the reviewer verdict of the event's head.
func (d Deps) findVerdict(ctx context.Context, req Request, ev EventInfo) (runView, error) {
	var v runView
	run := req.Run
	if run == "" {
		var err error
		if run, err = d.childRun(ctx, ev); err != nil {
			return v, err
		}
	}
	v.run = run
	if run == "" {
		return v, nil
	}
	events, has, err := journal.Load(ctx, d.Store, run)
	if err != nil {
		return v, err
	}
	if !has {
		if req.Run != "" {
			v.refusal, v.detail = ReasonRunNotFound, "the store holds no run "+run
		}
		return v, nil
	}
	v.events = events
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Kind != engine.KindCall || e.Op != "crw.review" || e.Event != engine.EvCompleted {
			continue
		}
		if dataStr(e.Data, "state") != "verdict" || dataStr(e.Data, "head") != ev.Head {
			continue
		}
		v.found, v.review = true, e
		break
	}
	return v, nil
}

// childRun is the newest issue-lane run whose actor is the child task of the event.
func (d Deps) childRun(ctx context.Context, ev EventInfo) (string, error) {
	if ev.ChildTask == "" {
		return "", nil
	}
	var run string
	err := d.Store.DB.QueryRowContext(ctx, `SELECT run_id FROM flow_runs WHERE actor_task_id = ? AND workflow = ? ORDER BY started_at DESC, rowid DESC LIMIT 1`, ev.ChildTask, IssueLane).Scan(&run)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", nil
		}
		return "", err
	}
	return run, nil
}

// verdictDoc is the VERDICT document the reviewer answered (internal/flow/workflows/lib/schemas.js).
type verdictDoc struct {
	Verdict  string `json:"verdict"`
	Criteria []struct {
		ID       string `json:"id"`
		Met      bool   `json:"met"`
		Evidence string `json:"evidence"`
	} `json:"criteria"`
	Defects []struct {
		Ref      string `json:"ref"`
		Kind     string `json:"kind"`
		Severity string `json:"severity"`
		File     string `json:"file"`
		Line     int    `json:"line"`
		Finding  string `json:"finding"`
	} `json:"defects"`
	Summary string `json:"summary"`
}

// judge runs the checks and, when they hold, the relay receive.
func (d Deps) judge(ctx context.Context, req Request, ev EventInfo, v runView, res Result) (Result, error) {
	res.ReviewerCall = dataStr(v.review.Data, "call")
	res.ReviewerModel = dataStr(v.review.Data, "reviewer_model")
	saw := dataStr(v.review.Data, "criteria_digest")
	res.CriteriaDigest = saw
	var rr struct {
		Verdict json.RawMessage `json:"verdict"`
	}
	var doc verdictDoc
	if err := json.Unmarshal([]byte(dataStr(v.review.Data, "result")), &rr); err != nil || json.Unmarshal(rr.Verdict, &doc) != nil || (doc.Verdict != "verified" && doc.Verdict != "needs_changes") {
		return refuse(res, ReasonVerdictInvalid, "the review row of the run holds no VERDICT document"), nil
	}
	res.Verdict = doc.Verdict

	// 1. the criteria the reviewer saw are the ones the relationship is judged under
	if ev.CriteriaDigest == "" || saw != ev.CriteriaDigest {
		return refuse(res, ReasonCriteriaDigestMismatch, fmt.Sprintf("the reviewer judged criteria %q, the relationship is judged under %q", saw, ev.CriteriaDigest)), nil
	}

	// 2. every commit of base..head was made by an executor call the run recorded
	base := firstText(req.Base, ev.Base, executorBase(v.events))
	res.Base = base
	if !headPattern.MatchString(base) {
		return refuse(res, ReasonEmptyRange, fmt.Sprintf("the base of the child's work is %q: name it with --base", base)), nil
	}
	commits, err := d.Commits(ctx, firstText(d.Repo), base, ev.Head)
	if err != nil {
		return res, err
	}
	if len(commits) == 0 {
		return refuse(res, ReasonEmptyRange, fmt.Sprintf("%s..%s holds no commit", base, ev.Head)), nil
	}
	made := commitCalls(v.events)
	calls := journalCalls(v.events)
	var authors []string
	seen := map[string]bool{}
	for _, c := range commits {
		call, ok := made[c]
		info, known := calls[call]
		if !ok || !known || info.Family != "executor" || info.Terminal != engine.StatusCompleted {
			res.Unattributed = append(res.Unattributed, c)
			continue
		}
		model := firstText(info.Configured.Model, info.Pair.Model)
		res.Attributions = append(res.Attributions, Attribution{Commit: c, Call: call, Model: model, Effort: firstText(info.Configured.Effort, info.Pair.Effort)})
		if !seen[call] {
			seen[call] = true
			authors = append(authors, info.Configured.Model)
		}
	}
	if len(res.Unattributed) > 0 {
		res = refuse(res, ReasonAuthorUnattributed, fmt.Sprintf("%d of the %d commits of %s..%s were made by no executor call of run %s: %s", len(res.Unattributed), len(commits), base, ev.Head, v.run, strings.Join(res.Unattributed, ", ")))
		res.Escalated = true
		if err := d.writeRefusal(ctx, ev, v, res, true); err != nil {
			return res, err
		}
		return res, nil
	}

	// 3. the reviewer's family differs from every author pair the run observed (the coordinator's own pair is not an author)
	reviewer := firstText(calls[res.ReviewerCall].Configured.Model, res.ReviewerModel)
	if why := independent(reviewer, authors); why != "" {
		if res.Override == "" {
			res = refuse(res, why, fmt.Sprintf("reviewer %q and the observed authors %v are not independent", reviewer, authors))
			if err := d.writeRefusal(ctx, ev, v, res, false); err != nil {
				return res, err
			}
			return res, nil
		}
		if _, _, err := d.appender(v.run).AppendOnce(ctx, report.NewRow{Kind: KindOverride, Node: dataStr(v.review.Data, "node"), Payload: map[string]any{
			"apply_key": "override:" + ev.ID + ":" + why + ":" + ev.Head, "event": ev.ID, "check": why, "reason": res.Override, "reviewer": reviewer, "authors": authors, "head": ev.Head, "turn": req.Turn,
		}}, "apply_key"); err != nil {
			return res, err
		}
	}

	// the verdict the relay is given
	file, err := d.verdictFile(ctx, req, ev, v, doc, &res)
	if err != nil {
		return res, err
	}
	if res.State == StateRefused {
		return res, nil
	}
	defer os.Remove(file)
	answer, err := d.Receive(ctx, ReceiveInput{Event: ev.ID, Turn: req.Turn, VerdictFile: file})
	if err != nil {
		res = refuse(res, ReasonReceiveRefused, err.Error())
		if werr := d.writeRefusal(ctx, ev, v, res, false); werr != nil {
			return res, werr
		}
		return res, nil
	}
	res.State, res.Relay = StateApplied, answer
	if _, _, err := d.appender(v.run).AppendOnce(ctx, report.NewRow{Kind: KindApplied, Node: dataStr(v.review.Data, "node"), Payload: map[string]any{
		"apply_key": "applied:" + ev.ID + ":" + res.ReviewerCall, "event": ev.ID, "verdict": doc.Verdict, "head": ev.Head, "base": base, "turn": req.Turn,
		"reviewer_call": res.ReviewerCall, "criteria_digest": saw, "override": res.Override, "commits": len(commits),
	}}, "apply_key"); err != nil {
		return res, err
	}
	return res, nil
}

func (d Deps) appender(run string) report.Appender {
	return report.ForeignAppender{Store: d.Store, Run: run, Now: d.now}
}

// writeRefusal records a refusal once for the event, the reason and the head, and for an escalated one the escalation row the pump tells
// the manager thread about.
func (d Deps) writeRefusal(ctx context.Context, ev EventInfo, v runView, res Result, escalate bool) error {
	node := dataStr(v.review.Data, "node")
	key := "refused:" + ev.ID + ":" + res.Reason + ":" + ev.Head
	if _, _, err := d.appender(v.run).AppendOnce(ctx, report.NewRow{Kind: KindRefused, Node: node, Payload: map[string]any{
		"apply_key": key, "event": ev.ID, "reason": res.Reason, "detail": res.Detail, "head": ev.Head, "unattributed": res.Unattributed, "escalated": escalate,
	}}, "apply_key"); err != nil {
		return err
	}
	if !escalate {
		return nil
	}
	id := "esc_" + digest(v.run, ev.ID, res.Reason, ev.Head)[:24]
	_, _, err := d.appender(v.run).AppendOnce(ctx, report.NewRow{Kind: engine.EvEscalation, Node: node, Payload: map[string]any{
		"escalation_id": id, "reason": res.Reason, "class": "integrity", "node": node, "detail": res.Detail, "event": ev.ID, "ask": "oversized_receipt",
	}}, "escalation_id")
	return err
}

// verdictFile writes the verdict file relay receive reads. A needs_changes verdict carries the dag-correct --prepare instruction line as
// the restoration block of one finding.
func (d Deps) verdictFile(ctx context.Context, req Request, ev EventInfo, v runView, doc verdictDoc, res *Result) (string, error) {
	type finding struct {
		ID          string `json:"id"`
		Verdict     string `json:"verdict"`
		Note        string `json:"note,omitempty"`
		Restoration bool   `json:"restoration,omitempty"`
	}
	var findings []finding
	for _, c := range doc.Criteria {
		f := finding{ID: c.ID, Verdict: "verified", Note: strings.TrimSpace(c.Evidence)}
		if !c.Met {
			f.Verdict = "needs_changes"
		}
		findings = append(findings, f)
	}
	if len(findings) == 0 {
		*res = refuse(*res, ReasonVerdictInvalid, "the VERDICT evaluates no criterion")
		return "", nil
	}
	out := map[string]any{"expect-criteria-digest": ev.CriteriaDigest}
	reason := "applied by crw flow apply from run " + v.run + ", reviewer call " + res.ReviewerCall
	if res.Override != "" {
		reason += "; override: " + res.Override
	}
	out["reason"] = reason
	out["verdict"] = doc.Verdict
	if doc.Verdict == "verified" {
		out["verified-head"] = ev.Head
	} else {
		if d.Correct == nil || req.ManifestRequest == "" || ev.Plan == "" || ev.Node == "" {
			*res = refuse(*res, ReasonCorrectionUnavailable, "a needs_changes verdict carries the dag-correct --prepare instruction line: name the plan node of the relationship and --manifest-request")
			return "", nil
		}
		line, err := d.Correct(ctx, CorrectionInput{Plan: ev.Plan, Node: ev.Node, Actor: ev.ParentTask, ManifestRequest: req.ManifestRequest})
		if err != nil {
			*res = refuse(*res, ReasonCorrectionUnavailable, err.Error())
			return "", nil
		}
		res.Correction = line
		carrier := 0
		for i, f := range findings {
			if f.Verdict == "needs_changes" {
				carrier = i
				break
			}
		}
		for _, df := range doc.Defects {
			findings[carrier].Note += fmt.Sprintf("\n%s %s %s:%d %s", df.Ref, df.Severity, df.File, df.Line, strings.TrimSpace(df.Finding))
		}
		findings[carrier].Note = strings.TrimSpace(findings[carrier].Note + "\n" + line)
		findings[carrier].Restoration = true
		findings[carrier].Verdict = "needs_changes"
		out["restoration"] = findings[carrier].ID
	}
	out["criteria"] = findings
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", err
	}
	dir := d.Dir
	if dir == "" {
		dir = os.TempDir()
	}
	f, err := os.CreateTemp(dir, "crw-apply-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return "", err
	}
	return filepath.Clean(f.Name()), nil
}

// independent says why a reviewer is not an independent second opinion of the authors: "" when its model family differs from every one of
// theirs. A pair that was never read back proves nothing.
func independent(reviewer string, authors []string) string {
	if reviewer == "" || len(authors) == 0 {
		return ReasonIndependenceUnproven
	}
	for _, a := range authors {
		if a == "" {
			return ReasonIndependenceUnproven
		}
		if execution.ModelFamily(a) == execution.ModelFamily(reviewer) {
			return ReasonIndependenceRefused
		}
	}
	return ""
}

// commitCalls maps each commit the run made (its completed crw.commit rows) to the executor call it committed.
func commitCalls(events []engine.Event) map[string]string {
	out := map[string]string{}
	for _, e := range events {
		if e.Kind != engine.KindCall || e.Op != "crw.commit" || e.Event != engine.EvCompleted {
			continue
		}
		if c, call := dataStr(e.Data, "commit"), dataStr(e.Data, "call"); c != "" && call != "" {
			out[c] = call
		}
	}
	return out
}

// journalCalls reads every agent call of the run from its rows.
func journalCalls(events []engine.Event) map[string]engine.CallInfo {
	j := engine.NewMemoryJournal()
	ids := map[string]bool{}
	for _, e := range events {
		if e.Kind == engine.KindCall && e.Op == "agent" {
			j.Append(e)
			ids[e.CallID] = true
		}
	}
	out := map[string]engine.CallInfo{}
	for id := range ids {
		if info, ok := engine.LookupCall(j, id); ok {
			out[id] = info
		}
	}
	return out
}

// executorBase is the commit the run's first executor worktree started from.
func executorBase(events []engine.Event) string {
	for _, e := range events {
		if e.Kind != engine.KindCall || e.Op != "agent" || e.Event != engine.EvPrepared || dataStr(e.Data, "family") != "executor" {
			continue
		}
		if alloc, ok := e.Data["allocation"].(map[string]any); ok {
			if s := dataStr(alloc, "start_commit"); s != "" {
				return s
			}
		}
	}
	return ""
}

func dataStr(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func firstText(texts ...string) string {
	for _, t := range texts {
		if t != "" {
			return t
		}
	}
	return ""
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
