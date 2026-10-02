package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// runKey names a run of a check across the readings of it: the name and the opaque run identity as a pair, never joined into one text and never ordered.
type runKey struct{ Name, Run string }

func keyOf(c Check) runKey { return runKey{c.Name, c.RunID} }

// newerReading is whether a reading of a run is newer than another: a later attempt, or, in the same attempt, a later time the forge last changed the run (a check run can be reset in
// place). Readings that carry no time cannot be told apart in one attempt, and the first stays.
func newerReading(a, b Check) bool {
	return a.Attempt > b.Attempt || (a.Attempt == b.Attempt && laterStamp(a.Stamp, b.Stamp))
}

// laterStamp is whether the time a is after the time b; times that do not parse, or are absent, cannot be told apart.
func laterStamp(a, b string) bool {
	x, errA := time.Parse(time.RFC3339Nano, a)
	y, errB := time.Parse(time.RFC3339Nano, b)
	return errA == nil && errB == nil && x.After(y)
}

// normalStamp is a time as the forge gave it, written in UTC, so the same instant in two spellings is one failure; text that is not a time stays as it was.
func normalStamp(s string) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return s
}

// reduceRuns keeps, of every run of the head, its newest reading. Everything that decides a judgement (the pending and failed checks, what an earlier judgement saw, whether a reading is
// older than the history) is computed from this reduction, so an earlier attempt that a listing still carries beside the newer one is never evidence of anything.
func reduceRuns(head string, checks []Check) map[runKey]Check {
	out := map[runKey]Check{}
	for _, c := range checks {
		if c.HeadSHA != head {
			continue
		}
		if cur, ok := out[keyOf(c)]; !ok || newerReading(c, cur) {
			out[keyOf(c)] = c
		}
	}
	return out
}

func failureLike(conclusion string) bool {
	return completedConclusions[conclusion] && conclusion != "success"
}

// failure is one required run counted as failed: the run and the reading of it that failed.
type failure struct {
	Name, Run string
	Attempt   int64
	Stamp     string
}

func (f failure) key() runKey { return runKey{f.Name, f.Run} }

func (f failure) String() string {
	text := f.Name + "|" + f.Run + "|" + strconv.FormatInt(f.Attempt, 10)
	if f.Stamp != "" {
		text += "|" + f.Stamp
	}
	return text
}

func failureStrings(list []failure) []string {
	out := make([]string, len(list))
	for i, f := range list {
		out[i] = f.String()
	}
	return out
}

// failuresJSON is the stored form of the failures of a judgement: a canonical list of objects, so no field is ever guessed out of a joined text.
func failuresJSON(list []failure) string {
	items := make([]any, len(list))
	for i, f := range list {
		item := map[string]any{"name": f.Name, "run": f.Run, "attempt": f.Attempt}
		if f.Stamp != "" {
			item["stamp"] = f.Stamp
		}
		items[i] = item
	}
	return dag.Canonical(items)
}

func parseFailures(text string) ([]failure, error) {
	var wire []struct {
		Name    string `json:"name"`
		Run     string `json:"run"`
		Attempt int64  `json:"attempt"`
		Stamp   string `json:"stamp"`
	}
	if err := json.Unmarshal([]byte(text), &wire); err != nil {
		return nil, err
	}
	out := make([]failure, len(wire))
	for i, w := range wire {
		out[i] = failure{w.Name, w.Run, w.Attempt, w.Stamp}
	}
	return out, nil
}

