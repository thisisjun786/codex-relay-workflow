package mergeturn

import (
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Python prints every answer here in its dicts' insertion order (json.dumps(indent=2)), and a
// Go map has none. Each shape the engine answers with is listed in the order mergeturn.py
// builds it; PythonOrder picks the first shape holding every key of a map. More specific
// shapes come first, because a peer or a blockedBy is a subset of a turn record's keys.
var shapes = [][]string{
	// _blocked_report readyPeers / withheldPeers
	{"turnId", "holderTaskId", "candidateHead", "prNumber", "reason"},
	// declare_ready blockedBy
	{"state", "turnId"},
	// a grant envelope: json.loads of sort_keys evidence, then the three read-time fields
	{"baseRef", "candidateHead", "grantId", "grantedFrom", "kind", "recipientTaskId", "repository", "sequence", "targetKey", "tenure", "turnId", "wake", "recordedAt", "acknowledgedAt", "acknowledgedBy"},
	// ledger()
	{"entryId", "kind", "fromState", "toState", "evidenceKind", "actorTaskId", "evidence", "idempotencyKey", "recordedAt"},
	// restatement_envelope + actorTaskId, recordedAt
	{"sequence", "from", "to", "evidence", "source", "actorTaskId", "recordedAt"},
	// a reading: TargetReader.tip
	{"sha", "source", "reference", "repository"},
	// land / resolve_unknown / release
	{"released", "outcome", "promoted", "baseObservation", "ledger"},
	// target()
	{"targetKey", "repository", "baseRef", "holder", "waiters", "nextReady", "occupied", "conflicts", "returnRequestedAt", "transportAcceptedAt", "releasedAt", "blocked"},
	// _blocked_report
	{"cause", "turnId", "holderTaskId", "candidateHead", "prNumber", "checkSnapshots", "lastResult", "lastRefusal", "lastCheckedHead", "ambiguousRestatements", "readyPeers", "withheldPeers"},
	// cmd_merge_turn_show --parent-task
	{"parentTaskId", "claims"},
	// _record, turn()'s additions, then what each caller appends
	{"turnId", "targetKey", "repository", "baseRef", "projectKey", "holderTaskId", "holderHostId", "relationshipId", "prNumber", "candidateHead", "declaredReady", "state", "tenure", "landedSha", "observedBaseSha", "checkedBaseSha", "closeReason", "requestedAt", "heldAt", "mergingAt", "closedAt", "updatedAt",
		"ledger", "grant", "unreadableGrants", "baseRestatements", "unreadableRestatements",
		"alreadyClaimed", "blockedBy", "checkId", "requiredDeclared", "headVerifiedAgainst", "baseObservation", "restated", "targetFree", "heldBackBy"},
}

// PythonOrder turns an answer of this package into ordered objects and lists, as Python
// prints it.
func PythonOrder(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		order := shapeFor(keys, x)
		slices.SortStableFunc(keys, func(a, b string) int {
			i, j := slices.Index(order, a), slices.Index(order, b)
			if i < 0 {
				i = len(order)
			}
			if j < 0 {
				j = len(order)
			}
			return i - j
		})
		out := make(contract.OrderedObject, 0, len(keys))
		for _, k := range keys {
			out = append(out, contract.Field{Key: k, Value: PythonOrder(x[k])})
		}
		return out
	case contract.OrderedObject:
		out := make(contract.OrderedObject, len(x))
		for i, f := range x {
			out[i] = contract.Field{Key: f.Key, Value: PythonOrder(f.Value)}
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = PythonOrder(item)
		}
		return out
	case []contract.OrderedObject:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = PythonOrder(item)
		}
		return out
	case []string:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = item
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = PythonOrder(item)
		}
		return out
	}
	return v
}

func shapeFor(keys []string, m map[string]any) []string {
	// A turn record carries "ledger" while the grant it holds does not; the ledger shape is
	// told apart from the record by entryId. Anything unmatched keeps sorted order.
	_, record := m["declaredReady"]
	for _, shape := range shapes {
		if record && shape[0] != "turnId" {
			continue
		}
		if record && len(shape) < 20 {
			continue
		}
		all := true
		for _, k := range keys {
			if !slices.Contains(shape, k) {
				all = false
				break
			}
		}
		if all {
			return shape
		}
	}
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	return sorted
}
