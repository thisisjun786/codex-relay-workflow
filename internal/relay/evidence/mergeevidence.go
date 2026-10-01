// Subset ported for todo 26 (mergeevidence.py review_shape_problems, review_problems and
// checks_problems as the merge turn calls them, providers=None); todo 24 owns and extends this.

package evidence

import (
	"math/big"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// Problem codes, mergeevidence.py:32-36.
const (
	Malformed              = "malformed_evidence"
	ReviewUnstated         = "review_unstated"
	ReviewIncomplete       = "review_incomplete"
	ChecksStale            = "checks_stale"
	RequiredUndeclared     = "required_undeclared"
	ReviewChangesRequested = "review_changes_requested"
	CandidateNotOpen       = "candidate_not_open"
	CandidateDraft         = "candidate_draft"
	CandidateConflicted    = "candidate_conflicted"
	CandidateBlocked       = "candidate_blocked"
	CandidateBehind        = "candidate_behind"
	CandidateUnknown       = "candidate_unknown"
)

// ReviewFields is REVIEW_FIELDS.
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
		bad("the review record is an object stating " + strings.Join(ReviewFields, ", ") + ", not a " + pyvalue.TypeName(review))
		return problems
	}
	if v, present := Lookup(o, "hasNextPage"); present {
		if _, isBool := v.(bool); !isBool {
			bad("hasNextPage is true or false, not a " + pyvalue.TypeName(v) + "; a truthy value of another type says nothing about pagination")
		}
	}
	for _, name := range counts {
		v, present := Lookup(o, name)
		if !present {
			continue
		}
		if n, isInt := Whole(v); !isInt {
			bad(name + " is a whole number, not a " + pyvalue.TypeName(v))
		} else if n.Sign() < 0 {
			bad(name + " is " + pyvalue.Repr(v) + ", and a count is never negative")
		}
	}
	if seen, present := Lookup(o, "threadsSeen"); present {
		items, isList := List(seen)
		if !isList {
			bad("threadsSeen is a list of thread identifiers, not a " + pyvalue.TypeName(seen) + "; a string would be counted one character at a time")
		} else {
			for position, one := range items {
				if _, isStr := one.(string); !isStr {
					bad("threadsSeen entry " + pyvalue.Str(int64(position)) + " is a thread identifier string, not a " + pyvalue.TypeName(one) + "; coercing it would let two different values agree")
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
		if _, present := Lookup(o, name); !present {
			unstated = append(unstated, Problem{Code: ReviewUnstated, Detail: "the review record does not state " + name})
		}
	}
	if len(unstated) > 0 {
		return unstated
	}
	var problems []Problem
	incomplete := func(detail string) { problems = append(problems, Problem{Code: ReviewIncomplete, Detail: detail}) }
	if pyvalue.Truthy(Get(o, "hasNextPage")) {
		incomplete("hasNextPage is still true, so the review was not enumerated")
	}
	if pages := intOrZero(Get(o, "pagesRead")); pages.Sign() < 1 {
		incomplete("no review page was read")
	}
	seen, _ := List(Get(o, "threadsSeen"))
	if text, ok := Get(o, "threadsSeen").(string); ok {
		seen = make([]any, 0, len([]rune(text)))
		for _, one := range text {
			seen = append(seen, string(one))
		}
	}
	total := intOrZero(Get(o, "totalCount"))
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
	if intOrZero(Get(o, "unresolved")).Sign() != 0 {
		incomplete(pyvalue.Str(Get(o, "unresolved")) + " threads are unresolved")
	}
	return problems
}

// intOrZero is int(v or 0) for a value the shape check already passed.
func intOrZero(v any) *big.Int { return Integer(Or(v, 0)) }

// attempt is int(entry.get("attempt", 1) or 1).
func attempt(entry any) *big.Int {
	o, _ := Object(entry)
	v, present := Lookup(o, "attempt")
	if !present || !pyvalue.Truthy(v) {
		return big.NewInt(1)
	}
	return Integer(v)
}

func textField(entry any, name string) string {
	o, _ := Object(entry)
	v, present := Lookup(o, name)
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
		bad("the head this evidence is about is a non-empty commit sha, not " + pyvalue.Repr(*head))
	}
	problems = append(problems, ReviewShapeProblems(review)...)
	if required != nil {
		items, ok := List(required)
		if !ok {
			bad("the required check names are a list, not a " + pyvalue.TypeName(required))
		} else {
			for _, one := range items {
				if _, ok := one.(string); !ok {
					bad("required check name " + pyvalue.Repr(one) + " is a string, not a " + pyvalue.TypeName(one))
				}
			}
		}
	}
	items, ok := List(checks)
	if !ok {
		bad("the restated checks are a list of check runs, not a " + pyvalue.TypeName(checks))
		return problems
	}
	for position, entry := range items {
		o, ok := Object(entry)
		where := "check entry " + pyvalue.Str(int64(position))
		if !ok {
			bad(where + " is an object naming its runId, name, headSha, conclusion and attempt, not a " + pyvalue.TypeName(entry))
			continue
		}
		for _, field := range []string{"runId", "name", "headSha", "conclusion"} {
			if value, present := Lookup(o, field); present {
				if _, ok := value.(string); !ok {
					bad(where + " states " + field + " as a " + pyvalue.TypeName(value) + ", not a string; coercing it would let two different runs agree")
				}
			}
		}
		value, present := Lookup(o, "attempt")
		if !present {
			bad(where + " does not state attempt, so which attempt is newest cannot be decided; an omitted attempt is not evidence that this is the newest")
		} else if n, ok := Whole(value); !ok {
			bad(where + " states attempt " + pyvalue.Repr(value) + ", which is not a whole number, so which attempt is newest cannot be decided")
		} else if n.Sign() < 1 {
			bad(where + " states attempt " + pyvalue.Repr(value) + ", and attempts are counted from one; a lower value is read as the first attempt and hides the newest one")
		}
	}
	return problems
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
	for _, entry := range checks {
		run := textField(entry, "runId")
		if attempt(entry).Cmp(highest[run]) != 0 {
			continue
		}
		o, _ := Object(entry)
		if Get(o, "headSha") != any(head) {
			return stale("check run "+pyvalue.StrRepr(run)+" reports head "+pyvalue.Repr(Get(o, "headSha"))+", not "+pyvalue.StrRepr(head), run)
		}
		if isRequired(textField(entry, "name")) && answers(entry, textField(entry, "name")) && Get(o, "conclusion") != "success" {
			return stale("required check "+pyvalue.Repr(Get(o, "name"))+" (run "+pyvalue.StrRepr(run)+") concluded "+pyvalue.Repr(Get(o, "conclusion"))+" on its newest attempt", run)
		}
	}
	present := map[string]bool{}
	for _, entry := range checks {
		o, _ := Object(entry)
		if attempt(entry).Cmp(highest[textField(entry, "runId")]) == 0 && Get(o, "conclusion") == "success" && answers(entry, textField(entry, "name")) {
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
					found = found || Get(o, "conclusion") == "success"
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
		quoted[i] = pyvalue.StrRepr(one)
	}
	return []Problem{{Code: ReviewChangesRequested, Detail: "these reviewers asked for changes and have not since approved or dismissed their own review: " + strings.Join(quoted, ", ")}}
}

// CandidateProblems is mergeevidence.candidate_problems.
func CandidateProblems(candidate any, strictBase bool) []Problem {
	o, ok := Object(candidate)
	if !ok {
		return []Problem{{Code: Malformed, Detail: "the candidate is an object stating state, isDraft and mergeStateStatus, not a " + pyvalue.TypeName(candidate)}}
	}
	var out []Problem
	state := strings.ToLower(forgeText(Get(o, "state")))
	if pyvalue.Truthy(Get(o, "merged")) {
		out = append(out, Problem{Code: CandidateNotOpen, Detail: "this pull request is already merged, so there is nothing left to hand over"})
	} else if state != "" && state != "open" {
		out = append(out, Problem{Code: CandidateNotOpen, Detail: "this pull request is " + pyvalue.StrRepr(state) + ", not open"})
	} else if state == "" {
		out = append(out, Problem{Code: CandidateUnknown, Detail: "the candidate does not say whether it is open"})
	}
	if pyvalue.Truthy(Get(o, "isDraft")) {
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
		if !pyvalue.Truthy(Get(o, "isDraft")) {
			out = append(out, Problem{Code: CandidateDraft, Detail: "the forge reports this candidate as a draft although the record says it is not"})
		}
	default:
		out = append(out, Problem{Code: CandidateUnknown, Detail: "the forge reports merge state " + pyvalue.Repr(Get(o, "mergeStateStatus")) + ", which is not a state this rule recognises; an unrecognised merge state is unknown rather than clean, because a forge that adds one must not acquire a passing verdict by default"})
	}
	return out
}