// requiredChecks classifies the checks the base branch requires for the exact head of the pull request, the way merge-evidence and the merge lane's own check read them: of every run
// only its newest reading counts, every such run of a required name (and of the integration the branch rule names for it) has to be a success, and no run ordering is guessed from the
// opaque run ids. A required (name, integration) pair with no run on that head, or with a run that has not finished, is pending; the runs that finished and are not a success are the
// failures. They count only when every required check has finished, so one red job seen before its siblings finish never uses up the retry.
func requiredChecks(pr PullRequest) (pending []string, failed []failure) {
	newest := reduceRuns(pr.HeadSHA, pr.Checks)
	runs := make([]runKey, 0, len(newest))
	for k := range newest {
		runs = append(runs, k)
	}
	slices.SortFunc(runs, func(a, b runKey) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Run, b.Run)
	})
	type need struct{ name, provider string }
	var needs []need
	for _, name := range pr.RequiredDeclared {
		if providers := pr.RequiredProviders[name]; len(providers) > 0 {
			for _, p := range providers {
				needs = append(needs, need{name, p})
			}
		} else {
			needs = append(needs, need{name, ""})
		}
	}
	seen := map[failure]bool{}
	for _, n := range needs {
		answered := false
		for _, k := range runs {
			c := newest[k]
			if c.Name != n.name || (n.provider != "" && c.Provider != n.provider) {
				continue
			}
			answered = true
			switch {
			case !completedConclusions[c.Conclusion]:
				pending = append(pending, n.name+": run "+c.RunID+" has not finished")
			case c.Conclusion != "success":
				f := failure{Name: c.Name, Run: c.RunID, Attempt: c.Attempt, Stamp: c.Stamp}
				if !seen[f] {
					seen[f] = true
					failed = append(failed, f)
				}
			}
		}
		if !answered {
			label := n.name
			if n.provider != "" {
				label += " (integration " + n.provider + ")"
			}
			pending = append(pending, label+": no run of the head")
		}
	}
	slices.Sort(pending)
	slices.SortFunc(failed, func(a, b failure) int { return strings.Compare(a.String(), b.String()) })
	return pending, failed
}

// judgeChecks applies the required-checks rule (5) to m, given the history of the same head.
func (s *Scheduler) judgeChecks(m *mergeCheck, h mergeHistory) {
	pending, failed := requiredChecks(m.Observed)
	if len(pending) == 0 {
		// failures count only once every required check has finished; a red job seen beside a running one is not recorded
		m.Failed = failed
	}
	names := strings.Join(failureStrings(failed), ", ")
	switch {
	case len(pending) > 0:
		m.Outcome, m.Reason = OutcomeChecksPending, "required checks have not finished on this head: "+strings.Join(pending, "; ")
	case len(failed) == 0 && len(m.Observed.CheckProblems) > 0:
		m.Outcome, m.Reason = OutcomeChecksPending, "the checks of this head are not usable evidence: "+strings.Join(m.Observed.CheckProblems, "; ")
	case len(failed) == 0:
		m.Outcome, m.Reason = OutcomeEligible, "every required check passed on this head"
		if h.failures > 0 {
			m.Reason = "every required check passed on this head after a recorded failure (a flaky check)"
		}
	case h.retries == 0:
		m.Outcome, m.Round, m.Reason = OutcomeRetrySameSHA, 1, "required checks failed ("+names+"): run them again on the same head, once"
	case !h.isNewFailure(failed):
		// the failure the retry round was opened on, read again: it is not a second failure
		m.Outcome, m.Round, m.Reason = OutcomeRetrySameSHA, 1, "required checks failed ("+names+") and the retry round is open: run them again on the same head, once"
	default:
		m.Outcome, m.Round, m.Reason = OutcomeEvicted, maxRounds, "a required check failed again on the same head after its retry ("+names+"): the node left the merge lane"
	}
}

type historicRow struct {
	seq             int64
	acceptance      string
	outcome, reason string
	round           int
	failed          []failure
}

// mergeHistory is what the judgements of one head already say. A failure counts once a judgement recorded it (every required check had finished); what a judgement saw of a run after that
// decides whether the next failure of it is the same failure read again or a new one.
type mergeHistory struct {
	retries  int
	failures int
	// last is the last failure recorded for each run; rerun marks the runs that a later judgement saw not failing (running, or passing); seen is the newest reading of each run any judgement
	// of the head read.
	last    map[runKey]failure
	rerun   map[runKey]bool
	seen    map[runKey]Check
	evicted *historicRow
}

