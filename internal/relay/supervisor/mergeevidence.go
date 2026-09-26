// Subset ported for todo 26 (mergeevidence.py review_shape_problems, review_problems and
// checks_problems as the merge turn calls them, providers=None); todo 24 owns and extends this.

package supervisor

import "strings"

// Problem codes, mergeevidence.py:32-36.
const (
	Malformed        = "malformed_evidence"
	ReviewUnstated   = "review_unstated"
	ReviewIncomplete = "review_incomplete"
	ChecksStale      = "checks_stale"
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
		bad("the review record is an object stating " + strings.Join(ReviewFields, ", ") + ", not a " + TypeName(review))
		return problems
	}
	if v, present := Lookup(o, "hasNextPage"); present {
		if _, isBool := v.(bool); !isBool {
			bad("hasNextPage is true or false, not a " + TypeName(v) + "; a truthy value of another type says nothing about pagination")
		}
	}
	for _, name := range counts {
		v, present := Lookup(o, name)
		if !present {
			continue
		}
		if n, isInt := PyInt(v); !isInt {
			bad(name + " is a whole number, not a " + TypeName(v))
		} else if n < 0 {
			bad(name + " is " + Repr(v) + ", and a count is never negative")
		}
	}
	if seen, present := Lookup(o, "threadsSeen"); present {
		items, isList := List(seen)
		if !isList {
			bad("threadsSeen is a list of thread identifiers, not a " + TypeName(seen) + "; a string would be counted one character at a time")
		} else {
			for position, one := range items {
				if _, isStr := one.(string); !isStr {
					bad("threadsSeen entry " + Text(int64(position)) + " is a thread identifier string, not a " + TypeName(one) + "; coercing it would let two different values agree")
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
	if Truthy(Get(o, "hasNextPage")) {
		incomplete("hasNextPage is still true, so the review was not enumerated")
	}
	if pages := intOrZero(Get(o, "pagesRead")); pages < 1 {
		incomplete("no review page was read")
	}
	seen, _ := List(Get(o, "threadsSeen"))
	total := intOrZero(Get(o, "totalCount"))
	var identifiers []string
	for _, one := range seen {
		text := ""
		if Truthy(one) {
			text = Text(one)
		}
		if strings.TrimSpace(text) != "" {
			identifiers = append(identifiers, Text(one))
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
	if int64(len(distinct)) != total {
		incomplete("totalCount is " + Text(total) + " and " + Text(int64(len(seen))) + " threads were seen")
	}
	if intOrZero(Get(o, "unresolved")) != 0 {
		incomplete(Text(Get(o, "unresolved")) + " threads are unresolved")
	}
	return problems
}

// intOrZero is int(v or 0) for a value the shape check already passed.
func intOrZero(v any) int64 {
	if !Truthy(v) {
		return 0
	}
	n, _ := IntOf(v)
	return n
}

// attempt is int(entry.get("attempt", 1) or 1).
func attempt(entry any) int64 {
	o, _ := Object(entry)
	v, present := Lookup(o, "attempt")
	if !present || !Truthy(v) {
		return 1
	}
	n, _ := IntOf(v)
	return n
}

func textField(entry any, name string) string {
	o, _ := Object(entry)
	v, present := Lookup(o, name)
	if !present {
		return ""
	}
	return Text(v)
}

// ChecksProblems is mergeevidence.checks_problems with require_declared=False and no
// providers, as the merge turn calls it. At most one problem.
func ChecksProblems(head string, required []string, checks []any) []Problem {
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
	highest := map[string]int64{}
	for _, entry := range checks {
		run := textField(entry, "runId")
		current, seen := highest[run]
		if !seen {
			current = -1
		}
		highest[run] = max(current, attempt(entry))
	}
	isRequired := func(name string) bool {
		for _, r := range required {
			if r == name {
				return true
			}
		}
		return false
	}
	for _, entry := range checks {
		run := textField(entry, "runId")
		if attempt(entry) != highest[run] {
			continue
		}
		o, _ := Object(entry)
		if Get(o, "headSha") != any(head) {
			return stale("check run "+StrRepr(run)+" reports head "+Repr(Get(o, "headSha"))+", not "+StrRepr(head), run)
		}
		if isRequired(textField(entry, "name")) && Get(o, "conclusion") != "success" {
			return stale("required check "+Repr(Get(o, "name"))+" (run "+StrRepr(run)+") concluded "+Repr(Get(o, "conclusion"))+" on its newest attempt", run)
		}
	}
	present := map[string]bool{}
	for _, entry := range checks {
		o, _ := Object(entry)
		if attempt(entry) == highest[textField(entry, "runId")] && Get(o, "conclusion") == "success" {
			present[textField(entry, "name")] = true
		}
	}
	var missing []string
	for _, name := range required {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return stale("these checks were declared required and are not present and successful in the restated set: "+Repr(missing), missing[0])
	}
	if len(required) == 0 && len(present) == 0 {
		return stale("no check declared required and nothing in the restated set succeeded on "+StrRepr(head)+", so nothing says this head is green", "")
	}
	return nil
}
