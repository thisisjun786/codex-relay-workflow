package evidence

import (
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

const (
	Ready                     = "ready"
	NotReady                  = "not_ready"
	Stale                     = "stale"
	UnknownVerdict            = "unknown"
	EnumerationTruncated      = "enumeration_truncated"
	EnumerationNotProgressing = "enumeration_not_progressing"
	EnumerationTotalMoved     = "enumeration_total_moved"
	EnumerationDuplicated     = "enumeration_duplicated"
	EnumerationCountDisagrees = "enumeration_count_disagrees"
	ReviewSetUnstable         = "review_set_unstable"
	CandidateMoved            = "candidate_moved"
	GatesMoved                = "gates_moved"
	BaseRefMissing            = "base_ref_missing"
	UnreadableCode            = "unreadable"
	RequiredGateConflict      = "required_gate_conflict"
	LateFinding               = "late_finding"
	RecordInvalid             = "record_invalid"
)

var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
var branchPattern = regexp.MustCompile(`^[^\s?#%:` + "`" + `^\\]+$`)

type ForgeUsage struct{ Detail string }

func (e *ForgeUsage) Error() string { return e.Detail }
func SplitRepository(value any) (string, string, error) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if value == nil {
		text = ""
	}
	if strings.Count(text, "/") != 1 {
		return "", "", &ForgeUsage{"a repository is written owner/name, not " + pyvalue.Repr(value)}
	}
	owner, name, _ := strings.Cut(text, "/")
	if !ownerPattern.MatchString(owner) || !namePattern.MatchString(name) {
		return "", "", &ForgeUsage{"a repository is written owner/name over the characters GitHub allows, not " + pyvalue.Repr(value)}
	}
	return owner, name, nil
}
func PullRequestNumber(value any) (any, error) {
	text := strings.TrimSpace(fmt.Sprint(value))
	if value == nil {
		text = ""
	}
	n, ok := value.(*big.Int)
	if !ok {
		n, ok = pyvalue.ParseInt(text)
	}
	if !ok {
		return 0, &ForgeUsage{"a pull request number is a positive whole number, not " + pyvalue.Repr(value)}
	}
	if n.Sign() < 1 {
		return 0, &ForgeUsage{"a pull request number is counted from one, not " + pyvalue.Repr(value)}
	}
	if n.IsInt64() {
		return int(n.Int64()), nil
	}
	return n, nil
}

func countReached(count int, limit int64) bool {
	return int64(count) >= limit
}

func BranchRef(value any) (string, error) {
	text := strings.TrimSpace(forgeText(value))
	if text == "" || strings.Contains(text, "..") || strings.HasPrefix(text, "/") || !branchPattern.MatchString(text) {
		return "", &ForgeUsage{"a branch name is a path segment without traversal or query characters, not " + pyvalue.Repr(value)}
	}
	return text, nil
}

type Page struct {
	Items []any
	Total any
	Next  any
}
type Enumeration struct {
	Name               string
	Items, Identifiers []any
	Pages              []map[string]any
	Total              any
	Complete           bool
	Problems           []Problem
}