// isNewFailure is whether any of the failures is not the failure the retry round was opened on, read again: a run no judgement recorded as failed, a reading of it that differs from the
// recorded one (another attempt, or the same run reset and completed at another time), or a run that a judgement saw not failing since it failed. Another check running again says
// nothing about this one.
func (h mergeHistory) isNewFailure(failed []failure) bool {
	for _, f := range failed {
		prev, had := h.last[f.key()]
		if !had || prev != f || h.rerun[f.key()] {
			return true
		}
	}
	return false
}

// staleReading is the check of the current reading that is older than a reading a judgement of the head already read, if any.
func (h mergeHistory) staleReading(head string, checks []Check) (Check, Check, bool) {
	for k, c := range reduceRuns(head, checks) {
		if prev, ok := h.seen[k]; ok && newerReading(prev, c) {
			return c, prev, true
		}
	}
	return Check{}, Check{}, false
}

// loadMergeHistory reads what the judgements of one head of a node already say, across every acceptance the node had of it and whichever pull request (or spelling of the repository: the forge
// does not tell owner/name apart by case) carried it: the checks ran on the commit, so a head that used up its retry or was evicted stays so when the node is accepted again at the same head.
// The rows come in the order they were written, each is verified against the digest it recorded, and each recorded listing is reduced to the newest reading of every run before anything is
// concluded from it.
func loadMergeHistory(ctx context.Context, q store.Querier, plan, node, forge, head string) (mergeHistory, error) {
	h := mergeHistory{last: map[runKey]failure{}, rerun: map[runKey]bool{}, seen: map[runKey]Check{}}
	rows, err := q.QueryContext(ctx, "SELECT c.check_seq, c.acceptance_id, c.outcome, c.reason, c.round_no, c.failed_required_json, c.checks_digest, c.evidence_json FROM dag_merge_checks c"+
		" JOIN dag_acceptances a ON a.acceptance_id = c.acceptance_id JOIN dag_acceptance_forge f ON f.acceptance_id = a.acceptance_id"+
		" WHERE a.plan_id = ? AND a.node_id = ? AND lower(f.forge_repository) = lower(?) AND c.observed_head_sha = ? ORDER BY c.rowid", plan, node, forge, head)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var r historicRow
		var failed, digest, evidence string
		if err := rows.Scan(&r.seq, &r.acceptance, &r.outcome, &r.reason, &r.round, &failed, &digest, &evidence); err != nil {
			return h, err
		}
		// the history decides how many retries are left, so a row that no longer digests to what was recorded (B-13) is not history
		if recomputed, err := RecomputeEvidenceDigest(evidence); err != nil || recomputed != digest {
			return h, refuse(contract.RefusalRevisionMismatch, "merge check %d of acceptance %s no longer digests to what was recorded", r.seq, r.acceptance)
		}
		if r.failed, err = parseFailures(failed); err != nil {
			return h, fmt.Errorf("a merge check of %s holds a failed list that is not JSON: %w", r.acceptance, err)
		}
		listing, err := parseEvidenceChecks(evidence)
		if err != nil {
			return h, err
		}
		checks := make([]Check, len(listing))
		for i, c := range listing {
			checks[i] = Check{Name: c.Name, RunID: c.RunID, HeadSHA: c.HeadSHA, Conclusion: c.Conclusion, Provider: c.Provider, Stamp: c.Stamp, Attempt: c.Attempt}
		}
		recorded := map[runKey]failure{}
		for _, f := range r.failed {
			recorded[f.key()] = f
		}
		for k, c := range reduceRuns(head, checks) {
			if prev, ok := h.seen[k]; !ok || newerReading(c, prev) {
				h.seen[k] = c
			}
			switch f, counted := recorded[k]; {
			case counted:
				if h.last[k] != f || h.rerun[k] {
					h.last[k] = f
					h.rerun[k] = false
				}
			case !failureLike(c.Conclusion):
				// seen running or passing; a failure that no judgement counted (a check was still running beside it) is neither
				if _, had := h.last[k]; had {
					h.rerun[k] = true
				}
			}
		}
		if r.outcome == OutcomeRetrySameSHA {
			h.retries++
		}
		if len(r.failed) > 0 {
			h.failures++
		}
		if r.outcome == OutcomeEvicted && h.evicted == nil {
			row := r
			h.evicted = &row
		}
	}
	return h, rows.Err()
}
