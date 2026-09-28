package routing

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CommandRecord encodes the Python dict construction order. Stored JSON remains canonical;
// replies retain their own order rather than applying one global key ranking.
func CommandRecord(command string, value any) any {
	if command == "product-show" {
		m := object(value)
		products := []any{}
		for _, v := range list(m["products"]) {
			p := object(v)
			entry := contract.OrderedObject{}
			for _, key := range sortedKeys(p) {
				if key != "coverage" && key != "bindings" {
					entry = append(entry, contract.Field{Key: key, Value: sortedRecord(p[key])})
				}
			}
			coverage := contract.OrderedObject{}
			for _, surface := range Surfaces {
				coverage = append(coverage, contract.Field{Key: surface, Value: orderRecord(object(p["coverage"])[surface], "state method reason", "")})
			}
			entry = append(entry, contract.Field{Key: "coverage", Value: coverage}, contract.Field{Key: "bindings", Value: sortedRecord(p["bindings"])})
			products = append(products, entry)
		}
		return contract.OrderedObject{{Key: "products", Value: products}, {Key: "policy", Value: sortedRecord(m["policy"])}}
	}
	orders := map[string]string{
		"product-register": "schema product workspace team familyLabel repositories surfaces triageProject testTarget redecided projectsRevised",
		"product-bind":     "schema product kind title components test observedAt source ref state goal project symptoms fixRef followUpOf redecided projectsQueued",
		"product-show":     "products policy",
		"route-policy":     "schema policy enabled minIndependentFixes requireSharedGoal requireCompletionCriteria basis withdrawn",
		"route-intake":     "faultId product workspace disposition stage project owner hold unverifiedCause recorded publication reason forwardedFrom",
		"route-classify":   "faultId successor replayed changed",
		"route-reconcile":  "queued bound read next",
		"route-show":       "routes projects attention next limits",
		"route-digest":     "quiet severe decisions resolutions routine linked projectsBound proposalsUnreached read next limits",
		"route-projects":   "queued skipped cancelled reason",
		"completion-check": "subject product origin verdict checks recurrences recorded",
	}
	return orderRecord(value, orders[command], command)
}
func sortedRecord(value any) any {
	if m, ok := value.(map[string]any); ok {
		out := contract.OrderedObject{}
		for _, k := range sortedKeys(m) {
			out = append(out, contract.Field{Key: k, Value: sortedRecord(m[k])})
		}
		return out
	}
	if values, ok := value.([]Object); ok {
		out := []any{}
		for _, v := range values {
			out = append(out, sortedRecord(v))
		}
		return out
	}
	if values, ok := value.([]any); ok {
		out := []any{}
		for _, v := range values {
			out = append(out, sortedRecord(v))
		}
		return out
	}
	return value
}
func orderRecord(value any, order, parent string) any {
	if values, ok := value.([]Object); ok {
		out := []any{}
		for _, v := range values {
			out = append(out, orderRecord(v, order, parent))
		}
		return out
	}
	if values, ok := value.([]any); ok {
		out := []any{}
		for _, v := range values {
			out = append(out, orderRecord(v, order, parent))
		}
		return out
	}
	m, ok := value.(map[string]any)
	if !ok {
		return value
	}
	keys := strings.Fields(order)
	out := contract.OrderedObject{}
	seen := map[string]bool{}
	add := func(key string) {
		v, ok := m[key]
		if !ok {
			return
		}
		seen[key] = true
		childOrder, childParent := "", key
		switch key {
		case "products":
			childOrder = "schema product workspace team familyLabel repositories surfaces triageProject testTarget coverage bindings"
		case "bindings":
			childOrder = "schema product kind title components test observedAt source ref state goal project symptoms fixRef followUpOf"
		case "surfaces":
			childParent = "surfaceMap"
		case "coverage":
			childOrder = "dev_run verification user_report real_use"
			childParent = "coverageMap"
		case "testTarget":
			childOrder = "team project"
		case "policy":
			childOrder = "schema policy enabled minIndependentFixes requireSharedGoal requireCompletionCriteria basis"
		case "redecided":
			childOrder = "faultId disposition project owner hold"
		case "projectsRevised":
			childOrder = "cancelled queued"
		case "projectsQueued":
			childOrder = "goal faultId trigger members"
		case "queued":
			if parent == "route-projects" || parent == "projectsRevised" {
				childOrder = "goal faultId trigger members"
			} else {
				childOrder = "kind label state toFault toIssue"
			}
		case "skipped":
			childOrder = "goal reasons"
		case "cancelled":
			childOrder = "faultId publicationId reasons"
		case "unverifiedCause":
			childOrder = "faultId product why"
		case "publication":
			childOrder = "publicationId kind trigger queued awaitingTarget awaitingRecord reason"
		case "routes", "projects":
			childOrder = "faultId product workspace disposition stage hold unverifiedCause project owner team origin classification supersededBy detail attention ledger"
		case "classification":
			childOrder = "product component symptom goal by at"
		case "goal":
			childOrder = "key criteria"
		case "ledger":
			childOrder = "state severity occurrences issue linkState linkedProject"
		case "severe", "decisions", "resolutions":
			childOrder = "faultId product disposition stage state severity claimedSeverity occurrences issue project owner hold unverifiedCause origin detail decision"
		case "linked":
			childOrder = "kind label state toFault toIssue"
		case "routine":
			childParent = "routineMap"
		case "checks":
			childOrder = "check verdict reason closure"
		case "closure":
			childOrder = "ready missing fix verification exception"
		case "fix", "verification":
			childOrder = "ref source"
		case "recurrences":
			childOrder = "check verdict faultId reason"
		case "recorded":
			childOrder = "check faultId recorded ledgerState"
		case "followUpOf":
			childOrder = "issue checks"
		}
		if parent == "surfaceMap" {
			childOrder = "method active"
		}
		if parent == "coverageMap" {
			childOrder = "state method reason"
		}
		if parent == "routineMap" {
			childOrder = "records newOccurrences"
		}
		out = append(out, contract.Field{Key: key, Value: orderRecord(v, childOrder, childParent)})
	}
	for _, key := range keys {
		add(key)
	}
	rest := []string{}
	for key := range m {
		if !seen[key] {
			rest = append(rest, key)
		}
	}
	slices.Sort(rest)
	for _, key := range rest {
		add(key)
	}
	return out
}
