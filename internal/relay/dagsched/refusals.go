package dagsched

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// refuse is a refusal under an existing reason (D-02: this build registers no new reason). The relay's exit code is 2 and
// the reason is machine readable; the closed scheduler reason, when there is one, travels in the detail.
func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &store.RefusedError{Reason: string(reason), Detail: fmt.Sprintf(format, args...)}
}

// orderedString is a string field of an ordered object, or "" when the key is absent or not a string.
func orderedString(o contract.OrderedObject, key string) string {
	for _, f := range o {
		if f.Key == key {
			s, _ := f.Value.(string)
			return s
		}
	}
	return ""
}

// refusalOfFinding is the existing refusal reason of a violated path of contract 4.4 (BlockedPaths); a reason with no row there is a disposition conflict. The
// closed reason travels in the detail.
func refusalOfFinding(f BlockedFinding) error {
	for _, p := range BlockedPaths {
		if p.Reason == f.Reason {
			return refuse(contract.RefusalReason(p.Refusal), "%s %s: %s", f.Code, f.Reason, f.Detail)
		}
	}
	return refuse(contract.RefusalDispositionConflict, "%s: %s", f.Reason, f.Detail)
}

// refusalOfReading is what a release of a node that is not ready answers: the existing reason that means the same, with the closed reason in the detail.
func refusalOfReading(n NodeReading) error {
	switch n.Reason {
	case SkipAlreadyOwned:
		return refuse(contract.RefusalDuplicateAssignment, "%s (%s): %s", n.NodeID, n.Reason, n.Detail)
	case DeferCapacityUnmeasured:
		return refuse(contract.RefusalCapacityUnmeasured, "%s (%s): %s", n.NodeID, n.Reason, n.Detail)
	case DeferHostMemory:
		// the host has no room for another child: the existing reason that means a bound is reached (D-02), with the closed reason and the values in the detail
		return refuse(contract.RefusalCapacityExhausted, "%s (%s): %s", n.NodeID, n.Reason, n.Detail)
	case DeferEditOverlap:
		return refuse(contract.RefusalRegionOverlap, "%s (%s): %s", n.NodeID, n.Reason, n.Detail)
	}
	if strings.HasPrefix(n.Reason, "blocked:") {
		code := ""
		for _, p := range BlockedPaths {
			if p.Reason == n.Reason {
				code = p.Code
				break
			}
		}
		return refusalOfFinding(BlockedFinding{Code: code, Reason: n.Reason, Detail: n.Detail})
	}
	return refuse(contract.RefusalDispositionConflict, "%s is not ready (%s): %s", n.NodeID, n.Reason, n.Detail)
}

func refuseCandidateMoved(format string, args ...any) error {
	return refuse(contract.RefusalMergeCandidateMoved, format, args...)
}

func refuseEvidenceMalformed(format string, args ...any) error {
	return refuse(contract.RefusalMergeEvidenceMalformed, format, args...)
}
