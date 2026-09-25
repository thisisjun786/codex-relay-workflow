package settings

import (
	"bytes"
	"encoding/json"
	"sort"
)

const observationLimits = "These are the settings the host reported at this observation, not a guarantee about the dispatched turn. The bridge holds no host-side exclusivity, so another client can change a thread's settings between the observation and turn/start. A matching value means the host recorded the request; it is not evidence that a provider honours it."

func Observed(response map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"approvalPolicy", "cwd", "model", "reasoningEffort", "runtimeWorkspaceRoots", "sandbox"} {
		if v := response[k]; v != nil {
			if k == "sandbox" {
				if n := Normalise(v); n != nil {
					v = n
				}
			}
			out[k] = v
		}
	}
	return out
}
func (c Contract) Findings(response map[string]any) []Finding {
	policy := response["approvalPolicy"]
	if policy == nil {
		return []Finding{{Unobservable, "approvalPolicy", c.approval(), nil}}
	}
	if policy != c.approval() {
		if _, ok := policy.(string); !ok {
			policy = "granular"
		}
		return []Finding{{UnsupportedApproval, "approvalPolicy", c.approval(), policy}}
	}
	asked, seen := c.Requested(), Observed(response)
	found := []Finding{}
	for _, field := range []string{"sandbox", "cwd", "runtimeWorkspaceRoots", "model", "reasoningEffort"} {
		expected, ok := asked[field]
		if !ok {
			continue
		}
		returned, present := seen[field]
		if !present {
			found = append(found, Finding{Unobservable, field, expected, nil})
			continue
		}
		if field == "sandbox" {
			exp := Normalise(expected)
			got := Normalise(returned)
			if got == nil {
				found = append(found, Finding{NotPreserved, field, exp, returned})
				continue
			}
			if c.ExpectedPolicy == nil {
				exp = map[string]any{"type": exp["type"]}
				got = map[string]any{"type": got["type"]}
			}
			expected, returned = exp, got
		}
		if !jsonEqual(expected, returned) {
			found = append(found, Finding{NotPreserved, field, expected, returned})
		}
	}
	return found
}

// jsonEqual is Python's == over decoded JSON values: a []string and the []any a host echoes
// are the same list. Values that cannot be encoded are never equal to anything.
func jsonEqual(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(left, right)
}

func (c Contract) Receipt(response map[string]any, at string) map[string]any {
	findings := c.Findings(response)
	asked := c.Requested()
	verified := []string{}
	unobservable := []string{}
	verification := "not_requested"
	if len(findings) > 0 {
		verification = "refused"
	} else if len(asked) > 0 {
		verification = "observed_at_" + at
		for k := range asked {
			verified = append(verified, k)
		}
		sort.Strings(verified)
	}
	for _, f := range findings {
		if f.Code == Unobservable {
			unobservable = append(unobservable, f.Field)
		}
	}
	sort.Strings(unobservable)
	return map[string]any{"requested": asked, "actual": Observed(response), "verified": verified, "unobservable": unobservable, "findings": findings, "verification": verification, "observationLimits": observationLimits}
}