func (e Enumeration) Record() map[string]any {
	distinct := map[string]bool{}
	for _, id := range e.Identifiers {
		if pyvalue.Truthy(id) {
			distinct[HashKey(id)] = true
		}
	}
	return map[string]any{"connection": e.Name, "pagesRead": len(e.Pages), "totalCount": e.Total, "distinct": len(distinct), "complete": e.Complete, "pages": e.Pages}
}
func EnumerateConnection(name string, budget int64, step func(any) (Page, error), identify func(any) any) (Enumeration, error) {
	found := Enumeration{Name: name}
	var token any
	used := []any{}
	for {
		if countReached(len(found.Pages), budget) {
			found.Problems = append(found.Problems, Problem{Code: EnumerationTruncated, Detail: fmt.Sprintf("the %s connection was still unfinished after %s pages, so what was read is a prefix; a prefix of a list is not a count of the list", name, pyvalue.Str(budget))})
			return found, nil
		}
		page, err := step(token)
		if err != nil {
			return found, err
		}
		total := page.Total
		found.Pages = append(found.Pages, map[string]any{"token": token, "returned": len(page.Items), "totalCount": total})
		if total != nil {
			if found.Total == nil {
				found.Total = total
			} else if !pyvalue.ItemEqual(total, found.Total) {
				found.Problems = append(found.Problems, Problem{Code: EnumerationTotalMoved, Detail: fmt.Sprintf("the %s connection reported %s items and then %s while it was being read, so no single set of them was ever observed", name, pyvalue.Str(found.Total), pyvalue.Str(total))})
			}
		}
		for _, item := range page.Items {
			found.Items = append(found.Items, item)
			found.Identifiers = append(found.Identifiers, identify(item))
		}
		if page.Next == nil {
			found.Complete = true
			break
		}
		repeated := pyvalue.ItemEqual(page.Next, token)
		for _, previous := range used {
			repeated = repeated || pyvalue.ItemEqual(previous, page.Next)
		}
		if repeated {
			found.Problems = append(found.Problems, Problem{Code: EnumerationNotProgressing, Detail: "the " + name + " connection handed back a page token it had already used, so the read was going in a circle and its page count is not evidence of distinct pages"})
			return found, nil
		}
		used = append(used, page.Next)
		token = page.Next
	}
	distinct := map[string]bool{}
	blank := false
	for _, id := range found.Identifiers {
		if !pyvalue.Truthy(id) {
			blank = true
		} else {
			distinct[HashKey(id)] = true
		}
	}
	if blank {
		found.Problems = append(found.Problems, Problem{Code: EnumerationDuplicated, Detail: "an item of the " + name + " connection came back with no identifier, so it cannot be told apart from another"})
	}
	if len(distinct) != len(found.Identifiers) {
		found.Problems = append(found.Problems, Problem{Code: EnumerationDuplicated, Detail: "the " + name + " connection returned the same identifier more than once, so the number of entries is not the number of items"})
	}
	if found.Total != nil && !pyvalue.ItemEqual(len(distinct), found.Total) {
		found.Problems = append(found.Problems, Problem{Code: EnumerationCountDisagrees, Detail: fmt.Sprintf("the %s connection says it holds %s items and %d distinct ones were read", name, pyvalue.Str(found.Total), len(distinct))})
	}
	return found, nil
}
func VerdictOf(problems []Problem) string {
	codes := map[string]bool{}
	for _, p := range problems {
		codes[p.Code] = true
	}
	for _, c := range []string{CandidateMoved, GatesMoved, BaseRefMissing} {
		if codes[c] {
			return Stale
		}
	}
	for _, c := range []string{EnumerationTruncated, EnumerationNotProgressing, EnumerationTotalMoved, EnumerationDuplicated, EnumerationCountDisagrees, ReviewSetUnstable, UnreadableCode, CandidateUnknown} {
		if codes[c] {
			return UnknownVerdict
		}
	}
	if len(problems) > 0 {
		return NotReady
	}
	return Ready
}
func ProviderMap(value any) map[string][]string {
	out := map[string][]string{}
	for _, f := range providerObject(value) {
		out[f.Key] = f.Value.([]string)
	}
	return out
}

// providerObject is forge._provider_map, retaining the input dict's insertion
// order for caller-visible repr while normalizing each integration list.
func providerObject(value any) contract.OrderedObject {
	o, _ := Object(value)
	out := contract.OrderedObject{}
	for _, f := range o {
		var names []string
		if list, ok := List(f.Value); ok {
			for _, one := range list {
				s := pyvalue.Str(one)
				if strings.TrimSpace(s) != "" {
					names = append(names, s)
				}
			}
		} else if f.Value != nil && strings.TrimSpace(pyvalue.Str(f.Value)) != "" {
			names = []string{pyvalue.Str(f.Value)}
		}
		slices.Sort(names)
		names = slices.Compact(names)
		if len(names) > 0 {
			out = append(out, contract.Field{Key: f.Key, Value: names})
		}
	}
	return out
}
