// Subset ported for todo 26 (mergeevidence.py review_shape_problems, review_problems and
// checks_problems as the merge turn calls them, providers=None); todo 24 owns and extends this.

package evidence

import (
	"math/big"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// Problem codes, mergeevidence.py:32-36.
const (
	Malformed              = "malformed_evidence"
	ReviewUnstated         = "review_unstated"
	ReviewIncomplete       = "review_incomplete"
	ChecksStale            = "checks_stale"
	ChecksNotRun           = "checks_not_run"
	RequiredUndeclared     = "required_undeclared"
	ReviewChangesRequested = "review_changes_requested"
	CandidateNotOpen       = "candidate_not_open"
	CandidateDraft         = "candidate_draft"
	CandidateConflicted    = "candidate_conflicted"
	CandidateBlocked       = "candidate_blocked"
	CandidateBehind        = "candidate_behind"
	CandidateUnknown       = "candidate_unknown"
)

// ReviewFields is REVIEW_FIELDS: the five members the child's own record states. A review thread
// that arrives after that record is not one of them. The coordinator's disposition of such a thread
// is a separate input of the restatement (latedisposition.go), never a review field, so the child's
// record keeps stating exactly these five and the coverage checks below read nothing else.
var ReviewFields = []string{"hasNextPage", "pagesRead", "totalCount", "threadsSeen", "unresolved"}

var counts = []string{"pagesRead", "totalCount", "unresolved"}

// Problem is mergeevidence.Problem.
type Problem struct{ Code, Detail, Incumbent string }

// Details is mergeevidence.details.
func Details(problems []Problem) []string {
	out := make([]string, len(problems))
	for i, p := range problems {
		out[i] = p.Detail
	}
	return out
}

// ReviewShapeProblems is mergeevidence.review_shape_problems (CRW-232).
func ReviewShapeProblems(review any) []Problem {
	var problems []Problem
	bad := func(detail string) { problems = append(problems, Problem{Code: Malformed, Detail: detail}) }
	o, ok := Object(review)
	if !ok {
		bad("the review record is an object stating " + strings.Join(ReviewFields, ", ") + ", not " + quote.Kind(review))
		return problems
	}
	if v, present := o.Lookup("hasNextPage"); present {
		if _, isBool := v.(bool); !isBool {
			bad("hasNextPage is true or false, not " + quote.Kind(v) + "; a truthy value of another type says nothing about pagination")
		}
	}
	for _, name := range counts {
		v, present := o.Lookup(name)
		if !present {
			continue
		}
		if n, isInt := Whole(v); !isInt {
			bad(name + " is a whole number, not " + quote.Kind(v))
		} else if n.Sign() < 0 {
			bad(name + " is " + pyvalue.Repr(v) + ", and a count is never negative")
		}
	}
	if seen, present := o.Lookup("threadsSeen"); present {
		items, isList := List(seen)
		if !isList {
			bad("threadsSeen is a list of thread identifiers, not " + quote.Kind(seen) + "; a string would be counted one character at a time")
		} else {
			for position, one := range items {
				if _, isStr := one.(string); !isStr {
					bad("threadsSeen entry " + pyvalue.Str(int64(position)) + " is a thread identifier string, not " + quote.Kind(one) + "; coercing it would let two different values agree")
				}
			}
		}
	}
	return problems
}

// ReviewProblems is mergeevidence.review_problems. Run ReviewShapeProblems first.
func ReviewProblems(review any) []Problem {
	o, _ := Object(review)
	var unstated []Problem
	for _, name := range ReviewFields {
		if _, present := o.Lookup(name); !present {
			unstated = append(unstated, Problem{Code: ReviewUnstated, Detail: "the review record does not state " + name})
		}
	}
	if len(unstated) > 0 {
		return unstated
	}
	var problems []Problem
	incomplete := func(detail string) { problems = append(problems, Problem{Code: ReviewIncomplete, Detail: detail}) }
	if pyvalue.Truthy(o.Get("hasNextPage")) {
		incomplete("hasNextPage is still true, so the review was not enumerated")
	}
	if pages := intOrZero(o.Get("pagesRead")); pages.Sign() < 1 {
		incomplete("no review page was read")
	}
	seen, _ := List(o.Get("threadsSeen"))
	if text, ok := o.Get("threadsSeen").(string); ok {
		seen = make([]any, 0, len([]rune(text)))
		for _, one := range text {
			seen = append(seen, string(one))
		}
	}
	total := intOrZero(o.Get("totalCount"))
	var identifiers []string
	for _, one := range seen {
		text := ""
		if pyvalue.Truthy(one) {
			text = pyvalue.Str(one)
		}
		if strings.TrimSpace(text) != "" {
			identifiers = append(identifiers, pyvalue.Str(one))
		}
	}
	distinct := map[string]bool{}
	for _, id := range identifiers {
		distinct[id] = true
	}
	if len(identifiers) != len(seen) {
		incomplete("threadsSeen contains a blank identifier")
	}
	if len(distinct) != len(identifiers) {
		incomplete("threadsSeen repeats an identifier, so its length is not a count of threads actually seen")
	}
	if big.NewInt(int64(len(distinct))).Cmp(total) != 0 {
		incomplete("totalCount is " + pyvalue.Str(total) + " and " + pyvalue.Str(int64(len(seen))) + " threads were seen")
	}
	if intOrZero(o.Get("unresolved")).Sign() != 0 {
		incomplete(pyvalue.Str(o.Get("unresolved")) + " threads are unresolved")
	}
	return problems
}

// intOrZero is int(v or 0) for a value the shape check already passed.
func intOrZero(v any) *big.Int { return Integer(Or(v, 0)) }

// attempt is int(entry.get("attempt", 1) or 1).
func attempt(entry any) *big.Int {
	o, _ := Object(entry)
	v, present := o.Lookup("attempt")
	if !present || !pyvalue.Truthy(v) {
		return big.NewInt(1)
	}
	return Integer(v)
}

func textField(entry any, name string) string {
	o, _ := Object(entry)
	v, present := o.Lookup(name)
	if !present {
		return ""
	}
	return pyvalue.Str(v)
}

// ShapeProblems is mergeevidence.shape_problems. It must run before semantic predicates.
func ShapeProblems(review, checks, required any, head *string) []Problem {
	var problems []Problem
	bad := func(detail string) { problems = append(problems, Problem{Code: Malformed, Detail: detail}) }
	if head != nil && strings.TrimSpace(*head) == "" {
		bad("the head this evidence is about is a non-empty commit sha, not " + quote.Value(*head))
	}
	problems = append(problems, ReviewShapeProblems(review)...)
	if required != nil {
		items, ok := List(required)
		if !ok {
			bad("the required check names are a list, not " + quote.Kind(required))
		} else {
			for _, one := range items {
				if _, ok := one.(string); !ok {
					bad("required check name " + quote.Value(one) + " is a string, not " + quote.Kind(one))
				}
			}
		}
	}
	items, ok := List(checks)
	if !ok {
		bad("the restated checks are a list of check runs, not " + quote.Kind(checks))
		return problems
	}
	for position, entry := range items {
		o, ok := Object(entry)
		where := "check entry " + pyvalue.Str(int64(position))
		if !ok {
			bad(where + " is an object naming its runId, name, headSha, conclusion and attempt, not " + quote.Kind(entry))
			continue
		}
		for _, field := range []string{"runId", "name", "headSha", "conclusion"} {
			if value, present := o.Lookup(field); present {
				if _, ok := value.(string); !ok {
					bad(where + " states " + field + " as " + quote.Kind(value) + ", not a string; coercing it would let two different runs agree")
				}
			}
		}
		value, present := o.Lookup("attempt")
		if !present {
			bad(where + " does not state attempt, so which attempt is newest cannot be decided; an omitted attempt is not evidence that this is the newest")
		} else if n, ok := Whole(value); !ok {
			bad(where + " states attempt " + quote.Value(value) + ", which is not a whole number, so which attempt is newest cannot be decided")
		} else if n.Sign() < 1 {
			bad(where + " states attempt " + quote.Value(value) + ", and attempts are counted from one; a lower value is read as the first attempt and hides the newest one")
		}
		if value, present := o.Lookup("notRun"); present {
			flag, isBool := value.(bool)
			if !isBool {
				bad(where + " states notRun as " + quote.Kind(value) + ", not true or false; the collector sets it to say a job began no step, and a value of another type cannot say that")
			} else if flag && !stepLessConclusion(o.Get("conclusion")) {
				// A job that began no step concluded cancelled, failure or timed_out, so a restated
				// record that marks any other conclusion could steer the lane toward a rerun on a
				// job the collector never produces that way (CRW-681).
				bad(where + " states notRun true on conclusion " + quote.Value(o.Get("conclusion")) + ", and a job that began no step concluded cancelled, failure or timed_out; another conclusion cannot be a job whose runner never picked it up")
			}
		}
		if value, present := o.Lookup("testSkipped"); present {
			flag, isBool := value.(bool)
			if !isBool {
				bad(where + " states testSkipped as " + quote.Kind(value) + ", not true or false; the collector sets it to say a test leg concluded success without running its test step, and a value of another type cannot say that")
			} else if flag {
				// Only a go-product test leg concluded success without running its tests, so a
				// restated record that marks any other job could steer the lane toward treating an
				// untested head as tested (CRW-824). The comparison is exact, as stepLessConclusion's
				// is: the collector emits the forge's canonical spelling.
				if !strings.HasPrefix(textField(entry, "name"), lightLegPrefix) {
					bad(where + " states testSkipped true on " + quote.Value(o.Get("name")) + ", and only a " + lightLegPrefix + "*) leg concluded success without running its tests; another job name cannot be one")
				} else if o.Get("conclusion") != "success" {
					bad(where + " states testSkipped true on conclusion " + quote.Value(o.Get("conclusion")) + ", and a leg whose tests were skipped concluded success; another conclusion cannot be a leg whose tests were skipped")
				}
			}
		}
	}
	return problems
}

// stepLessConclusion reports whether a conclusion is exactly one a job that began no step can
// carry: cancelled, failure or timed_out. The comparison is exact rather than normalized, because
// the collector emits the forge's canonical spelling and a restated record that spells one of these
// another way must be refused rather than read as one of them (CRW-681).
func stepLessConclusion(conclusion any) bool {
	s, ok := conclusion.(string)
	if !ok {
		return false
	}
	switch s {
	case "cancelled", "failure", "timed_out":
		return true
	}
	return false
}

// workflowRun is the workflow run a check entry belongs to, read from the collector's
// "workflow-run:<run id>:<job name>#<index>" identity. An entry that is not a workflow job (a
// published check run or a commit status) names its own run and holds no job, so it answers "".
func workflowRun(runId string) string {
	rest, ok := strings.CutPrefix(runId, "workflow-run:")
	if !ok {
		return ""
	}
	run, _, ok := strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	return run
}

// notRunJobs lists the names of the entries of one workflow run, at one attempt, that the
// collector marked notRun: the jobs whose runner never picked them up. The flag is read as a strict
// boolean, so a restated record that states anything else cannot steer the lane toward a rerun.
func notRunJobs(checks []any, run string, at *big.Int) []string {
	if run == "" {
		return nil
	}
	var names []string
	for _, entry := range checks {
		if workflowRun(textField(entry, "runId")) != run || attempt(entry).Cmp(at) != 0 {
			continue
		}
		o, _ := Object(entry)
		// A job that began no step concluded cancelled, failure or timed_out: the collector marks
		// nothing else, and a restated record that says otherwise cannot steer the lane toward a
		// rerun (CRW-661, CRW-681).
		if !stepLessConclusion(o.Get("conclusion")) {
			continue
		}
		flag, isBool := o.Get("notRun").(bool)
		if !isBool || !flag {
			continue
		}
		names = append(names, textField(entry, "name"))
	}
	slices.Sort(names)
	return names
}

// testSkippedJobs lists the names of the entries of one workflow run that the collector marked
// testSkipped: go-product test legs that concluded success without running their test step
// (CRW-824). The flag is read as a strict boolean and the name must be a test leg, so a restated
// record that states anything else cannot make the lane treat an untested head as tested. Each leg
// is read at its OWN newest attempt within the run rather than at the judged check's attempt: a
// rerun of the failed jobs leaves the successful skipped legs at an earlier attempt than the
// rerun's dev-gate, and that leg still says the head's tests did not run. Reading it at its own
// newest attempt only ever refuses more, so it cannot widen what merges.
func testSkippedJobs(checks []any, run string, highest map[string]*big.Int) []string {
	if run == "" {
		return nil
	}
	var names []string
	for _, entry := range checks {
		runId := textField(entry, "runId")
		if workflowRun(runId) != run {
			continue
		}
		if newest, seen := highest[runId]; seen && attempt(entry).Cmp(newest) != 0 {
			// An entry the same job superseded is not the job's result: its newest attempt is.
			continue
		}
		if !strings.HasPrefix(textField(entry, "name"), lightLegPrefix) {
			continue
		}
		o, _ := Object(entry)
		if o.Get("conclusion") != "success" {
			continue
		}
		flag, isBool := o.Get("testSkipped").(bool)
		if !isBool || !flag {
			continue
		}
		names = append(names, textField(entry, "name"))
	}
	slices.Sort(names)
	return names
}

// testedElsewhere reports whether another workflow run on this head answered the same required
// check, for the same integration the branch rule names, successfully and with its tests actually
// run: the lane's own repair for a light run is to label the pull request crw-lane, which starts a
// full run on the same head, and that run's evidence is what the head should be judged on. Without
// this the earlier light run's entry would refuse the head forever, and the documented repair could
// never produce merge evidence (CRW-824).
//
// The substitute must be a workflow run of the same integration: a namesake from another provider
// does not answer this branch's gate, and a published check run or a commit status holds no jobs at
// all, so reading either as the evidence that repaired a light run would let an untested head
// through. A candidate that holds a skipped leg of its own is not the evidence either, and a leg
// whose step list the collector could not read leaves its own unreadable problem, which refuses the
// whole reading before this predicate is consulted.
func testedElsewhere(checks []any, head, name, provider, run string, highest map[string]*big.Int) bool {
	for _, entry := range checks {
		runId := textField(entry, "runId")
		candidate := workflowRun(runId)
		if candidate == "" || candidate == run || textField(entry, "name") != name {
			continue
		}
		if newest, seen := highest[runId]; seen && attempt(entry).Cmp(newest) != 0 {
			continue
		}
		o, _ := Object(entry)
		if o.Get("headSha") != any(head) || o.Get("conclusion") != "success" || textField(entry, "provider") != provider {
			continue
		}
		if len(testSkippedJobs(checks, candidate, highest)) == 0 {
			return true
		}
	}
	return false
}

// beganFailureBeside reports whether the same workflow run and attempt as a required check holds a
// job other than that check which began a step (the collector did not mark it notRun) and concluded
// failure or timed_out. Such a job says the commit was tested and failed, so a not-run sibling
// cannot make the required failure read as a runner problem (CRW-676, CRW-681). The judged check is
// excluded by its own run identity, which the collector makes unique per job entry within a run
// attempt. A sibling is read at its own newest attempt, so an older failure the same job later
// replaced by a success is not a failure standing beside the required check (CRW-681).
func beganFailureBeside(checks []any, run string, at *big.Int, requiredRunId string, highest map[string]*big.Int) bool {
	if run == "" {
		return false
	}
	for _, entry := range checks {
		runId := textField(entry, "runId")
		if runId == requiredRunId {
			continue
		}
		if workflowRun(runId) != run || attempt(entry).Cmp(at) != 0 {
			continue
		}
		if newest, seen := highest[runId]; seen && attempt(entry).Cmp(newest) != 0 {
			// An entry the same job superseded is not the job's result: its newest attempt is.
			continue
		}
		o, _ := Object(entry)
		if !failureConclusion(o.Get("conclusion")) {
			continue
		}
		if flag, isBool := o.Get("notRun").(bool); isBool && flag {
			continue
		}
		return true
	}
	return false
}

// failureConclusion reports whether a sibling conclusion is exactly a real failure: failure or
// timed_out. A cancelled, skipped or neutral job is not a failure standing beside the required
// check, and a restated record that spells a real failure another way cannot make the lane treat
// the run as tested (CRW-681).
func failureConclusion(conclusion any) bool {
	s, ok := conclusion.(string)
	if !ok {
		return false
	}
	switch s {
	case "failure", "timed_out":
		return true
	}
	return false
}

// ChecksProblems is mergeevidence.checks_problems with the merge-turn defaults.
func ChecksProblems(head string, required []string, checks []any) []Problem {
	return ChecksProblemsWith(head, required, checks, false, nil)
}

// ChecksProblemsWith is the full child-side predicate, including declaration and provider identity.
func ChecksProblemsWith(head string, required []string, checks []any, requireDeclared bool, providers map[string][]string) []Problem {
	if requireDeclared && required == nil {
		return []Problem{{Code: RequiredUndeclared, Detail: "the record does not state which checks this branch requires, so a failing required check cannot be told from a failing optional one; read the branch protection and declare the names, or declare an empty list to say it requires none"}}
	}
	stale := func(detail, incumbent string) []Problem {
		return []Problem{{Code: ChecksStale, Detail: detail, Incumbent: incumbent}}
	}
	if len(checks) == 0 {
		return stale("no check runs were restated, so nothing says this head is green", "")
	}
	for _, entry := range checks {
		if strings.TrimSpace(textField(entry, "runId")) == "" || strings.TrimSpace(textField(entry, "name")) == "" {
			return stale("a restated check carries no runId or no name, so there is nothing to say which check it is or to compare against a required set", "")
		}
	}
	highest := map[string]*big.Int{}
	for _, entry := range checks {
		run := textField(entry, "runId")
		current, seen := highest[run]
		if !seen {
			current = big.NewInt(-1)
		}
		if candidate := attempt(entry); candidate.Cmp(current) > 0 {
			current = candidate
		}
		highest[run] = current
	}
	isRequired := func(name string) bool {
		for _, r := range required {
			if r == name {
				return true
			}
		}
		return false
	}
	answers := func(entry any, name string) bool {
		wanted := providers[name]
		if len(wanted) == 0 {
			return true
		}
		provider := textField(entry, "provider")
		for _, one := range wanted {
			if provider == one {
				return true
			}
		}
		return false
	}
	// Every newest required non-success is read before the answer is chosen, and the answer is
	// checks_not_run only when every one of them is a job the runner never picked up. With more
	// than one required check, returning on whichever the forge enumerated first would let the
	// same set of results read as checks_not_run or as checks_stale by accident of order, and one
	// required check that stands unexplained says the commit was tested and failed, whatever the
	// others say. The named run is the lowest (run, name) pair, so the detail does not move with
	// the enumeration either (CRW-661).
	var firstRun, firstName string
	var firstConclusion any
	unexplained := false
	var notRunKey, notRunRun, notRunName string
	var notRunConclusion any
	var notRunNames []string
	var lightKey, lightRun, lightName string
	for _, entry := range checks {
		run := textField(entry, "runId")
		if attempt(entry).Cmp(highest[run]) != 0 {
			continue
		}
		o, _ := Object(entry)
		if o.Get("headSha") != any(head) {
			return stale("check run "+pyvalue.StrRepr(run)+" reports head "+pyvalue.Repr(o.Get("headSha"))+", not "+pyvalue.StrRepr(head), run)
		}
		name := textField(entry, "name")
		if !isRequired(name) || !answers(entry, name) {
			continue
		}
		// A required check whose own run attempt holds a go-product test leg that concluded
		// success without running its tests is not evidence, even when the required check itself
		// succeeded -- which is the shape CI light mode produces, where dev-gate is green and the
		// five test legs skipped their work (CRW-824). The answer is the existing checks_stale: a
		// light leg makes the run no merge evidence, never a rerun. The lowest (run, name) pair is
		// kept, so the detail does not move with the enumeration (CRW-661).
		if skipped := testSkippedJobs(checks, workflowRun(run), highest); len(skipped) > 0 {
			// Another run of the same required check on this head that ran its tests is the
			// evidence: the lane's repair for a light run is a labeled full run on the same head.
			if !testedElsewhere(checks, head, name, textField(entry, "provider"), workflowRun(run), highest) {
				if key := run + "\x00" + name; lightKey == "" || key < lightKey {
					lightKey, lightRun, lightName = key, run, skipped[0]
				}
			}
		}
		if o.Get("conclusion") == "success" {
			continue
		}
		if firstRun == "" {
			firstRun, firstName, firstConclusion = run, name, o.Get("conclusion")
		}
		// A required check that did not succeed because its runner never picked the jobs up is
		// told apart from one that failed: the lane may rerun the newest run's failed jobs once
		// on the same head instead of returning its turn. The reading is not a merge condition,
		// so the answer stays not ready either way (CRW-661).
		jobs := notRunJobs(checks, workflowRun(run), attempt(entry))
		if len(jobs) == 0 {
			unexplained = true
			continue
		}
		// A job that began a step and failed for real, in the same run attempt, is the commit
		// being tested and failing: a not-run sibling cannot make the required failure read as a
		// runner problem, or the lane reruns a real test failure once as if it were one (CRW-676).
		if beganFailureBeside(checks, workflowRun(run), attempt(entry), run, highest) {
			unexplained = true
			continue
		}
		if key := run + "\x00" + name; notRunKey == "" || key < notRunKey {
			notRunKey, notRunRun, notRunName, notRunConclusion, notRunNames = key, run, name, o.Get("conclusion"), jobs
		}
	}
	if lightRun != "" {
		return []Problem{{Code: ChecksStale, Detail: pyvalue.Repr(lightName) + " skipped its tests (CI light mode); label the pull request crw-lane and judge the full run", Incumbent: lightRun}}
	}
	if notRunRun != "" && !unexplained {
		return []Problem{{Code: ChecksNotRun, Detail: "required check " + pyvalue.Repr(notRunName) + " (run " + pyvalue.StrRepr(notRunRun) + ") concluded " + pyvalue.Repr(notRunConclusion) + " on its newest attempt, and workflow run " + pyvalue.StrRepr(workflowRun(notRunRun)) + " holds jobs that began no step, so no runner picked them up rather than the code failing: " + pyvalue.Repr(notRunNames) + ". Rerun the failed jobs of that run once on the same head", Incumbent: notRunRun}}
	}
	if firstRun != "" {
		return stale("required check "+pyvalue.Repr(firstName)+" (run "+pyvalue.StrRepr(firstRun)+") concluded "+pyvalue.Repr(firstConclusion)+" on its newest attempt", firstRun)
	}
	present := map[string]bool{}
	for _, entry := range checks {
		o, _ := Object(entry)
		if attempt(entry).Cmp(highest[textField(entry, "runId")]) == 0 && o.Get("conclusion") == "success" && answers(entry, textField(entry, "name")) {
			present[textField(entry, "name")] = true
		}
	}
	var unanswered []string
	incumbent := ""
	for _, name := range required {
		for _, provider := range providers[name] {
			found := false
			for _, entry := range checks {
				if textField(entry, "name") == name && textField(entry, "provider") == provider && attempt(entry).Cmp(highest[textField(entry, "runId")]) == 0 {
					o, _ := Object(entry)
					found = found || o.Get("conclusion") == "success"
				}
			}
			if !found {
				if len(unanswered) == 0 {
					incumbent = name
				}
				unanswered = append(unanswered, name+" (integration "+provider+")")
			}
		}
	}
	if len(unanswered) > 0 {
		return stale("these required checks have no successful run from the integration the branch rule names: "+pyvalue.Repr(unanswered), incumbent)
	}
	var missing []string
	for _, name := range required {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		qualified := ""
		for _, name := range missing {
			if len(providers[name]) > 0 {
				qualified = " from the provider the branch rule names"
			}
		}
		return stale("these checks were declared required and are not present and successful in the restated set"+qualified+": "+pyvalue.Repr(missing), missing[0])
	}
	if len(required) == 0 && len(present) == 0 {
		return stale("no check declared required and nothing in the restated set succeeded on "+pyvalue.StrRepr(head)+", so nothing says this head is green", "")
	}
	return nil
}

// HandoffProblems applies shape first, then the child-side review, review-state and check rules.
func HandoffProblems(head string, review, checks, required any, providers map[string][]string, reviews []any) []Problem {
	headPtr := &head
	if malformed := ShapeProblems(review, checks, required, headPtr); len(malformed) > 0 {
		return malformed
	}
	checkItems, _ := List(checks)
	var names []string
	if required != nil {
		items, _ := List(required)
		names = make([]string, len(items))
		for i, one := range items {
			names[i] = one.(string)
		}
	}
	return append(append(ReviewProblems(review), ReviewStateProblems(reviews)...), ChecksProblemsWith(head, names, checkItems, true, providers)...)
}

// ReviewStateProblems grades the latest decisive review state per author.
func ReviewStateProblems(reviews []any) []Problem {
	type state struct{ when, value string }
	latest := map[string]state{}
	for _, raw := range reviews {
		_, ok := Object(raw)
		if !ok {
			continue
		}
		entry := Dict(raw, false)
		author, value := forgeText(entry["author"]), strings.ToUpper(forgeText(entry["state"]))
		if value != "APPROVED" && value != "CHANGES_REQUESTED" && value != "DISMISSED" {
			continue
		}
		when := forgeText(entry["submittedAt"])
		if old, ok := latest[author]; !ok || when >= old.when {
			latest[author] = state{when, value}
		}
	}
	var asking []string
	for author, one := range latest {
		if one.value == "CHANGES_REQUESTED" {
			asking = append(asking, author)
		}
	}
	if len(asking) == 0 {
		return nil
	}
	slices.Sort(asking)
	quoted := make([]string, len(asking))
	for i, one := range asking {
		quoted[i] = quote.Value(one)
	}
	return []Problem{{Code: ReviewChangesRequested, Detail: "these reviewers asked for changes and have not since approved or dismissed their own review: " + strings.Join(quoted, ", ")}}
}

// CandidateProblems is mergeevidence.candidate_problems.
func CandidateProblems(candidate any, strictBase bool) []Problem {
	o, ok := Object(candidate)
	if !ok {
		return []Problem{{Code: Malformed, Detail: "the candidate is an object stating state, isDraft and mergeStateStatus, not " + quote.Kind(candidate)}}
	}
	var out []Problem
	state := strings.ToLower(forgeText(o.Get("state")))
	if pyvalue.Truthy(o.Get("merged")) {
		out = append(out, Problem{Code: CandidateNotOpen, Detail: "this pull request is already merged, so there is nothing left to hand over"})
	} else if state != "" && state != "open" {
		out = append(out, Problem{Code: CandidateNotOpen, Detail: "this pull request is " + quote.Value(state) + ", not open"})
	} else if state == "" {
		out = append(out, Problem{Code: CandidateUnknown, Detail: "the candidate does not say whether it is open"})
	}
	if pyvalue.Truthy(o.Get("isDraft")) {
		out = append(out, Problem{Code: CandidateDraft, Detail: "the pull request is still a draft, so the review it reports was never actually requested; mark it ready for review before handing it over"})
	}
	status := strings.ToLower(textField(candidate, "mergeStateStatus"))
	switch status {
	case "clean", "has_hooks", "unstable":
	case "dirty":
		out = append(out, Problem{Code: CandidateConflicted, Detail: "the candidate does not merge cleanly into its base"})
	case "blocked":
		out = append(out, Problem{Code: CandidateBlocked, Detail: "the forge reports this candidate blocked by its own branch rules, so something it requires is not satisfied yet"})
	case "behind":
		if strictBase {
			out = append(out, Problem{Code: CandidateBehind, Detail: "the candidate is behind its base and this branch requires branches to be current before merging"})
		}
	case "draft":
		if !pyvalue.Truthy(o.Get("isDraft")) {
			out = append(out, Problem{Code: CandidateDraft, Detail: "the forge reports this candidate as a draft although the record says it is not"})
		}
	default:
		out = append(out, Problem{Code: CandidateUnknown, Detail: "the forge reports merge state " + quote.Value(o.Get("mergeStateStatus")) + ", which is not a state this rule recognises; an unrecognised merge state is unknown rather than clean, because a forge that adds one must not acquire a passing verdict by default"})
	}
	return out
}
