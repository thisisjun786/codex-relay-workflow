package evidence

import (
	"encoding/json"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"math/big"
	"strings"
	"testing"
)

func details(problems []Problem) []string {
	out := make([]string, len(problems))
	for i, p := range problems {
		out[i] = p.Detail
	}
	return out
}

// Every refusal the merge-evidence command prints on a malformed handoff names a value the way Go
// does: a kind such as "a string" or "an object", a string in double quotes, any other value as
// its JSON. These texts are returned to the caller and never stored (merge-turn records only the
// ReviewProblems and ChecksProblems wording).
func TestReturnedOnlyMessagesNameValuesAsGoDoes(t *testing.T) {
	head := " "
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"review is not an object", details(ReviewShapeProblems("x")), []string{
			"the review record is an object stating " + strings.Join(ReviewFields, ", ") + ", not a string"}},
		{"review fields of the wrong kind", details(ReviewShapeProblems(contract.OrderedObject{
			{Key: "hasNextPage", Value: json.Number("1")},
			{Key: "unresolved", Value: []any{}},
			{Key: "threadsSeen", Value: json.Number("1")},
		})), []string{
			"hasNextPage is true or false, not a number; a truthy value of another type says nothing about pagination",
			"unresolved is a whole number, not an array",
			"threadsSeen is a list of thread identifiers, not a number; a string would be counted one character at a time"}},
		{"thread identifier of the wrong kind", details(ReviewShapeProblems(contract.OrderedObject{{Key: "threadsSeen", Value: []any{json.Number("3")}}})), []string{
			"threadsSeen entry 0 is a thread identifier string, not a number; coercing it would let two different values agree"}},
		{"shape of head, required and checks", details(ShapeProblems(contract.OrderedObject{}, nil, "x", &head)), []string{
			"the head this evidence is about is a non-empty commit sha, not \" \"",
			"the required check names are a list, not a string",
			"the restated checks are a list of check runs, not null"}},
		{"required name and check entries", details(ShapeProblems(contract.OrderedObject{}, []any{"x", contract.OrderedObject{
			{Key: "runId", Value: json.Number("7")}, {Key: "attempt", Value: "2"}}}, []any{json.Number("1")}, nil)), []string{
			"required check name 1 is a string, not a number",
			"check entry 0 is an object naming its runId, name, headSha, conclusion and attempt, not a string",
			"check entry 1 states runId as a number, not a string; coercing it would let two different runs agree",
			"check entry 1 states attempt \"2\", which is not a whole number, so which attempt is newest cannot be decided"}},
		{"candidate is not an object", details(CandidateProblems([]any{}, false)), []string{
			"the candidate is an object stating state, isDraft and mergeStateStatus, not an array"}},
		{"candidate with a state the rule does not know", details(CandidateProblems(contract.OrderedObject{{Key: "state", Value: "closed"}, {Key: "mergeStateStatus", Value: "wobbly"}}, false))[:1], []string{
			"this pull request is \"closed\", not open"}},
		{"handoff record is not an object", details(RestateProblems("abc", []any{}, map[string]any{})), []string{
			"a handoff record is an object, not an array"}},
	} {
		if strings.Join(c.got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", c.name, c.got, c.want)
		}
	}
}

// A repository, a pull request number and a branch name that cannot be read are named as the
// caller wrote them: a string in double quotes, a number as its digits.
func TestForgeUsageNamesTheValueAsGoQuotesIt(t *testing.T) {
	_, _, err := SplitRepository("two words")
	if err == nil || err.Error() != "a repository is written owner/name, not \"two words\"" {
		t.Errorf("SplitRepository: %v", err)
	}
	_, _, err = SplitRepository("a b/c")
	if err == nil || err.Error() != "a repository is written owner/name over the characters GitHub allows, not \"a b/c\"" {
		t.Errorf("SplitRepository characters: %v", err)
	}
	if _, err = PullRequestNumber("seven"); err == nil || err.Error() != "a pull request number is a positive whole number, not \"seven\"" {
		t.Errorf("PullRequestNumber text: %v", err)
	}
	if _, err = PullRequestNumber(big.NewInt(-5)); err == nil || err.Error() != "a pull request number is counted from one, not -5" {
		t.Errorf("PullRequestNumber big: %v", err)
	}
	if _, err = BranchRef("a..b"); err == nil || err.Error() != "a branch name is a path segment without traversal or query characters, not \"a..b\"" {
		t.Errorf("BranchRef: %v", err)
	}
}
